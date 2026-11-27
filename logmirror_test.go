package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateRootLogs(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "g711-radio-2026-11-26.log"), "old day")
	writeTestFile(t, filepath.Join(root, "g711-radio-2026-11-27.log"), "today, root copy")
	writeTestFile(t, filepath.Join(root, "g711-radio.log"), "legacy")
	writeTestFile(t, filepath.Join(root, "service-out.log"), "nssm stdout")
	writeTestFile(t, filepath.Join(root, "service-err-20261127T010101.123.log"), "nssm stderr, rotated")
	writeTestFile(t, filepath.Join(root, "service-outage-notes.log"), "not ours")
	writeTestFile(t, filepath.Join(root, "nssm-stdout.log"), "not ours")
	writeTestFile(t, filepath.Join(root, "config.json"), "{}")
	writeTestFile(t, filepath.Join(root, "g711-radio-keep.log", "nested.log"), "inside a directory")
	// Already present in logs/: must not be overwritten.
	writeTestFile(t, filepath.Join(root, "logs", "g711-radio-2026-11-27.log"), "today, logs copy")

	report := migrateRootLogs(root)
	if len(report) != 1 || !strings.Contains(report[0], "moved 5 log file(s)") {
		t.Fatalf("report = %q", report)
	}

	read := func(parts ...string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(append([]string{root}, parts...)...))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if got := read("logs", "g711-radio-2026-11-26.log"); got != "old day" {
		t.Errorf("moved day log = %q", got)
	}
	if got := read("logs", "g711-radio.log"); got != "legacy" {
		t.Errorf("moved legacy log = %q", got)
	}
	if got := read("logs", "service-out.log"); got != "nssm stdout" {
		t.Errorf("moved service-out log = %q", got)
	}
	if got := read("logs", "service-err-20261127T010101.123.log"); got != "nssm stderr, rotated" {
		t.Errorf("moved rotated service-err log = %q", got)
	}
	if got := read("logs", "g711-radio-2026-11-27.log"); got != "today, logs copy" {
		t.Errorf("existing log was overwritten: %q", got)
	}
	if got := read("logs", "g711-radio-2026-11-27.moved-1.log"); got != "today, root copy" {
		t.Errorf("colliding log = %q", got)
	}
	for _, left := range []string{"nssm-stdout.log", "service-outage-notes.log", "config.json", filepath.Join("g711-radio-keep.log", "nested.log")} {
		if _, err := os.Stat(filepath.Join(root, left)); err != nil {
			t.Errorf("%s should have been left alone: %v", left, err)
		}
	}
	for _, gone := range []string{"g711-radio-2026-11-26.log", "g711-radio-2026-11-27.log", "g711-radio.log", "service-out.log", "service-err-20261127T010101.123.log"} {
		if _, err := os.Stat(filepath.Join(root, gone)); !os.IsNotExist(err) {
			t.Errorf("%s should have been moved out of the root", gone)
		}
	}

	if report := migrateRootLogs(root); len(report) != 0 {
		t.Errorf("second run should do nothing, got %q", report)
	}
}

func TestMoveFileNoClobberMissingSource(t *testing.T) {
	root := t.TempDir()
	if _, err := moveFileNoClobber(filepath.Join(root, "absent.log"), filepath.Join(root, "logs", "absent.log")); !os.IsNotExist(err) {
		t.Fatalf("err = %v, want not-exist", err)
	}
	if _, err := os.Stat(filepath.Join(root, "logs")); !os.IsNotExist(err) {
		t.Fatal("destination folder should not be created when there is nothing to move")
	}
}
