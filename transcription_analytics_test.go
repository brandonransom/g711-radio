package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWhisperResultMetadata(t *testing.T) {
	tests := []struct {
		name, body                       string
		confidence, probability, logprob *float64
	}{
		{name: "text only", body: `{"text":"test"}`},
		{name: "zero is reported", body: `{"confidence":0,"avg_logprob":0}`, confidence: floatPtr(0), logprob: floatPtr(0)},
		{name: "top level takes precedence", body: `{"confidence":0.9,"avg_logprob":-0.2,"segments":[{"confidence":0.1,"avg_logprob":-1}]}`, confidence: floatPtr(0.9), logprob: floatPtr(-0.2)},
		{name: "cpp diagnostics are not confidence", body: `{"segments":[{"avg_logprob":-0.2,"words":[{"probability":0.8},{"probability":0.6}]},{"avg_logprob":-0.6,"words":[{"probability":0.4}]}]}`, probability: floatPtr(0.6), logprob: floatPtr(-0.4)},
		{name: "null and missing do not dilute averages", body: `{"segments":[{},{"confidence":null,"avg_logprob":null},{"confidence":0.8,"avg_logprob":-0.5}]}`, confidence: floatPtr(0.8), logprob: floatPtr(-0.5)},
		{name: "invalid ranges are not scores", body: `{"confidence":80,"avg_logprob":3,"segments":[{"confidence":-1,"avg_logprob":2,"words":[{"probability":-1},{"probability":5}]}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var r whisperResult
			if err := json.Unmarshal([]byte(tt.body), &r); err != nil {
				t.Fatal(err)
			}
			r.summarizeMetadata()
			assertOptionalFloat(t, "confidence", r.Confidence, tt.confidence)
			assertOptionalFloat(t, "token probability", r.TokenProbability, tt.probability)
			assertOptionalFloat(t, "log probability", r.AvgLogprob, tt.logprob)
		})
	}
}

func floatPtr(v float64) *float64 { return &v }

func assertOptionalFloat(t *testing.T, name string, got, want *float64) {
	t.Helper()
	if (got == nil) != (want == nil) {
		t.Fatalf("%s presence = %v, want %v", name, got, want)
	}
	if got != nil && math.Abs(*got-*want) > 1e-9 {
		t.Fatalf("%s = %g, want %g", name, *got, *want)
	}
}

func TestTranscriptionMetricsPersistenceAndRanges(t *testing.T) {
	dir := t.TempDir()
	clock := &fakeClock{t: time.Date(2026, 10, 1, 10, 0, 0, 0, time.Local)}
	a := newTestStore(t, dir, clock)
	a.TranscriptionAttempt(transcriptionAttempt{
		Host: "http://fast", Model: "small (reported)", Outcome: "success",
		Ms: 1000, QueueMs: 500, AudioMs: 10000, Confidence: floatPtr(0),
		TokenProbability: floatPtr(0.8), AvgLogprob: floatPtr(-0.2),
	})
	a.TranscriptionAttempt(transcriptionAttempt{Host: "http://fast", Outcome: "retry", Ms: 30000, QueueMs: 500})
	a.TranscriptionAttempt(transcriptionAttempt{Host: "http://fast", Outcome: "failed", Ms: 30000, QueueMs: 500})
	a.Close()
	a = newTestStore(t, dir, clock)
	if got := a.dashboard(1, nil, nil).Transcription.Total; got.Attempts != 3 || got.Succeeded != 1 || got.Failed != 1 || got.Retries != 1 || got.Average != "1.00s" || got.Confidence != "0.0% (1 clips)" {
		t.Fatalf("replayed metrics = %+v", got)
	}
	clock.t = clock.t.AddDate(0, 0, 1)
	a.TranscriptionAttempt(transcriptionAttempt{Host: "http://slow", Outcome: "success", Ms: 4000, QueueMs: 100, AudioMs: 20000, NoSpeech: true, Filtered: true})
	a.TranscriptionAttempt(transcriptionAttempt{Host: "http://fast", Outcome: "success", Ms: 2000, QueueMs: 200, AudioMs: 20000})
	check := func(a *analyticsStore) {
		t.Helper()
		d := a.dashboard(0, nil, nil, func() transcriptionSnapshot {
			return transcriptionSnapshot{Enabled: true, Queued: 3, Hosts: []transcriptionHost{
				{Host: "http://fast", Model: "new-model (configured)", Ready: true, Busy: true},
				{Host: "http://idle"},
			}}
		}).Transcription
		if d.Configured != 2 || d.Available != 1 || d.Busy != 1 || d.Queued != 3 {
			t.Fatalf("live metrics = %+v", d)
		}
		total := d.Total
		if total.Attempts != 5 || total.Succeeded != 3 || total.Failed != 1 || total.Retries != 1 || total.NoSpeech != 1 || total.Filtered != 1 ||
			total.Average != "2.33s" || total.QueueAverage != "0.36s" || total.RTF != "0.140" ||
			total.Confidence != "0.0% (1 clips)" || total.TokenProbability != "80.0% (1 clips)" || total.Logprob != "-0.200 (1 clips)" {
			t.Fatalf("total = %+v", total)
		}
		if len(d.Hosts) != 3 {
			t.Fatalf("host count = %d", len(d.Hosts))
		}
		fast, idle, slow := d.Hosts[0], d.Hosts[1], d.Hosts[2]
		if fast.Host != "http://fast" || fast.RTF != "0.100" || fast.Relative != "2.00x" || fast.Share != "66.7%" ||
			fast.Status != "busy" || fast.Model != "new-model (configured)" || fast.RangeModels != "small (reported)" {
			t.Fatalf("fast host = %+v", fast)
		}
		if idle.Status != "unavailable / starting" || idle.Average != "-" || idle.Confidence != "not reported" {
			t.Fatalf("idle host = %+v", idle)
		}
		if slow.Status != "not configured" || slow.Relative != "0.50x" || slow.Share != "33.3%" {
			t.Fatalf("historical host = %+v", slow)
		}
		if a.dashboard(0, nil, nil).Visitors != 0 {
			t.Fatal("transcription attempts counted as visitors")
		}
	}
	check(a)
	if today := a.dashboard(1, nil, nil).Transcription.Total; today.Attempts != 2 || today.Succeeded != 2 || today.Failed != 0 || today.Confidence != "not reported" {
		t.Fatalf("date range = %+v", today)
	}
	a.Close()
	clock.t = clock.t.AddDate(0, 0, 10)
	a = newTestStore(t, dir, clock, func(c *analyticsConfig) { c.RetentionDays = 5 })
	check(a)
	if _, err := os.Stat(filepath.Join(dir, "events", "2026-10-01.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("raw events not pruned: %v", err)
	}
}

func TestTranscriptionDashboardAndExport(t *testing.T) {
	a := newTestStore(t, t.TempDir(), &fakeClock{t: time.Now()})
	a.TranscriptionAttempt(transcriptionAttempt{
		Host: "http://host", Model: "<script> (reported)", Outcome: "success", Ms: 1500, AudioMs: 10000,
	})
	mux := http.NewServeMux()
	a.RegisterDashboard(mux, "/private123456", nil, nil, (*whisperPool)(nil).transcriptionSnapshot)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/private123456?days=1", nil))
	body := w.Body.String()
	for _, want := range []string{`id="transcription"`, "1.50s", "0.150", "transcription disabled", "&lt;script&gt; (reported)", "not reported"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	if strings.Contains(body, "<script>") {
		t.Fatal("model metadata not escaped")
	}
	w = httptest.NewRecorder()
	a.exportEvents(w, 1)
	rows, err := csv.NewReader(strings.NewReader(w.Body.String())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	column := len(rows[0]) - 1
	if rows[0][column] != "transcription_json" {
		t.Fatal("missing transcription CSV column")
	}
	found := false
	for _, row := range rows[1:] {
		if row[1] != evTranscription {
			continue
		}
		var attempt transcriptionAttempt
		if err := json.Unmarshal([]byte(row[column]), &attempt); err != nil {
			t.Fatal(err)
		}
		if attempt.Host != "http://host" || attempt.Ms != 1500 {
			t.Fatalf("exported attempt = %+v", attempt)
		}
		found = true
	}
	if !found {
		t.Fatal("no transcription event exported")
	}
}

func TestWhisperWorkerRecordsTranscriptionMetrics(t *testing.T) {
	for _, tt := range []struct {
		name, body, text                    string
		success, failed, noSpeech, filtered int
	}{
		{"verbose", `{"text":"Engine 4","model":"large-v3","confidence":0.9,"segments":[{"avg_logprob":-0.2,"words":[{"probability":0.8}]}]}`, "Engine 4", 1, 0, 0, 0},
		{"text only", `{"text":"Engine 4"}`, "Engine 4", 1, 0, 0, 0},
		{"empty", `{"text":""}`, noSpeechMarker, 1, 0, 1, 0},
		{"filtered", `{"text":"Thank you for watching."}`, noSpeechMarker, 1, 0, 1, 1},
		{"failed", `{"error":"failed to decode audio"}`, "[transcription failed]", 0, 1, 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a := newTestStore(t, t.TempDir(), &fakeClock{t: time.Now()})
			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			defer close(release)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/health" {
					_, _ = io.WriteString(w, `{"status":"ok"}`)
					return
				}
				if err := r.ParseMultipartForm(1 << 20); err != nil {
					t.Error(err)
				}
				if r.FormValue("response_format") != "verbose_json" {
					t.Error("metadata not requested")
				}
				entered <- struct{}{}
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				_, _ = io.WriteString(w, tt.body)
			}))
			defer srv.Close()
			cfg := whisperConfig{RemoteServers: []string{srv.URL}, RemoteModels: map[string]string{srv.URL: "medium"}}
			cfg.setDefaults()
			hub := newTranscriptHub("", "", log.New(io.Discard, "", 0))
			_, events := hub.subscribe()
			p := newWhisperPool(cfg, hub, log.New(io.Discard, "", 0))
			p.analytics = a
			defer p.Close()
			if err := p.Start(); err != nil {
				t.Fatal(err)
			}
			p.Submit(transcriptJob{clipID: "c1", wavPath: writeTestWAV(t), queuedAt: time.Now().Add(-time.Second)})
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("request not started")
			}
			s := p.transcriptionSnapshot()
			if len(s.Hosts) != 1 || !s.Hosts[0].Ready || !s.Hosts[0].Busy || s.Hosts[0].Model != "medium (configured)" {
				t.Fatalf("active snapshot = %+v", s)
			}
			release <- struct{}{}
			if ev := awaitTranscript(t, events, 5*time.Second); ev.Text != tt.text {
				t.Fatalf("transcript = %q, want %q", ev.Text, tt.text)
			}
			a.mu.Lock()
			agg := *a.days[a.today].Transcription[srv.URL]
			a.mu.Unlock()
			if agg.Attempts != 1 || agg.Succeeded != tt.success || agg.Failed != tt.failed || agg.NoSpeech != tt.noSpeech || agg.Filtered != tt.filtered || agg.QueueMs < 1000 {
				t.Fatalf("recorded metrics = %+v", agg)
			}
			if tt.success == 1 && agg.AudioMs != 250 {
				t.Fatalf("audio duration = %d, want 250ms", agg.AudioMs)
			}
			if tt.name == "verbose" {
				if agg.ConfidenceN != 1 || agg.LogprobN != 1 || agg.TokenProbabilityN != 1 || p.transcriptionSnapshot().Hosts[0].Model != "large-v3 (reported)" {
					t.Fatalf("metadata missing: %+v", agg)
				}
			} else if agg.ConfidenceN != 0 || agg.LogprobN != 0 || agg.TokenProbabilityN != 0 {
				t.Fatalf("invented metadata: %+v", agg)
			}
		})
	}
}

func TestWhisperRemoteModelLabelsValidate(t *testing.T) {
	for _, tt := range []struct {
		name  string
		cfg   whisperConfig
		valid bool
	}{
		{"normalized", whisperConfig{RemoteServers: []string{"host:8080/"}, RemoteModels: map[string]string{"http://host:8080": "medium"}}, true},
		{"unknown host", whisperConfig{RemoteServers: []string{"host:8080"}, RemoteModels: map[string]string{"another:8080": "medium"}}, false},
		{"local mode labels rejected", whisperConfig{ModelPath: "model.bin", RemoteModels: map[string]string{"host:8080": "medium"}}, false},
		{"empty label", whisperConfig{RemoteServers: []string{"host:8080"}, RemoteModels: map[string]string{"host:8080": " "}}, false},
		{"duplicate normalized labels", whisperConfig{RemoteServers: []string{"host:8080"}, RemoteModels: map[string]string{"host:8080": "medium", "http://host:8080/": "large"}}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.cfg.validate(); (err == nil) != tt.valid {
				t.Fatalf("validate() = %v, valid = %v", err, tt.valid)
			}
		})
	}
}

func TestTranscriptionEmptyDashboard(t *testing.T) {
	var buf bytes.Buffer
	a := newTestStore(t, t.TempDir(), &fakeClock{t: time.Now()})
	if err := analyticsTemplate.Execute(&buf, a.dashboard(1, nil, nil)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "No transcription hosts or metrics yet.") {
		t.Fatal("missing empty state")
	}
}

func TestWhisperAvailabilityTracksHealthWithoutJobs(t *testing.T) {
	var healthy atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			http.Error(w, "loading", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	defer srv.Close()
	p, _ := newTestPool(t, whisperConfig{RemoteServers: []string{srv.URL}})
	ep := p.endpoints[0]
	check := func(ready bool) {
		t.Helper()
		s := p.transcriptionSnapshot()
		if len(s.Hosts) != 1 || s.Hosts[0].Ready != ready || s.Hosts[0].Busy || s.Hosts[0].Checked.IsZero() {
			t.Fatalf("health snapshot = %+v, ready = %v", s, ready)
		}
	}
	check(false)
	healthy.Store(true)
	if err := p.probeRemote(ep); err != nil {
		t.Fatal(err)
	}
	check(true)
	healthy.Store(false)
	if err := p.probeRemote(ep); err == nil {
		t.Fatal("failed health check returned success")
	}
	check(false)
}

type failedInferenceTransport struct{ entered chan struct{} }

func (f failedInferenceTransport) RoundTrip(*http.Request) (*http.Response, error) {
	f.entered <- struct{}{}
	return nil, errors.New("connection refused")
}

func TestWhisperRetryAttemptRecordedOnce(t *testing.T) {
	a := newTestStore(t, t.TempDir(), &fakeClock{t: time.Now()})
	cfg := whisperConfig{}
	cfg.setDefaults()
	p := newWhisperPool(cfg, newTranscriptHub("", "", log.New(io.Discard, "", 0)), log.New(io.Discard, "", 0))
	p.analytics = a
	defer p.Close()
	entered := make(chan struct{}, 1)
	p.httpClient.Transport = failedInferenceTransport{entered: entered}
	ep := newWhisperEndpoint("dead", "http://127.0.0.1:1", false)
	p.endpoints = []*whisperEndpoint{ep}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.worker(ep)
	}()
	p.Submit(transcriptJob{clipID: "retry", wavPath: writeTestWAV(t)})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("inference not attempted")
	}
	requeued := make(chan transcriptJob, 1)
	go func() {
		if job, _, ok := p.nextJob(); ok {
			requeued <- job
		}
	}()
	select {
	case job := <-requeued:
		if job.retries != 1 || job.queuedAt.IsZero() {
			t.Fatalf("requeued job = %+v", job)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("unreachable attempt was not requeued")
	}
	a.mu.Lock()
	agg := *a.days[a.today].Transcription[ep.baseURL]
	a.mu.Unlock()
	if agg.Attempts != 1 || agg.Retries != 1 || agg.Failed != 0 || agg.Succeeded != 0 {
		t.Fatalf("retry metrics = %+v", agg)
	}
}

func TestWhisperCloseDoesNotDrainQueuedJobs(t *testing.T) {
	cfg := whisperConfig{}
	cfg.setDefaults()
	p := newWhisperPool(cfg, newTranscriptHub("", "", log.New(io.Discard, "", 0)), log.New(io.Discard, "", 0))
	p.Submit(transcriptJob{clipID: "pending"})
	p.Close()
	if _, _, ok := p.nextJob(); ok {
		t.Fatal("closed pool picked up a queued job")
	}
	if p.queueDepth() != 1 {
		t.Fatal("shutdown drained the pending queue")
	}
}
