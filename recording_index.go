package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Recording index modes (see recordingIndexConfig).
const (
	recordingIndexFull   = "full"
	recordingIndexLean   = "lean"
	recordingIndexWindow = "window"
	recordingIndexOff    = "off"

	defaultRecordingIndexWindowDays = 30
)

// recordingIndexConfig selects how /transcripts/history finds recordings.
// Every mode rebuilds from the WAV files and transcript logs at startup;
// WAV files on disk stay the source of truth.
//
//   - "full" (default): every recording plus its transcript text in memory.
//     Fastest; roughly 350 bytes per recording.
//   - "lean": every recording in memory, but transcript text is read from
//     the per-stream log on demand. Roughly 100 bytes per recording.
//   - "window": like "lean", but only the last windowDays are indexed;
//     older ranges fall back to scanning the audio folders.
//   - "off": no index; every history request scans the audio folders.
type recordingIndexConfig struct {
	Mode       string `json:"mode"`
	WindowDays int    `json:"windowDays"`
}

func (c *recordingIndexConfig) normalize() error {
	c.Mode = strings.ToLower(strings.TrimSpace(c.Mode))
	if c.Mode == "" {
		c.Mode = recordingIndexFull
	}
	switch c.Mode {
	case recordingIndexFull, recordingIndexLean, recordingIndexOff:
	case recordingIndexWindow:
		if c.WindowDays < 0 {
			return fmt.Errorf("windowDays must not be negative")
		}
		if c.WindowDays == 0 {
			c.WindowDays = defaultRecordingIndexWindowDays
		}
	default:
		return fmt.Errorf("unknown mode %q (want full, lean, window or off)", c.Mode)
	}
	return nil
}

// indexedRecording is one WAV file. Text is only populated in full mode;
// the other modes remember where the latest transcript line sits in the
// stream's log (textOff < 0 means no transcript yet).
type indexedRecording struct {
	unixNano   int64
	durationMs int32
	textLen    int32
	textOff    int64
	name       string
	text       string
}

type streamRecordingIndex struct {
	mu      sync.RWMutex
	info    streamInfo
	dir     string // absolute WAV directory
	logPath string
	ready   bool
	recs    []indexedRecording // sorted by unixNano ascending
	// pending holds live events published while the startup build runs;
	// they are replayed once it finishes so nothing is lost.
	pending   []pendingIndexEvent
	lastPrune time.Time
}

type pendingIndexEvent struct {
	ev      transcriptEvent
	off     int64
	lineLen int
}

type recordingIndex struct {
	mode        string
	window      time.Duration
	audioLogDir string
	logDir      string
	logger      *log.Logger

	mu      sync.RWMutex
	streams map[string]*streamRecordingIndex // keyed by recordingDirKey
}

// newRecordingIndex returns nil (no index) for mode "off" or when there is
// no audio archive to index.
func newRecordingIndex(cfg recordingIndexConfig, audioLogDir, logDir string, logger *log.Logger) *recordingIndex {
	if cfg.Mode == recordingIndexOff || audioLogDir == "" {
		return nil
	}
	idx := &recordingIndex{
		mode:        cfg.Mode,
		audioLogDir: audioLogDir,
		logDir:      logDir,
		logger:      logger,
		streams:     make(map[string]*streamRecordingIndex),
	}
	if cfg.Mode == recordingIndexWindow {
		idx.window = time.Duration(cfg.WindowDays) * 24 * time.Hour
	}
	return idx
}

func recordingDirKey(stateName, groupName, streamName string) string {
	safe := func(s string) string { return unsafeChars.ReplaceAllString(s, "_") }
	return safe(stateName) + "/" + safe(groupName) + "/" + safe(streamName)
}

// register adds a stream to the index. Call for every stream before build.
func (idx *recordingIndex) register(info streamInfo) {
	if idx == nil {
		return
	}
	key := recordingDirKey(info.StateName, info.GroupName, info.StreamName)
	s := &streamRecordingIndex{
		info: info,
		dir:  filepath.Join(idx.audioLogDir, filepath.FromSlash(key)),
	}
	if idx.logDir != "" {
		s.logPath = filepath.Join(idx.logDir, logFilename(info.StreamName))
	}
	idx.mu.Lock()
	idx.streams[key] = s
	idx.mu.Unlock()
}

// build indexes every registered stream, one at a time to keep the startup
// memory and disk load low. Until a stream is ready, history requests for
// it fall back to scanning its folder.
func (idx *recordingIndex) build() {
	if idx == nil {
		return
	}
	idx.mu.RLock()
	streams := make([]*streamRecordingIndex, 0, len(idx.streams))
	for _, s := range idx.streams {
		streams = append(streams, s)
	}
	idx.mu.RUnlock()

	started := time.Now()
	total := 0
	for _, s := range streams {
		n, err := idx.buildStream(s)
		if err != nil {
			idx.logger.Printf("recording index: %s: %v (history for this stream will scan the folder)", s.info.StreamName, err)
			continue
		}
		total += n
	}
	idx.logger.Printf("recording index (%s): %d recordings across %d streams indexed in %s",
		idx.mode, total, len(streams), time.Since(started).Round(time.Millisecond))
}

type loggedClipInfo struct {
	durationMs int
	textOff    int64
	textLen    int
	text       string
	hasText    bool
}

func (idx *recordingIndex) buildStream(s *streamRecordingIndex) (int, error) {
	logged, err := idx.readLog(s)
	if err != nil {
		idx.logger.Printf("recording index: read transcript log for %s: %v", s.info.StreamName, err)
		logged = nil
	}

	entries, err := os.ReadDir(s.dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	cutoff := idx.cutoff(time.Now())
	recs := make([]indexedRecording, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".wav") {
			continue
		}
		name := entry.Name()
		ts, ok := recordingTimestamp(name)
		if !ok {
			fi, err := entry.Info()
			if err != nil {
				continue
			}
			ts = fi.ModTime()
		}
		if !cutoff.IsZero() && ts.Before(cutoff) {
			continue
		}
		rec := indexedRecording{unixNano: ts.UnixNano(), name: name, textOff: -1}
		if li, ok := logged[name]; ok {
			rec.durationMs = int32(li.durationMs)
			if li.hasText {
				idx.setText(&rec, li.text, li.textOff, li.textLen)
			}
		}
		if rec.durationMs == 0 {
			if d, err := wavDurationMs(filepath.Join(s.dir, name)); err == nil {
				rec.durationMs = int32(d)
			}
		}
		recs = append(recs, rec)
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].unixNano < recs[j].unixNano })

	s.mu.Lock()
	s.recs = recs
	pending := s.pending
	s.pending = nil
	for _, p := range pending {
		idx.apply(s, p.ev, p.off, p.lineLen)
	}
	s.ready = true
	s.lastPrune = time.Now()
	n := len(s.recs)
	s.mu.Unlock()
	return n, nil
}

// readLog walks the stream's JSON-lines log once, collecting each WAV's
// duration and the location (and, in full mode, text) of its latest
// transcript. Old lines that predate wavFilename are matched through the
// audio URL or their clip's per-process clip ID.
func (idx *recordingIndex) readLog(s *streamRecordingIndex) (map[string]*loggedClipInfo, error) {
	if s.logPath == "" {
		return nil, nil
	}
	f, err := os.Open(s.logPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := make(map[string]*loggedClipInfo)
	clipNames := make(map[string]string) // legacy clip-N -> WAV filename
	get := func(name string) *loggedClipInfo {
		li := out[name]
		if li == nil {
			li = &loggedClipInfo{}
			out[name] = li
		}
		return li
	}

	r := bufio.NewReaderSize(f, 64*1024)
	var off int64
	for {
		line, err := r.ReadBytes('\n')
		lineOff := off
		off += int64(len(line))
		if len(line) > 0 {
			var ev transcriptEvent
			if json.Unmarshal(line, &ev) == nil {
				name := eventWAVName(ev)
				if name == "" && ev.ClipID != "" {
					name = clipNames[ev.ClipID]
				}
				if name != "" {
					switch ev.Type {
					case "clip":
						if ev.ClipID != "" {
							clipNames[ev.ClipID] = name
						}
						if ev.DurationMs > 0 {
							get(name).durationMs = ev.DurationMs
						}
					case "transcript":
						li := get(name)
						li.hasText = true
						li.textOff = lineOff
						li.textLen = len(line)
						if idx.mode == recordingIndexFull {
							li.text = ev.Text
						}
					}
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

func eventWAVName(ev transcriptEvent) string {
	if ev.WAVFilename != "" {
		return ev.WAVFilename
	}
	if ev.AudioURL != "" {
		return path.Base(ev.AudioURL)
	}
	return ""
}

func (idx *recordingIndex) setText(rec *indexedRecording, text string, off int64, lineLen int) {
	if idx.mode == recordingIndexFull {
		rec.text = text
		return
	}
	rec.textOff = off
	rec.textLen = int32(lineLen)
}

// cutoff is the oldest timestamp the index holds (zero = unbounded).
func (idx *recordingIndex) cutoff(now time.Time) time.Time {
	if idx.window <= 0 {
		return time.Time{}
	}
	return now.Add(-idx.window)
}

// observe records a newly published clip or transcript. off/lineLen are
// where the event's line was appended to the stream log (off < 0 if it
// was not logged).
func (idx *recordingIndex) observe(ev transcriptEvent, off int64, lineLen int) {
	if idx == nil || (ev.Type != "clip" && ev.Type != "transcript") || ev.AudioURL == "" {
		return
	}
	idx.mu.RLock()
	s := idx.streams[recordingDirKey(ev.StateName, ev.GroupName, ev.StreamName)]
	idx.mu.RUnlock()
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready {
		s.pending = append(s.pending, pendingIndexEvent{ev: ev, off: off, lineLen: lineLen})
		return
	}
	idx.apply(s, ev, off, lineLen)
	idx.prune(s)
}

// apply must be called with s.mu held.
func (idx *recordingIndex) apply(s *streamRecordingIndex, ev transcriptEvent, off int64, lineLen int) {
	name := eventWAVName(ev)
	if name == "" {
		return
	}
	i := s.find(name)
	if i < 0 {
		ts := ev.Timestamp
		if parsed, ok := recordingTimestamp(name); ok {
			ts = parsed
		}
		if cutoff := idx.cutoff(time.Now()); !cutoff.IsZero() && ts.Before(cutoff) {
			return
		}
		rec := indexedRecording{unixNano: ts.UnixNano(), name: name, textOff: -1}
		i = sort.Search(len(s.recs), func(j int) bool { return s.recs[j].unixNano > rec.unixNano })
		s.recs = append(s.recs, indexedRecording{})
		copy(s.recs[i+1:], s.recs[i:])
		s.recs[i] = rec
	}
	rec := &s.recs[i]
	switch ev.Type {
	case "clip":
		if ev.DurationMs > 0 {
			rec.durationMs = int32(ev.DurationMs)
		}
	case "transcript":
		if idx.mode == recordingIndexFull || off >= 0 {
			idx.setText(rec, ev.Text, off, lineLen)
		}
	}
}

// find returns the index of the named recording, or -1. Recording names
// embed their timestamp, so a binary search usually lands on it directly.
func (s *streamRecordingIndex) find(name string) int {
	if ts, ok := recordingTimestamp(name); ok {
		target := ts.UnixNano()
		i := sort.Search(len(s.recs), func(j int) bool { return s.recs[j].unixNano >= target })
		for ; i < len(s.recs) && s.recs[i].unixNano == target; i++ {
			if s.recs[i].name == name {
				return i
			}
		}
	}
	for i := len(s.recs) - 1; i >= 0; i-- {
		if s.recs[i].name == name {
			return i
		}
	}
	return -1
}

// prune drops recordings that aged out of a window index, at most hourly.
// Must be called with s.mu held.
func (idx *recordingIndex) prune(s *streamRecordingIndex) {
	if idx.window <= 0 || time.Since(s.lastPrune) < time.Hour {
		return
	}
	s.lastPrune = time.Now()
	cutoff := idx.cutoff(s.lastPrune).UnixNano()
	i := sort.Search(len(s.recs), func(j int) bool { return s.recs[j].unixNano >= cutoff })
	if i > 0 {
		s.recs = append([]indexedRecording(nil), s.recs[i:]...)
	}
}

// history answers a history request from the index. ok is false when the
// index can't answer (stream still building, or a window index asked for
// a range older than it holds); the caller then scans the folder.
func (idx *recordingIndex) history(info streamInfo, since, until time.Time) (events []transcriptEvent, ok bool) {
	if idx == nil {
		return nil, false
	}
	key := recordingDirKey(info.StateName, info.GroupName, info.StreamName)
	idx.mu.RLock()
	s := idx.streams[key]
	idx.mu.RUnlock()
	if s == nil {
		return nil, false
	}
	if cutoff := idx.cutoff(time.Now()); !cutoff.IsZero() && (since.IsZero() || since.Before(cutoff)) {
		return nil, false
	}
	if until.IsZero() {
		until = time.Now()
	}

	s.mu.RLock()
	if !s.ready {
		s.mu.RUnlock()
		return nil, false
	}
	lo := 0
	if !since.IsZero() {
		sinceNano := since.UnixNano()
		lo = sort.Search(len(s.recs), func(j int) bool { return s.recs[j].unixNano > sinceNano })
	}
	untilNano := until.UnixNano()
	hi := sort.Search(len(s.recs), func(j int) bool { return s.recs[j].unixNano > untilNano })
	var matched []indexedRecording
	if hi > lo {
		matched = append([]indexedRecording(nil), s.recs[lo:hi]...)
	}
	s.mu.RUnlock()

	var logFile *os.File
	if idx.mode != recordingIndexFull && s.logPath != "" {
		if f, err := os.Open(s.logPath); err == nil {
			logFile = f
			defer f.Close()
		}
	}

	urlPrefix := "/audio/" + key + "/"
	events = make([]transcriptEvent, 0, len(matched)*2)
	for _, rec := range matched {
		ts := time.Unix(0, rec.unixNano).UTC()
		base := transcriptEvent{
			Type:        "clip",
			ClipID:      rec.name,
			StreamID:    info.ID,
			StreamName:  info.StreamName,
			StateName:  info.StateName,
			GroupName:   info.GroupName,
			AudioURL:    urlPrefix + rec.name,
			DurationMs:  int(rec.durationMs),
			Timestamp:   ts,
			WAVFilename: rec.name,
		}
		events = append(events, base)

		text, hasText := rec.text, rec.text != ""
		if logFile != nil && rec.textOff >= 0 && rec.textLen > 0 {
			text, hasText = readLoggedText(logFile, rec.textOff, int(rec.textLen))
		}
		if hasText {
			tr := base
			tr.Type = "transcript"
			tr.Text = text
			tr.DurationMs = 0
			events = append(events, tr)
		}
	}
	return events, true
}

func readLoggedText(f *os.File, off int64, n int) (string, bool) {
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, off); err != nil && !errors.Is(err, io.EOF) {
		return "", false
	}
	var ev transcriptEvent
	if json.Unmarshal(buf, &ev) != nil || ev.Type != "transcript" {
		return "", false
	}
	return ev.Text, true
}
