package main

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type indexFixture struct {
	audioDir string
	hub      *transcriptHub
	info     streamInfo
	wavDir   string
}

func newIndexFixture(t *testing.T, cfg recordingIndexConfig) *indexFixture {
	t.Helper()
	if err := cfg.normalize(); err != nil {
		t.Fatal(err)
	}
	audioDir := t.TempDir()
	logDir := t.TempDir()
	logger := log.New(io.Discard, "", 0)
	hub := newTranscriptHub(logDir, "", logger)
	hub.index = newRecordingIndex(cfg, audioDir, logDir, logger)
	info := streamInfo{ID: "s1", StateName: "New Mexico", GroupName: "Cibola NF", StreamName: "Capilla"}
	wavDir := filepath.Join(audioDir, "New_Mexico", "Cibola_NF", "Capilla")
	if err := os.MkdirAll(wavDir, 0755); err != nil {
		t.Fatal(err)
	}
	hub.index.register(info)
	return &indexFixture{audioDir: audioDir, hub: hub, info: info, wavDir: wavDir}
}

func (f *indexFixture) writeWAV(t *testing.T, ts time.Time, samples int) (string, string) {
	t.Helper()
	name := "Capilla_" + ts.UTC().Format("2006-01-02T15_04_05Z") + ".wav"
	wav, err := encodePCM16WAV(make([]int16, samples), recSampleRate)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.wavDir, name), wav, 0644); err != nil {
		t.Fatal(err)
	}
	return name, "/audio/New_Mexico/Cibola_NF/Capilla/" + name
}

func (f *indexFixture) event(typ, name, url, text string, ts time.Time) transcriptEvent {
	return transcriptEvent{
		Type: typ, ClipID: name, StreamID: f.info.ID, StreamName: f.info.StreamName,
		StateName: f.info.StateName, GroupName: f.info.GroupName,
		AudioURL: url, Text: text, Timestamp: ts, WAVFilename: name,
	}
}

func textsByClip(events []transcriptEvent) map[string]string {
	out := map[string]string{}
	for _, ev := range events {
		if ev.Type == "transcript" {
			out[ev.ClipID] = ev.Text
		}
	}
	return out
}

func clipNames(events []transcriptEvent) []string {
	var out []string
	for _, ev := range events {
		if ev.Type == "clip" {
			out = append(out, ev.ClipID)
		}
	}
	return out
}

func TestRecordingIndexModesMatchFolderScan(t *testing.T) {
	for _, mode := range []string{recordingIndexFull, recordingIndexLean} {
		t.Run(mode, func(t *testing.T) {
			f := newIndexFixture(t, recordingIndexConfig{Mode: mode})
			old := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
			newer := old.Add(time.Hour)
			oldName, oldURL := f.writeWAV(t, old, recSampleRate)
			newName, _ := f.writeWAV(t, newer, recSampleRate/2)

			// A legacy log keyed by a per-process clip ID, with no wavFilename.
			f.hub.appendLog(transcriptEvent{Type: "clip", ClipID: "clip-3", StreamName: "Capilla", AudioURL: oldURL, DurationMs: 1000, Timestamp: old})
			f.hub.appendLog(transcriptEvent{Type: "transcript", ClipID: "clip-3", StreamName: "Capilla", Text: "first", Timestamp: old})
			f.hub.appendLog(transcriptEvent{Type: "transcript", ClipID: "clip-3", StreamName: "Capilla", Text: "retried", Timestamp: old})

			f.hub.index.build()

			events, ok := f.hub.index.history(f.info, time.Time{}, time.Time{})
			if !ok {
				t.Fatal("index did not answer")
			}
			if got := clipNames(events); len(got) != 2 || got[0] != oldName || got[1] != newName {
				t.Fatalf("clips = %v", got)
			}
			if events[0].DurationMs != 1000 || events[0].StreamID != "s1" || events[0].AudioURL != oldURL {
				t.Fatalf("clip event = %#v", events[0])
			}
			if texts := textsByClip(events); texts[oldName] != "retried" || len(texts) != 1 {
				t.Fatalf("texts = %v", texts)
			}
			for _, ev := range events {
				if ev.ClipID == newName && ev.Type == "clip" && ev.DurationMs != 500 {
					t.Fatalf("header duration = %d, want 500", ev.DurationMs)
				}
			}

			// Bounded range excludes the older recording.
			bounded, _ := f.hub.index.history(f.info, old, time.Time{})
			if got := clipNames(bounded); len(got) != 1 || got[0] != newName {
				t.Fatalf("bounded clips = %v", got)
			}
		})
	}
}

func TestRecordingIndexTracksLiveClipsAndTranscripts(t *testing.T) {
	f := newIndexFixture(t, recordingIndexConfig{Mode: recordingIndexLean})
	f.hub.index.build()

	ts := time.Now().UTC().Truncate(time.Second).Add(-time.Minute)
	name, url := f.writeWAV(t, ts, recSampleRate)
	clip := f.event("clip", name, url, "", ts)
	clip.DurationMs = 1000
	f.hub.Publish(clip)
	f.hub.Publish(f.event("transcribing", name, url, "", ts))

	events, _ := f.hub.RecordingHistory(f.audioDir, f.info, time.Time{}, time.Time{})
	if got := clipNames(events); len(got) != 1 || got[0] != name {
		t.Fatalf("clips after live clip = %v", got)
	}
	if len(textsByClip(events)) != 0 {
		t.Fatalf("unexpected transcript before whisper finished: %#v", events)
	}

	f.hub.Publish(f.event("transcript", name, url, "Engine 4 responding.", ts))
	events, _ = f.hub.RecordingHistory(f.audioDir, f.info, time.Time{}, time.Time{})
	if texts := textsByClip(events); texts[name] != "Engine 4 responding." {
		t.Fatalf("texts = %v", texts)
	}
}

func TestRecordingIndexReplaysEventsPublishedDuringBuild(t *testing.T) {
	f := newIndexFixture(t, recordingIndexConfig{Mode: recordingIndexFull})
	ts := time.Date(2026, 9, 2, 8, 0, 0, 0, time.UTC)
	name, url := f.writeWAV(t, ts, recSampleRate)

	if _, ok := f.hub.index.history(f.info, time.Time{}, time.Time{}); ok {
		t.Fatal("index answered before it was built")
	}
	// Published before the build: queued, then replayed.
	f.hub.Publish(f.event("transcript", name, url, "queued", ts))
	f.hub.index.build()

	events, ok := f.hub.index.history(f.info, time.Time{}, time.Time{})
	if !ok || textsByClip(events)[name] != "queued" {
		t.Fatalf("events = %#v", events)
	}
}

func TestRecordingIndexWindowFallsBackForOlderRanges(t *testing.T) {
	f := newIndexFixture(t, recordingIndexConfig{Mode: recordingIndexWindow, WindowDays: 7})
	recent := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	stale := recent.Add(-30 * 24 * time.Hour)
	recentName, _ := f.writeWAV(t, recent, recSampleRate)
	staleName, _ := f.writeWAV(t, stale, recSampleRate)
	f.hub.index.build()

	events, ok := f.hub.index.history(f.info, time.Now().Add(-24*time.Hour), time.Time{})
	if !ok {
		t.Fatal("window index should answer a 24h range")
	}
	if got := clipNames(events); len(got) != 1 || got[0] != recentName {
		t.Fatalf("clips = %v", got)
	}
	if _, ok := f.hub.index.history(f.info, time.Time{}, time.Time{}); ok {
		t.Fatal("window index must not answer an all-time range")
	}
	all, err := f.hub.RecordingHistory(f.audioDir, f.info, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if got := clipNames(all); len(got) != 2 {
		t.Fatalf("fallback clips = %v, want %s and %s", got, staleName, recentName)
	}
}

func TestRecordingIndexConfig(t *testing.T) {
	var cfg recordingIndexConfig
	if err := cfg.normalize(); err != nil || cfg.Mode != recordingIndexFull {
		t.Fatalf("default = %#v, %v", cfg, err)
	}
	cfg = recordingIndexConfig{Mode: "Window"}
	if err := cfg.normalize(); err != nil || cfg.WindowDays != defaultRecordingIndexWindowDays {
		t.Fatalf("window default = %#v, %v", cfg, err)
	}
	cfg = recordingIndexConfig{Mode: "bogus"}
	if err := cfg.normalize(); err == nil {
		t.Fatal("expected error for unknown mode")
	}
	if newRecordingIndex(recordingIndexConfig{Mode: recordingIndexOff}, "audio", "logs", nil) != nil {
		t.Fatal("off mode should not build an index")
	}
}
