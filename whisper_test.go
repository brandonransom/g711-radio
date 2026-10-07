package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestShouldAutoTranscribeClipLengthWindow(t *testing.T) {
	tests := []struct {
		name       string
		cfg        *whisperConfig
		durationMs int
		want       bool
	}{
		{
			name:       "nil config never auto-transcribes",
			cfg:        nil,
			durationMs: 30000,
			want:       false,
		},
		{
			name:       "unset minimum disables automatic transcription",
			cfg:        &whisperConfig{},
			durationMs: 30000,
			want:       false,
		},
		{
			name:       "shorter than minimum is skipped",
			cfg:        &whisperConfig{AutoTranscribeMinClipMs: 20000},
			durationMs: 19999,
			want:       false,
		},
		{
			name:       "exactly the minimum qualifies",
			cfg:        &whisperConfig{AutoTranscribeMinClipMs: 20000},
			durationMs: 20000,
			want:       true,
		},
		{
			name:       "no maximum means no upper bound",
			cfg:        &whisperConfig{AutoTranscribeMinClipMs: 20000},
			durationMs: 60 * 60 * 1000,
			want:       true,
		},
		{
			name:       "exactly the maximum qualifies",
			cfg:        &whisperConfig{AutoTranscribeMinClipMs: 20000, AutoTranscribeMaxClipMs: 120000},
			durationMs: 120000,
			want:       true,
		},
		{
			name:       "longer than maximum is skipped",
			cfg:        &whisperConfig{AutoTranscribeMinClipMs: 20000, AutoTranscribeMaxClipMs: 120000},
			durationMs: 120001,
			want:       false,
		},
		{
			name:       "inside the window qualifies",
			cfg:        &whisperConfig{AutoTranscribeMinClipMs: 20000, AutoTranscribeMaxClipMs: 120000},
			durationMs: 45000,
			want:       true,
		},
		{
			// Contradictory bounds can't be satisfied; startup logs a warning.
			name:       "maximum below minimum excludes everything",
			cfg:        &whisperConfig{AutoTranscribeMinClipMs: 60000, AutoTranscribeMaxClipMs: 30000},
			durationMs: 45000,
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldAutoTranscribe(tt.cfg, tt.durationMs); got != tt.want {
				t.Fatalf("shouldAutoTranscribe(%+v, %d) = %v, want %v", tt.cfg, tt.durationMs, got, tt.want)
			}
		})
	}
}

// A negative maximum is normalized to zero ("no upper bound") rather than
// silently rejecting every clip.
func TestSetDefaultsNormalizesNegativeAutoTranscribeBounds(t *testing.T) {
	cfg := &whisperConfig{AutoTranscribeMinClipMs: -1, AutoTranscribeMaxClipMs: -1}
	cfg.setDefaults()

	if cfg.AutoTranscribeMinClipMs != 0 {
		t.Fatalf("AutoTranscribeMinClipMs = %d, want 0", cfg.AutoTranscribeMinClipMs)
	}
	if cfg.AutoTranscribeMaxClipMs != 0 {
		t.Fatalf("AutoTranscribeMaxClipMs = %d, want 0", cfg.AutoTranscribeMaxClipMs)
	}

	withMin := &whisperConfig{AutoTranscribeMinClipMs: 20000, AutoTranscribeMaxClipMs: -5}
	withMin.setDefaults()
	if !shouldAutoTranscribe(withMin, 10*60*1000) {
		t.Fatal("a negative maximum should behave as no upper bound")
	}
}

// --- whisper-server integration ---------------------------------------------

const fakeWhisperServerEnv = "G711_FAKE_WHISPER_SERVER"

// TestMain lets the test binary double as a fake whisper.cpp whisper-server,
// so local mode's process supervision can be tested without whisper.cpp.
func TestMain(m *testing.M) {
	if os.Getenv(fakeWhisperServerEnv) == "1" {
		runFakeWhisperServer()
		return
	}
	os.Exit(m.Run())
}

func runFakeWhisperServer() {
	args := os.Args[1:]
	var host, port string
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "--host":
			host = args[i+1]
		case "--port":
			port = args[i+1]
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/inference", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"text": "args=" + strings.Join(args, " ") + " beam_size=" + r.FormValue("beam_size"),
		})
	})
	_ = http.ListenAndServe(net.JoinHostPort(host, port), mux)
}

func writeTestWAV(t *testing.T) string {
	t.Helper()
	wav, err := encodePCM16WAV(make([]int16, recSampleRate/4), recSampleRate)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "clip.wav")
	if err := os.WriteFile(path, wav, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func newTestPool(t *testing.T, cfg whisperConfig) (*whisperPool, chan transcriptEvent) {
	t.Helper()
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	cfg.setDefaults()
	hub := newTranscriptHub("", "", log.New(io.Discard, "", 0))
	_, events := hub.subscribe()
	pool := newWhisperPool(cfg, hub, log.New(io.Discard, "", 0))
	t.Cleanup(pool.Close)
	if err := pool.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return pool, events
}

// awaitTranscript returns the next real transcript, skipping the
// "transcribing" status events the pool now emits when a clip is picked up.
func awaitTranscript(t *testing.T, events chan transcriptEvent, timeout time.Duration) transcriptEvent {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev := <-events:
			if ev.Type == "transcribing" {
				continue
			}
			return ev
		case <-deadline:
			t.Fatal("timed out waiting for a transcript")
			return transcriptEvent{}
		}
	}
}

func TestWhisperConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     whisperConfig
		wantErr string
	}{
		{name: "local mode", cfg: whisperConfig{ModelPath: "m.bin", ServerArgs: []string{"-t", "4", "-fa"}}},
		{name: "remote mode", cfg: whisperConfig{RemoteServers: []string{"gpu:8080", "https://gpu:8081/"}}},
		{name: "typed inference params", cfg: whisperConfig{InferenceParams: map[string]any{"beam_size": 5.0, "prompt": "x", "suppress_nst": true}}},
		{name: "legacy binaryPath", cfg: whisperConfig{LegacyBinaryPath: "whisper-cli"}, wantErr: "serverBinaryPath"},
		{name: "legacy remoteHost", cfg: whisperConfig{LegacyRemoteHost: "gpu:8090"}, wantErr: "remoteServers"},
		{name: "reserved server arg", cfg: whisperConfig{ServerArgs: []string{"--port", "1"}}, wantErr: "--port"},
		{name: "reserved server arg with =", cfg: whisperConfig{ServerArgs: []string{"--model=x"}}, wantErr: "--model"},
		{name: "reserved inference param", cfg: whisperConfig{InferenceParams: map[string]any{"response_format": "text"}}, wantErr: "response_format"},
		{name: "unsupported param type", cfg: whisperConfig{InferenceParams: map[string]any{"x": []any{1.0}}}, wantErr: "inferenceParams.x"},
		{name: "bad remote URL scheme", cfg: whisperConfig{RemoteServers: []string{"ftp://gpu"}}, wantErr: "http"},
		{name: "empty remote URL", cfg: whisperConfig{RemoteServers: []string{" "}}, wantErr: "empty"},
		{name: "ports overflow", cfg: whisperConfig{LocalBasePort: 65535, Workers: 2}, wantErr: "65535"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestInferenceFormFields(t *testing.T) {
	fields := inferenceFormFields(map[string]any{
		"beam_size":    5.0,
		"temperature":  0.2,
		"language":     "es",
		"suppress_nst": true,
	})
	want := map[string]string{
		"beam_size":       "5",
		"temperature":     "0.2",
		"language":        "es", // user value overrides the default
		"suppress_nst":    "true",
		"best_of":         "5", // whisper-cli default kept
		"no_timestamps":   "true",
		"response_format": "verbose_json",
	}
	for key, value := range want {
		if fields[key] != value {
			t.Errorf("field %s = %q, want %q", key, fields[key], value)
		}
	}
}

func TestRemoteTranscriptionSendsInferenceRequest(t *testing.T) {
	type received struct {
		form    map[string]string
		fileLen int
	}
	got := make(chan received, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/inference":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("parse form: %v", err)
			}
			form := map[string]string{}
			for k, v := range r.MultipartForm.Value {
				form[k] = v[0]
			}
			file, _, err := r.FormFile("file")
			n := 0
			if err == nil {
				data, _ := io.ReadAll(file)
				n = len(data)
			}
			got <- received{form: form, fileLen: n}
			_, _ = w.Write([]byte(`{"text":" Engine 4 responding.\n"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	pool, events := newTestPool(t, whisperConfig{
		RemoteServers:   []string{srv.URL},
		InferenceParams: map[string]any{"beam_size": 5.0, "prompt": "Forest Service radio"},
	})
	wavPath := writeTestWAV(t)
	pool.Submit(transcriptJob{info: streamInfo{StreamName: "Admin Net"}, clipID: "c1", wavPath: wavPath})

	ev := awaitTranscript(t, events, 5*time.Second)
	if ev.Text != "Engine 4 responding." || ev.ClipID != "c1" {
		t.Fatalf("event = %+v", ev)
	}
	req := <-got
	wav, _ := os.ReadFile(wavPath)
	if req.fileLen != len(wav) {
		t.Errorf("uploaded %d bytes, want %d", req.fileLen, len(wav))
	}
	for key, value := range map[string]string{
		"beam_size": "5", "prompt": "Forest Service radio",
		"language": "en", "no_timestamps": "true", "response_format": "verbose_json",
	} {
		if req.form[key] != value {
			t.Errorf("form %s = %q, want %q", key, req.form[key], value)
		}
	}
}

func TestRemoteTranscriptionReportsServerError(t *testing.T) {
	// whisper-server reports some failures as a 200 with an error body.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"error":"failed to read audio data"}`))
	}))
	defer srv.Close()

	pool, events := newTestPool(t, whisperConfig{RemoteServers: []string{srv.URL}})
	pool.Submit(transcriptJob{clipID: "c1", wavPath: writeTestWAV(t)})
	if ev := awaitTranscript(t, events, 5*time.Second); ev.Text != "[transcription failed]" {
		t.Fatalf("text = %q, want failure marker", ev.Text)
	}
}

func TestRemoteTranscriptionFailsOverToHealthyServer(t *testing.T) {
	// A listener that is closed immediately gives a URL that refuses connections.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadURL := "http://" + ln.Addr().String()
	ln.Close()

	var served atomic.Int32
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/inference" {
			served.Add(1)
		}
		_, _ = w.Write([]byte(`{"text":"ok"}`))
	}))
	defer healthy.Close()

	pool, events := newTestPool(t, whisperConfig{RemoteServers: []string{deadURL, healthy.URL}})
	for i := 0; i < 4; i++ {
		pool.Submit(transcriptJob{clipID: fmt.Sprintf("c%d", i), wavPath: writeTestWAV(t)})
	}
	for i := 0; i < 4; i++ {
		if ev := awaitTranscript(t, events, 5*time.Second); ev.Text != "ok" {
			t.Fatalf("clip %s text = %q, want ok", ev.ClipID, ev.Text)
		}
	}
	if served.Load() != 4 {
		t.Fatalf("healthy server handled %d clips, want 4", served.Load())
	}
}

func TestLocalModeLaunchesSupervisedWhisperServers(t *testing.T) {
	t.Setenv(fakeWhisperServerEnv, "1")
	model := filepath.Join(t.TempDir(), "ggml-test.bin")
	if err := os.WriteFile(model, []byte("model"), 0o644); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	basePort := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	pool, events := newTestPool(t, whisperConfig{
		ServerBinaryPath: os.Args[0],
		ModelPath:        model,
		Workers:          2,
		LocalBasePort:    basePort,
		ServerArgs:       []string{"-t", "3", "-fa"},
		InferenceParams:  map[string]any{"beam_size": 4.0},
	})
	pool.Submit(transcriptJob{clipID: "c1", wavPath: writeTestWAV(t)})
	pool.Submit(transcriptJob{clipID: "c2", wavPath: writeTestWAV(t)})

	for i := 0; i < 2; i++ {
		ev := awaitTranscript(t, events, 20*time.Second)
		for _, want := range []string{"-m " + model, "--host 127.0.0.1", "-t 3 -fa", "beam_size=4"} {
			if !strings.Contains(ev.Text, want) {
				t.Fatalf("transcript %q missing %q", ev.Text, want)
			}
		}
	}

	pool.Close()
	for _, ep := range pool.endpoints {
		if err := checkWhisperHealth(context.Background(), ep.baseURL, time.Second); err == nil {
			t.Errorf("%s still serving after Close", ep.baseURL)
		}
	}
}

func TestConfigExampleLoads(t *testing.T) {
	cfg, err := loadConfig("config.example.json")
	if err != nil {
		t.Fatalf("loadConfig(config.example.json): %v", err)
	}
	if !cfg.Whisper.enabled() || cfg.Whisper.isRemote() {
		t.Fatalf("example whisper block should configure local mode: %+v", cfg.Whisper)
	}
	disabled := map[string]bool{}
	for _, sg := range cfg.streamGroups {
		for _, sub := range sg.SubGroups {
			for _, s := range sub.Streams {
				disabled[s.StreamName] = s.DisableAutoTranscribe
			}
		}
	}
	if !disabled["Forest Net (Repeater)"] || disabled["Forest Net"] {
		t.Fatalf("disableAutoTranscribe not parsed per stream: %+v", disabled)
	}
}

func TestConfigLegacyRegionsKey(t *testing.T) {
	const streams = `{"Oregon": {"Umatilla NF": [{"streamName": "Pomeroy", "udpPort": 5004}]}}`
	write := func(body string) string {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	cfg, err := loadConfig(write(`{"httpPort": 8080, "regions": ` + streams + `}`))
	if err != nil {
		t.Fatalf("legacy regions key: %v", err)
	}
	if len(cfg.streamGroups) != 1 || cfg.streamGroups[0].StateName != "Oregon" {
		t.Fatalf("legacy regions key not loaded as states: %+v", cfg.streamGroups)
	}

	if _, err := loadConfig(write(`{"httpPort": 8080, "states": ` + streams + `, "regions": ` + streams + `}`)); err == nil {
		t.Fatal("expected an error when both states and regions are set")
	}
}

func TestWhisperPoolBulkPriorityAndDedupe(t *testing.T) {
	p := newWhisperPool(whisperConfig{}, nil, log.New(io.Discard, "", 0))
	job := func(name string) transcriptJob {
		return transcriptJob{clipID: name, wavPath: filepath.Join("audio", name+".wav")}
	}
	if got := p.SubmitBulk(job("b1")); got != bulkQueued {
		t.Fatalf("SubmitBulk b1 = %v, want queued", got)
	}
	if got := p.SubmitBulk(job("b1")); got != bulkDuplicate {
		t.Fatalf("SubmitBulk duplicate = %v, want duplicate", got)
	}
	p.SubmitBulk(job("b2"))
	if !p.Submit(job("n1")) {
		t.Fatal("Submit n1 should queue")
	}
	// Requesting a bulk-queued clip directly promotes it ahead of the backlog.
	if p.Submit(job("b2")) {
		t.Fatal("Submit b2 should report it was already queued")
	}
	if got := p.queueDepth(); got != 3 {
		t.Fatalf("queueDepth = %d, want 3", got)
	}
	var order []string
	for i := 0; i < 3; i++ {
		j, _, ok := p.nextJob()
		if !ok {
			t.Fatal("nextJob returned !ok")
		}
		order = append(order, j.clipID)
	}
	if strings.Join(order, ",") != "n1,b2,b1" {
		t.Fatalf("order = %v, want n1,b2,b1", order)
	}
	// Taken jobs may be queued again.
	if got := p.SubmitBulk(job("b1")); got != bulkQueued {
		t.Fatalf("re-SubmitBulk b1 = %v, want queued", got)
	}
	for i := 1; i < maxBulkQueue; i++ {
		p.SubmitBulk(job(fmt.Sprintf("x%d", i)))
	}
	if got := p.SubmitBulk(job("overflow")); got != bulkFull {
		t.Fatalf("SubmitBulk past cap = %v, want full", got)
	}
}
