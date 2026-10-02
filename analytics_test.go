package main

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36"

func TestAnonymizeIP(t *testing.T) {
	cases := map[string]string{
		"203.0.113.77":          "203.0.113.0/24",
		"::ffff:198.51.100.9":   "198.51.100.0/24",
		"2001:db8:1234:5678::1": "2001:db8:1234::/48",
		"fe80::1%eth0":          "fe80::/48",
		"not-an-ip":             "",
		"":                      "",
	}
	for in, want := range cases {
		if got := anonymizeIP(in); got != want {
			t.Errorf("anonymizeIP(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseUserAgent(t *testing.T) {
	cases := []struct {
		ua                  string
		browser, os, device string
	}{
		{testUA, "Chrome", "Windows", "desktop"},
		{"Mozilla/5.0 (Windows NT 10.0) AppleWebKit/537.36 Chrome/126.0 Safari/537.36 Edg/126.0", "Edge", "Windows", "desktop"},
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 Version/17.0 Mobile/15E148 Safari/604.1", "Safari", "iOS", "mobile"},
		{"Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 Chrome/126.0 Mobile Safari/537.36", "Chrome", "Android", "mobile"},
		{"Mozilla/5.0 (Linux; Android 13; SM-X700) AppleWebKit/537.36 Chrome/126.0 Safari/537.36", "Chrome", "Android", "tablet"},
		{"Mozilla/5.0 (Macintosh; Intel Mac OS X 14.0; rv:127.0) Gecko/20100101 Firefox/127.0", "Firefox", "macOS", "desktop"},
		{"Googlebot/2.1 (+http://www.google.com/bot.html)", "Bot", "Bot", "bot"},
		{"curl/8.4.0", "Bot", "Bot", "bot"},
		{"", "Unknown", "Unknown", "bot"},
	}
	for _, c := range cases {
		got := parseUserAgent(c.ua)
		if got.Browser != c.browser || got.OS != c.os || got.Device != c.device {
			t.Errorf("parseUserAgent(%q) = %+v, want %s/%s/%s", c.ua, got, c.browser, c.os, c.device)
		}
	}
}

func TestPageLabel(t *testing.T) {
	cases := []struct {
		url  string
		want string
		ok   bool
	}{
		{"/", "/", true},
		{"/index.html", "/", true},
		{"/section.html?embed=1&favorites=1", "", false},
		{"/section.html?favorites=1", "/section.html (favorites)", true},
		{"/section.html?state=UT", "/section.html (UT)", true},
		{"/app.js", "", false},
		{"/transcripts.html", "/transcripts.html", true},
	}
	for _, c := range cases {
		got, ok := pageLabel(httptest.NewRequest(http.MethodGet, c.url, nil))
		if got != c.want || ok != c.ok {
			t.Errorf("pageLabel(%q) = %q,%v want %q,%v", c.url, got, ok, c.want, c.ok)
		}
	}
}

func TestExternalReferrer(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://radio.example:8080/", nil)
	r.Header.Set("Referer", "http://radio.example:8080/section.html")
	if got := externalReferrer(r); got != "" {
		t.Errorf("internal referrer counted: %q", got)
	}
	r.Header.Set("Referer", "https://www.Search.example/q?x=secret")
	if got := externalReferrer(r); got != "search.example" {
		t.Errorf("externalReferrer = %q", got)
	}
}

func TestAnalyticsConfigNormalize(t *testing.T) {
	c := analyticsConfig{}
	if err := c.normalize(); err != nil || c.Dir != "analytics" || c.RetentionDays != defaultAnalyticsRetentionDays {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	for _, bad := range []string{"stats", "/stats", "/short", "/has space here!", "/" + strings.Repeat("a", 65)} {
		c := analyticsConfig{DashboardPath: bad}
		if c.normalize() == nil {
			t.Errorf("dashboardPath %q accepted", bad)
		}
	}
	good := analyticsConfig{DashboardPath: "/" + randomToken()}
	if err := good.normalize(); err != nil {
		t.Errorf("random token rejected: %v", err)
	}
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func newTestStore(t *testing.T, dir string, clock *fakeClock, mutate ...func(*analyticsConfig)) *analyticsStore {
	t.Helper()
	cfg := analyticsConfig{Dir: dir}
	for _, m := range mutate {
		m(&cfg)
	}
	a, err := openAnalyticsStoreAt(cfg, log.New(io.Discard, "", 0), clock.now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a
}

func testRequest(method, target, ip string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	r.RemoteAddr = ip + ":51234"
	r.Header.Set("User-Agent", testUA)
	return r
}

func serveOK(h http.Handler, r *http.Request) {
	h.ServeHTTP(httptest.NewRecorder(), r)
}

func TestAnalyticsStoreLifecycle(t *testing.T) {
	dir := t.TempDir()
	clock := &fakeClock{t: time.Date(2025, 3, 10, 9, 0, 0, 0, time.Local)}
	a := newTestStore(t, dir, clock)

	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	pv := a.PageviewMiddleware(ok)
	serveOK(pv, testRequest(http.MethodGet, "/", "203.0.113.77"))
	serveOK(pv, testRequest(http.MethodGet, "/section.html?state=UT", "203.0.113.77"))
	serveOK(pv, testRequest(http.MethodGet, "/section.html?embed=1", "203.0.113.77"))
	serveOK(pv, testRequest(http.MethodGet, "/", "198.51.100.5"))
	serveOK(pv, testRequest(http.MethodGet, "/app.js", "198.51.100.5"))
	bot := testRequest(http.MethodGet, "/", "192.0.2.1")
	bot.Header.Set("User-Agent", "Googlebot/2.1")
	serveOK(pv, bot)
	notFound := a.PageviewMiddleware(http.NotFoundHandler())
	serveOK(notFound, testRequest(http.MethodGet, "/missing.html", "198.51.100.5"))

	audio := a.AudioMiddleware(ok)
	serveOK(audio, testRequest(http.MethodGet, "/audio/StreamA/clip.wav", "198.51.100.5"))
	partial := testRequest(http.MethodGet, "/audio/StreamA/clip.wav", "198.51.100.5")
	partial.Header.Set("Range", "bytes=1000-")
	serveOK(audio, partial)

	a.ListenStart(testRequest(http.MethodPost, "/offer", "203.0.113.77"), "StreamA", "", "p1")
	a.ListenStart(testRequest(http.MethodPost, "/offer", "198.51.100.5"), "StreamA", "", "p2")
	a.ListenStart(testRequest(http.MethodPost, "/offer", "198.51.100.5"), "StreamA", "", "p3")
	a.ListenConnected("p1")
	a.ListenConnected("p2")
	clock.t = clock.t.Add(90 * time.Second)
	a.ListenLeaving("p3", endLeft) // never connected stays never_connected
	a.ListenEnded("p1", false)
	a.ListenEnded("p2", true)
	a.ListenEnded("p3", false)

	a.ListenStart(testRequest(http.MethodPost, "/offer", "198.51.100.5"), "StreamA", "", "p4")
	a.ListenStart(testRequest(http.MethodPost, "/offer", "198.51.100.5"), "StreamA", "", "p5")
	a.ListenConnected("p4")
	a.ListenConnected("p5")
	clock.t = clock.t.Add(30 * time.Second)
	a.ListenLeaving("p4", endLeft)
	a.ListenLeaving("p5", endStopped)
	a.ListenLeaving("p5", "bogus")
	a.ListenEnded("p4", true) // a leave beacon wins over the later ICE failure
	a.ListenEnded("p5", false)

	a.Record(testRequest(http.MethodPost, "/transcripts/bulk", "198.51.100.5"), analyticsEvent{Type: evTranscriptBulk, Count: 4})
	a.Record(testRequest(http.MethodPost, "/feedback", "198.51.100.5"), analyticsEvent{Type: evFeedback, Detail: "bad", Count: 1})

	check := func(label string, a *analyticsStore) {
		t.Helper()
		d := a.days["2025-03-10"]
		if d == nil {
			t.Fatalf("%s: no day aggregate", label)
		}
		if d.Visitors != 2 || d.Pageviews != 3 || d.BotHits != 1 {
			t.Errorf("%s: visitors=%d pageviews=%d bots=%d", label, d.Visitors, d.Pageviews, d.BotHits)
		}
		if d.Plays != 1 || d.BulkClips != 4 || d.FeedbackBad != 1 || d.Corrections != 1 {
			t.Errorf("%s: plays=%d bulkClips=%d bad=%d corrections=%d", label, d.Plays, d.BulkClips, d.FeedbackBad, d.Corrections)
		}
		n := d.Networks["203.0.113.0/24"]
		if n == nil || n.Visitors != 1 || n.Pageviews != 2 || n.Listens != 1 {
			t.Errorf("%s: network 203.0.113.0/24 = %+v", label, n)
		}
		s := d.Streams["StreamA"]
		if s == nil || s.Attempts != 5 || s.Connected != 4 || s.NeverConnected != 1 || s.EndedClosed != 1 || s.EndedFailed != 1 ||
			s.EndedLeft != 1 || s.EndedStopped != 1 || s.ListenMs != 240000 {
			t.Errorf("%s: stream = %+v", label, s)
		}
		if d.PeakConcurrent != 2 {
			t.Errorf("%s: peak = %d", label, d.PeakConcurrent)
		}
	}
	check("live", a)

	raw, err := os.ReadFile(filepath.Join(dir, "events", "2025-03-10.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "203.0.113.77") || strings.Contains(string(raw), "Mozilla") {
		t.Fatal("raw events contain a full IP address or User-Agent")
	}

	// Restart the same day: totals are rebuilt and the salt is reused, so a
	// returning visitor is not counted twice.
	a.Close()
	b := newTestStore(t, dir, clock)
	check("replayed", b)
	serveOK(b.PageviewMiddleware(ok), testRequest(http.MethodGet, "/", "203.0.113.77"))
	if v := b.days["2025-03-10"].Visitors; v != 2 {
		t.Errorf("visitor counted twice after restart: %d", v)
	}

	// Next day: the previous day is rolled up and a new visitor hash is used.
	clock.t = clock.t.Add(24 * time.Hour)
	serveOK(b.PageviewMiddleware(ok), testRequest(http.MethodGet, "/", "203.0.113.77"))
	if _, err := os.Stat(filepath.Join(dir, "daily", "2025-03-10.json")); err != nil {
		t.Fatalf("rollup not written: %v", err)
	}
	if v := b.days["2025-03-11"].Visitors; v != 1 {
		t.Errorf("day 2 visitors = %d", v)
	}
	if v := b.days["2025-03-10"].Pageviews; v != 4 {
		t.Errorf("day 1 pageviews after rollover = %d", v)
	}

	// After the retention period, raw events are pruned but totals remain.
	b.Close()
	clock.t = clock.t.AddDate(0, 0, 10)
	c := newTestStore(t, dir, clock, func(cfg *analyticsConfig) { cfg.RetentionDays = 5 })
	if _, err := os.Stat(filepath.Join(dir, "events", "2025-03-10.jsonl")); !os.IsNotExist(err) {
		t.Errorf("old raw events not pruned: %v", err)
	}
	if d := c.days["2025-03-10"]; d == nil || d.Pageviews != 4 || d.Networks["203.0.113.0/24"] == nil {
		t.Errorf("rolled-up totals lost after prune: %+v", d)
	}
}

func TestAnalyticsNilStoreIsSafe(t *testing.T) {
	var a *analyticsStore
	r := testRequest(http.MethodGet, "/", "203.0.113.1")
	a.Record(r, analyticsEvent{Type: evPageview})
	a.ListenStart(r, "s", "", "p")
	a.ListenConnected("p")
	a.ListenEnded("p", false)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	serveOK(a.PageviewMiddleware(h), r)
	serveOK(a.AudioMiddleware(h), r)
	serveOK(a.EventMiddleware(http.MethodPost, evDownload, h), r)
	a.Close()
}

func TestLocationOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "locations.csv")
	content := "# cidr,label\n10.0.0.0/8,Corp WAN\n10.20.30.0/24,\"Building 7, Floor 2\"\nbad line\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	g := newGeoLocator("", path, log.New(io.Discard, "", 0))
	defer g.Close()
	if got := g.Lookup("10.20.30.0/24").Label(); got != "Building 7, Floor 2" {
		t.Errorf("longest prefix = %q", got)
	}
	if got := g.Lookup("10.99.1.0/24").Label(); got != "Corp WAN" {
		t.Errorf("broad prefix = %q", got)
	}
	if got := g.Lookup("203.0.113.0/24").Label(); got != "" {
		t.Errorf("unmatched = %q", got)
	}
}

func TestDashboardAndExports(t *testing.T) {
	dir := t.TempDir()
	clock := &fakeClock{t: time.Date(2025, 3, 10, 14, 0, 0, 0, time.Local)}
	a := newTestStore(t, dir, clock)
	serveOK(a.PageviewMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})),
		testRequest(http.MethodGet, "/", "203.0.113.77"))
	a.ListenStart(testRequest(http.MethodPost, "/offer", "203.0.113.77"), "Stream <A>", "", "p1")
	a.ListenConnected("p1")

	path := "/" + randomToken()
	mux := http.NewServeMux()
	a.RegisterDashboard(mux, path, func() map[string]int { return map[string]int{"Stream <A>": 1} }, func() []streamInfo {
		return []streamInfo{{StateName: "Utah", GroupName: "Ashley NF", StreamName: "Net <1>"}, {StateName: "Utah", GroupName: "Ashley NF", StreamName: "Net 2"}}
	})
	a.Transmission(streamInfo{StateName: "Utah", GroupName: "Ashley NF", StreamName: "Net <1>"}, clock.t.Add(-time.Minute), 12000)

	get := func(target string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		return rec
	}

	for _, days := range []string{"", "?days=1", "?days=0", "?days=365"} {
		rec := get(path + days)
		if rec.Code != http.StatusOK {
			t.Fatalf("dashboard%s status %d", days, rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "203.0.113.0/24") {
			t.Errorf("dashboard%s missing network", days)
		}
		if strings.Contains(body, "Stream <A>") || strings.Contains(body, "Net <1>") {
			t.Errorf("dashboard%s did not escape stream name", days)
		}
		if !strings.Contains(body, "Radio activity") || !strings.Contains(body, "Net &lt;1&gt;") || !strings.Contains(body, "1/2") {
			t.Errorf("dashboard%s missing radio activity", days)
		}
		if rec.Header().Get("Cache-Control") != "no-store" || !strings.Contains(rec.Header().Get("X-Robots-Tag"), "noindex") {
			t.Errorf("dashboard%s missing private headers", days)
		}
	}

	for _, csvPath := range []string{"/events.csv", "/networks.csv"} {
		rec := get(path + csvPath + "?days=0")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status %d", csvPath, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "203.0.113.0/24") {
			t.Errorf("%s missing network:\n%s", csvPath, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "203.0.113.77") {
			t.Errorf("%s leaks full IP", csvPath)
		}
	}

	if rec := get(path + "/radio.csv?days=7"); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), "Utah,Ashley NF,Net <1>,true,1,0.2,12.0,") ||
		!strings.Contains(rec.Body.String(), "Utah,Ashley NF,Net 2,true,0,0.0,,") {
		t.Errorf("radio.csv:\n%s", rec.Body.String())
	}

	if rec := get("/" + strings.Repeat("x", 20)); rec.Code != http.StatusNotFound {
		t.Errorf("unregistered path served: %d", rec.Code)
	}
}

func TestHandleListenLeaveValidation(t *testing.T) {
	s := &webrtcServer{}
	cases := []struct {
		method, body string
		want         int
	}{
		{http.MethodGet, "", http.StatusMethodNotAllowed},
		{http.MethodPost, "token=x&reason=nope", http.StatusBadRequest},
		{http.MethodPost, "token=unknown&reason=left", http.StatusNoContent},
		{http.MethodPost, "token=unknown&reason=stopped", http.StatusNoContent},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, "/listen-leave", strings.NewReader(c.body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		s.handleListenLeave(w, r)
		if w.Code != c.want {
			t.Errorf("%s %q: got %d, want %d", c.method, c.body, w.Code, c.want)
		}
	}
}

func TestTransmissions(t *testing.T) {
	dir := t.TempDir()
	clock := &fakeClock{t: time.Date(2025, 3, 10, 15, 0, 0, 0, time.Local)} // a Monday
	a := newTestStore(t, dir, clock)
	n1 := streamInfo{StateName: "Utah", GroupName: "Ashley NF", StreamName: "Net 1"}
	n2 := streamInfo{StateName: "Utah", GroupName: "Dixie NF", StreamName: "Net 2"}
	a.Transmission(n1, time.Date(2025, 3, 10, 9, 59, 0, 0, time.Local), 120000) // spans 09:59-10:01
	a.Transmission(n1, time.Date(2025, 3, 10, 14, 0, 0, 0, time.Local), 3000)
	a.Transmission(n2, time.Date(2025, 3, 10, 14, 30, 0, 0, time.Local), 500)
	a.Transmission(n2, time.Date(2025, 3, 10, 14, 31, 0, 0, time.Local), 0) // ignored

	check := func(label string, a *analyticsStore) {
		t.Helper()
		d := a.days["2025-03-10"]
		r := d.Radio[n1.displayName()]
		if r == nil || r.Count != 2 || r.Ms != 123000 || r.State != "Utah" || r.Group != "Ashley NF" || r.Name != "Net 1" {
			t.Errorf("%s: net 1 = %+v", label, r)
		}
		if d.TxHour[9] != 1 || d.TxHour[14] != 2 || d.TxHourMs[9] != 60000 || d.TxHourMs[10] != 60000 || d.TxHourMs[14] != 3500 {
			t.Errorf("%s: hours count=%v ms=%v", label, d.TxHour, d.TxHourMs)
		}
		if d.TxLengths[0] != 1 || d.TxLengths[1] != 1 || d.TxLengths[6] != 1 {
			t.Errorf("%s: lengths = %v", label, d.TxLengths)
		}
	}
	check("live", a)
	a.Close()
	b := newTestStore(t, dir, clock)
	check("replayed", b)

	inv := func() []streamInfo {
		return []streamInfo{n1, n2, {StateName: "Alaska", GroupName: "Chugach NF", StreamName: "Glacier"}}
	}
	rd := b.radioStats(b.daysInRange(1), 1, inv)
	if rd.States != 2 || rd.Forests != 3 || rd.Streams != 3 || rd.ActiveStreams != 2 || rd.Count != 3 || rd.BusiestHour != "09:00–10:00" {
		t.Errorf("summary = %+v", rd)
	}
	if len(rd.ByState) != 2 || rd.ByState[0].Name != "Utah" || rd.ByState[0].Active != 2 || rd.ByState[0].Streams != 2 || rd.ByState[1].Count != 0 {
		t.Errorf("by state = %+v", rd.ByState)
	}
	if len(rd.ByStream) != 3 || rd.ByStream[0].Name != "Net 1" || rd.ByStream[2].Last != "–" {
		t.Errorf("by stream = %+v", rd.ByStream)
	}
	if rd.HeatCount[0].Cells[14].Label != "2" || rd.HeatAir[0].Cells[9].Label != "1" {
		t.Errorf("heat = %+v / %+v", rd.HeatCount[0].Cells[14], rd.HeatAir[0].Cells[9])
	}

	// A recording that began before midnight is filed under the new day, and
	// totals survive the rollup.
	clock.t = time.Date(2025, 3, 11, 0, 0, 5, 0, time.Local)
	b.Transmission(n1, time.Date(2025, 3, 10, 23, 59, 50, 0, time.Local), 15000)
	if d := b.days["2025-03-11"]; d == nil || d.Radio[n1.displayName()].Count != 1 {
		t.Errorf("cross-midnight transmission not filed under today")
	}
	b.Close()
	c := newTestStore(t, dir, clock)
	if d := c.days["2025-03-10"]; d == nil || d.Radio[n1.displayName()] == nil || d.Radio[n1.displayName()].Count != 2 {
		t.Errorf("rolled-up radio totals lost")
	}
	// Retired streams still show up, flagged.
	rd = c.radioStats(c.daysInRange(0), 0, func() []streamInfo { return []streamInfo{n2} })
	retired := 0
	for _, r := range rd.ByStream {
		if r.Retired {
			retired++
		}
	}
	if rd.Streams != 1 || retired != 1 {
		t.Errorf("retired handling: streams=%d retired=%d", rd.Streams, retired)
	}
}
