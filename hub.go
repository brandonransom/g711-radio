package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// transcriptEvent is the JSON payload sent to SSE subscribers.
type transcriptEvent struct {
	StreamID   string    `json:"streamId"`
	StreamName string    `json:"streamName"`
	RegionName string    `json:"regionName"`
	GroupName  string    `json:"groupName"`
	Text       string    `json:"text"`
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

// appendLog writes one transcript event as a JSON line to the stream's log file.
func (h *transcriptHub) appendLog(event transcriptEvent) {
	path := filepath.Join(h.logDir, event.StreamID+".log")
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
func (h *transcriptHub) History(streamID string, maxAge time.Duration) ([]transcriptEvent, error) {
	path := filepath.Join(h.logDir, streamID+".log")
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	cutoff := time.Now().Add(-maxAge)
	var events []transcriptEvent
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var ev transcriptEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}
		if ev.Timestamp.After(cutoff) {
			events = append(events, ev)
		}
	}
	return events, scanner.Err()
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
