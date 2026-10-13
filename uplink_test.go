package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

const testUplinkToken = "test-uplink-token-0123456789abcdef"

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestOriginServesEdgePath(t *testing.T) {
	cases := map[string]bool{
		"/streams":              true,
		"/transcripts/history":  true,
		"/audio/AK/x.wav":       true,
		"/audio/../secret":      false,
		"/audio/a/../../secret": false,
		"/secret":               false,
		"/":                     false,
		"/streams/../secret":    false,
	}
	for p, want := range cases {
		if got := originServesEdgePath(p); got != want {
			t.Errorf("originServesEdgePath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestLiveFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	frames := [][]byte{[]byte("abc"), nil, bytes.Repeat([]byte{7}, 160)}
	for _, f := range frames {
		if err := writeLiveFrame(&buf, f); err != nil {
			t.Fatal(err)
		}
	}
	for i, want := range frames {
		got, err := readLiveFrame(&buf)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("frame %d = %v, want %v", i, got, want)
		}
	}
	if _, err := readLiveFrame(&buf); err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
}

func TestUplinkConfigValidate(t *testing.T) {
	good := uplinkConfig{URL: "https://edge.example.gov", Token: testUplinkToken}
	if err := good.validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	bad := []uplinkConfig{
		{URL: "http://edge.example.gov", Token: testUplinkToken},
		{URL: "https://edge.example.gov/sub", Token: testUplinkToken},
		{URL: "https://edge.example.gov", Token: "short"},
		{URL: "https://edge.example.gov", Token: testUplinkToken, Proxy: "socks5://p:1080"},
	}
	for _, c := range bad {
		if err := c.validate(); err == nil {
			t.Errorf("expected %+v to be rejected", c)
		}
	}
}

// TestEdgeExampleConfigLoads checks config.edge.example.json plus a secrets
// file with edgeToken is a valid edge config, and that uplinks in the
// secrets file reach an origin config.
func TestEdgeExampleConfigLoads(t *testing.T) {
	example, err := os.ReadFile("config.edge.example.json")
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	os.WriteFile(configPath, example, 0o600)
	os.WriteFile(secretsPath, []byte(`{"edgeToken":"`+testUplinkToken+`"}`), 0o600)
	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("example edge config rejected: %v", err)
	}
	if cfg.Edge == nil || cfg.Edge.Token != testUplinkToken || cfg.WebRTCUDPPort != 3478 {
		t.Fatalf("unexpected edge config: %+v", cfg)
	}

	os.WriteFile(configPath, []byte(`{"httpPort":443,"states":{"S":{"G":[{"streamName":"A","udpPort":5000}]}}}`), 0o600)
	os.WriteFile(secretsPath, []byte(`{"uplinks":[{"url":"https://edge.example.gov","token":"`+testUplinkToken+`"}]}`), 0o600)
	cfg, err = loadConfig(configPath)
	if err != nil {
		t.Fatalf("origin config with uplinks rejected: %v", err)
	}
	if len(cfg.Uplinks) != 1 || cfg.Uplinks[0].Token != testUplinkToken {
		t.Fatalf("uplinks not loaded from secrets: %+v", cfg.Uplinks)
	}
}

func TestValidateEdgeConfig(t *testing.T) {
	c := appConfig{Edge: &edgeConfig{Token: testUplinkToken}}
	if err := validateEdgeConfig(&c); err != nil {
		t.Fatalf("valid edge config rejected: %v", err)
	}
	c.AudioLogDir = "audio"
	if err := validateEdgeConfig(&c); err == nil || !strings.Contains(err.Error(), "audioLogDir") {
		t.Fatalf("expected audioLogDir to be rejected, got %v", err)
	}
	c = appConfig{Edge: &edgeConfig{Token: "short"}}
	if err := validateEdgeConfig(&c); err == nil {
		t.Fatal("expected short token to be rejected")
	}
}

// TestEdgeEndToEnd runs an origin and an edge in-process, connected through
// a real TLS uplink, and checks everything the edge mirrors or proxies.
func TestEdgeEndToEnd(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	codec := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: sampleRateHz, Channels: 1}
	frameDuration := 20 * time.Millisecond

	// Origin with one station fed by the test instead of UDP.
	originHub := newTranscriptHub("", "", logger)
	originSt := &station{
		info:          streamInfo{StateName: "Alaska", GroupName: "Tongass", ForestName: "Tongass", ID: "stream-1", StreamName: "Radio 1", UDPPort: 5000},
		codec:         codec,
		frameDuration: frameDuration,
		logger:        logger,
		subscribers:   make(map[string]*subscriber),
		broadcastChan: make(chan media.Sample, 64),
		done:          make(chan struct{}),
		conflictAddr:  "10.1.2.3",
	}
	go originSt.runBroadcaster()
	defer close(originSt.broadcastChan)
	origin := &webrtcServer{
		logger:      logger,
		hub:         originHub,
		streams:     map[string]*station{originSt.info.ID: originSt},
		stateGroups: []stateGroup{{StateName: "Alaska", SubGroups: []subGroup{{GroupName: "Tongass", Streams: []streamInfo{originSt.info}}}}},
	}
	originMux := http.NewServeMux()
	originMux.HandleFunc("/streams", origin.handleStreams)
	originMux.HandleFunc("/stream-status", origin.handleStreamStatus)
	originMux.Handle("/transcripts", originHub)
	originMux.HandleFunc("/audio/", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "xff="+r.Header.Get("X-Forwarded-For"))
	})
	originMux.HandleFunc("/secret", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "leaked")
	})

	// Edge.
	api, err := newWebRTCAPI(appConfig{}, logger)
	if err != nil {
		t.Fatal(err)
	}
	edgeCfg := appConfig{Edge: &edgeConfig{Token: testUplinkToken, StreamsRefreshSeconds: 1, FeedLingerSeconds: 1}}
	edge := newEdgeServer(ctx, edgeCfg, api, codec, frameDuration, nil, logger)
	static := fstest.MapFS{"index.html": {Data: []byte("page")}}
	srv := httptest.NewUnstartedServer(edge.Handler(static, ""))
	srv.StartTLS()
	defer srv.Close()
	defer edge.tunnel.close()
	edge.Run()

	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	client := srv.Client()

	// A wrong token is refused.
	if _, err := dialUplink(ctx, uplinkConfig{URL: srv.URL, Token: "wrong-token-0123456789abcdef", CAFile: caFile}); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected 401 for a bad token, got %v", err)
	}

	go runUplink(ctx, uplinkConfig{URL: srv.URL, Token: testUplinkToken, CAFile: caFile}, origin.edgeHandler(originMux), logger)

	// Stream list is mirrored, without the internal UDP port.
	waitFor(t, "edge stream sync", func() bool { return len(edge.web.stationList()) == 1 })
	resp, err := client.Get(srv.URL + "/streams")
	if err != nil {
		t.Fatal(err)
	}
	var groups []stateGroup
	json.NewDecoder(resp.Body).Decode(&groups)
	resp.Body.Close()
	if len(groups) != 1 || groups[0].SubGroups[0].Streams[0].ID != "stream-1" || groups[0].SubGroups[0].Streams[0].UDPPort != 0 {
		t.Fatalf("unexpected /streams on edge: %+v", groups)
	}

	// Proxied requests carry the visitor's address, not a spoofed one.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/audio/x.wav", nil)
	req.Header.Set("X-Forwarded-For", "6.6.6.6")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "xff=127.0.0.1" {
		t.Fatalf("proxied /audio/ = %q", body)
	}

	// Non-allowlisted origin paths are unreachable through the uplink.
	resp, err = edge.originGet(ctx, "/secret")
	if err == nil {
		resp.Body.Close()
		t.Fatal("origin served a non-allowlisted path to the edge")
	}

	// Stream status hides internal source addresses.
	resp, err = client.Get(srv.URL + "/stream-status")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"hasConflict":true`) || strings.Contains(string(body), "10.1.2.3") {
		t.Fatalf("unexpected /stream-status on edge: %s", body)
	}

	// Transcript events are relayed.
	_, events := edge.hub.subscribe()
	got := false
	for i := 0; i < 200 && !got; i++ {
		originHub.Publish(transcriptEvent{Type: "transcript", StreamID: "stream-1", Text: "hello"})
		select {
		case ev := <-events:
			got = ev.Text == "hello"
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !got {
		t.Fatal("transcript event was not relayed to the edge")
	}

	// Live audio flows only while the edge station has a listener.
	edgeSt, ok := edge.web.stationByID("stream-1")
	if !ok {
		t.Fatal("edge station missing")
	}
	originTaps := func() int {
		originSt.mu.RLock()
		defer originSt.mu.RUnlock()
		return len(originSt.taps)
	}
	if originTaps() != 0 {
		t.Fatal("feed started without a listener")
	}
	edgeSt.mu.Lock()
	edgeSt.subscribers["fake"] = &subscriber{}
	edgeSt.mu.Unlock()
	edgeSt.notifySubscribersChanged()

	_, edgeFrames := edgeSt.addTap()
	frame := bytes.Repeat([]byte{0x55}, frameSizeBytes)
	received := false
	for i := 0; i < 400 && !received; i++ {
		originSt.enqueueBroadcast(media.Sample{Data: frame, Duration: frameDuration})
		select {
		case f := <-edgeFrames:
			received = bytes.Equal(f, frame)
		case <-time.After(25 * time.Millisecond):
		}
	}
	if !received {
		t.Fatal("live audio did not reach the edge station")
	}

	edgeSt.mu.Lock()
	delete(edgeSt.subscribers, "fake")
	edgeSt.mu.Unlock()
	edgeSt.notifySubscribersChanged()
	waitFor(t, "feed to stop after linger", func() bool { return originTaps() == 0 })

	// Health reports the connected origin.
	resp, err = client.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz = %d", resp.StatusCode)
	}
}
