package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

var unsafeChars = regexp.MustCompile(`[^a-zA-Z0-9_\-]`)

// transcriptEvent is the JSON payload sent to SSE subscribers.
// When Type == "clip", the audio is ready but text may be empty (pending transcription).
// When Type == "transcript", the text has been filled in for a prior clip (matched by ClipID).
type transcriptEvent struct {
	Type       string    `json:"type"` // "clip" or "transcript"
	ClipID     string    `json:"clipId"`
	StreamID   string    `json:"streamId"`
	StreamName string    `json:"streamName"`
	RegionName string    `json:"regionName"`
	GroupName  string    `json:"groupName"`
	Text       string    `json:"text,omitempty"`
	AudioURL   string    `json:"audioUrl,omitempty"`
	DurationMs int       `json:"durationMs,omitempty"`
	Timestamp  time.Time `json:"timestamp"`
}

// transcriptHub fans out transcript events to SSE subscribers.
type transcriptHub struct {
	mu      sync.RWMutex
	subs    map[string]chan transcriptEvent // keyed by arbitrary subscriber ID
	nextID  uint64
	logDir  string
	logger  *log.Logger
}

func newTranscriptHub(logDir string, logger *log.Logger) *transcriptHub {
	if logDir != "" {
		_ = os.MkdirAll(logDir, 0755)
	}
	return &transcriptHub{
		subs:   make(map[string]chan transcriptEvent),
		logDir: logDir,
		logger: logger,
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

// Publish sends an event to all subscribers and appends it to the stream log.
func (h *transcriptHub) Publish(event transcriptEvent) {
	// Write to per-stream log file.
	if h.logDir != "" {
		h.appendLog(event)
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

// History returns transcript events for a stream within the last maxAge duration.
// streamID is used to look up the stream name via the event's StreamID field,
// but the file is keyed by stream name. The caller passes the streamID and we
// scan for a matching log by checking all files — or the caller can pass streamName directly.
// For simplicity we accept either streamID or streamName as the lookup key.
func (h *transcriptHub) History(streamID string, maxAge time.Duration) ([]transcriptEvent, error) {
	// Try to find a log file whose events match this streamID.
	// We scan all .log files in the dir and return the first match.
	entries, err := os.ReadDir(h.logDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	cutoff := time.Now().Add(-maxAge)
	var events []transcriptEvent

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".log" {
			continue
		}
		path := filepath.Join(h.logDir, entry.Name())
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		matched := false
		var fileEvents []transcriptEvent
		for scanner.Scan() {
			var ev transcriptEvent
			if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
				continue
			}
			if ev.StreamID == streamID {
				matched = true
				if ev.Timestamp.After(cutoff) {
					fileEvents = append(fileEvents, ev)
				}
			}
		}
		f.Close()
		if matched {
			events = fileEvents
			break
		}
	}
	return events, nil
}

// HasRecentActivity returns true if any clip event for streamID has been logged
// within the last maxAge duration. This is a lightweight scan used to determine
// whether a stream has been heard recently even after a server restart.
func (h *transcriptHub) HasRecentActivity(streamID string, maxAge time.Duration) bool {
	if h.logDir == "" {
		return false
	}
	entries, err := os.ReadDir(h.logDir)
	if err != nil {
		return false
	}
	cutoff := time.Now().Add(-maxAge)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".log" {
			continue
		}
		path := filepath.Join(h.logDir, entry.Name())
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		found := false
		for scanner.Scan() {
			var ev transcriptEvent
			if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
				continue
			}
			if ev.StreamID == streamID && ev.Type == "clip" && ev.Timestamp.After(cutoff) {
				found = true
				break
			}
		}
		f.Close()
		if found {
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
