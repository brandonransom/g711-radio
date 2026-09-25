package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecordingDownloadHandlerCreatesZip(t *testing.T) {
	audioDir := t.TempDir()
	relative := filepath.Join("Region", "Forest", "Dispatch", "Dispatch_2026-09-25T12_00_00Z.wav")
	fullPath := filepath.Join(audioDir, relative)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		t.Fatal(err)
	}
	want := []byte("test wav data")
	if err := os.WriteFile(fullPath, want, 0644); err != nil {
		t.Fatal(err)
	}

	body, err := json.Marshal(recordingDownloadRequest{
		AudioURLs: []string{"/audio/Region/Forest/Dispatch/Dispatch_2026-09-25T12_00_00Z.wav"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/recordings/download", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	recordingDownloadHandler(audioDir, log.New(io.Discard, "", 0)).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	reader, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if len(reader.File) != 1 {
		t.Fatalf("ZIP contains %d files, want 1", len(reader.File))
	}
	if reader.File[0].Name != filepath.ToSlash(relative) {
		t.Fatalf("ZIP filename = %q, want %q", reader.File[0].Name, filepath.ToSlash(relative))
	}
	file, err := reader.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("ZIP content = %q, want %q", got, want)
	}
}

func TestRecordingDownloadHandlerRejectsTraversal(t *testing.T) {
	body := `{"audioUrls":["/audio/../secret.wav"]}`
	req := httptest.NewRequest(http.MethodPost, "/recordings/download", strings.NewReader(body))
	rec := httptest.NewRecorder()
	recordingDownloadHandler(t.TempDir(), log.New(io.Discard, "", 0)).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}
