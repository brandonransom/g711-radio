package main

import (
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeBackfillWAV(t *testing.T, dir, name string, samples int, modTime time.Time) string {
	t.Helper()
	wav, err := encodePCM16WAV(make([]int16, samples), recSampleRate)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, wav, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeBackfillLog(t *testing.T, logDir, streamName string, events ...transcriptEvent) {
	t.Helper()
	var b strings.Builder
	for _, ev := range events {
		line, _ := json.Marshal(ev)
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(logDir, logFilename(streamName)), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTranscriptBackfillQueuesUntranscribedOldestFirst(t *testing.T) {
	audioDir := t.TempDir()
	logDir := t.TempDir()
	old := time.Now().Add(-24 * time.Hour)

	enabled := &station{
		info: streamInfo{ID: "a", StateName: "Oregon", GroupName: "Umatilla NF", StreamName: "Admin Net"},
		cfg:  streamConfig{StreamName: "Admin Net"},
	}
	disabled := &station{
		info: streamInfo{ID: "b", StateName: "Oregon", GroupName: "Umatilla NF", StreamName: "Admin Net (Repeater)"},
		cfg:  streamConfig{StreamName: "Admin Net (Repeater)", DisableAutoTranscribe: true},
	}
	dirFor := func(st *station) string {
		safe := func(v string) string { return unsafeChars.ReplaceAllString(v, "_") }
		d := filepath.Join(audioDir, safe(st.info.StateName), safe(st.info.GroupName), safe(st.info.StreamName))
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		return d
	}
	aDir, bDir := dirFor(enabled), dirFor(disabled)
	second := recSampleRate
	writeBackfillWAV(t, aDir, "Admin_Net_2024-01-01T00_00_00Z.wav", second, old)    // transcribed
	writeBackfillWAV(t, aDir, "Admin_Net_2024-01-01T00_02_00Z.wav", second, old)    // failed earlier
	writeBackfillWAV(t, aDir, "Admin_Net_2024-01-01T00_01_00Z.wav", second, old)    // never transcribed
	writeBackfillWAV(t, aDir, "Admin_Net_2024-01-01T00_03_00Z.wav", second, old)    // no speech
	writeBackfillWAV(t, aDir, "Admin_Net_2024-01-01T00_04_00Z.wav", second/10, old) // too short
	recentName := "Admin_Net_" + time.Now().UTC().Format("2006-01-02T15_04_05Z") + ".wav"
	writeBackfillWAV(t, aDir, recentName, second, time.Now()) // live pipeline's job
	writeBackfillWAV(t, bDir, "Admin_Net__Repeater__2024-01-01T00_00_00Z.wav", second, old)

	writeBackfillLog(t, logDir, "Admin Net",
		transcriptEvent{Type: "clip", WAVFilename: "Admin_Net_2024-01-01T00_01_00Z.wav"},
		transcriptEvent{Type: "transcript", WAVFilename: "Admin_Net_2024-01-01T00_00_00Z.wav", Text: "Engine 4"},
		transcriptEvent{Type: "transcript", AudioURL: "/audio/x/y/z/Admin_Net_2024-01-01T00_02_00Z.wav", Text: failedMarker},
		transcriptEvent{Type: "transcript", WAVFilename: "Admin_Net_2024-01-01T00_03_00Z.wav", Text: noSpeechMarker},
	)

	pool := newWhisperPool(whisperConfig{}, nil, log.New(io.Discard, "", 0))
	wCfg := &whisperConfig{AutoTranscribeMinClipMs: 500}
	b := newTranscriptBackfill(func() []*station { return []*station{enabled, disabled} },
		pool, audioDir, logDir, wCfg, log.New(io.Discard, "", 0))

	var order []string
	for i := 0; i < 5; i++ {
		b.step()
		_, queued, _ := pool.backfillState()
		if queued == 0 {
			break
		}
		if queued != 1 {
			t.Fatalf("step queued %d backfill jobs, want 1 (single server)", queued)
		}
		// A queued backfill job blocks further submissions.
		b.step()
		if _, q, _ := pool.backfillState(); q != 1 {
			t.Fatalf("second step queued more work: %d", q)
		}
		job, _, ok := pool.nextJob()
		if !ok || !job.backfill {
			t.Fatalf("nextJob = %+v, %v", job, ok)
		}
		if !strings.HasPrefix(job.audioURL, "/audio/Oregon/Umatilla_NF/Admin_Net/") {
			t.Errorf("audioURL = %q", job.audioURL)
		}
		order = append(order, job.clipID)
		pool.finishJob(job)
	}
	want := "Admin_Net_2024-01-01T00_01_00Z.wav,Admin_Net_2024-01-01T00_02_00Z.wav"
	if got := strings.Join(order, ","); got != want {
		t.Fatalf("backfill order = %s, want %s", got, want)
	}

	// Failed clips are retried once per run, not in a loop.
	b.lastScan = time.Time{}
	b.step()
	if _, q, _ := pool.backfillState(); q != 0 {
		t.Fatalf("rescan requeued %d already-attempted clips", q)
	}
}

func TestTranscriptBackfillWaitsForEmptyQueue(t *testing.T) {
	audioDir, logDir := t.TempDir(), t.TempDir()
	st := &station{info: streamInfo{StateName: "S", GroupName: "G", StreamName: "N"}, cfg: streamConfig{StreamName: "N"}}
	dir := filepath.Join(audioDir, "S", "G", "N")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeBackfillWAV(t, dir, "N_2024-01-01T00_00_00Z.wav", recSampleRate, time.Now().Add(-time.Hour))

	pool := newWhisperPool(whisperConfig{}, nil, log.New(io.Discard, "", 0))
	pool.Submit(transcriptJob{clipID: "live", wavPath: filepath.Join(dir, "live.wav")})
	b := newTranscriptBackfill(func() []*station { return []*station{st} },
		pool, audioDir, logDir, &whisperConfig{AutoTranscribeMinClipMs: 500}, log.New(io.Discard, "", 0))
	b.step()
	if _, q, _ := pool.backfillState(); q != 0 {
		t.Fatalf("backfill queued %d jobs while live work was waiting", q)
	}
	job, _, _ := pool.nextJob()
	b.step()
	if _, q, _ := pool.backfillState(); q != 1 {
		t.Fatalf("backfill queued %d jobs once the queue emptied, want 1", q)
	}
	pool.finishJob(job)
}

func TestWhisperPoolShortestLiveClipFirst(t *testing.T) {
	p := newWhisperPool(whisperConfig{}, nil, log.New(io.Discard, "", 0))
	now := time.Now()
	add := func(name string, size int64, waited time.Duration, retries int) {
		p.queue = append(p.queue, transcriptJob{clipID: name, sizeBytes: size, queuedAt: now.Add(-waited), retries: retries})
	}
	add("long", 900, time.Second, 0)
	add("short", 100, 0, 0)
	add("mid", 500, 0, 0)
	take := func() string {
		p.mu.Lock()
		defer p.mu.Unlock()
		job, _ := p.takeJobLocked()
		return job.clipID
	}
	if got := take(); got != "short" {
		t.Fatalf("first = %s, want short", got)
	}
	// A clip that has waited too long runs next regardless of length.
	add("tiny", 10, 0, 0)
	p.queue[0].queuedAt = now.Add(-liveJobMaxReorderWait)
	if got := take(); got != "long" {
		t.Fatalf("aged clip = %s, want long", got)
	}
	// A retried clip keeps its place at the front.
	p.queue = append([]transcriptJob{{clipID: "retry", sizeBytes: 999, queuedAt: now, retries: 1}}, p.queue...)
	if got := take(); got != "retry" {
		t.Fatalf("retried clip = %s, want retry", got)
	}
	if got := take(); got != "tiny" {
		t.Fatalf("next = %s, want tiny", got)
	}
}

func TestWhisperPoolKeepsServerFreeForLiveClips(t *testing.T) {
	p := newWhisperPool(whisperConfig{}, nil, log.New(io.Discard, "", 0))
	p.endpoints = []*whisperEndpoint{{}, {}, {}}
	for _, name := range []string{"b1", "b2", "b3"} {
		p.SubmitBulk(transcriptJob{clipID: name, wavPath: name + ".wav", backfill: true})
	}
	p.SubmitBulk(transcriptJob{clipID: "user", wavPath: "user.wav"})

	var order []string
	var jobs []transcriptJob
	for i := 0; i < 3; i++ {
		job, _, ok := p.nextJob()
		if !ok {
			t.Fatal("nextJob !ok")
		}
		order = append(order, job.clipID)
		jobs = append(jobs, job)
	}
	if got := strings.Join(order, ","); got != "b1,b2,user" {
		t.Fatalf("order = %s, want b1,b2,user (one server held back from backfill)", got)
	}
	if !p.isBusy() {
		t.Fatal("pool running a user bulk job should be busy")
	}
	p.finishJob(jobs[2])
	if p.isBusy() {
		t.Fatal("backfill-only work should not count as busy")
	}
	if !p.isPending("b1.wav") || !p.isPending("b3.wav") || p.isPending("user.wav") {
		t.Fatal("isPending mismatch")
	}
	p.finishJob(jobs[0])
	if job, _, _ := p.nextJob(); job.clipID != "b3" {
		t.Fatalf("after a slot freed got %s, want b3", job.clipID)
	}
}
