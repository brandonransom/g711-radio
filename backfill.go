package main

import (
	"bufio"
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// backfillMinAge leaves recent clips to the live pipeline, which may
	// still be writing, queueing or transcribing them.
	backfillMinAge = 10 * time.Minute
	// backfillRescanIdle is how soon the archive is rescanned once every
	// known candidate has been queued; backfillRescanMax bounds how stale the
	// candidate list may get while it is still being worked through.
	backfillRescanIdle = 10 * time.Minute
	backfillRescanMax  = time.Hour
	backfillPoll       = 2 * time.Second
	backfillStartDelay = 30 * time.Second
	failedMarker       = "[transcription failed]"
)

// backfillCandidate is one archived clip with no transcript yet.
type backfillCandidate struct {
	info     streamInfo
	wavPath  string
	audioURL string
	start    time.Time
}

// transcriptBackfill feeds archived clips that never got a transcript into
// the whisper pool, oldest first, whenever the transcription queue is
// otherwise empty. Only clips that would have been auto-transcribed are
// considered: streams with disableAutoTranscribe are skipped, as are clips
// outside the auto-transcribe length window. It never runs more backfill
// jobs than whisperPool.backfillSlots allows, so a server stays free for
// live clips.
type transcriptBackfill struct {
	stations    func() []*station
	pool        *whisperPool
	audioLogDir string
	logDir      string
	whisper     *whisperConfig
	logger      *log.Logger
	minAge      time.Duration
	now         func() time.Time

	pending  []backfillCandidate
	lastScan time.Time
	// attempted holds clips already queued by this process, so a clip whose
	// transcription fails is retried at most once per run. ineligible caches
	// clips outside the auto-transcribe window (their length never changes).
	attempted  map[string]struct{}
	ineligible map[string]struct{}
	submitted  int
}

func newTranscriptBackfill(stations func() []*station, pool *whisperPool, audioLogDir, logDir string, whisper *whisperConfig, logger *log.Logger) *transcriptBackfill {
	if logger == nil {
		logger = log.New(os.Stderr, "", log.LstdFlags)
	}
	return &transcriptBackfill{
		stations:    stations,
		pool:        pool,
		audioLogDir: audioLogDir,
		logDir:      logDir,
		whisper:     whisper,
		logger:      logger,
		minAge:      backfillMinAge,
		now:         time.Now,
		attempted:   make(map[string]struct{}),
		ineligible:  make(map[string]struct{}),
	}
}

// startTranscriptBackfill runs the backfill until ctx ends.
func startTranscriptBackfill(ctx context.Context, b *transcriptBackfill) {
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(backfillStartDelay):
		}
		ticker := time.NewTicker(backfillPoll)
		defer ticker.Stop()
		for {
			b.step()
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// step queues more backfill work if the pool has nothing else to do.
func (b *transcriptBackfill) step() {
	otherQueued, backfillQueued, backfillRunning := b.pool.backfillState()
	if otherQueued > 0 || backfillQueued > 0 {
		return
	}
	free := b.pool.backfillSlots() - backfillRunning
	if free <= 0 {
		return
	}
	now := b.now()
	sinceScan := now.Sub(b.lastScan)
	if (len(b.pending) == 0 && sinceScan >= backfillRescanIdle) || sinceScan >= backfillRescanMax {
		b.scan()
	}
	for free > 0 && len(b.pending) > 0 {
		c := b.pending[0]
		b.pending = b.pending[1:]
		if _, done := b.attempted[c.wavPath]; done || b.pool.isPending(c.wavPath) {
			continue
		}
		b.attempted[c.wavPath] = struct{}{}
		job := transcriptJob{
			info:     c.info,
			clipID:   filepath.Base(c.wavPath),
			wavPath:  c.wavPath,
			audioURL: c.audioURL,
			start:    c.start,
			backfill: true,
		}
		if b.pool.SubmitBulk(job) != bulkQueued {
			continue
		}
		free--
		b.submitted++
		if len(b.pending) == 0 {
			b.logger.Printf("transcript backfill: queued %d archived clips so far; caught up", b.submitted)
		} else if b.submitted%100 == 0 {
			b.logger.Printf("transcript backfill: queued %d archived clips so far (%d left)", b.submitted, len(b.pending))
		}
	}
}

// scan rebuilds the oldest-first list of clips that still need a transcript.
func (b *transcriptBackfill) scan() {
	b.lastScan = b.now()
	cutoff := b.lastScan.Add(-b.minAge)
	safe := func(v string) string { return unsafeChars.ReplaceAllString(v, "_") }
	var found []backfillCandidate
	for _, st := range b.stations() {
		if st.cfg.DisableAutoTranscribe {
			continue
		}
		info := st.info
		parts := []string{safe(info.StateName), safe(info.GroupName), safe(info.StreamName)}
		dir := filepath.Join(b.audioLogDir, parts[0], parts[1], parts[2])
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		var done map[string]struct{}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.EqualFold(filepath.Ext(name), ".wav") || strings.HasPrefix(name, ".") {
				continue
			}
			wavPath := filepath.Join(dir, name)
			if _, skip := b.attempted[wavPath]; skip {
				continue
			}
			if _, skip := b.ineligible[wavPath]; skip {
				continue
			}
			fi, err := e.Info()
			if err != nil || fi.ModTime().After(cutoff) {
				continue
			}
			start, ok := recordingTimestamp(name)
			if !ok {
				start = fi.ModTime()
			}
			if start.After(cutoff) {
				continue
			}
			if done == nil {
				done = b.transcribedClips(info.StreamName)
			}
			if _, ok := done[name]; ok {
				continue
			}
			durationMs, err := wavDurationMs(wavPath)
			if err != nil || !shouldAutoTranscribe(b.whisper, durationMs) {
				b.ineligible[wavPath] = struct{}{}
				continue
			}
			found = append(found, backfillCandidate{
				info:     info,
				wavPath:  wavPath,
				audioURL: "/audio/" + strings.Join(append(parts, name), "/"),
				start:    start,
			})
		}
	}
	sort.SliceStable(found, func(i, j int) bool {
		if !found[i].start.Equal(found[j].start) {
			return found[i].start.Before(found[j].start)
		}
		return found[i].wavPath < found[j].wavPath
	})
	b.pending = found
	if len(found) > 0 {
		b.logger.Printf("transcript backfill: %d archived clips need a transcript (oldest %s)",
			len(found), found[0].start.Format(time.RFC3339))
	}
}

// transcribedClips returns the WAV names with a finished transcript in the
// stream's log. "No speech" counts as finished; a failure does not.
func (b *transcriptBackfill) transcribedClips(streamName string) map[string]struct{} {
	done := make(map[string]struct{})
	f, err := os.Open(filepath.Join(b.logDir, logFilename(streamName)))
	if err != nil {
		return done
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if !strings.Contains(string(line), `"transcript"`) {
			continue
		}
		var ev transcriptEvent
		if json.Unmarshal(line, &ev) != nil || ev.Type != "transcript" {
			continue
		}
		name := eventWAVName(ev)
		if name == "" {
			continue
		}
		if strings.TrimSpace(ev.Text) == failedMarker {
			// A later success for the same clip still wins; only a failure
			// with nothing after it leaves the clip untranscribed.
			continue
		}
		done[name] = struct{}{}
	}
	return done
}
