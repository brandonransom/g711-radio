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
			StateName: "Oregon",
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

func TestFeedbackCorrectionIsSharedAndBroadcast(t *testing.T) {
	s, path := newFeedbackServer(t)
	s.hub = newTranscriptHub("", "", log.New(io.Discard, "", 0))
	s.streams = map[string]*station{
		"pomeroy": {info: streamInfo{ID: "pomeroy", StreamName: "Pomeroy Net", StateName: "Oregon", GroupName: "Umatilla NF"}},
	}
	subID, ch := s.hub.subscribe()
	defer s.hub.unsubscribe(subID)

	const url = "/audio/Oregon/Umatilla_NF/Pomeroy_Net/a.wav"
	resp := postFeedback(t, s, map[string]string{
		"streamId":    "pomeroy",
		"wavFilename": "a.wav",
		"audioUrl":    url,
		"original":    "engine six thirty to dispatch",
		"corrected":   "Engine 632 to dispatch",
	})
	if resp.Code != http.StatusNoContent {
		t.Fatalf("status = %d", resp.Code)
	}

	select {
	case ev := <-ch:
		if ev.Type != "correction" || ev.Corrected != "Engine 632 to dispatch" || ev.Text != "engine six thirty to dispatch" {
			t.Errorf("event = %+v", ev)
		}
		// Stream pages filter SSE by stream ID, so it must be resolved.
		if ev.StreamID != "pomeroy" {
			t.Errorf("streamId = %q, want pomeroy", ev.StreamID)
		}
	default:
		t.Fatal("no correction event was broadcast")
	}

	// The configured stream's metadata wins over the client's display name.
	if got := readFeedbackCSV(t, path)[1]; got[3] != "Pomeroy Net" || got[4] != "Oregon" {
		t.Errorf("stream metadata = %q/%q", got[3], got[4])
	}

	history := []transcriptEvent{{Type: "clip", AudioURL: url}, {Type: "clip", AudioURL: "/audio/other.wav"}}
	s.feedback.ApplyToHistory(history)
	if history[0].Corrected != "Engine 632 to dispatch" || history[1].Corrected != "" {
		t.Errorf("history = %+v", history)
	}

	// A restart rebuilds the shared view from the CSV.
	reloaded := newFeedbackStore(path, log.New(io.Discard, "", 0))
	if c, ok := reloaded.Correction(url); !ok || c.Corrected != "Engine 632 to dispatch" {
		t.Errorf("after reload = %+v, %v", c, ok)
	}
}

func TestFeedbackLatestCorrectionWinsAndRevertRestoresWhisper(t *testing.T) {
	s, path := newFeedbackServer(t)
	s.hub = newTranscriptHub("", "", log.New(io.Discard, "", 0))
	subID, ch := s.hub.subscribe()
	defer s.hub.unsubscribe(subID)

	const url = "/audio/r/g/s/a.wav"
	post := func(corrected, rating string) {
		t.Helper()
		resp := postFeedback(t, s, map[string]string{
			"audioUrl": url, "original": "whisper text", "corrected": corrected, "rating": rating,
		})
		if resp.Code != http.StatusNoContent {
			t.Fatalf("status = %d", resp.Code)
		}
	}
	drain := func() []transcriptEvent {
		var evs []transcriptEvent
		for {
			select {
			case ev := <-ch:
				evs = append(evs, ev)
			default:
				return evs
			}
		}
	}

	post("first fix", "")
	post("second fix", "")
	if c, _ := s.feedback.Correction(url); c.Corrected != "second fix" {
		t.Errorf("latest = %q, want second fix", c.Corrected)
	}
	drain()

	// Resubmitting the same text, or a bare rating, changes nothing anyone
	// sees, so nothing is broadcast.
	post("second fix", "")
	post("", "good")
	if evs := drain(); len(evs) != 0 {
		t.Errorf("unchanged display broadcast %+v", evs)
	}

	// Submitting whisper's own text reverts the recording.
	post("whisper text", "good")
	if _, ok := s.feedback.Correction(url); ok {
		t.Error("correction still shown after revert")
	}
	evs := drain()
	if len(evs) != 1 || evs[0].Type != "correction" || evs[0].Corrected != "" {
		t.Errorf("revert events = %+v", evs)
	}

	if _, ok := newFeedbackStore(path, log.New(io.Discard, "", 0)).Correction(url); ok {
		t.Error("revert was not preserved across a reload")
	}
}

func TestFeedbackIgnoresCorrectionsWithoutRecordingURL(t *testing.T) {
	s, _ := newFeedbackServer(t)
	resp := postFeedback(t, s, map[string]string{
		"wavFilename": "a.wav", "audioUrl": "https://elsewhere/x.wav", "corrected": "text",
	})
	if resp.Code != http.StatusNoContent {
		t.Fatalf("status = %d", resp.Code)
	}
	if len(s.feedback.corrections) != 0 {
		t.Errorf("non-archive URL was shared: %+v", s.feedback.corrections)
	}
}

func TestStreamForAudioURLMatchesRecordingHistoryPaths(t *testing.T) {
	s := &webrtcServer{streams: map[string]*station{
		"pomeroy": {info: streamInfo{ID: "pomeroy", StreamName: "Pomeroy Net", StateName: "Oregon", GroupName: "Umatilla NF"}},
		"capilla": {info: streamInfo{ID: "capilla", StreamName: "Capilla", StateName: "New Mexico", GroupName: "Cibola NF"}},
	}}
	tests := []struct {
		url    string
		wantID string
	}{
		{"/audio/Oregon/Umatilla_NF/Pomeroy_Net/2026-09-30T12_00_00Z.wav", "pomeroy"},
		{"/audio/New_Mexico/Cibola_NF/Capilla/a.wav", "capilla"},
		{"/audio/Oregon/Umatilla_NF/Unknown/a.wav", ""},
		{"/audio/Oregon/Umatilla_NF/Pomeroy_Net/../../x.wav", ""},
		{"/audio/Oregon/Umatilla_NF/Pomeroy_Net/", ""},
	}
	for _, tt := range tests {
		info, ok := s.streamForAudioURL(tt.url)
		if ok != (tt.wantID != "") || info.ID != tt.wantID {
			t.Errorf("streamForAudioURL(%q) = %q, %v; want %q", tt.url, info.ID, ok, tt.wantID)
		}
	}
}
