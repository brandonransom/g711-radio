package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func newFeedbackServer(t *testing.T) (*webrtcServer, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "transcript-feedback.csv")
	return &webrtcServer{
		logger:      log.New(io.Discard, "", 0),
		usageLogger: &usageLogger{},
		clips:       make(map[string]clipRecord),
		feedback:    newFeedbackStore(path, log.New(io.Discard, "", 0)),
	}, path
}

func postFeedback(t *testing.T, s *webrtcServer, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/transcripts/feedback", bytes.NewReader(data))
	rec := httptest.NewRecorder()
	s.handleTranscriptFeedback(rec, req)
	return rec
}

func readFeedbackCSV(t *testing.T, path string) [][]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return rows
}

func TestFeedbackRecordsCorrection(t *testing.T) {
	s, path := newFeedbackServer(t)

	resp := postFeedback(t, s, map[string]string{
		"wavFilename": "Pomeroy_Net_2026-10-01T01_00_00Z.wav",
		"clipId":      "clip-3",
		"streamName":  "Pomeroy Net",
		"audioUrl":    "/audio/Oregon/Umatilla NF/Pomeroy Net/clip.wav",
		"original":    "engine six thirty two",
		"corrected":   "Engine 632\n to dispatch",
	})
	if resp.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body %s", resp.Code, resp.Body.String())
	}

	rows := readFeedbackCSV(t, path)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want header + 1", len(rows))
	}
	if !reflect.DeepEqual(rows[0], feedbackHeader) {
		t.Fatalf("header = %v", rows[0])
	}
	got := rows[1]
	if got[1] != "Pomeroy_Net_2026-10-01T01_00_00Z.wav" || got[2] != "clip-3" {
		t.Errorf("clip identity = %q/%q", got[1], got[2])
	}
	// A correction with no explicit rating implies the transcript was wrong.
	if got[7] != string(feedbackBad) {
		t.Errorf("rating = %q, want %q", got[7], feedbackBad)
	}
	// Newlines must be folded so one record stays one CSV row.
	if got[9] != "Engine 632 to dispatch" {
		t.Errorf("corrected = %q", got[9])
	}
}

func TestFeedbackAppendsWithSingleHeader(t *testing.T) {
	s, path := newFeedbackServer(t)

	for _, rating := range []string{"good", "bad"} {
		resp := postFeedback(t, s, map[string]string{
			"wavFilename": "clip.wav",
			"rating":      rating,
		})
		if resp.Code != http.StatusNoContent {
			t.Fatalf("rating %s: status = %d", rating, resp.Code)
		}
	}

	rows := readFeedbackCSV(t, path)
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want header + 2", len(rows))
	}
	if rows[1][7] != "good" || rows[2][7] != "bad" {
		t.Errorf("ratings = %q, %q", rows[1][7], rows[2][7])
	}
}

func TestFeedbackPrefersServerSideClipMetadata(t *testing.T) {
	s, path := newFeedbackServer(t)
	s.clips["clip-9"] = clipRecord{
		clipID: "clip-9",
		info: streamInfo{
			StreamName: "Pomeroy Net",
			RegionName: "Oregon",
			GroupName:  "Umatilla NF",
		},
		wavPath:  filepath.Join("D:", "audio", "Pomeroy", "real.wav"),
		audioURL: "/audio/Oregon/Umatilla NF/Pomeroy Net/real.wav",
	}

	// The client claims a different stream; the registry must win, and the
	// filename/URL it omitted must be filled in from the clip record.
	resp := postFeedback(t, s, map[string]string{
		"clipId":     "clip-9",
		"streamName": "attacker supplied",
		"rating":     "bad",
	})
	if resp.Code != http.StatusNoContent {
		t.Fatalf("status = %d", resp.Code)
	}

	got := readFeedbackCSV(t, path)[1]
	if got[1] != "real.wav" {
		t.Errorf("filename = %q, want real.wav", got[1])
	}
	if got[3] != "Pomeroy Net" || got[4] != "Oregon" || got[5] != "Umatilla NF" {
		t.Errorf("stream metadata = %q/%q/%q", got[3], got[4], got[5])
	}
	if got[6] != "/audio/Oregon/Umatilla NF/Pomeroy Net/real.wav" {
		t.Errorf("audioUrl = %q", got[6])
	}
}

func TestFeedbackRejectsUselessOrUnidentifiedRows(t *testing.T) {
	tests := []struct {
		name string
		body map[string]string
		want int
	}{
		{
			name: "no rating and no correction",
			body: map[string]string{"wavFilename": "clip.wav"},
			want: http.StatusBadRequest,
		},
		{
			name: "no clip identity",
			body: map[string]string{"rating": "good"},
			want: http.StatusBadRequest,
		},
		{
			name: "unknown rating",
			body: map[string]string{"wavFilename": "clip.wav", "rating": "amazing"},
			want: http.StatusBadRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, path := newFeedbackServer(t)
			if got := postFeedback(t, s, tt.body); got.Code != tt.want {
				t.Fatalf("status = %d, want %d", got.Code, tt.want)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("rejected request still wrote the corpus (stat err %v)", err)
			}
		})
	}
}

func TestFeedbackRejectsNonPost(t *testing.T) {
	s, _ := newFeedbackServer(t)
	rec := httptest.NewRecorder()
	s.handleTranscriptFeedback(rec, httptest.NewRequest(http.MethodGet, "/transcripts/feedback", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", rec.Code)
	}
}

// A deployment with no audio archive has no WAVs to pair corrections with,
// so the endpoint reports itself unavailable rather than silently accepting
// data it will discard.
func TestFeedbackDisabledWithoutArchive(t *testing.T) {
	s := &webrtcServer{
		logger:      log.New(io.Discard, "", 0),
		usageLogger: &usageLogger{},
		clips:       make(map[string]clipRecord),
		feedback:    newFeedbackStore("", log.New(io.Discard, "", 0)),
	}
	if got := postFeedback(t, s, map[string]string{"wavFilename": "c.wav", "rating": "good"}); got.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", got.Code)
	}
}

func TestFeedbackClampsOversizedText(t *testing.T) {
	s, path := newFeedbackServer(t)
	resp := postFeedback(t, s, map[string]string{
		"wavFilename": "clip.wav",
		"corrected":   strings.Repeat("é", maxFeedbackTextLen),
	})
	if resp.Code != http.StatusNoContent {
		t.Fatalf("status = %d", resp.Code)
	}
	got := readFeedbackCSV(t, path)[1][9]
	if len(got) > maxFeedbackTextLen {
		t.Errorf("stored %d bytes, want <= %d", len(got), maxFeedbackTextLen)
	}
	// Truncation must land on a rune boundary, never mid-sequence.
	if strings.ContainsRune(got, '\uFFFD') {
		t.Error("truncation split a multi-byte rune")
	}
}
