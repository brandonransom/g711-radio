package main

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSaveAudioClipWritesEveryBackup(t *testing.T) {
	root := t.TempDir()
	primary, b1, b2 := filepath.Join(root, "p"), filepath.Join(root, "b1"), filepath.Join(root, "b2")
	info := streamInfo{StateName: "Utah", GroupName: "Dixie NF", StreamName: "Admin Net"}
	start := time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)
	wavPath, _, err := saveAudioClip(primary, []string{b1, "", b2}, info, make([]int16, 800), start, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(wavPath)
	if err != nil {
		t.Fatal(err)
	}
	rel, _ := filepath.Rel(primary, wavPath)
	for _, dir := range []string{b1, b2} {
		got, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil || string(got) != string(want) {
			t.Fatalf("backup %s: err=%v, identical=%v", dir, err, string(got) == string(want))
		}
	}

	// An unreachable backup doesn't stop the others.
	blocker := filepath.Join(root, "file")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	b3 := filepath.Join(root, "b3")
	if _, _, err := saveAudioClip(primary, []string{blocker, b3}, info, make([]int16, 800), start.Add(time.Minute), log.New(io.Discard, "", 0)); err != nil {
		t.Fatalf("primary failed because of a backup: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(b3, "Utah", "Dixie_NF", "Admin_Net")); len(entries) != 1 {
		t.Fatalf("second backup not written after the first failed: %d files", len(entries))
	}
}

func TestAudioBackupDirsConfig(t *testing.T) {
	cfg := appConfig{AudioBackupDir2: "c"}
	if got := audioBackupDirs(&cfg); len(got) != 1 || got[0] != "c" {
		t.Fatalf("got %v", got)
	}
	cfg.AudioBackupDir = "b"
	if got := audioBackupDirs(&cfg); len(got) != 2 || got[0] != "b" || got[1] != "c" {
		t.Fatalf("got %v", got)
	}
}

func TestRadioArchiveBackfill(t *testing.T) {
	root := t.TempDir()
	audio := filepath.Join(root, "audio")
	logger := log.New(io.Discard, "", 0)
	glacier := streamInfo{StateName: "Alaska", GroupName: "Chugach NF - Primary", StreamName: "Glacier Net - Primary"}
	retired := streamInfo{StateName: "Utah", GroupName: "Old NF", StreamName: "Gone Net"}
	save := func(info streamInfo, at time.Time, ms int) {
		t.Helper()
		if _, _, err := saveAudioClip(audio, nil, info, make([]int16, ms*recSampleRate/1000), at, logger); err != nil {
			t.Fatal(err)
		}
	}

	live := time.Date(2025, 3, 10, 9, 30, 0, 500e6, time.Local)
	clock := &fakeClock{t: live}
	a := newTestStore(t, filepath.Join(root, "analytics"), clock)
	a.Transmission(glacier, live, 2000) // live counting began here
	save(glacier, live, 2000)           // ...and this is its WAV: must not be counted again

	twoDaysBefore := live.AddDate(0, 0, -2)
	save(glacier, twoDaysBefore, 4000)
	save(glacier, live.Add(-time.Hour), 1000)
	save(retired, twoDaysBefore.Add(time.Hour), 3000)
	if err := os.WriteFile(filepath.Join(audio, "Alaska", "Chugach_NF_-_Primary", "Glacier_Net_-_Primary", "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	clock.t = live.Add(2 * time.Hour)
	done := make(chan struct{})
	a.BackfillRadio(audio, []streamInfo{glacier}, done)
	<-done

	rd := a.radioStats(a.withRadioArchive(a.daysInRange(0), 0), 0, func() []streamInfo { return []streamInfo{glacier} })
	if rd.Count != 4 {
		t.Fatalf("transmissions = %d, want 4 (1 live + 3 archived)", rd.Count)
	}
	var glacierRow, retiredRow *radioRow
	for i := range rd.ByStream {
		switch rd.ByStream[i].Name {
		case "Glacier Net - Primary":
			glacierRow = &rd.ByStream[i]
		case "Gone_Net":
			retiredRow = &rd.ByStream[i]
		}
	}
	if glacierRow == nil || glacierRow.Count != 3 || glacierRow.AirMs != 7000 {
		t.Fatalf("configured stream row = %+v, want archive merged under its real name", glacierRow)
	}
	if retiredRow == nil || !retiredRow.Retired || retiredRow.Count != 1 {
		t.Fatalf("unconfigured archive folder row = %+v", retiredRow)
	}
	if rd.ArchiveNote == "" {
		t.Fatal("missing archive note")
	}

	// Archive-only days appear in the daily table but don't dilute the
	// visitor average, and the stored live totals are untouched.
	data := a.dashboard(0, nil, func() []streamInfo { return []streamInfo{glacier} })
	if len(data.Daily) != 2 || data.From != twoDaysBefore.Format(dateLayout) {
		t.Fatalf("daily rows = %d from %s", len(data.Daily), data.From)
	}
	if got := a.days[live.Format(dateLayout)].Radio[glacier.displayName()].Count; got != 1 {
		t.Fatalf("live day totals modified: count %d", got)
	}

	// A later start reads the saved file instead of rescanning.
	if err := os.RemoveAll(audio); err != nil {
		t.Fatal(err)
	}
	a.Close()
	b := newTestStore(t, filepath.Join(root, "analytics"), clock)
	done = make(chan struct{})
	b.BackfillRadio(audio, []streamInfo{glacier}, done)
	<-done
	if rd := b.radioStats(b.withRadioArchive(b.daysInRange(0), 0), 0, nil); rd.Count != 4 {
		t.Fatalf("after reload transmissions = %d, want 4", rd.Count)
	}
	if rd := b.radioStats(b.withRadioArchive(b.daysInRange(1), 1), 1, nil); rd.Count != 2 {
		t.Fatalf("today's transmissions = %d, want 2 (1 live + 1 archived earlier today)", rd.Count)
	}
}

func TestRadioArchiveWithoutLiveData(t *testing.T) {
	root := t.TempDir()
	audio := filepath.Join(root, "audio")
	info := streamInfo{StateName: "Utah", GroupName: "Ashley NF", StreamName: "Vernal Net"}
	now := time.Date(2025, 3, 10, 9, 0, 0, 0, time.Local)
	if _, _, err := saveAudioClip(audio, nil, info, make([]int16, 8000), now.Add(-time.Minute), log.New(io.Discard, "", 0)); err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{t: now}
	a := newTestStore(t, filepath.Join(root, "analytics"), clock)
	done := make(chan struct{})
	a.BackfillRadio(audio, nil, done)
	<-done
	if rd := a.radioStats(a.withRadioArchive(a.daysInRange(1), 1), 1, nil); rd.Count != 1 {
		t.Fatalf("transmissions = %d, want 1", rd.Count)
	}

	// No archive and no saved file: nothing happens.
	c := newTestStore(t, filepath.Join(root, "other"), clock)
	done = make(chan struct{})
	c.BackfillRadio("", nil, done)
	<-done
	if c.archive != nil {
		t.Fatal("unexpected archive data")
	}
}
