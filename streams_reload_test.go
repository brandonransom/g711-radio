package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0644); err != nil {
		t.Fatal(err)
	}
}

func TestSplitConfigExampleMatchesInline(t *testing.T) {
	dir := t.TempDir()
	copyFile(t, "config.split.example.json", filepath.Join(dir, "config.json"))
	copyFile(t, "streams.example.json", filepath.Join(dir, "streams.json"))

	split, err := loadConfig(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("split example: %v", err)
	}
	inline, err := loadConfig("config.example.json")
	if err != nil {
		t.Fatalf("inline example: %v", err)
	}
	if !reflect.DeepEqual(split.streamGroups, inline.streamGroups) || split.totalStreams != inline.totalStreams {
		t.Fatal("split example streams differ from the inline example")
	}
	if split.streamSource().streamsFile() != filepath.Join(dir, "streams.json") {
		t.Fatalf("streams file resolved to %q", split.streamSource().streamsFile())
	}
	if inline.streamSource().streamsFile() != "config.example.json" {
		t.Fatalf("inline source = %q", inline.streamSource().streamsFile())
	}
}

func TestStreamsConfigValidation(t *testing.T) {
	const streams = `{"Oregon": {"Umatilla NF": [{"streamName": "Pomeroy", "udpPort": 5004}]}}`
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	write("streams.json", `{"states": `+streams+`}`)

	if _, err := loadConfig(write("both.json", `{"httpPort": 8080, "streamsFile": "streams.json", "states": `+streams+`}`)); err == nil {
		t.Error("expected an error when both streamsFile and states are set")
	}
	write("bad-streams.json", `{"states": `+streams+`, "httpPort": 1}`)
	if _, err := loadConfig(write("bad.json", `{"httpPort": 8080, "streamsFile": "bad-streams.json"}`)); err == nil {
		t.Error("expected unknown fields in the streams file to be rejected")
	}
	if _, err := loadConfig(write("missing.json", `{"httpPort": 8080, "streamsFile": "nope.json"}`)); err == nil {
		t.Error("expected a missing streams file to be an error")
	}
	dup := `{"Oregon": {"Umatilla NF": [{"streamName": "Pomeroy", "udpPort": 5004}, {"streamName": " Pomeroy ", "udpPort": 5005}]}}`
	if _, err := loadConfig(write("dup.json", `{"httpPort": 8080, "states": `+dup+`}`)); err == nil || !strings.Contains(err.Error(), "duplicate streamName") {
		t.Errorf("duplicate stream names: err = %v", err)
	}
	write("legacy-streams.json", `{"regions": `+streams+`}`)
	cfg, err := loadConfig(write("legacy.json", `{"httpPort": 8080, "streamsFile": "legacy-streams.json"}`))
	if err != nil || cfg.totalStreams != 1 {
		t.Errorf("legacy regions in streams file: %v (streams=%d)", err, cfg.totalStreams)
	}
}

func TestSettingsFingerprint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	write := func(body string) {
		if err := os.WriteFile(path, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	src := streamSource{configFile: path}
	write(`{"httpPort": 8080, "states": {"A": {}}}`)
	a, _ := src.settingsFingerprint()
	write(`{ "states": {"B": {"G": []}},   "httpPort": 8080 }`)
	b, _ := src.settingsFingerprint()
	if a == "" || a != b {
		t.Error("inline stream or formatting edits should not count as a settings change")
	}
	write(`{"httpPort": 8443, "states": {"B": {}}}`)
	if c, _ := src.settingsFingerprint(); c == b {
		t.Error("httpPort change not detected")
	}
	split := streamSource{configFile: path, streamsPath: "streams.json"}
	write(`{"httpPort": 8443, "states": {"A": {}}}`)
	d, _ := split.settingsFingerprint()
	write(`{"httpPort": 8443, "states": {"Z": {}}}`)
	if e, _ := split.settingsFingerprint(); d == e {
		t.Error("with a streams file, any config.json edit is a settings change")
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func freeUDPPorts(t *testing.T, n int) []int {
	t.Helper()
	conns := make([]net.PacketConn, n)
	ports := make([]int, n)
	for i := range conns {
		c, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		conns[i] = c
		ports[i] = c.LocalAddr().(*net.UDPAddr).Port
	}
	for _, c := range conns {
		_ = c.Close()
	}
	return ports
}

func portInUse(port int) bool {
	c, err := net.ListenPacket("udp", fmt.Sprintf(":%d", port))
	if err != nil {
		return true
	}
	_ = c.Close()
	return false
}

func newReloadTestServer(t *testing.T) (*webrtcServer, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	s := &webrtcServer{
		logger:       log.New(logs, "", 0),
		streams:      map[string]*station{},
		streamsByKey: map[string]*station{},
		clips:        map[string]clipRecord{},
		clipJobs:     make(chan func(), 16),
		runtime: stationRuntime{
			ctx:           ctx,
			stop:          func() { t.Error("ingest error stopped the server") },
			frameDuration: 20 * time.Millisecond,
		},
	}
	t.Cleanup(func() {
		stations := s.stationList()
		cancel()
		for _, st := range stations {
			<-st.done
		}
	})
	return s, logs
}

func statesOf(streams ...streamConfig) []configuredState {
	return []configuredState{{StateName: "Oregon", SubGroups: []configuredSubGroup{{GroupName: "Umatilla NF", Streams: streams}}}}
}

func stationNamed(s *webrtcServer, name string) *station {
	s.streamsMu.RLock()
	defer s.streamsMu.RUnlock()
	return s.streamsByKey[streamKey("Oregon", "Umatilla NF", name)]
}

func TestApplyStreamsReconciles(t *testing.T) {
	s, _ := newReloadTestServer(t)
	p := freeUDPPorts(t, 3)

	if failed := s.applyStreams(statesOf(
		streamConfig{StreamName: "Keep", UDPPort: p[0]},
		streamConfig{StreamName: "Change", UDPPort: p[1], TimeZone: "UTC"},
		streamConfig{StreamName: "Remove", UDPPort: p[2]},
	), true); failed != 0 {
		t.Fatalf("initial apply: %d failed", failed)
	}
	keep, change, remove := stationNamed(s, "Keep"), stationNamed(s, "Change"), stationNamed(s, "Remove")
	if keep == nil || change == nil || remove == nil {
		t.Fatal("initial stations missing")
	}

	// The removed stream's port moves to a new stream in the same edit.
	if failed := s.applyStreams(statesOf(
		streamConfig{StreamName: "Keep", UDPPort: p[0]},
		streamConfig{StreamName: "Change", UDPPort: p[1], TimeZone: "America/Denver"},
		streamConfig{StreamName: "Added", UDPPort: p[2]},
	), false); failed != 0 {
		t.Fatalf("reload: %d failed", failed)
	}

	if stationNamed(s, "Keep") != keep {
		t.Error("unchanged stream was restarted")
	}
	select {
	case <-keep.done:
		t.Error("unchanged stream was stopped")
	default:
	}
	changed := stationNamed(s, "Change")
	if changed == nil || changed == change || changed.info.ID != change.info.ID || changed.info.TimeZone != "America/Denver" {
		t.Errorf("changed stream not restarted under the same ID: %+v", changed)
	}
	select {
	case <-remove.done:
	default:
		t.Error("removed stream still running")
	}
	if _, ok := s.stationByID(remove.info.ID); ok || stationNamed(s, "Remove") != nil {
		t.Error("removed stream still listed")
	}
	added := stationNamed(s, "Added")
	if added == nil || !portInUse(p[2]) {
		t.Fatal("added stream not listening on the freed port")
	}
	if got := s.streamInventory(); len(got) != 3 || got[0].StreamName != "Keep" || got[2].StreamName != "Added" {
		t.Errorf("inventory = %+v", got)
	}

	// Removing everything releases every port.
	s.applyStreams(nil, false)
	for _, port := range p {
		if portInUse(port) {
			t.Errorf("port %d still bound after removal", port)
		}
	}
	if len(s.stationList()) != 0 || len(s.stateGroupList()) != 0 {
		t.Error("stations remain after removing all streams")
	}
}

func TestApplyStreamsReportsFailedPort(t *testing.T) {
	s, logs := newReloadTestServer(t)
	busy, err := net.ListenPacket("udp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	port := busy.LocalAddr().(*net.UDPAddr).Port
	free := freeUDPPorts(t, 1)[0]

	failed := s.applyStreams(statesOf(
		streamConfig{StreamName: "Busy", UDPPort: port},
		streamConfig{StreamName: "Fine", UDPPort: free},
	), false)
	if failed != 1 || stationNamed(s, "Fine") == nil || stationNamed(s, "Busy") != nil {
		t.Fatalf("failed=%d; one bad port must not block the others", failed)
	}
	if !strings.Contains(logs.String(), `stream "Busy"`) {
		t.Errorf("failure not logged: %s", logs.String())
	}

	busy.Close()
	if failed := s.applyStreams(statesOf(
		streamConfig{StreamName: "Busy", UDPPort: port},
		streamConfig{StreamName: "Fine", UDPPort: free},
	), false); failed != 0 || stationNamed(s, "Busy") == nil {
		t.Fatalf("retry after the port freed: failed=%d", failed)
	}
}

func TestRemovedStreamFlushesClipInProgress(t *testing.T) {
	s, _ := newReloadTestServer(t)
	port := freeUDPPorts(t, 1)[0]
	s.applyStreams(statesOf(streamConfig{StreamName: "Rec", UDPPort: port}), true)
	st := stationNamed(s, "Rec")
	got := make(chan int, 1)
	st.recorder = newRecorderState(time.Hour, time.Hour, func(samples []int16, _ time.Time) { got <- len(samples) })
	st.recorder.Push(make([]int16, 160), time.Now())

	s.applyStreams(nil, false)
	select {
	case n := <-got:
		if n != 160 {
			t.Errorf("flushed %d samples, want 160", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("clip in progress was not finished when its stream was removed")
	}
}

func TestWatchStreamConfigAppliesValidChanges(t *testing.T) {
	s, logs := newReloadTestServer(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	streamsPath := filepath.Join(dir, "streams.json")
	p := freeUDPPorts(t, 2)
	writeStreams := func(body string) {
		if err := os.WriteFile(streamsPath, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	one := fmt.Sprintf(`{"states": {"Oregon": {"Umatilla NF": [{"streamName": "A", "udpPort": %d}]}}}`, p[0])
	two := fmt.Sprintf(`{"states": {"Oregon": {"Umatilla NF": [{"streamName": "A", "udpPort": %d}, {"streamName": "B", "udpPort": %d}]}}}`, p[0], p[1])
	if err := os.WriteFile(cfgPath, []byte(`{"httpPort": 8080, "streamsFile": "streams.json"}`), 0644); err != nil {
		t.Fatal(err)
	}
	writeStreams(one)
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	s.applyStreams(cfg.streamGroups, true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.watchStreamConfig(ctx, cfg.streamSource(), 10*time.Millisecond)

	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s; log:\n%s", what, logs.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	writeStreams(two)
	waitFor("stream B to be added", func() bool { return stationNamed(s, "B") != nil })
	a := stationNamed(s, "A")

	writeStreams(`{"states": {`) // invalid: rejected, streams kept
	waitFor("the invalid file to be rejected", func() bool { return strings.Contains(logs.String(), "rejected") })
	if len(s.stationList()) != 2 || stationNamed(s, "A") != a {
		t.Fatal("an invalid streams file changed the running streams")
	}

	if err := os.WriteFile(cfgPath, []byte(`{"httpPort": 9090, "streamsFile": "streams.json"}`), 0644); err != nil {
		t.Fatal(err)
	}
	waitFor("the restart-required warning", func() bool { return strings.Contains(logs.String(), "restart to apply") })

	writeStreams(one)
	waitFor("stream B to be removed", func() bool { return stationNamed(s, "B") == nil })
	if stationNamed(s, "A") != a {
		t.Error("stream A restarted although unchanged")
	}
}
