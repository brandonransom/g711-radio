package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Whisper has no online learning: nothing a listener clicks here changes the
// model's behaviour on the next clip. The value of this file is the corpus it
// accumulates. A corrected transcript paired with the WAV that produced it is
// (a) a source of domain vocabulary to fold into whisper's initial prompt
// (see whisperConfig.InferenceParams), (b) ground truth for comparing model /
// beam-size / prompt changes against each other instead of guessing, and
// (c) a labelled dataset for an eventual fine-tune. All three need the same
// thing — audio plus a human-approved transcript — so that is what gets
// written, append-only, in a format a spreadsheet or a training script can
// both read.

// Bounds on client-supplied strings. Corrections are a sentence or two of
// radio traffic; anything beyond this is a malfunctioning or hostile client,
// and the cap keeps a single POST from bloating the corpus.
const (
	maxFeedbackTextLen = 4000
	maxFeedbackMetaLen = 200
)

// feedbackRating is the listener's verdict on a transcript.
type feedbackRating string

const (
	feedbackGood feedbackRating = "good"
	feedbackBad  feedbackRating = "bad"
)

// feedbackRecord is one listener's assessment of one clip's transcript.
type feedbackRecord struct {
	Timestamp   time.Time
	WAVFilename string
	ClipID      string
	StreamName  string
	RegionName  string
	GroupName   string
	AudioURL    string
	Rating      feedbackRating
	Original    string // what whisper produced
	Corrected   string // what the listener says it should have been
	ClientIP    string
}

// feedbackHeader is the CSV header. Append new columns at the end only:
// existing files are never rewritten, so reordering would silently
// misinterpret every row already on disk.
var feedbackHeader = []string{
	"timestamp", "filename", "clipId", "streamName", "regionName",
	"groupName", "audioUrl", "rating", "original", "corrected", "clientIp",
}

// feedbackStore appends transcript feedback to a CSV file. Like the
// transcript archive it lives in the audio archive directory so the
// corrections travel with the WAVs they describe — a correction is worthless
// for prompt mining or fine-tuning once separated from its audio. A zero
// store (empty path) silently discards writes, which is what happens when no
// audioLogDir is configured and there are no WAVs to pair corrections with.
type feedbackStore struct {
	mu     sync.Mutex
	path   string
	logger *log.Logger
}

func newFeedbackStore(path string, logger *log.Logger) *feedbackStore {
	if path != "" {
		_ = os.MkdirAll(filepath.Dir(path), 0755)
	}
	return &feedbackStore{path: path, logger: logger}
}

func (s *feedbackStore) enabled() bool { return s != nil && s.path != "" }

// Append writes one feedback row, creating the file and header if needed.
func (s *feedbackStore) Append(rec feedbackRecord) error {
	if !s.enabled() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	writeHeader := false
	if fi, err := os.Stat(s.path); err != nil || fi.Size() == 0 {
		writeHeader = true
	}

	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open %s: %w", s.path, err)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	if writeHeader {
		if err := w.Write(feedbackHeader); err != nil {
			return fmt.Errorf("write header %s: %w", s.path, err)
		}
	}
	row := []string{
		rec.Timestamp.UTC().Format(time.RFC3339),
		rec.WAVFilename,
		rec.ClipID,
		rec.StreamName,
		rec.RegionName,
		rec.GroupName,
		rec.AudioURL,
		string(rec.Rating),
		collapseSpace(rec.Original),
		collapseSpace(rec.Corrected),
		rec.ClientIP,
	}
	if err := w.Write(row); err != nil {
		return fmt.Errorf("write %s: %w", s.path, err)
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return fmt.Errorf("flush %s: %w", s.path, err)
	}
	return nil
}

// collapseSpace folds newlines and runs of whitespace into single spaces so
// one record stays one CSV row and stays diffable against whisper output,
// which is itself normalised this way before archiving.
func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// clampLen truncates over-long client input to max bytes, backing off to the
// nearest rune boundary so a malformed POST can't write a multi-megabyte
// cell or a broken UTF-8 sequence into the corpus. Truncation is done by
// byte index rather than by re-encoding a rune slice, so a 64 KB body costs
// a constant scan rather than a quadratic one.
func clampLen(s string, max int) string {
	if len(s) <= max {
		return s
	}
	// A rune is at most 4 bytes, so a valid boundary is within 3 bytes.
	for i := max; i > max-utf8.UTFMax && i > 0; i-- {
		if utf8.RuneStart(s[i]) {
			return s[:i]
		}
	}
	return s[:max]
}

// handleTranscriptFeedback records a listener's verdict on a transcript.
//
// The clip is identified by WAV filename where possible. ClipID is only
// unique within a single server process (see nextClipID), so a correction
// keyed by ClipID alone becomes unresolvable after a restart — useless for
// pairing with audio later. Filename is the identity that survives.
func (s *webrtcServer) handleTranscriptFeedback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.feedback.enabled() {
		http.Error(w, "feedback disabled", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		ClipID      string `json:"clipId"`
		WAVFilename string `json:"wavFilename"`
		AudioURL    string `json:"audioUrl"`
		StreamName  string `json:"streamName"`
		Rating      string `json:"rating"`
		Original    string `json:"original"`
		Corrected   string `json:"corrected"`
	}
	// Cap the body itself: clampLen protects individual fields, but without
	// this a single request could still stream unbounded JSON into memory.
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	rec := feedbackRecord{
		Timestamp:   time.Now(),
		WAVFilename: clampLen(strings.TrimSpace(req.WAVFilename), maxFeedbackMetaLen),
		ClipID:      clampLen(strings.TrimSpace(req.ClipID), maxFeedbackMetaLen),
		StreamName:  clampLen(strings.TrimSpace(req.StreamName), maxFeedbackMetaLen),
		AudioURL:    clampLen(strings.TrimSpace(req.AudioURL), maxFeedbackMetaLen),
		Original:    clampLen(req.Original, maxFeedbackTextLen),
		Corrected:   clampLen(req.Corrected, maxFeedbackTextLen),
		ClientIP:    getClientIP(r),
	}

	switch feedbackRating(strings.TrimSpace(req.Rating)) {
	case feedbackGood:
		rec.Rating = feedbackGood
	case feedbackBad:
		rec.Rating = feedbackBad
	case "":
		// A correction with no explicit rating is implicitly "this was
		// wrong" — recording it as such keeps the rating column usable for
		// counting error rates.
		if rec.Corrected != "" {
			rec.Rating = feedbackBad
		}
	default:
		http.Error(w, "invalid rating", http.StatusBadRequest)
		return
	}

	// A row with neither a rating nor a correction carries no signal.
	if rec.Rating == "" && rec.Corrected == "" {
		http.Error(w, "nothing to record", http.StatusBadRequest)
		return
	}
	// Without a filename or clip ID the audio can never be paired back up,
	// which is the only reason this corpus exists.
	if rec.WAVFilename == "" && rec.ClipID == "" && rec.AudioURL == "" {
		http.Error(w, "missing clip identity", http.StatusBadRequest)
		return
	}

	// Prefer server-side stream metadata where the clip is still in the
	// registry; the client only knows the display name.
	if rec.ClipID != "" {
		s.clipMu.RLock()
		clip, ok := s.clips[rec.ClipID]
		s.clipMu.RUnlock()
		if ok {
			rec.StreamName = clip.info.StreamName
			rec.RegionName = clip.info.RegionName
			rec.GroupName = clip.info.GroupName
			if rec.WAVFilename == "" {
				rec.WAVFilename = wavBaseName(clip.wavPath)
			}
			if rec.AudioURL == "" {
				rec.AudioURL = clip.audioURL
			}
		}
	}

	if err := s.feedback.Append(rec); err != nil {
		s.logger.Printf("transcript feedback: %v", err)
		http.Error(w, "could not record feedback", http.StatusInternalServerError)
		return
	}

	label := rec.WAVFilename
	if label == "" {
		label = rec.ClipID
	}
	if rec.Corrected != "" {
		s.logger.Printf("transcript feedback: %s rated %s for %s (correction supplied)", rec.ClientIP, rec.Rating, label)
	} else {
		s.logger.Printf("transcript feedback: %s rated %s for %s", rec.ClientIP, rec.Rating, label)
	}
	s.usageLogger.logUsage("transcript_feedback", map[string]string{
		"client_ip": rec.ClientIP,
		"clip_id":   rec.ClipID,
		"stream":    rec.StreamName,
		"source":    string(rec.Rating),
	})

	w.WriteHeader(http.StatusNoContent)
}
