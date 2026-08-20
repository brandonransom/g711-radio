package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
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
	mu     sync.RWMutex
	subs   map[string]chan transcriptEvent // keyed by arbitrary subscriber ID
	nextID uint64
	logger *log.Logger
}

func newTranscriptHub(logger *log.Logger) *transcriptHub {
	return &transcriptHub{
		subs:   make(map[string]chan transcriptEvent),
		logger: logger,
	}
}

func (h *transcriptHub) subscribe() (string, chan transcriptEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	id := fmt.Sprintf("sub-%d", h.nextID)
	ch := make(chan transcriptEvent, 32)
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

// Publish sends an event to all subscribers. Slow subscribers are dropped.
func (h *transcriptHub) Publish(event transcriptEvent) {
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

	ticker := time.NewTicker(15 * time.Second)
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
