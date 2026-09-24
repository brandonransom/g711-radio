package main

import (
	"encoding/csv"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"testing"
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
