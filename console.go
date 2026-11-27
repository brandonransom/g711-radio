package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	consoleBufferLines  = 2000
	consoleMaxLineBytes = 8 << 10
	consoleKeepAlive    = 20 * time.Second
)

//go:embed templates/console.html
var consoleTemplateText string

var consoleTemplate = template.Must(template.New("console").Parse(consoleTemplateText))

// logBuffer is an io.Writer that keeps the most recent server log lines in
// memory and lets any number of readers follow them live. Lines are numbered
// from 1 for the life of the process; the buffer holds the newest
// consoleBufferLines of them.
type logBuffer struct {
	mu      sync.Mutex
	lines   []string
	count   int
	last    uint64
	partial []byte
	signal  chan struct{}

	done     chan struct{}
	doneOnce sync.Once
}

func newLogBuffer() *logBuffer {
	return &logBuffer{
		lines:  make([]string, consoleBufferLines),
		signal: make(chan struct{}),
		done:   make(chan struct{}),
	}
}

// Write splits p into lines; an unterminated tail is held until its newline
// arrives. It never fails and never blocks on readers.
func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.partial = append(b.partial, p...)
	added := false
	for {
		i := bytes.IndexByte(b.partial, '\n')
		if i < 0 {
			break
		}
		b.add(string(bytes.TrimSuffix(b.partial[:i], []byte("\r"))))
		b.partial = b.partial[i+1:]
		added = true
	}
	if len(b.partial) > consoleMaxLineBytes {
		b.add(string(b.partial))
		b.partial = nil
		added = true
	}
	if added {
		close(b.signal)
		b.signal = make(chan struct{})
	}
	return len(p), nil
}

func (b *logBuffer) add(line string) {
	if len(line) > consoleMaxLineBytes {
		line = line[:consoleMaxLineBytes] + " ...[truncated]"
	}
	b.last++
	b.lines[(b.last-1)%uint64(len(b.lines))] = line
	if b.count < len(b.lines) {
		b.count++
	}
}

// Close ends every stream following the buffer so a server shutdown is not
// held up by open console connections.
func (b *logBuffer) Close() {
	b.doneOnce.Do(func() { close(b.done) })
}

// wait returns a channel that is closed when more lines arrive. Fetch it
// before calling Since so a line written in between is never missed.
func (b *logBuffer) wait() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.signal
}

type consoleUpdate struct {
	Lines []string `json:"lines"`
	// Next is the sequence number to pass as "after" on the next call.
	Next uint64 `json:"next"`
	// Reset means the caller's position was ahead of the buffer (the server
	// restarted), so its display should be cleared before showing Lines.
	Reset bool `json:"reset"`
	// Missed means lines between the caller's position and the first line
	// returned were dropped because the buffer wrapped.
	Missed bool `json:"missed"`
}

// Since returns every buffered line numbered after "after" (0 for everything).
func (b *logBuffer) Since(after uint64) consoleUpdate {
	b.mu.Lock()
	defer b.mu.Unlock()
	u := consoleUpdate{Lines: []string{}, Next: b.last}
	if after > b.last {
		u.Reset = true
		after = 0
	}
	from := after + 1
	if oldest := b.last - uint64(b.count) + 1; from < oldest {
		u.Missed = after > 0
		from = oldest
	}
	for seq := from; seq <= b.last; seq++ {
		u.Lines = append(u.Lines, b.lines[(seq-1)%uint64(len(b.lines))])
	}
	return u
}

// registerConsole serves the live server console at path+"/console", behind
// the same super user login as the dashboard's other admin features.
func (a *analyticsStore) registerConsole(mux *http.ServeMux, path string) {
	page := path + "/console"
	stream := page + "/stream"
	mux.HandleFunc(page, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		privateHeaders(w)
		if !a.super.authorize(w, r) {
			return
		}
		nonce := randomToken()
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'nonce-"+nonce+"'; connect-src 'self'")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := consoleTemplate.Execute(w, struct{ Nonce, StreamURL string }{nonce, stream}); err != nil {
			a.logger.Printf("console: render page: %v", err)
		}
	})
	mux.HandleFunc(stream, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		privateHeaders(w)
		if !a.super.authorize(w, r) {
			return
		}
		after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
		serveConsoleStream(w, r, a.console, after)
	})
}

// serveConsoleStream sends the buffered lines after "after", then every new
// line as it is logged, as Server-Sent Events until the client leaves or the
// buffer is closed.
func serveConsoleStream(w http.ResponseWriter, r *http.Request, buf *logBuffer, after uint64) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	keepAlive := time.NewTicker(consoleKeepAlive)
	defer keepAlive.Stop()
	for {
		signal := buf.wait()
		if u := buf.Since(after); len(u.Lines) > 0 || u.Reset || u.Missed {
			data, err := json.Marshal(u)
			if err != nil {
				return
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				return
			}
			flusher.Flush()
			after = u.Next
		}
		select {
		case <-signal:
		case <-keepAlive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-buf.done:
			return
		case <-r.Context().Done():
			return
		}
	}
}
