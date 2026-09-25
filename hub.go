package main

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var unsafeChars = regexp.MustCompile(`[^a-zA-Z0-9_\-]`)

// transcriptEvent is the JSON payload sent to SSE subscribers.
// When Type == "clip", the audio is ready but text may be empty (pending transcription).
// When Type == "transcript", the text has been filled in for a prior clip (matched by ClipID).
type transcriptEvent struct {
	Type        string    `json:"type"` // "clip" or "transcript"
	ClipID      string    `json:"clipId"`
	StreamID    string    `json:"streamId"`
	StreamName  string    `json:"streamName"`
	RegionName  string    `json:"regionName"`
	GroupName   string    `json:"groupName"`
	Text        string    `json:"text,omitempty"`
	AudioURL    string    `json:"audioUrl,omitempty"`
	DurationMs  int       `json:"durationMs,omitempty"`
	Timestamp   time.Time `json:"timestamp"`
	WAVFilename string    `json:"-"`
}

// transcriptHub fans out transcript events to SSE subscribers.
type transcriptHub struct {
	mu     sync.RWMutex
	subs   map[string]chan transcriptEvent // keyed by arbitrary subscriber ID
	nextID uint64
	logDir string
	logger *log.Logger

	// archiveMu/archivePath guard the permanent transcript archive (see
	// appendArchive). Separate from mu/logDir since the archive is a
	// single flat CSV file — living inside the primary audio archive
	// directory, not the per-stream "transcripts" logDir above — and is
	// orthogonal to the per-stream JSON logs and SSE fan-out.
	archiveMu   sync.Mutex
	archivePath string
}

func newTranscriptHub(logDir string, archivePath string, logger *log.Logger) *transcriptHub {
	if logDir != "" {
		_ = os.MkdirAll(logDir, 0755)
	}
	if archivePath != "" {
		_ = os.MkdirAll(filepath.Dir(archivePath), 0755)
	}
	return &transcriptHub{
		subs:        make(map[string]chan transcriptEvent),
		logDir:      logDir,
		archivePath: archivePath,
		logger:      logger,
	}
}

func (h *transcriptHub) subscribe() (string, chan transcriptEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	id := fmt.Sprintf("sub-%d", h.nextID)
	ch := make(chan transcriptEvent, 256)
	h.subs[id] = ch
	return id, ch
}

func (h *transcriptHub) unsubscribe(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ch, ok := h.subs[id]; ok {
		delete(h.subs, id)
		close(ch)
	}
}

// Publish sends an event to all subscribers, appends it to the per-stream
// log, and — for actual transcript text — appends it to the permanent
// transcript archive.
func (h *transcriptHub) Publish(event transcriptEvent) {
	// Write to per-stream log file.
	if h.logDir != "" {
		h.appendLog(event)
	}

	// Write to the permanent, never-pruned transcript archive. Only
	// "transcript" events carrying actual transcribed speech belong in a
	// log of "the output of whisper transcriptions" — bracketed markers
	// like "[transcription failed]"/"[no speech detected]" (see
	// whisperPool.worker) are UI status, not real output, so they're
	// excluded here.
	if event.Type == "transcript" && event.Text != "" && !strings.HasPrefix(event.Text, "[") {
		h.appendArchive(event)
	}

	h.mu.RLock()
	targets := make(map[string]chan transcriptEvent, len(h.subs))
	for id, ch := range h.subs {
		targets[id] = ch
	}
	h.mu.RUnlock()

	for id, ch := range targets {
		select {
		case ch <- event:
		default:
			h.logger.Printf("transcript hub: dropping slow subscriber %s", id)
			h.unsubscribe(id)
		}
	}
}

// logFilename returns a safe filename derived from the stream name.
func logFilename(streamName string) string {
	safe := unsafeChars.ReplaceAllString(streamName, "_")
	return safe + ".log"
}

// appendLog writes one transcript event as a JSON line to the stream's log file.
func (h *transcriptHub) appendLog(event transcriptEvent) {
	path := filepath.Join(h.logDir, logFilename(event.StreamName))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		h.logger.Printf("transcript log: open %s: %v", path, err)
		return
	}
	defer f.Close()
	if err := json.NewEncoder(f).Encode(event); err != nil {
		h.logger.Printf("transcript log: write %s: %v", path, err)
	}
}

// appendArchive appends one row — WAV filename, stream name, transcript text —
// to the permanent transcript archive CSV file (writing a header row first
// if the file is new/empty). Unlike appendLog's per-stream JSON files, this
// file is opened append-only and is never truncated, rewritten, or pruned
// anywhere in this codebase, so it accumulates every transcript ever
// produced for the life of the deployment. Guarded by its own mutex
// (distinct from mu, which guards the SSE subscriber map) since this is a
// simple serialized file-append independent of fan-out.
func (h *transcriptHub) appendArchive(event transcriptEvent) {
	if h.archivePath == "" {
		return
	}
	h.archiveMu.Lock()
	defer h.archiveMu.Unlock()

	writeHeader := false
	if fi, err := os.Stat(h.archivePath); err != nil || fi.Size() == 0 {
		writeHeader = true
	}

	f, err := os.OpenFile(h.archivePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		h.logger.Printf("transcript archive: open %s: %v", h.archivePath, err)
		return
	}
	defer f.Close()

	w := csv.NewWriter(f)
	if writeHeader {
		if err := w.Write([]string{"filename", "streamName", "transcript"}); err != nil {
			h.logger.Printf("transcript archive: write header %s: %v", h.archivePath, err)
		}
	}

	text := strings.Join(strings.Fields(event.Text), " ")
	if err := w.Write([]string{event.WAVFilename, event.StreamName, text}); err != nil {
		h.logger.Printf("transcript archive: write %s: %v", h.archivePath, err)
	}
	w.Flush()
	if err := w.Error(); err != nil {
		h.logger.Printf("transcript archive: flush %s: %v", h.archivePath, err)
	}
}

// History returns transcript events for a stream with a timestamp in
// (since, until]. A zero since means "from the beginning of recorded
// history" and a zero until means "up to now" — callers wanting the full,
// never-pruned history for a stream (see the package doc comment on
// AudioLogDir: audio and transcripts are kept indefinitely) simply pass
// zero values for both. Lookups are keyed by stream name rather than the
// server's runtime stream ID: log files are already written
// one-per-stream-name (see logFilename), and stream IDs are randomly
// regenerated on every server restart (see nextStreamID), so they can't be
// used to find events written during a previous run.
func (h *transcriptHub) History(streamName string, since, until time.Time) ([]transcriptEvent, error) {
	if h.logDir == "" || streamName == "" {
		return nil, nil
	}

	path := filepath.Join(h.logDir, logFilename(streamName))
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if until.IsZero() {
		until = time.Now()
	}
	var events []transcriptEvent
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var ev transcriptEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}
		if !since.IsZero() && !ev.Timestamp.After(since) {
			continue
		}
		if ev.Timestamp.After(until) {
			continue
		}
		events = append(events, ev)
	}
	return events, nil
}

// HasRecentActivity returns true if any clip event for streamName has been
// logged within the last maxAge duration. This is a lightweight scan used to
// determine whether a stream has been heard recently even after a server
// restart (see History for why this is keyed by stream name, not the
// server's runtime stream ID).
func (h *transcriptHub) HasRecentActivity(streamName string, maxAge time.Duration) bool {
	if h.logDir == "" || streamName == "" {
		return false
	}

	path := filepath.Join(h.logDir, logFilename(streamName))
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	cutoff := time.Now().Add(-maxAge)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var ev transcriptEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}
		if ev.Type == "clip" && ev.Timestamp.After(cutoff) {
			return true
		}
	}
	return false
}

// ServeHTTP handles GET /transcripts as a Server-Sent Events stream.
// Optional query param: ?streamId=stream-7420 to filter to one stream.
func (h *transcriptHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	filterID := r.URL.Query().Get("streamId")

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// Send a keep-alive comment immediately so the browser knows the connection is live.
	fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	subID, ch := h.subscribe()
	defer h.unsubscribe(subID)

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return

		case <-ticker.C:
			// Keep-alive ping.
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()

		case event, ok := <-ch:
			if !ok {
				return
			}
			if filterID != "" && event.StreamID != filterID {
				continue
			}
			data, err := json.Marshal(event)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}
