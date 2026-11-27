package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	serverLogPrefix = "g711-radio-"
	// serverLogSubdir is the folder, under the working directory, that holds
	// the server's own log files (daily server logs, mping-peers.log).
	serverLogSubdir = "logs"
	// archiveLogsSubdir and archiveTranscriptLogsSubdir are where the server
	// logs and the per-stream transcript logs are replicated inside every
	// audio archive directory (the archive sync then keeps them identical).
	archiveLogsSubdir           = "logs"
	archiveTranscriptLogsSubdir = "transcript-logs"
)

// legacyRootLogPattern matches the log files older versions (or the service
// wrapper's output redirection) left directly in the working directory:
// g711-radio.log, g711-radio-YYYY-MM-DD.log, and NSSM-style
// service-out*.log / service-err*.log including timestamped rotations.
var legacyRootLogPattern = regexp.MustCompile(`^(g711-radio|service-out|service-err)([-_.].*)?\.log$`)

func isLegacyRootLog(name string) bool {
	return legacyRootLogPattern.MatchString(name)
}

// moveFileNoClobber moves src to dst, creating dst's folder. An existing dst
// is never overwritten: the file is moved to "<name>.moved-N<ext>" instead.
// It returns the path the file ended up at.
func moveFileNoClobber(src, dst string) (string, error) {
	if _, err := os.Lstat(src); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	ext := filepath.Ext(dst)
	stem := strings.TrimSuffix(dst, ext)
	target := dst
	for n := 1; ; n++ {
		_, err := os.Lstat(target)
		if os.IsNotExist(err) {
			break
		}
		if err != nil {
			return "", err
		}
		target = fmt.Sprintf("%s.moved-%d%s", stem, n, ext)
	}
	if err := os.Rename(src, target); err != nil {
		return "", err
	}
	return target, nil
}

// migrateRootLogs moves the log files that older versions left in dir
// (g711-radio*.log and the service wrapper's service-out*/service-err*.log)
// into dir/logs. A file that
// cannot be moved (for example one held open by another running copy of the
// server) is left in place. It returns lines for the caller to log, since
// this runs before the logger exists.
func migrateRootLogs(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []string{fmt.Sprintf("log migration: cannot read %s: %v", dir, err)}
	}
	dst := filepath.Join(dir, serverLogSubdir)
	var failures []string
	moved := 0
	for _, e := range entries {
		if !e.Type().IsRegular() || !isLegacyRootLog(e.Name()) {
			continue
		}
		if _, err := moveFileNoClobber(filepath.Join(dir, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			failures = append(failures, fmt.Sprintf("log migration: could not move %s: %v", e.Name(), err))
			continue
		}
		moved++
	}
	var report []string
	if moved > 0 {
		report = append(report, fmt.Sprintf("log migration: moved %d log file(s) from %s to %s", moved, dir, dst))
	}
	return append(report, failures...)
}

// dailyLogWriter writes every log line to a per-day file
// (g711-radio-YYYY-MM-DD.log, local date) in each configured directory,
// starting a fresh file when the date changes. Files are only ever appended
// to, never deleted or truncated. A directory that cannot be written (e.g.
// an unreachable network mount) is skipped without affecting the others.
type dailyLogWriter struct {
	mu   sync.Mutex
	dirs []string
	day  string
	open map[string]*os.File
	now  func() time.Time
}

func newDailyLogWriter(dirs ...string) *dailyLogWriter {
	return &dailyLogWriter{dirs: dirs, open: map[string]*os.File{}, now: time.Now}
}

func dailyLogName(t time.Time) string {
	return serverLogPrefix + t.Format("2006-01-02") + ".log"
}

// AddDir starts mirroring to dir too (no-op if already present).
func (w *dailyLogWriter) AddDir(dir string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, d := range w.dirs {
		if sameFilePath(d, dir) {
			return
		}
	}
	w.dirs = append(w.dirs, dir)
}

func (w *dailyLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	t := w.now()
	day := t.Format("2006-01-02")
	if day != w.day {
		for k, f := range w.open {
			f.Close()
			delete(w.open, k)
		}
		w.day = day
	}
	name := dailyLogName(t)
	for _, dir := range w.dirs {
		f := w.open[dir]
		if f == nil {
			if err := os.MkdirAll(dir, 0755); err != nil {
				continue
			}
			var err error
			f, err = os.OpenFile(filepath.Join(dir, name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
			if err != nil {
				continue
			}
			w.open[dir] = f
		}
		if _, err := f.Write(p); err != nil {
			f.Close()
			delete(w.open, dir)
		}
	}
	return len(p), nil
}

func (w *dailyLogWriter) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for k, f := range w.open {
		f.Close()
		delete(w.open, k)
	}
}

func sameFilePath(a, b string) bool {
	aa, err1 := filepath.Abs(a)
	bb, err2 := filepath.Abs(b)
	return err1 == nil && err2 == nil && filepath.Clean(aa) == filepath.Clean(bb)
}

// replicateFileAppendOnly copies src to dst when dst is missing or smaller
// (i.e. lacks appended lines). Nothing is ever deleted; a larger dst is left
// alone. Reports whether a copy was made.
func replicateFileAppendOnly(src, dst string) (bool, error) {
	si, err := os.Stat(src)
	if err != nil {
		return false, err
	}
	if di, err := os.Stat(dst); err == nil && di.Size() >= si.Size() {
		return false, nil
	}
	return true, copyArchiveFileAtomic(src, dst, si.ModTime())
}

// replicateTranscriptLogs copies the per-stream transcript logs in logDir and
// the transcript CSVs in the primary archive into every archive directory's
// transcript-logs folder (the CSVs already live in the primary archive and
// are replicated by the archive sync, so only logDir is handled here).
func replicateTranscriptLogs(logDir string, archiveDirs []string, logger *log.Logger) {
	if logDir == "" {
		return
	}
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || shouldSkipArchiveFile(e.Name()) {
			continue
		}
		src := filepath.Join(logDir, e.Name())
		for _, dir := range archiveDirs {
			dst := filepath.Join(dir, archiveTranscriptLogsSubdir, e.Name())
			if _, err := replicateFileAppendOnly(src, dst); err != nil && logger != nil {
				logger.Printf("transcript log replication: %s -> %s: %v", src, dst, err)
			}
		}
	}
}

func startTranscriptLogReplication(done <-chan struct{}, logDir string, archiveDirs []string, interval time.Duration, logger *log.Logger) {
	if logDir == "" || len(archiveDirs) == 0 {
		return
	}
	if interval <= 0 {
		interval = defaultArchiveSyncIdleInterval
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			replicateTranscriptLogs(logDir, archiveDirs, logger)
			select {
			case <-t.C:
			case <-done:
				replicateTranscriptLogs(logDir, archiveDirs, logger)
				return
			}
		}
	}()
}

var _ = fmt.Sprintf
var _ io.Writer
