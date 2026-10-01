package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
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

	// corrections is the latest listener correction for each recording,
	// keyed by audio URL — unlike a bare WAV filename, that is unique across
	// streams. It is what every browser is shown in place of whisper's text,
	// and is rebuilt from the CSV at startup so it survives restarts.
	corrections map[string]sharedCorrection
}

// sharedCorrection is the correction currently displayed for one recording.
type sharedCorrection struct {
	Original  string // what whisper produced
	Corrected string
}

func newFeedbackStore(path string, logger *log.Logger) *feedbackStore {
	s := &feedbackStore{path: path, logger: logger, corrections: make(map[string]sharedCorrection)}
	if path != "" {
		_ = os.MkdirAll(filepath.Dir(path), 0755)
		if err := s.loadCorrections(); err != nil && !os.IsNotExist(err) {
			logger.Printf("transcript feedback: load corrections from %s: %v", path, err)
		}
	}
	return s
}

func (s *feedbackStore) enabled() bool { return s != nil && s.path != "" }

// loadCorrections replays the CSV in order so the latest row for each
// recording wins, exactly as it did while the server was running. Columns
// are located by header name since the header only ever grows.
func (s *feedbackStore) loadCorrections() error {
	f, err := os.Open(s.path)
	if err != nil {
		return err
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if err != nil {
		if err == io.EOF {
			return nil
		}
		return err
	}
	col := make(map[string]int, len(header))
	for i, name := range header {
		col[strings.TrimSpace(name)] = i
	}
	urlCol, okURL := col["audioUrl"]
	origCol, okOrig := col["original"]
	corrCol, okCorr := col["corrected"]
	if !okURL || !okOrig || !okCorr {
		return fmt.Errorf("missing audioUrl/original/corrected columns")
	}
	field := func(row []string, i int) string {
		if i < len(row) {
			return row[i]
		}
		return ""
	}
	for {
		row, err := r.Read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		s.applyCorrection(field(row, urlCol), field(row, origCol), field(row, corrCol))
	}
}

// applyCorrection updates the shared correction for one recording and
// reports whether what listeners see changed. Callers hold s.mu, or are
// loading before the store is shared.
//
// A correction identical to whisper's output reverts the recording to
// whisper's text, so a bad edit can be undone by anyone — latest wins. A
// row with no correction (a bare rating) leaves the display alone.
func (s *feedbackStore) applyCorrection(audioURL, original, corrected string) bool {
	if !strings.HasPrefix(audioURL, "/audio/") || corrected == "" {
		return false
	}
	original, corrected = collapseSpace(original), collapseSpace(corrected)
	prev, had := s.corrections[audioURL]
	if corrected == original {
		delete(s.corrections, audioURL)
		return had
	}
	next := sharedCorrection{Original: original, Corrected: corrected}
	s.corrections[audioURL] = next
	return !had || prev != next
}

// Correction returns the correction currently shown for a recording.
func (s *feedbackStore) Correction(audioURL string) (sharedCorrection, bool) {
	if !s.enabled() {
		return sharedCorrection{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.corrections[audioURL]
	return c, ok
}

// ApplyToHistory attaches the current correction to every history event for
// a corrected recording, so a page load shows what live listeners see.
func (s *feedbackStore) ApplyToHistory(events []transcriptEvent) {
	if !s.enabled() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.corrections) == 0 {
		return
	}
	for i := range events {
		if c, ok := s.corrections[events[i].AudioURL]; ok {
			events[i].Corrected = c.Corrected
		}
	}
}

// Append writes one feedback row, creating the file and header if needed,
// and reports whether the recording's shared correction changed as a result.
func (s *feedbackStore) Append(rec feedbackRecord) (bool, error) {
	if !s.enabled() {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	writeHeader := false
	if fi, err := os.Stat(s.path); err != nil || fi.Size() == 0 {
		writeHeader = true
	}

	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return false, fmt.Errorf("open %s: %w", s.path, err)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	if writeHeader {
		if err := w.Write(feedbackHeader); err != nil {
			return false, fmt.Errorf("write header %s: %w", s.path, err)
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
		return false, fmt.Errorf("write %s: %w", s.path, err)
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return false, fmt.Errorf("flush %s: %w", s.path, err)
	}
	// Only once the row is durable, so the display never shows a correction
	// that a restart would forget.
	return s.applyCorrection(rec.AudioURL, rec.Original, rec.Corrected), nil
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
		StreamID    string `json:"streamId"`
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

	// Prefer server-side stream metadata: the clip registry while the clip
	// is still in it, otherwise the configured stream the page names. The
	// client's display name is the last resort.
	var info streamInfo
	if st, ok := s.streams[strings.TrimSpace(req.StreamID)]; ok && st != nil {
		info = st.info
	}
	if rec.ClipID != "" {
		s.clipMu.RLock()
		clip, ok := s.clips[rec.ClipID]
		s.clipMu.RUnlock()
		if ok {
			info = clip.info
			if rec.WAVFilename == "" {
				rec.WAVFilename = wavBaseName(clip.wavPath)
			}
			if clip.audioURL != "" {
				// The recording's real URL, so the shared correction is
				// keyed to the row every browser actually renders.
				rec.AudioURL = clip.audioURL
			}
		}
	}
	if info.StreamName != "" {
		rec.StreamName = info.StreamName
		rec.RegionName = info.RegionName
		rec.GroupName = info.GroupName
	}

	changed, err := s.feedback.Append(rec)
	if err != nil {
		s.logger.Printf("transcript feedback: %v", err)
		http.Error(w, "could not record feedback", http.StatusInternalServerError)
		return
	}

	// Push the new text to every open page. Without a stream ID the stream
	// pages' filtered SSE connections would never see it, though it still
	// reaches them on their next history load.
	if changed && s.hub != nil {
		ev := transcriptEvent{
			Type:        "correction",
			ClipID:      rec.ClipID,
			StreamID:    info.ID,
			StreamName:  rec.StreamName,
			RegionName:  rec.RegionName,
			GroupName:   rec.GroupName,
			Text:        collapseSpace(rec.Original),
			AudioURL:    rec.AudioURL,
			Timestamp:   rec.Timestamp,
			WAVFilename: rec.WAVFilename,
		}
		if c, ok := s.feedback.Correction(rec.AudioURL); ok {
			ev.Corrected = c.Corrected
		}
		s.hub.Publish(ev)
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
