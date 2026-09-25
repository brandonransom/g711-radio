package main

import (
	"bufio"
	"encoding/binary"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var unsafeChars = regexp.MustCompile(`[^a-zA-Z0-9_\-]`)
var recordingTimestampPattern = regexp.MustCompile(`_(\d{4}-\d{2}-\d{2}T\d{2}_\d{2}_\d{2}Z)\.wav$`)

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

// History returns persisted transcript-log events for a stream with a timestamp in
// (since, until]. A zero since means "from the beginning of recorded
// history" and a zero until means "up to now". Lookups are keyed by stream
// name rather than the server's runtime stream ID: log files are already
// written one-per-stream-name (see logFilename), and stream IDs are randomly
// regenerated on every server restart (see nextStreamID).
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

// RecordingHistory returns every WAV recording currently present in the
// configured audio archive for a stream, enriched with any matching
// transcript-log events. WAV files are the source of truth so recordings
// remain visible if the server restarts with a missing or relocated
// transcripts directory. Log-only events are retained for compatibility.
func (h *transcriptHub) RecordingHistory(audioLogDir string, info streamInfo, since, until time.Time) ([]transcriptEvent, error) {
	logged, err := h.History(info.StreamName, since, until)
	if err != nil {
		h.logger.Printf("recording history: read transcript log for %s: %v", info.StreamName, err)
		logged = nil
	}

	recordings, err := scanStreamRecordings(audioLogDir, info, since, until, h.logger)
	if err != nil {
		return nil, err
	}

	recordingByURL := make(map[string]int, len(recordings))
	for i := range recordings {
		recordingByURL[recordings[i].AudioURL] = i
	}

	// Preserve the original clip ID where a persisted clip event still
	// exists, allowing its transcript event to merge with the filesystem
	// recording exactly as it did before the restart.
	logClipToRecording := make(map[string]int)
	for _, ev := range logged {
		if ev.Type != "clip" || ev.AudioURL == "" {
			continue
		}
		if i, ok := recordingByURL[ev.AudioURL]; ok {
			if ev.ClipID != "" {
				recordings[i].ClipID = ev.ClipID
				logClipToRecording[ev.ClipID] = i
			}
			if recordings[i].DurationMs == 0 {
				recordings[i].DurationMs = ev.DurationMs
			}
		}
	}

	events := make([]transcriptEvent, 0, len(recordings)+len(logged))
	events = append(events, recordings...)
	for _, ev := range logged {
		ev.StreamID = info.ID
		ev.StreamName = info.StreamName
		ev.RegionName = info.RegionName
		ev.GroupName = info.GroupName

		recordingIndex, matched := recordingByURL[ev.AudioURL]
		if !matched && ev.ClipID != "" {
			recordingIndex, matched = logClipToRecording[ev.ClipID]
		}
		if matched {
			if ev.Type == "clip" {
				continue
			}
			ev.ClipID = events[recordingIndex].ClipID
			ev.AudioURL = events[recordingIndex].AudioURL
			ev.Timestamp = events[recordingIndex].Timestamp
		}
		events = append(events, ev)
	}
	return events, nil
}

func scanStreamRecordings(audioLogDir string, info streamInfo, since, until time.Time, logger *log.Logger) ([]transcriptEvent, error) {
	if audioLogDir == "" || info.StreamName == "" {
		return nil, nil
	}
	if until.IsZero() {
		until = time.Now()
	}

	safe := func(s string) string {
		return unsafeChars.ReplaceAllString(s, "_")
	}
	regionName := safe(info.RegionName)
	groupName := safe(info.GroupName)
	streamName := safe(info.StreamName)
	dir := filepath.Join(audioLogDir, regionName, groupName, streamName)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	recordings := make([]transcriptEvent, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".wav") {
			continue
		}

		timestamp, ok := recordingTimestamp(entry.Name())
		if !ok {
			fileInfo, err := entry.Info()
			if err != nil {
				logger.Printf("recording history: stat %s/%s: %v", info.StreamName, entry.Name(), err)
				continue
			}
			timestamp = fileInfo.ModTime()
		}
		if !since.IsZero() && !timestamp.After(since) {
			continue
		}
		if timestamp.After(until) {
			continue
		}

		wavPath := filepath.Join(dir, entry.Name())
		durationMs, err := wavDurationMs(wavPath)
		if err != nil {
			logger.Printf("recording history: read duration %s: %v", wavPath, err)
		}
		audioURL := "/" + path.Join("audio", regionName, groupName, streamName, entry.Name())
		recordings = append(recordings, transcriptEvent{
			Type:        "clip",
			ClipID:      entry.Name(),
			StreamID:    info.ID,
			StreamName:  info.StreamName,
			RegionName:  info.RegionName,
			GroupName:   info.GroupName,
			AudioURL:    audioURL,
			DurationMs:  durationMs,
			Timestamp:   timestamp,
			WAVFilename: entry.Name(),
		})
	}
	return recordings, nil
}

func recordingTimestamp(filename string) (time.Time, bool) {
	match := recordingTimestampPattern.FindStringSubmatch(filename)
	if len(match) != 2 {
		return time.Time{}, false
	}
	timestamp, err := time.Parse("2006-01-02T15_04_05Z", match[1])
	return timestamp, err == nil
}

// wavDurationMs reads the RIFF fmt/data chunks rather than assuming a fixed
// 44-byte header, so recordings produced by other WAV encoders also work.
func wavDurationMs(wavPath string) (int, error) {
	f, err := os.Open(wavPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	var header [12]byte
	if _, err := io.ReadFull(f, header[:]); err != nil {
		return 0, err
	}
	if string(header[:4]) != "RIFF" || string(header[8:]) != "WAVE" {
		return 0, fmt.Errorf("%s is not a RIFF/WAVE file", wavPath)
	}

	var byteRate uint32
	var dataSize uint32
	for {
		var chunkHeader [8]byte
		if _, err := io.ReadFull(f, chunkHeader[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return 0, err
		}
		chunkSize := binary.LittleEndian.Uint32(chunkHeader[4:])
		switch string(chunkHeader[:4]) {
		case "fmt ":
			if chunkSize < 12 {
				return 0, fmt.Errorf("%s has an invalid fmt chunk", wavPath)
			}
			var format [12]byte
			if _, err := io.ReadFull(f, format[:]); err != nil {
				return 0, err
			}
			byteRate = binary.LittleEndian.Uint32(format[8:12])
			if _, err := f.Seek(int64(chunkSize-12), io.SeekCurrent); err != nil {
				return 0, err
			}
		case "data":
			dataSize = chunkSize
			if _, err := f.Seek(int64(chunkSize), io.SeekCurrent); err != nil {
				return 0, err
			}
		default:
			if _, err := f.Seek(int64(chunkSize), io.SeekCurrent); err != nil {
				return 0, err
			}
		}
		if chunkSize%2 != 0 {
			if _, err := f.Seek(1, io.SeekCurrent); err != nil {
				return 0, err
			}
		}
		if byteRate > 0 && dataSize > 0 {
			return int(uint64(dataSize) * 1000 / uint64(byteRate)), nil
		}
	}
	if byteRate == 0 || dataSize == 0 {
		return 0, fmt.Errorf("%s is missing WAV format or audio data", wavPath)
	}
	return int(uint64(dataSize) * 1000 / uint64(byteRate)), nil
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
