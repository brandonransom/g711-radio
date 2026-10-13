package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultArchiveSyncIdleInterval = 60 * time.Second
	defaultArchiveSyncBusyInterval = 10 * time.Minute
	defaultArchiveSyncMinFileAge   = 5 * time.Second
)

type archiveFileInfo struct {
	fullPath string
	size     int64
	modTime  time.Time
}

// shouldSkipArchiveFile reports whether a file or directory should be ignored
// by the archive sync (temporary files, hidden directories, etc.).
func shouldSkipArchiveFile(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	if strings.HasSuffix(name, ".tmp") || strings.HasSuffix(name, ".sync.tmp") {
		return true
	}
	return false
}

// scanArchiveDir walks dir and returns a map of relative slash-separated
// paths to archiveFileInfo. Files younger than minAge are skipped to avoid
// copying files currently being written.
func scanArchiveDir(dir string, minAge time.Duration, now time.Time) (map[string]archiveFileInfo, error) {
	result := make(map[string]archiveFileInfo)
	cleanDir := filepath.Clean(dir)

	err := filepath.WalkDir(cleanDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != cleanDir && shouldSkipArchiveFile(name) {
				return filepath.SkipDir
			}
			return nil
		}
		if shouldSkipArchiveFile(name) {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		modTime := info.ModTime()
		if minAge > 0 && now.Sub(modTime) < minAge {
			return nil
		}

		rel, err := filepath.Rel(cleanDir, path)
		if err != nil {
			return nil
		}
		// Normalize to slash-separated relative path
		relSlash := filepath.ToSlash(rel)
		result[relSlash] = archiveFileInfo{
			fullPath: path,
			size:     info.Size(),
			modTime:  modTime,
		}
		return nil
	})

	if err != nil {
		return nil, err
	}
	return result, nil
}

// copyArchiveFileAtomic copies src to dst atomically using a temporary file
// in dst's parent directory, preserving modification times. It never deletes
// any file.
func copyArchiveFileAtomic(src, dst string, modTime time.Time) error {
	dstDir := filepath.Dir(dst)
	if err := os.MkdirAll(dstDir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dstDir, err)
	}

	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open src %s: %w", src, err)
	}
	defer srcFile.Close()

	tmpPattern := fmt.Sprintf(".%s.sync-%d.tmp", filepath.Base(dst), time.Now().UnixNano())
	tmpPath := filepath.Join(dstDir, tmpPattern)
	tmpFile, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("create tmp %s: %w", tmpPath, err)
	}

	_, copyErr := io.Copy(tmpFile, srcFile)
	syncErr := tmpFile.Sync()
	closeErr := tmpFile.Close()

	if copyErr != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("copy %s to %s: %w", src, tmpPath, copyErr)
	}
	if syncErr != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("sync tmp %s: %w", tmpPath, syncErr)
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close tmp %s: %w", tmpPath, closeErr)
	}

	if !modTime.IsZero() {
		_ = os.Chtimes(tmpPath, modTime, modTime)
	}

	// Windows: os.Rename fails if target exists. Remove destination only right before rename if replacing.
	if err := os.Rename(tmpPath, dst); err != nil {
		_ = os.Remove(dst)
		if retryErr := os.Rename(tmpPath, dst); retryErr != nil {
			_ = os.Remove(tmpPath)
			return fmt.Errorf("rename %s to %s: %w", tmpPath, dst, retryErr)
		}
	}
	return nil
}

// syncArchives checks all configured audio/transcript archive directories and
// copies any missing or incomplete files so all directories match. It never
// deletes files. If a file exists in one location, it will exist in all
// locations.
func syncArchives(ctx context.Context, dirs []string, minAge time.Duration, isBusy func() bool, logger *log.Logger) (int, error) {
	if len(dirs) < 2 {
		return 0, nil
	}

	now := time.Now()
	scanned := make([]map[string]archiveFileInfo, len(dirs))
	allRelPaths := make(map[string]struct{})

	for i, dir := range dirs {
		if fi, err := os.Stat(dir); err != nil {
			if os.IsNotExist(err) {
				if mkErr := os.MkdirAll(dir, 0755); mkErr != nil {
					return 0, fmt.Errorf("archive sync: mkdir %s: %w", dir, mkErr)
				}
			} else {
				return 0, fmt.Errorf("archive sync: stat %s: %w", dir, err)
			}
		} else if !fi.IsDir() {
			return 0, fmt.Errorf("archive sync: %s is not a directory", dir)
		}

		files, err := scanArchiveDir(dir, minAge, now)
		if err != nil {
			return 0, fmt.Errorf("archive sync: scan %s: %w", dir, err)
		}
		scanned[i] = files
		for rel := range files {
			allRelPaths[rel] = struct{}{}
		}
	}

	copiedCount := 0

	for rel := range allRelPaths {
		if ctx != nil && ctx.Err() != nil {
			return copiedCount, ctx.Err()
		}

		// Find the best source copy across all directories that have this file.
		bestIdx := -1
		var bestInfo archiveFileInfo
		for i := range dirs {
			info, ok := scanned[i][rel]
			if !ok {
				continue
			}
			if bestIdx == -1 {
				bestIdx = i
				bestInfo = info
				continue
			}
			// Prefer larger file (in case of incomplete writes), then newer file
			if info.size > bestInfo.size {
				bestIdx = i
				bestInfo = info
			} else if info.size == bestInfo.size && info.modTime.After(bestInfo.modTime) {
				bestIdx = i
				bestInfo = info
			}
		}

		if bestIdx == -1 {
			continue
		}

		// Copy best version to every directory missing it or holding a smaller version
		for i, dir := range dirs {
			if i == bestIdx {
				continue
			}
			if ctx != nil && ctx.Err() != nil {
				return copiedCount, ctx.Err()
			}

			existing, exists := scanned[i][rel]
			needsCopy := false
			if !exists {
				needsCopy = true
			} else if existing.size < bestInfo.size {
				// Smaller file indicates incomplete write or missing appended lines
				needsCopy = true
			}

			if needsCopy {
				dstPath := filepath.Join(dir, filepath.FromSlash(rel))
				if err := copyArchiveFileAtomic(bestInfo.fullPath, dstPath, bestInfo.modTime); err != nil {
					if logger != nil {
						logger.Printf("archive sync: copy error %s -> %s: %v", bestInfo.fullPath, dstPath, err)
					}
					continue
				}
				copiedCount++
				if logger != nil {
					logger.Printf("archive sync: copied %s -> %s", rel, dir)
				}

				// If system is busy during a batch copy, throttle slightly to avoid I/O spikes
				if isBusy != nil && isBusy() {
					select {
					case <-time.After(20 * time.Millisecond):
					case <-ctx.Done():
						return copiedCount, ctx.Err()
					}
				}
			}
		}
	}

	return copiedCount, nil
}

// startArchiveSync launches the periodic background routine that checks and
// synchronizes audio and transcript folders. During slow periods it runs
// frequently; when the system is busy it runs infrequently.
func startArchiveSync(ctx context.Context, dirs []string, idleInterval, busyInterval time.Duration, isBusy func() bool, logger *log.Logger) {
	if len(dirs) < 2 {
		return
	}
	if idleInterval <= 0 {
		idleInterval = defaultArchiveSyncIdleInterval
	}
	if busyInterval <= 0 {
		busyInterval = defaultArchiveSyncBusyInterval
	}
	if isBusy == nil {
		isBusy = func() bool { return false }
	}

	if logger != nil {
		logger.Printf("archive sync: watching %d directories (idle interval: %s, busy interval: %s)",
			len(dirs), idleInterval, busyInterval)
	}

	go func() {
		// Initial sync delay so startup isn't bogged down
		initialWait := 10 * time.Second
		select {
		case <-time.After(initialWait):
		case <-ctx.Done():
			return
		}

		for {
			busy := isBusy()
			waitInterval := idleInterval
			if busy {
				waitInterval = busyInterval
			}

			started := time.Now()
			n, err := syncArchives(ctx, dirs, defaultArchiveSyncMinFileAge, isBusy, logger)
			if err != nil && logger != nil && ctx.Err() == nil {
				logger.Printf("archive sync: error: %v", err)
			} else if n > 0 && logger != nil {
				logger.Printf("archive sync: fixed %d inconsistency(ies) across archives in %s", n, time.Since(started).Round(time.Millisecond))
			}

			// Decide next sleep interval
			if isBusy() {
				waitInterval = busyInterval
			} else {
				waitInterval = idleInterval
			}

			select {
			case <-time.After(waitInterval):
			case <-ctx.Done():
				return
			}
		}
	}()
}
