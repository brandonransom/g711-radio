package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestSyncArchivesCopiesMissingFiles(t *testing.T) {
	dirA := filepath.Join(t.TempDir(), "archiveA")
	dirB := filepath.Join(t.TempDir(), "archiveB")

	nestedA := filepath.Join(dirA, "Alaska", "Chugach NF", "Glacier Net")
	if err := os.MkdirAll(nestedA, 0755); err != nil {
		t.Fatal(err)
	}
	wav1Content := []byte("RIFF1234WAVEfmt audio data from A")
	wav1Path := filepath.Join(nestedA, "Glacier Net_2026-10-12T19_00_00Z.wav")
	if err := os.WriteFile(wav1Path, wav1Content, 0644); err != nil {
		t.Fatal(err)
	}

	csvContent := []byte("filename,streamName,transcript\nclip.wav,Glacier Net,hello\n")
	csvPath := filepath.Join(dirA, "transcripts.csv")
	if err := os.WriteFile(csvPath, csvContent, 0644); err != nil {
		t.Fatal(err)
	}

	nestedB := filepath.Join(dirB, "Alaska", "Chugach NF", "Seward Net")
	if err := os.MkdirAll(nestedB, 0755); err != nil {
		t.Fatal(err)
	}
	wav2Content := []byte("RIFF5678WAVEfmt audio data from B")
	wav2Path := filepath.Join(nestedB, "Seward Net_2026-10-12T19_05_00Z.wav")
	if err := os.WriteFile(wav2Path, wav2Content, 0644); err != nil {
		t.Fatal(err)
	}

	// Set file modification times to the past so minAge doesn't skip them
	oldTime := time.Now().Add(-10 * time.Minute)
	_ = os.Chtimes(wav1Path, oldTime, oldTime)
	_ = os.Chtimes(csvPath, oldTime, oldTime)
	_ = os.Chtimes(wav2Path, oldTime, oldTime)

	copied, err := syncArchives(context.Background(), []string{dirA, dirB}, 0, nil, nil)
	if err != nil {
		t.Fatalf("syncArchives failed: %v", err)
	}
	// Expect 3 copies: wav1 A->B, csv A->B, wav2 B->A
	if copied != 3 {
		t.Fatalf("copied = %d, want 3", copied)
	}

	// Verify wav1 in dirB
	wav1InB := filepath.Join(dirB, "Alaska", "Chugach NF", "Glacier Net", "Glacier Net_2026-10-12T19_00_00Z.wav")
	dataB, err := os.ReadFile(wav1InB)
	if err != nil {
		t.Fatalf("expected wav1 in dirB: %v", err)
	}
	if !bytes.Equal(dataB, wav1Content) {
		t.Errorf("wav1 content mismatch in dirB")
	}

	// Verify csv in dirB
	csvInB := filepath.Join(dirB, "transcripts.csv")
	csvDataB, err := os.ReadFile(csvInB)
	if err != nil {
		t.Fatalf("expected csv in dirB: %v", err)
	}
	if !bytes.Equal(csvDataB, csvContent) {
		t.Errorf("csv content mismatch in dirB")
	}

	// Verify wav2 in dirA
	wav2InA := filepath.Join(dirA, "Alaska", "Chugach NF", "Seward Net", "Seward Net_2026-10-12T19_05_00Z.wav")
	dataA, err := os.ReadFile(wav2InA)
	if err != nil {
		t.Fatalf("expected wav2 in dirA: %v", err)
	}
	if !bytes.Equal(dataA, wav2Content) {
		t.Errorf("wav2 content mismatch in dirA")
	}

	// Running syncArchives again should find 0 inconsistencies
	copiedAgain, err := syncArchives(context.Background(), []string{dirA, dirB}, 0, nil, nil)
	if err != nil {
		t.Fatalf("second sync failed: %v", err)
	}
	if copiedAgain != 0 {
		t.Fatalf("copiedAgain = %d, want 0", copiedAgain)
	}
}

func TestSyncArchivesResolvesMismatchedSize(t *testing.T) {
	dirA := filepath.Join(t.TempDir(), "archiveA")
	dirB := filepath.Join(t.TempDir(), "archiveB")

	_ = os.MkdirAll(dirA, 0755)
	_ = os.MkdirAll(dirB, 0755)

	fullData := bytes.Repeat([]byte("A"), 1000)
	partialData := bytes.Repeat([]byte("A"), 400)

	fileA := filepath.Join(dirA, "recording.wav")
	fileB := filepath.Join(dirB, "recording.wav")

	if err := os.WriteFile(fileA, fullData, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileB, partialData, 0644); err != nil {
		t.Fatal(err)
	}

	oldTime := time.Now().Add(-10 * time.Minute)
	_ = os.Chtimes(fileA, oldTime, oldTime)
	_ = os.Chtimes(fileB, oldTime, oldTime)

	copied, err := syncArchives(context.Background(), []string{dirA, dirB}, 0, nil, nil)
	if err != nil {
		t.Fatalf("syncArchives failed: %v", err)
	}
	if copied != 1 {
		t.Fatalf("copied = %d, want 1", copied)
	}

	updatedB, err := os.ReadFile(fileB)
	if err != nil {
		t.Fatalf("read fileB: %v", err)
	}
	if len(updatedB) != 1000 {
		t.Errorf("len(fileB) = %d, want 1000", len(updatedB))
	}
}

func TestSyncArchivesSkipsRecentFiles(t *testing.T) {
	dirA := filepath.Join(t.TempDir(), "archiveA")
	dirB := filepath.Join(t.TempDir(), "archiveB")

	_ = os.MkdirAll(dirA, 0755)
	_ = os.MkdirAll(dirB, 0755)

	fileA := filepath.Join(dirA, "in_flight.wav")
	if err := os.WriteFile(fileA, []byte("active write"), 0644); err != nil {
		t.Fatal(err)
	}
	// Mod time is now (younger than minAge)
	now := time.Now()
	_ = os.Chtimes(fileA, now, now)

	copied, err := syncArchives(context.Background(), []string{dirA, dirB}, 5*time.Second, nil, nil)
	if err != nil {
		t.Fatalf("sync failed: %v", err)
	}
	if copied != 0 {
		t.Errorf("copied = %d, want 0 (file was too new)", copied)
	}

	// Make file old enough
	old := now.Add(-10 * time.Second)
	_ = os.Chtimes(fileA, old, old)

	copied, err = syncArchives(context.Background(), []string{dirA, dirB}, 5*time.Second, nil, nil)
	if err != nil {
		t.Fatalf("sync failed: %v", err)
	}
	if copied != 1 {
		t.Errorf("copied = %d, want 1", copied)
	}
}

func TestSyncArchivesThreeDirectories(t *testing.T) {
	dirA := filepath.Join(t.TempDir(), "a")
	dirB := filepath.Join(t.TempDir(), "b")
	dirC := filepath.Join(t.TempDir(), "c")

	_ = os.MkdirAll(dirA, 0755)
	_ = os.MkdirAll(dirB, 0755)
	_ = os.MkdirAll(dirC, 0755)

	fileA := filepath.Join(dirA, "fromA.wav")
	fileC := filepath.Join(dirC, "fromC.wav")

	_ = os.WriteFile(fileA, []byte("A"), 0644)
	_ = os.WriteFile(fileC, []byte("C"), 0644)

	old := time.Now().Add(-10 * time.Minute)
	_ = os.Chtimes(fileA, old, old)
	_ = os.Chtimes(fileC, old, old)

	copied, err := syncArchives(context.Background(), []string{dirA, dirB, dirC}, 0, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// fileA copied to B and C (2), fileC copied to A and B (2) -> 4 copies
	if copied != 4 {
		t.Errorf("copied = %d, want 4", copied)
	}

	for _, d := range []string{dirA, dirB, dirC} {
		if _, err := os.Stat(filepath.Join(d, "fromA.wav")); err != nil {
			t.Errorf("fromA.wav missing in %s", d)
		}
		if _, err := os.Stat(filepath.Join(d, "fromC.wav")); err != nil {
			t.Errorf("fromC.wav missing in %s", d)
		}
	}
}

func TestStartArchiveSyncRunsAndStops(t *testing.T) {
	dirA := filepath.Join(t.TempDir(), "archiveA")
	dirB := filepath.Join(t.TempDir(), "archiveB")
	_ = os.MkdirAll(dirA, 0755)
	_ = os.MkdirAll(dirB, 0755)

	fileA := filepath.Join(dirA, "test.wav")
	_ = os.WriteFile(fileA, []byte("test audio"), 0644)
	old := time.Now().Add(-10 * time.Minute)
	_ = os.Chtimes(fileA, old, old)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var busyCalls atomic.Int32
	isBusy := func() bool {
		busyCalls.Add(1)
		return false
	}

	startArchiveSync(ctx, []string{dirA, dirB}, 50*time.Millisecond, 200*time.Millisecond, isBusy, nil)

	// Cancel context after a short time
	time.Sleep(200 * time.Millisecond)
	cancel()
}

func TestServerBusyDetection(t *testing.T) {
	stIdle := &station{lastPacketAt: time.Now().Add(-1 * time.Minute)}
	if stIdle.isBusy(10 * time.Second) {
		t.Errorf("expected idle station to not be busy")
	}

	stActive := &station{lastPacketAt: time.Now()}
	if !stActive.isBusy(10 * time.Second) {
		t.Errorf("expected active station to be busy")
	}

	stRec := &station{recorder: &recorderState{recording: true}}
	if !stRec.isBusy(10 * time.Second) {
		t.Errorf("expected recording station to be busy")
	}

	srv := &webrtcServer{
		streams:  map[string]*station{"s1": stIdle},
		clipJobs: make(chan func(), 10),
	}
	if srv.isBusy() {
		t.Errorf("expected srv to not be busy when stations are idle")
	}

	srv.clipJobs <- func() {}
	if !srv.isBusy() {
		t.Errorf("expected srv with clipJobs to be busy")
	}
}
