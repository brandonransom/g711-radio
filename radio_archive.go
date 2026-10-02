package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Radio history from the audio archive.
//
// Live transmission counting (analyticsStore.Transmission) starts when
// analytics is first enabled. To give the dashboard the history from before
// then, every WAV in the audio archive that started before a cutoff — the
// moment live counting began — is counted once, in the background, and the
// totals are kept in <analytics dir>/radio_archive.json. Live counts cover
// recordings that start at or after the cutoff and the archive file covers
// everything before it, so the two never overlap. Delete the file to rescan.

const radioArchiveFileName = "radio_archive.json"

// radioArchiveVersion 2 added stream-local weekday/hour totals; older files
// are rescanned.
const radioArchiveVersion = 2

// radioDay is the radio part of a dayAgg, as saved in radio_archive.json.
type radioDay struct {
	Radio     map[string]*txAgg    `json:"radio"`
	TxHour    [24]int              `json:"txHour"`
	TxHourMs  [24]int64            `json:"txHourMs"`
	TxLengths [txLengthBuckets]int `json:"txLengths"`
	TxLocal   [7][24]int           `json:"txLocal"`
	TxLocalMs [7][24]int64         `json:"txLocalMs"`
}

type radioArchiveFile struct {
	Version    int                 `json:"version"`
	Cutoff     time.Time           `json:"cutoff"`
	ScannedAt  time.Time           `json:"scannedAt"`
	Recordings int                 `json:"recordings"`
	Days       map[string]radioDay `json:"days"`
}

func (a *analyticsStore) radioArchivePath() string {
	return filepath.Join(a.cfg.Dir, radioArchiveFileName)
}

// BackfillRadio loads the radio history counted from the audio archive,
// scanning audioLogDir in the background if that has never been done. The
// cutoff is fixed before it returns, so call it before any recorder starts.
// Scanning reads every WAV header once, which can take a while on a large
// archive. done (optional) is closed when loading finishes. Safe to call on
// a nil store.
func (a *analyticsStore) BackfillRadio(audioLogDir string, inventory []streamInfo, done chan<- struct{}) {
	finish := func() {
		if done != nil {
			close(done)
		}
	}
	if a == nil {
		finish()
		return
	}
	if f, err := a.readRadioArchive(); err == nil {
		a.setRadioArchive(f)
		finish()
		return
	} else if !errors.Is(err, os.ErrNotExist) {
		a.logger.Printf("analytics: %s unreadable (%v); rescanning the audio archive", radioArchiveFileName, err)
	}
	if audioLogDir == "" {
		finish()
		return
	}

	cutoff := a.liveRadioStart()
	a.mu.Lock()
	a.archiveNote = fmt.Sprintf("Reading the audio archive for transmissions before %s…", cutoff.Format("2006-01-02 15:04"))
	a.mu.Unlock()
	a.logger.Printf("analytics: counting transmissions in the audio archive before %s", cutoff.Format(time.RFC3339))
	go func() {
		defer finish()
		a.scanAndSaveRadioArchive(audioLogDir, inventory, cutoff)
	}()
}

func (a *analyticsStore) scanAndSaveRadioArchive(audioLogDir string, inventory []streamInfo, cutoff time.Time) {
	started := time.Now()
	days, n, err := scanRadioArchive(audioLogDir, inventory, cutoff, a.now().Location(), func(n int) {
		a.logger.Printf("analytics: audio archive scan: %d recordings so far", n)
	})
	if err != nil {
		a.logger.Printf("analytics: audio archive scan failed: %v", err)
		a.mu.Lock()
		a.archiveNote = "Reading the audio archive failed; see the server log. Restart to retry."
		a.mu.Unlock()
		return
	}
	f := &radioArchiveFile{Version: radioArchiveVersion, Cutoff: cutoff, ScannedAt: a.now(), Recordings: n, Days: map[string]radioDay{}}
	for date, d := range days {
		f.Days[date] = radioDay{Radio: d.Radio, TxHour: d.TxHour, TxHourMs: d.TxHourMs, TxLengths: d.TxLengths, TxLocal: d.TxLocal, TxLocalMs: d.TxLocalMs}
	}
	if err := a.writeRadioArchive(f); err != nil {
		a.logger.Printf("analytics: save %s: %v (the archive will be rescanned next start)", radioArchiveFileName, err)
	}
	a.setRadioArchive(f)
	a.logger.Printf("analytics: counted %d archived recordings across %d days in %s",
		n, len(days), time.Since(started).Round(time.Second))
}

func (a *analyticsStore) readRadioArchive() (*radioArchiveFile, error) {
	b, err := os.ReadFile(a.radioArchivePath())
	if err != nil {
		return nil, err
	}
	var f radioArchiveFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	if f.Cutoff.IsZero() {
		return nil, fmt.Errorf("missing cutoff")
	}
	if f.Version < radioArchiveVersion {
		return nil, fmt.Errorf("written by an older version (no stream-local times)")
	}
	return &f, nil
}

func (a *analyticsStore) writeRadioArchive(f *radioArchiveFile) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	tmp := a.radioArchivePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, a.radioArchivePath())
}

func (a *analyticsStore) setRadioArchive(f *radioArchiveFile) {
	days := make(map[string]*dayAgg, len(f.Days))
	for date, rd := range f.Days {
		d := &dayAgg{Date: date, Radio: rd.Radio, TxHour: rd.TxHour, TxHourMs: rd.TxHourMs, TxLengths: rd.TxLengths, TxLocal: rd.TxLocal, TxLocalMs: rd.TxLocalMs}
		if d.Radio == nil {
			d.Radio = map[string]*txAgg{}
		}
		days[date] = d
	}
	note := fmt.Sprintf("Before %s, figures come from %d recordings counted in the audio archive.",
		f.Cutoff.In(a.now().Location()).Format("2006-01-02 15:04"), f.Recordings)
	a.mu.Lock()
	a.archive = days
	a.archiveNote = note
	a.mu.Unlock()
}

// liveRadioStart is when live transmission counting began: the earliest
// live transmission on record, or now if there is none yet. Call it before
// the recorders start so every live recording begins after it.
func (a *analyticsStore) liveRadioStart() time.Time {
	a.mu.Lock()
	first := ""
	for date, d := range a.days {
		if len(d.Radio) > 0 && (first == "" || date < first) {
			first = date
		}
	}
	a.mu.Unlock()
	if first == "" {
		return a.now()
	}
	var start time.Time
	_ = a.readEvents(first, func(ev analyticsEvent) {
		if ev.Type == evTransmission && (start.IsZero() || ev.Time.Before(start)) {
			start = ev.Time
		}
	})
	if start.IsZero() {
		// Raw events already pruned: count the archive up to that day.
		if t, err := time.ParseInLocation(dateLayout, first, a.now().Location()); err == nil {
			return t
		}
		return a.now()
	}
	return start
}

// scanRadioArchive counts every <state>/<group>/<stream>/*.wav under root
// whose filename timestamp is before cutoff. Folders that match a configured
// stream use its real names (so totals merge with live counts); others are
// counted under their folder names. progress is called every 50,000 files.
func scanRadioArchive(root string, inventory []streamInfo, cutoff time.Time, loc *time.Location, progress func(int)) (map[string]*dayAgg, int, error) {
	known := make(map[string]streamInfo, len(inventory))
	for _, s := range inventory {
		known[recordingDirKey(s.StateName, s.GroupName, s.StreamName)] = s
	}
	// Filenames carry whole seconds, so a recording that began in the
	// cutoff's own second is left to the live count.
	limit := cutoff.Truncate(time.Second)
	days := map[string]*dayAgg{}
	n := 0

	subdirs := func(dir string) ([]string, error) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		var out []string
		for _, e := range entries {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				out = append(out, e.Name())
			}
		}
		sort.Strings(out)
		return out, nil
	}

	states, err := subdirs(root)
	if err != nil {
		return nil, 0, err
	}
	for _, state := range states {
		groups, err := subdirs(filepath.Join(root, state))
		if err != nil {
			continue
		}
		for _, group := range groups {
			streams, err := subdirs(filepath.Join(root, state, group))
			if err != nil {
				continue
			}
			for _, stream := range streams {
				info, ok := known[state+"/"+group+"/"+stream]
				if !ok {
					info = streamInfo{StateName: state, GroupName: group, StreamName: stream}
				}
				dir := filepath.Join(root, state, group, stream)
				entries, err := os.ReadDir(dir)
				if err != nil {
					continue
				}
				for _, e := range entries {
					name := e.Name()
					if e.IsDir() || !strings.EqualFold(filepath.Ext(name), ".wav") {
						continue
					}
					ts, ok := recordingTimestamp(name)
					if !ok || !ts.Before(limit) {
						continue
					}
					ms, err := wavDurationMs(filepath.Join(dir, name))
					if err != nil || ms <= 0 {
						continue
					}
					t := ts.In(loc)
					date := t.Format(dateLayout)
					d := days[date]
					if d == nil {
						d = &dayAgg{Date: date, Radio: map[string]*txAgg{}}
						days[date] = d
					}
					d.addTransmission(analyticsEvent{
						Time: t, Type: evTransmission, Stream: info.displayName(),
						State: info.StateName, Group: info.GroupName, Name: info.StreamName,
						DurationMs: int64(ms), TZ: info.TimeZone,
					})
					n++
					if progress != nil && n%50000 == 0 {
						progress(n)
					}
				}
			}
		}
	}
	return days, n, nil
}

// withRadioArchive returns the days with archive radio totals merged in,
// adding radio-only days where the archive has data and the live store
// doesn't. The input slice and its maps are not modified.
func (a *analyticsStore) withRadioArchive(agg []dayAgg, days int) []dayAgg {
	from, to := a.rangeDates(days)
	a.mu.Lock()
	archive := a.archive
	a.mu.Unlock()
	out := append([]dayAgg(nil), agg...)
	if len(archive) == 0 {
		return out
	}
	index := make(map[string]int, len(out))
	for i, d := range out {
		index[d.Date] = i
	}
	for date, ad := range archive {
		if (from != "" && date < from) || date > to {
			continue
		}
		if i, ok := index[date]; ok {
			mergeRadio(&out[i], ad)
			continue
		}
		d := newDayAgg(date)
		d.visitors = nil
		mergeRadio(d, ad)
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date < out[j].Date })
	return out
}

// mergeRadio adds src's radio totals to dst, copying dst's radio map first
// so stored day totals are never changed.
func mergeRadio(dst, src *dayAgg) {
	radio := make(map[string]*txAgg, len(dst.Radio)+len(src.Radio))
	for k, t := range dst.Radio {
		c := *t
		radio[k] = &c
	}
	for k, t := range src.Radio {
		if c := radio[k]; c != nil {
			c.Count += t.Count
			c.Ms += t.Ms
			if t.Last > c.Last {
				c.Last = t.Last
			}
			continue
		}
		c := *t
		radio[k] = &c
	}
	dst.Radio = radio
	for h := 0; h < 24; h++ {
		dst.TxHour[h] += src.TxHour[h]
		dst.TxHourMs[h] += src.TxHourMs[h]
	}
	for i := range dst.TxLengths {
		dst.TxLengths[i] += src.TxLengths[i]
	}
	for wd := 0; wd < 7; wd++ {
		for h := 0; h < 24; h++ {
			dst.TxLocal[wd][h] += src.TxLocal[wd][h]
			dst.TxLocalMs[wd][h] += src.TxLocalMs[wd][h]
		}
	}
}
