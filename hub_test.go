package main

import (
	"encoding/csv"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestAppendArchiveWritesWAVFilename(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "transcripts.csv")
	hub := newTranscriptHub("", archivePath, log.New(io.Discard, "", 0))

	hub.appendArchive(transcriptEvent{
		WAVFilename: "Dispatch_2026-09-24T23_46_44Z.wav",
		StreamName:  "Dispatch",
		Text:        "  test   transcript  ",
	})

	f, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"filename", "streamName", "transcript"},
		{"Dispatch_2026-09-24T23_46_44Z.wav", "Dispatch", "test transcript"},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("archive rows = %#v, want %#v", rows, want)
	}
}

func TestRecordingHistoryIncludesWAVFilesAndMergesTranscripts(t *testing.T) {
	audioDir := t.TempDir()
	logDir := t.TempDir()
	logger := log.New(io.Discard, "", 0)
	hub := newTranscriptHub(logDir, "", logger)
	info := streamInfo{
		ID:         "current-stream-id",
		RegionName: "New Mexico",
		GroupName:  "Cibola NF",
		StreamName: "Capilla",
	}

	filename := "Capilla_2026-09-25T11_54_05Z.wav"
	wavDir := filepath.Join(audioDir, "New_Mexico", "Cibola_NF", "Capilla")
	if err := os.MkdirAll(wavDir, 0755); err != nil {
		t.Fatal(err)
	}
	wav, err := encodePCM16WAV(make([]int16, recSampleRate*2), recSampleRate)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wavDir, filename), wav, 0644); err != nil {
		t.Fatal(err)
	}

	timestamp := time.Date(2026, 9, 25, 11, 54, 5, 0, time.UTC)
	audioURL := "/audio/New_Mexico/Cibola_NF/Capilla/" + filename
	hub.appendLog(transcriptEvent{
		Type:       "clip",
		ClipID:     "clip-before-restart",
		StreamID:   "old-stream-id",
		StreamName: info.StreamName,
		AudioURL:   audioURL,
		DurationMs: 2000,
		Timestamp:  timestamp,
	})
	hub.appendLog(transcriptEvent{
		Type:       "transcript",
		ClipID:     "clip-before-restart",
		StreamID:   "old-stream-id",
		StreamName: info.StreamName,
		Text:       "historical transcript",
		AudioURL:   audioURL,
		Timestamp:  timestamp,
	})

	events, err := hub.RecordingHistory(audioDir, info, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2: %#v", len(events), events)
	}

	clip := events[0]
	if clip.Type != "clip" || clip.ClipID != "clip-before-restart" {
		t.Fatalf("clip event = %#v", clip)
	}
	if clip.StreamID != info.ID {
		t.Fatalf("clip stream ID = %q, want %q", clip.StreamID, info.ID)
	}
	if clip.DurationMs != 2000 {
		t.Fatalf("clip duration = %d, want 2000", clip.DurationMs)
	}
	if !clip.Timestamp.Equal(timestamp) {
		t.Fatalf("clip timestamp = %s, want %s", clip.Timestamp, timestamp)
	}

	transcript := events[1]
	if transcript.Type != "transcript" || transcript.Text != "historical transcript" {
		t.Fatalf("transcript event = %#v", transcript)
	}
	if transcript.ClipID != clip.ClipID || transcript.StreamID != info.ID {
		t.Fatalf("transcript was not normalized to current clip/stream: %#v", transcript)
	}
}

func TestRecordingHistoryUsesWAVWithoutTranscriptLog(t *testing.T) {
	audioDir := t.TempDir()
	logger := log.New(io.Discard, "", 0)
	hub := newTranscriptHub(t.TempDir(), "", logger)
	info := streamInfo{
		ID:         "stream-1",
		RegionName: "Region",
		GroupName:  "Group",
		StreamName: "Dispatch",
	}

	filename := "Dispatch_2026-09-25T12_00_00Z.wav"
	wavDir := filepath.Join(audioDir, "Region", "Group", "Dispatch")
	if err := os.MkdirAll(wavDir, 0755); err != nil {
		t.Fatal(err)
	}
	wav, err := encodePCM16WAV(make([]int16, recSampleRate/2), recSampleRate)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wavDir, filename), wav, 0644); err != nil {
		t.Fatal(err)
	}

	events, err := hub.RecordingHistory(audioDir, info, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %#v", len(events), events)
	}
	if events[0].ClipID != filename || events[0].DurationMs != 500 {
		t.Fatalf("filesystem clip = %#v", events[0])
	}
}
