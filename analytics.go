package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// analyticsConfig is the optional "analytics" block in config.json.
type analyticsConfig struct {
	// Dir holds raw event files (events/), daily totals (daily/), and the
	// current day's visitor-hash salt. Default "analytics".
	Dir string `json:"dir"`
	// DashboardPath is the unlinked URL of the stats page, e.g.
	// "/k3v9q2m7xw4t8bnr5hza". Empty disables the dashboard (events are
	// still recorded).
	DashboardPath string `json:"dashboardPath"`
	// RetentionDays is how long raw events are kept before only the daily
	// totals remain. Default 90; a negative value keeps raw events forever.
	RetentionDays int `json:"retentionDays"`
	// GeoIPDatabase is an optional MaxMind-format (.mmdb) city database,
	// e.g. DB-IP "IP to City Lite" or MaxMind GeoLite2-City.
	GeoIPDatabase string `json:"geoipDatabase"`
	// LocationOverridesFile is an optional "cidr,label" CSV whose labels
	// take precedence over the GeoIP database (longest prefix wins).
	LocationOverridesFile string `json:"locationOverridesFile"`
}

const defaultAnalyticsRetentionDays = 90

var dashboardPathPattern = regexp.MustCompile(`^/[A-Za-z0-9_-]{12,64}$`)

// reservedRoutes are exact paths the server already serves; the dashboard
// must not shadow any of them.
var reservedRoutes = []string{"/streams", "/stream-status", "/stream-activity", "/offer", "/listen-leave", "/transcripts", "/recordings", "/audio"}

func (c *analyticsConfig) normalize() error {
	if c.Dir == "" {
		c.Dir = "analytics"
	}
	if c.RetentionDays == 0 {
		c.RetentionDays = defaultAnalyticsRetentionDays
	}
	if c.DashboardPath != "" {
		if !dashboardPathPattern.MatchString(c.DashboardPath) {
			return fmt.Errorf(`dashboardPath must be "/" followed by 12-64 letters, digits, "-" or "_" (e.g. "/%s")`, randomToken())
		}
		for _, r := range reservedRoutes {
			if strings.EqualFold(c.DashboardPath, r) {
				return fmt.Errorf("dashboardPath %q collides with an existing route", c.DashboardPath)
			}
		}
	}
	return nil
}

// Event types.
const (
	evServerStart     = "server_start"
	evPageview        = "pageview"
	evListenOffer     = "listen_offer"
	evListenConnected = "listen_connected"
	evListenEnd       = "listen_end"
	evAudioPlay       = "audio_play"
	evDownload        = "recordings_download"
	evTranscriptReq   = "transcript_request"
	evTranscriptBulk  = "transcript_bulk"
	evFeedback        = "feedback"
	evTransmission    = "transmission"
)

// listen_end details. left and stopped come from the client's leave beacon
// (POST /listen-leave); failed means no beacon arrived and the connection
// timed out, i.e. a network drop (or, rarely, a beacon the browser lost).
const (
	endNeverConnected = "never_connected"
	endClosed         = "closed"  // closed by the server (shutdown, stream removed)
	endFailed         = "failed"  // dropped: ICE/DTLS failure or timeout
	endLeft           = "left"    // tab closed, reloaded or navigated away
	endStopped        = "stopped" // listener pressed Disconnect / switched stream
)

// analyticsEvent is one raw event line in events/YYYY-MM-DD.jsonl. Net is the
// truncated client network; Visitor is a hash that changes daily and cannot
// be reversed to an address.
type analyticsEvent struct {
	Time       time.Time `json:"t"`
	Type       string    `json:"type"`
	Net        string    `json:"net,omitempty"`
	Visitor    string    `json:"v,omitempty"`
	Page       string    `json:"page,omitempty"`
	Referrer   string    `json:"ref,omitempty"`
	Browser    string    `json:"browser,omitempty"`
	OS         string    `json:"os,omitempty"`
	Device     string    `json:"device,omitempty"`
	Stream     string    `json:"stream,omitempty"`
	DurationMs int64     `json:"ms,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	Count      int       `json:"n,omitempty"`
	// State, Group and Name identify the radio stream of a transmission.
	State string `json:"state,omitempty"`
	Group string `json:"group,omitempty"`
	Name  string `json:"name,omitempty"`
}

// txAgg is one radio stream's transmissions for a day.
type txAgg struct {
	State string `json:"state"`
	Group string `json:"group"`
	Name  string `json:"name"`
	Count int    `json:"count"`
	Ms    int64  `json:"ms"`
	Last  string `json:"last"`
}

// txLengthBounds are the upper limits (ms) of the transmission-length
// histogram buckets; the final bucket is everything longer.
var txLengthBounds = [...]int64{2000, 5000, 10000, 30000, 60000, 120000, 300000}

const txLengthBuckets = len(txLengthBounds) + 1

var txLengthLabels = [txLengthBuckets]string{"under 2s", "2–5s", "5–10s", "10–30s", "30s–1m", "1–2m", "2–5m", "5m+"}

func txLengthBucket(ms int64) int {
	for i, b := range txLengthBounds {
		if ms < b {
			return i
		}
	}
	return len(txLengthBounds)
}

type netAgg struct {
	Visitors  int    `json:"visitors"`
	Pageviews int    `json:"pageviews"`
	Listens   int    `json:"listens"`
	ListenMs  int64  `json:"listenMs"`
	Plays     int    `json:"plays"`
	FirstSeen string `json:"firstSeen"`
	LastSeen  string `json:"lastSeen"`
}

type streamAgg struct {
	Attempts       int     `json:"attempts"`
	Connected      int     `json:"connected"`
	NeverConnected int     `json:"neverConnected"`
	EndedClosed    int     `json:"endedClosed"`
	EndedFailed    int     `json:"endedFailed"`
	EndedLeft      int     `json:"endedLeft"`
	EndedStopped   int     `json:"endedStopped"`
	ListenMs       int64   `json:"listenMs"`
	DurationsSec   []int64 `json:"durationsSec"`
}

// dayAgg is one day's totals. Completed days are saved to daily/DATE.json
// and outlive the raw events they were built from.
type dayAgg struct {
	Date               string                `json:"date"`
	Visitors           int                   `json:"visitors"`
	Pageviews          int                   `json:"pageviews"`
	BotHits            int                   `json:"botHits"`
	Pages              map[string]int        `json:"pages"`
	Referrers          map[string]int        `json:"referrers"`
	Browsers           map[string]int        `json:"browsers"`
	OSes               map[string]int        `json:"oses"`
	Devices            map[string]int        `json:"devices"`
	Networks           map[string]*netAgg    `json:"networks"`
	Streams            map[string]*streamAgg `json:"streams"`
	HourViews          [24]int               `json:"hourViews"`
	HourListens        [24]int               `json:"hourListens"`
	PeakConcurrent     int                   `json:"peakConcurrent"`
	PeakAt             string                `json:"peakAt"`
	Plays              int                   `json:"plays"`
	PlaysByStream      map[string]int        `json:"playsByStream"`
	Downloads          int                   `json:"downloads"`
	TranscriptRequests int                   `json:"transcriptRequests"`
	BulkRequests       int                   `json:"bulkRequests"`
	BulkClips          int                   `json:"bulkClips"`
	FeedbackGood       int                   `json:"feedbackGood"`
	FeedbackBad        int                   `json:"feedbackBad"`
	Corrections        int                   `json:"corrections"`
	// Radio side: transmissions per stream (keyed by display name), by
	// starting hour, airtime per hour, and a length histogram.
	Radio     map[string]*txAgg    `json:"radio"`
	TxHour    [24]int              `json:"txHour"`
	TxHourMs  [24]int64            `json:"txHourMs"`
	TxLengths [txLengthBuckets]int `json:"txLengths"`

	visitors map[string]struct{}
}

func newDayAgg(date string) *dayAgg {
	d := &dayAgg{Date: date}
	d.init()
	return d
}

func (d *dayAgg) init() {
	for _, m := range []*map[string]int{&d.Pages, &d.Referrers, &d.Browsers, &d.OSes, &d.Devices, &d.PlaysByStream} {
		if *m == nil {
			*m = map[string]int{}
		}
	}
	if d.Networks == nil {
		d.Networks = map[string]*netAgg{}
	}
	if d.Streams == nil {
		d.Streams = map[string]*streamAgg{}
	}
	if d.Radio == nil {
		d.Radio = map[string]*txAgg{}
	}
	if d.visitors == nil {
		d.visitors = map[string]struct{}{}
	}
}

// peerTrack remembers a WebRTC listener between its offer and its end so the
// end event can report how it went and how long it lasted.
type peerTrack struct {
	base        analyticsEvent
	connectedAt time.Time
	leaveReason string
}

type analyticsStore struct {
	cfg       analyticsConfig
	logger    *log.Logger
	now       func() time.Time
	eventsDir string
	dailyDir  string
	saltPath  string

	mu         sync.Mutex
	file       *os.File
	fileDate   string
	today      string
	saltDate   string
	salt       []byte
	days       map[string]*dayAgg
	concurrent int
	peers      map[string]*peerTrack

	geo *geoLocator
}

const dateLayout = "2006-01-02"

func openAnalyticsStore(cfg analyticsConfig, logger *log.Logger) (*analyticsStore, error) {
	return openAnalyticsStoreAt(cfg, logger, time.Now)
}

func openAnalyticsStoreAt(cfg analyticsConfig, logger *log.Logger, now func() time.Time) (*analyticsStore, error) {
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	a := &analyticsStore{
		cfg:       cfg,
		logger:    logger,
		now:       now,
		eventsDir: filepath.Join(cfg.Dir, "events"),
		dailyDir:  filepath.Join(cfg.Dir, "daily"),
		saltPath:  filepath.Join(cfg.Dir, "salt.json"),
		days:      map[string]*dayAgg{},
		peers:     map[string]*peerTrack{},
	}
	for _, d := range []string{a.eventsDir, a.dailyDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	a.geo = newGeoLocator(cfg.GeoIPDatabase, cfg.LocationOverridesFile, logger)
	a.today = now().Format(dateLayout)
	if err := a.load(); err != nil {
		return nil, err
	}
	a.loadSalt()
	a.mu.Lock()
	a.appendLocked(analyticsEvent{Time: now(), Type: evServerStart})
	a.mu.Unlock()
	a.maintain()
	return a, nil
}

// load rebuilds in-memory totals: saved daily files for finished days, and a
// replay of raw events for today and for any finished day not yet rolled up.
func (a *analyticsStore) load() error {
	rollups, _ := filepath.Glob(filepath.Join(a.dailyDir, "*.json"))
	for _, p := range rollups {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var d dayAgg
		if err := json.Unmarshal(b, &d); err != nil || d.Date == "" {
			a.logger.Printf("analytics: skipping unreadable daily file %s: %v", p, err)
			continue
		}
		d.init()
		a.days[d.Date] = &d
	}
	for _, date := range a.rawDates() {
		if _, done := a.days[date]; done && date != a.today {
			continue
		}
		delete(a.days, date)
		if err := a.replay(date); err != nil {
			return err
		}
		if date < a.today {
			a.saveDay(a.days[date])
		}
	}
	return nil
}

func (a *analyticsStore) rawDates() []string {
	files, _ := filepath.Glob(filepath.Join(a.eventsDir, "*.jsonl"))
	dates := make([]string, 0, len(files))
	for _, f := range files {
		dates = append(dates, strings.TrimSuffix(filepath.Base(f), ".jsonl"))
	}
	sort.Strings(dates)
	return dates
}

func (a *analyticsStore) replay(date string) error {
	return a.readEvents(date, func(ev analyticsEvent) { a.apply(ev) })
}

func (a *analyticsStore) readEvents(date string, fn func(analyticsEvent)) error {
	f, err := os.Open(filepath.Join(a.eventsDir, date+".jsonl"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var ev analyticsEvent
		if json.Unmarshal(sc.Bytes(), &ev) == nil {
			fn(ev)
		}
	}
	return sc.Err()
}

func (a *analyticsStore) saveDay(d *dayAgg) {
	if d == nil {
		return
	}
	b, err := json.Marshal(d)
	if err != nil {
		return
	}
	tmp := filepath.Join(a.dailyDir, d.Date+".json.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		a.logger.Printf("analytics: write daily totals: %v", err)
		return
	}
	if err := os.Rename(tmp, filepath.Join(a.dailyDir, d.Date+".json")); err != nil {
		a.logger.Printf("analytics: save daily totals: %v", err)
	}
}

type saltFile struct {
	Date string `json:"date"`
	Salt string `json:"salt"`
}

// loadSalt restores today's salt so a restart doesn't count everyone twice.
// Previous days' salts are overwritten, so old visitor hashes can't be
// re-linked to addresses.
func (a *analyticsStore) loadSalt() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if b, err := os.ReadFile(a.saltPath); err == nil {
		var s saltFile
		if json.Unmarshal(b, &s) == nil && s.Date == a.today {
			if salt, err := hex.DecodeString(s.Salt); err == nil && len(salt) == 32 {
				a.saltDate, a.salt = s.Date, salt
				return
			}
		}
	}
	a.rotateSaltLocked(a.today)
}

func (a *analyticsStore) rotateSaltLocked(date string) {
	salt := make([]byte, 32)
	_, _ = rand.Read(salt)
	a.saltDate, a.salt = date, salt
	b, _ := json.Marshal(saltFile{Date: date, Salt: hex.EncodeToString(salt)})
	if err := os.WriteFile(a.saltPath, b, 0o600); err != nil {
		a.logger.Printf("analytics: save salt: %v", err)
	}
}

// rolloverLocked finishes earlier days once the date changes.
func (a *analyticsStore) rolloverLocked(date string) {
	if date == a.today {
		return
	}
	if date > a.today {
		if d := a.days[a.today]; d != nil {
			a.saveDay(d)
			d.visitors = nil
		}
		a.today = date
	}
	if a.saltDate != a.today {
		a.rotateSaltLocked(a.today)
	}
}

func (a *analyticsStore) visitorHash(ip, ua string) string {
	h := sha256.New()
	h.Write(a.salt)
	h.Write([]byte(ip))
	h.Write([]byte{0})
	h.Write([]byte(ua))
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// baseEvent fills in the anonymized client fields for a request.
func (a *analyticsStore) baseEventLocked(r *http.Request, typ string) analyticsEvent {
	ip := getClientIP(r)
	ua := r.UserAgent()
	info := parseUserAgent(ua)
	ev := analyticsEvent{
		Time:    a.now(),
		Type:    typ,
		Net:     anonymizeIP(ip),
		Browser: info.Browser,
		OS:      info.OS,
		Device:  info.Device,
	}
	a.rolloverLocked(ev.Time.Format(dateLayout))
	ev.Visitor = a.visitorHash(ip, ua)
	return ev
}

func (a *analyticsStore) appendLocked(ev analyticsEvent) {
	date := ev.Time.Format(dateLayout)
	a.rolloverLocked(date)
	if a.file == nil || a.fileDate != date {
		if a.file != nil {
			_ = a.file.Close()
			a.file = nil
		}
		f, err := os.OpenFile(filepath.Join(a.eventsDir, date+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			a.logger.Printf("analytics: open event file: %v", err)
		} else {
			a.file, a.fileDate = f, date
		}
	}
	if a.file != nil {
		b, _ := json.Marshal(ev)
		if _, err := a.file.Write(append(b, '\n')); err != nil {
			a.logger.Printf("analytics: write event: %v", err)
		}
	}
	a.apply(ev)
}

func (a *analyticsStore) dayLocked(date string) *dayAgg {
	d := a.days[date]
	if d == nil {
		d = newDayAgg(date)
		d.PeakConcurrent = a.concurrent
		a.days[date] = d
	}
	if d.visitors == nil {
		d.visitors = map[string]struct{}{}
	}
	return d
}

// apply folds one event into the day totals. It is used both live and when
// replaying raw files, so both paths always agree.
func (a *analyticsStore) apply(ev analyticsEvent) {
	d := a.dayLocked(ev.Time.Format(dateLayout))
	if ev.Type == evServerStart {
		a.concurrent = 0
		return
	}
	if ev.Device == "bot" {
		d.BotHits++
		return
	}
	stamp := ev.Time.Format(time.RFC3339)
	var n *netAgg
	if ev.Net != "" {
		n = d.Networks[ev.Net]
		if n == nil {
			n = &netAgg{FirstSeen: stamp}
			d.Networks[ev.Net] = n
		}
		n.LastSeen = stamp
	}
	if ev.Visitor != "" {
		if _, seen := d.visitors[ev.Visitor]; !seen {
			d.visitors[ev.Visitor] = struct{}{}
			d.Visitors++
			d.Browsers[ev.Browser]++
			d.OSes[ev.OS]++
			d.Devices[ev.Device]++
			if n != nil {
				n.Visitors++
			}
		}
	}
	stream := func() *streamAgg {
		s := d.Streams[ev.Stream]
		if s == nil {
			s = &streamAgg{}
			d.Streams[ev.Stream] = s
		}
		return s
	}
	hour := ev.Time.Hour()
	switch ev.Type {
	case evPageview:
		d.Pageviews++
		d.Pages[ev.Page]++
		d.HourViews[hour]++
		if ev.Referrer != "" {
			d.Referrers[ev.Referrer]++
		}
		if n != nil {
			n.Pageviews++
		}
	case evListenOffer:
		stream().Attempts++
	case evListenConnected:
		stream().Connected++
		d.HourListens[hour]++
		if n != nil {
			n.Listens++
		}
		a.concurrent++
		if a.concurrent > d.PeakConcurrent {
			d.PeakConcurrent = a.concurrent
			d.PeakAt = stamp
		}
	case evListenEnd:
		s := stream()
		switch ev.Detail {
		case endNeverConnected:
			s.NeverConnected++
		case endClosed, endFailed, endLeft, endStopped:
			switch ev.Detail {
			case endClosed:
				s.EndedClosed++
			case endFailed:
				s.EndedFailed++
			case endLeft:
				s.EndedLeft++
			case endStopped:
				s.EndedStopped++
			}
			if a.concurrent > 0 {
				a.concurrent--
			}
			s.ListenMs += ev.DurationMs
			s.DurationsSec = append(s.DurationsSec, ev.DurationMs/1000)
			if n != nil {
				n.ListenMs += ev.DurationMs
			}
		}
	case evAudioPlay:
		d.Plays++
		d.PlaysByStream[ev.Stream]++
		if n != nil {
			n.Plays++
		}
	case evTransmission:
		t := d.Radio[ev.Stream]
		if t == nil {
			t = &txAgg{State: ev.State, Group: ev.Group, Name: ev.Name}
			d.Radio[ev.Stream] = t
		}
		t.Count++
		t.Ms += ev.DurationMs
		if end := ev.Time.Add(time.Duration(ev.DurationMs) * time.Millisecond).Format(time.RFC3339); end > t.Last {
			t.Last = end
		}
		d.TxHour[hour]++
		d.TxLengths[txLengthBucket(ev.DurationMs)]++
		spreadByHour(&d.TxHourMs, ev.Time, ev.DurationMs)
	case evDownload:
		d.Downloads++
	case evTranscriptReq:
		d.TranscriptRequests++
	case evTranscriptBulk:
		d.BulkRequests++
		d.BulkClips += ev.Count
	case evFeedback:
		if ev.Detail == "good" {
			d.FeedbackGood++
		} else if ev.Detail == "bad" {
			d.FeedbackBad++
		}
		d.Corrections += ev.Count
	}
}

// spreadByHour adds a span's milliseconds to the hours it covers, stopping at
// midnight (the rest of a span past midnight is rare and short).
func spreadByHour(hours *[24]int64, start time.Time, ms int64) {
	end := start.Add(time.Duration(ms) * time.Millisecond)
	for t := start; t.Before(end) && t.Day() == start.Day(); {
		next := t.Truncate(time.Hour).Add(time.Hour)
		if next.After(end) {
			next = end
		}
		hours[t.Hour()] += next.Sub(t).Milliseconds()
		t = next
	}
}

// Transmission records one radio transmission (a finished recording) on a
// stream. Safe to call on a nil store.
func (a *analyticsStore) Transmission(info streamInfo, start time.Time, durationMs int) {
	if a == nil || durationMs <= 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	ev := analyticsEvent{
		Time: start, Type: evTransmission, Stream: info.displayName(),
		State: info.StateName, Group: info.GroupName, Name: info.StreamName,
		DurationMs: int64(durationMs),
	}
	// A recording that began before midnight is filed under today; earlier
	// days are already rolled up.
	if now := a.now(); start.Format(dateLayout) != now.Format(dateLayout) {
		ev.Time = now
	}
	a.appendLocked(ev)
}

// Record logs one request-driven event. Safe to call on a nil store.
func (a *analyticsStore) Record(r *http.Request, ev analyticsEvent) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	base := a.baseEventLocked(r, ev.Type)
	base.Page, base.Referrer, base.Stream = ev.Page, ev.Referrer, ev.Stream
	base.DurationMs, base.Detail, base.Count = ev.DurationMs, ev.Detail, ev.Count
	a.appendLocked(base)
}

// ListenStart records a WebRTC offer for a stream.
func (a *analyticsStore) ListenStart(r *http.Request, stream, peerID string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	ev := a.baseEventLocked(r, evListenOffer)
	ev.Stream = stream
	a.peers[peerID] = &peerTrack{base: ev}
	a.appendLocked(ev)
}

// ListenConnected records that a listener's audio connection came up.
func (a *analyticsStore) ListenConnected(peerID string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.peers[peerID]
	if p == nil || !p.connectedAt.IsZero() {
		return
	}
	p.connectedAt = a.now()
	ev := p.base
	ev.Time, ev.Type = p.connectedAt, evListenConnected
	a.appendLocked(ev)
}

// ListenLeaving notes why the client says it is ending a connection (endLeft
// or endStopped), so the following ListenEnded records that instead of a
// generic close or failure.
func (a *analyticsStore) ListenLeaving(peerID, reason string) {
	if a == nil || (reason != endLeft && reason != endStopped) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if p := a.peers[peerID]; p != nil {
		p.leaveReason = reason
	}
}

// ListenEnded records the end of a listener's connection. failed is true
// when WebRTC reported a failure rather than an orderly close.
func (a *analyticsStore) ListenEnded(peerID string, failed bool) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.peers[peerID]
	if p == nil {
		return
	}
	delete(a.peers, peerID)
	ev := p.base
	ev.Time, ev.Type = a.now(), evListenEnd
	switch {
	case p.connectedAt.IsZero():
		ev.Detail = endNeverConnected
	case p.leaveReason != "":
		ev.Detail = p.leaveReason
	case failed:
		ev.Detail = endFailed
	default:
		ev.Detail = endClosed
	}
	if !p.connectedAt.IsZero() {
		ev.DurationMs = ev.Time.Sub(p.connectedAt).Milliseconds()
	}
	a.appendLocked(ev)
}

// PageviewMiddleware counts successful requests for user-facing pages.
func (a *analyticsStore) PageviewMiddleware(next http.Handler) http.Handler {
	if a == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, ok := pageLabel(r)
		if !ok || r.Method != http.MethodGet {
			next.ServeHTTP(w, r)
			return
		}
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if rec.status < 400 {
			a.Record(r, analyticsEvent{Type: evPageview, Page: page, Referrer: externalReferrer(r)})
		}
	})
}

// AudioMiddleware counts recording plays under /audio/. Browsers fetch audio
// in several byte ranges; only the request starting at byte 0 is counted.
func (a *analyticsStore) AudioMiddleware(next http.Handler) http.Handler {
	if a == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")
		if r.Method != http.MethodGet || (rng != "" && !strings.HasPrefix(rng, "bytes=0-")) {
			next.ServeHTTP(w, r)
			return
		}
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if rec.status < 400 {
			stream := path.Dir(strings.TrimPrefix(r.URL.Path, "/audio/"))
			a.Record(r, analyticsEvent{Type: evAudioPlay, Stream: stream})
		}
	})
}

// EventMiddleware counts successful requests of the given method as typ.
func (a *analyticsStore) EventMiddleware(method, typ string, next http.Handler) http.Handler {
	if a == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if r.Method == method && rec.status < 400 {
			a.Record(r, analyticsEvent{Type: typ})
		}
	})
}

// maintain saves finished days, rotates the salt at midnight, and deletes raw
// event files older than the retention period (their daily totals remain).
func (a *analyticsStore) maintain() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rolloverLocked(a.now().Format(dateLayout))
	if a.cfg.RetentionDays < 0 {
		return
	}
	cutoff := a.now().AddDate(0, 0, -a.cfg.RetentionDays).Format(dateLayout)
	for _, date := range a.rawDates() {
		if date >= cutoff || date == a.fileDate {
			continue
		}
		if _, err := os.Stat(filepath.Join(a.dailyDir, date+".json")); err != nil {
			a.saveDay(a.days[date])
			if _, err := os.Stat(filepath.Join(a.dailyDir, date+".json")); err != nil {
				continue
			}
		}
		if err := os.Remove(filepath.Join(a.eventsDir, date+".jsonl")); err != nil {
			a.logger.Printf("analytics: prune %s: %v", date, err)
		}
	}
}

// Run performs hourly maintenance until ctx ends.
func (a *analyticsStore) Run(ctx context.Context) {
	if a == nil {
		return
	}
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.maintain()
		}
	}
}

// Close closes the event file. Today's totals need no saving: they are
// rebuilt from the raw event file at startup.
func (a *analyticsStore) Close() {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.file != nil {
		_ = a.file.Close()
		a.file = nil
	}
	a.geo.Close()
}
