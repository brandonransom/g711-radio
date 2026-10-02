package main

import (
	_ "embed"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed templates/analytics.html
var analyticsTemplateText string

var analyticsTemplate = template.Must(template.New("analytics").Parse(analyticsTemplateText))

// liveListenerFunc reports current connected listeners per stream name.
type liveListenerFunc func() map[string]int

// inventoryFunc lists the configured radio streams.
type inventoryFunc func() []streamInfo

// radioRow is one stream, forest or state in the radio activity tables.
type radioRow struct {
	Name     string
	State    string
	Group    string
	Streams  int // configured streams (forest/state rows)
	Active   int // streams with at least one transmission
	Count    int
	AirMs    int64
	Air      string
	Avg      string
	PerDay   string
	Last     string
	Pct      float64 // share of the largest airtime, for bar widths
	Retired  bool    // has history but is no longer configured
	sortName string
}

type radioData struct {
	States, Forests, Streams int
	ActiveStreams            int
	Count                    int
	Air                      string
	Avg                      string
	PerDay                   string
	DutyPct                  string // airtime as a share of stream-hours in the range
	BusiestHour              string
	ByState                  []radioRow
	ByForest                 []radioRow
	ByStream                 []radioRow
	Lengths                  []kv
	HeatCount                []heatRow
	HeatAir                  []heatRow
	ArchiveNote              string // where pre-deployment history came from
}

type kv struct {
	Key   string
	Value int
	Pct   float64 // share of the largest value, for bar widths
	Extra string
}

type netRow struct {
	Network   string
	Location  string
	Override  bool
	Country   string
	Visitors  int
	Pageviews int
	Listens   int
	ListenMs  int64
	Listen    string
	Plays     int
	FirstSeen string
	LastSeen  string
}

type streamRow struct {
	Name           string
	Attempts       int
	Connected      int
	NeverConnected int
	EndedClosed    int
	EndedFailed    int
	EndedLeft      int
	EndedStopped   int
	SuccessPct     string
	Total          string
	Median         string
	Average        string
	listenMs       int64
}

type dailyRow struct {
	Date      string
	Visitors  int
	Pageviews int
	Listens   int
	Tx        int
	Air       string
	Pct       float64
}

type heatCell struct {
	Value int64
	Label string
	Title string
	Alpha float64
}

type heatRow struct {
	Day   string
	Cells [24]heatCell
}

type rangeOpt struct {
	Days   int
	Label  string
	Active bool
}

type dashData struct {
	BasePath      string
	Days          int
	From, To      string
	Ranges        []rangeOpt
	Generated     string
	LiveTotal     int
	Live          []kv
	Visitors      int
	AvgVisitors   string
	Pageviews     int
	Listens       int
	NetworkCount  int
	BotHits       int
	ListenTotal   string
	MedianListen  string
	PeakListeners int
	PeakAt        string
	Daily         []dailyRow
	Pages         []kv
	Referrers     []kv
	Browsers      []kv
	OSes          []kv
	Devices       []kv
	Locations     []kv
	Countries     []kv
	Networks      []netRow
	NetworksMore  int
	Streams       []streamRow
	Health        streamRow
	HeatListens   []heatRow
	HeatViews     []heatRow
	Plays         int
	PlaysByStream []kv
	Downloads     int
	Transcripts   int
	BulkRequests  int
	BulkClips     int
	FeedbackGood  int
	FeedbackBad   int
	Corrections   int
	RawFrom       string
	Retention     string
	GeoSource     string
	Radio         radioData
}

var dashRanges = []rangeOpt{{1, "Today", false}, {7, "7 days", false}, {30, "30 days", false}, {90, "90 days", false}, {365, "1 year", false}, {0, "All time", false}}

// parseDays reads ?days=; 0 means all time.
func parseDays(r *http.Request) int {
	days, err := strconv.Atoi(r.URL.Query().Get("days"))
	if err != nil || days < 0 {
		return 30
	}
	return days
}

// rangeDates returns the inclusive date bounds for a range ("" = unbounded).
func (a *analyticsStore) rangeDates(days int) (from, to string) {
	to = a.now().Format(dateLayout)
	if days > 0 {
		from = a.now().AddDate(0, 0, -(days - 1)).Format(dateLayout)
	}
	return from, to
}

// RegisterDashboard serves the stats page and its CSV exports at path.
func (a *analyticsStore) RegisterDashboard(mux *http.ServeMux, path string, live liveListenerFunc, inventory inventoryFunc) {
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		privateHeaders(w)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		data := a.dashboard(parseDays(r), live, inventory)
		data.BasePath = path
		if err := analyticsTemplate.Execute(w, data); err != nil {
			a.logger.Printf("analytics: render dashboard: %v", err)
		}
	})
	mux.HandleFunc(path+"/events.csv", func(w http.ResponseWriter, r *http.Request) {
		privateHeaders(w)
		a.exportEvents(w, parseDays(r))
	})
	mux.HandleFunc(path+"/networks.csv", func(w http.ResponseWriter, r *http.Request) {
		privateHeaders(w)
		a.exportNetworks(w, parseDays(r))
	})
	mux.HandleFunc(path+"/radio.csv", func(w http.ResponseWriter, r *http.Request) {
		privateHeaders(w)
		a.exportRadio(w, parseDays(r), inventory)
	})
}

// privateHeaders keeps the dashboard out of caches, search engines, and
// Referer headers sent to other sites.
func privateHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Robots-Tag", "noindex, nofollow")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'")
}

// daysInRange returns copies of the day totals within the range, oldest first.
func (a *analyticsStore) daysInRange(days int) []dayAgg {
	from, to := a.rangeDates(days)
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]dayAgg, 0, len(a.days))
	for date, d := range a.days {
		if (from == "" || date >= from) && date <= to {
			if date == a.today {
				// Only today's totals change after creation; copy it deeply so
				// the dashboard can read it without holding the lock.
				var c dayAgg
				if b, err := json.Marshal(d); err == nil && json.Unmarshal(b, &c) == nil {
					out = append(out, c)
				}
				continue
			}
			out = append(out, *d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date < out[j].Date })
	return out
}

// mergeNetworks sums per-network totals across days.
func mergeNetworks(days []dayAgg) map[string]*netAgg {
	nets := map[string]*netAgg{}
	for _, d := range days {
		for k, n := range d.Networks {
			m := nets[k]
			if m == nil {
				m = &netAgg{FirstSeen: n.FirstSeen}
				nets[k] = m
			}
			m.Visitors += n.Visitors
			m.Pageviews += n.Pageviews
			m.Listens += n.Listens
			m.ListenMs += n.ListenMs
			m.Plays += n.Plays
			if n.FirstSeen < m.FirstSeen {
				m.FirstSeen = n.FirstSeen
			}
			if n.LastSeen > m.LastSeen {
				m.LastSeen = n.LastSeen
			}
		}
	}
	return nets
}

func (a *analyticsStore) dashboard(days int, live liveListenerFunc, inventory inventoryFunc) dashData {
	agg := a.daysInRange(days)
	all := a.withRadioArchive(agg, days)
	data := dashData{Days: days, Generated: a.now().Format("2006-01-02 15:04:05 MST"), GeoSource: a.geo.Source()}
	data.From, data.To = a.rangeDates(days)
	if data.From == "" && len(all) > 0 {
		data.From = all[0].Date
	}
	for _, r := range dashRanges {
		r.Active = r.Days == days
		data.Ranges = append(data.Ranges, r)
	}
	if a.cfg.RetentionDays < 0 {
		data.Retention = "kept forever"
	} else {
		data.Retention = fmt.Sprintf("kept %d days", a.cfg.RetentionDays)
	}
	if raw := a.rawDates(); len(raw) > 0 {
		data.RawFrom = raw[0]
	}

	if live != nil {
		m := live()
		for k, v := range m {
			data.LiveTotal += v
			data.Live = append(data.Live, kv{Key: k, Value: v})
		}
		data.Live = rank(data.Live, 0)
	}

	pages, refs, browsers, oses, devices, plays := map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}
	streams := map[string]*streamAgg{}
	var durations []int64
	var heatL, heatV [7][24]int
	maxDaily := 0
	// all includes radio-only days from the audio archive; their site
	// totals are zero, so they add nothing except radio and daily rows.
	for _, d := range all {
		data.Visitors += d.Visitors
		data.Pageviews += d.Pageviews
		data.BotHits += d.BotHits
		data.Plays += d.Plays
		data.Downloads += d.Downloads
		data.Transcripts += d.TranscriptRequests
		data.BulkRequests += d.BulkRequests
		data.BulkClips += d.BulkClips
		data.FeedbackGood += d.FeedbackGood
		data.FeedbackBad += d.FeedbackBad
		data.Corrections += d.Corrections
		if d.PeakConcurrent > data.PeakListeners {
			data.PeakListeners, data.PeakAt = d.PeakConcurrent, shortStamp(d.PeakAt)
		}
		addMap(pages, d.Pages)
		addMap(refs, d.Referrers)
		addMap(browsers, d.Browsers)
		addMap(oses, d.OSes)
		addMap(devices, d.Devices)
		addMap(plays, d.PlaysByStream)
		dayListens := 0
		for name, s := range d.Streams {
			m := streams[name]
			if m == nil {
				m = &streamAgg{}
				streams[name] = m
			}
			m.Attempts += s.Attempts
			m.Connected += s.Connected
			m.NeverConnected += s.NeverConnected
			m.EndedClosed += s.EndedClosed
			m.EndedFailed += s.EndedFailed
			m.EndedLeft += s.EndedLeft
			m.EndedStopped += s.EndedStopped
			m.ListenMs += s.ListenMs
			m.DurationsSec = append(m.DurationsSec, s.DurationsSec...)
			durations = append(durations, s.DurationsSec...)
			dayListens += s.Connected
		}
		data.Listens += dayListens
		if t, err := time.Parse(dateLayout, d.Date); err == nil {
			wd := (int(t.Weekday()) + 6) % 7 // Monday first
			for h := 0; h < 24; h++ {
				heatL[wd][h] += d.HourListens[h]
				heatV[wd][h] += d.HourViews[h]
			}
		}
		row := dailyRow{Date: d.Date, Visitors: d.Visitors, Pageviews: d.Pageviews, Listens: dayListens}
		var dayAir int64
		for _, t := range d.Radio {
			row.Tx += t.Count
			dayAir += t.Ms
		}
		row.Air = fmtDuration(dayAir)
		if d.Visitors > maxDaily {
			maxDaily = d.Visitors
		}
		data.Daily = append(data.Daily, row)
	}
	for i := range data.Daily {
		if maxDaily > 0 {
			data.Daily[i].Pct = 100 * float64(data.Daily[i].Visitors) / float64(maxDaily)
		}
	}
	for i, j := 0, len(data.Daily)-1; i < j; i, j = i+1, j-1 {
		data.Daily[i], data.Daily[j] = data.Daily[j], data.Daily[i]
	}
	if len(agg) > 0 {
		data.AvgVisitors = fmt.Sprintf("%.1f", float64(data.Visitors)/float64(len(agg)))
	}
	data.Pages = rank(mapToKV(pages), 25)
	data.Referrers = rank(mapToKV(refs), 25)
	data.Browsers = rank(mapToKV(browsers), 0)
	data.OSes = rank(mapToKV(oses), 0)
	data.Devices = rank(mapToKV(devices), 0)
	data.PlaysByStream = rank(mapToKV(plays), 25)
	data.MedianListen = fmtSeconds(median(durations))

	var health streamAgg
	var totalMs int64
	for name, s := range streams {
		row := makeStreamRow(name, s)
		data.Streams = append(data.Streams, row)
		health.Attempts += s.Attempts
		health.Connected += s.Connected
		health.NeverConnected += s.NeverConnected
		health.EndedClosed += s.EndedClosed
		health.EndedFailed += s.EndedFailed
		health.EndedLeft += s.EndedLeft
		health.EndedStopped += s.EndedStopped
		health.ListenMs += s.ListenMs
		totalMs += s.ListenMs
	}
	health.DurationsSec = durations
	data.Health = makeStreamRow("All streams", &health)
	data.ListenTotal = fmtDuration(totalMs)
	sort.Slice(data.Streams, func(i, j int) bool { return data.Streams[i].listenMs > data.Streams[j].listenMs })

	data.HeatListens = heatRows(heatL)
	data.HeatViews = heatRows(heatV)

	nets := mergeNetworks(agg)
	data.NetworkCount = len(nets)
	locs, countries := map[string]int{}, map[string]int{}
	for k, n := range nets {
		g := a.geo.Lookup(k)
		label := g.Label()
		if label == "" {
			label = "Unknown"
		}
		country := g.Country
		if country == "" {
			country = "Unknown"
		}
		locs[label] += n.Visitors
		countries[country] += n.Visitors
		data.Networks = append(data.Networks, netRow{
			Network: k, Location: g.Label(), Override: g.Override != "", Country: g.Country,
			Visitors: n.Visitors, Pageviews: n.Pageviews, Listens: n.Listens, ListenMs: n.ListenMs,
			Listen: fmtDuration(n.ListenMs), Plays: n.Plays,
			FirstSeen: shortStamp(n.FirstSeen), LastSeen: shortStamp(n.LastSeen),
		})
	}
	sort.Slice(data.Networks, func(i, j int) bool {
		x, y := data.Networks[i], data.Networks[j]
		if x.Visitors != y.Visitors {
			return x.Visitors > y.Visitors
		}
		if x.ListenMs != y.ListenMs {
			return x.ListenMs > y.ListenMs
		}
		return x.Network < y.Network
	})
	const maxNetRows = 200
	if len(data.Networks) > maxNetRows {
		data.NetworksMore = len(data.Networks) - maxNetRows
		data.Networks = data.Networks[:maxNetRows]
	}
	data.Locations = rank(mapToKV(locs), 30)
	data.Countries = rank(mapToKV(countries), 0)
	data.Radio = a.radioStats(all, days, inventory)
	return data
}

func makeStreamRow(name string, s *streamAgg) streamRow {
	row := streamRow{
		Name: name, Attempts: s.Attempts, Connected: s.Connected, NeverConnected: s.NeverConnected,
		EndedClosed: s.EndedClosed, EndedFailed: s.EndedFailed, EndedLeft: s.EndedLeft, EndedStopped: s.EndedStopped,
		Total: fmtDuration(s.ListenMs), Median: fmtSeconds(median(s.DurationsSec)), listenMs: s.ListenMs,
		SuccessPct: "–", Average: "–",
	}
	if s.Attempts > 0 {
		row.SuccessPct = fmt.Sprintf("%.0f%%", 100*float64(s.Connected)/float64(s.Attempts))
	}
	if n := len(s.DurationsSec); n > 0 {
		row.Average = fmtDuration(s.ListenMs / int64(n))
	}
	return row
}

func heatRows(h [7][24]int) []heatRow {
	var v [7][24]int64
	for d := range h {
		for hr := range h[d] {
			v[d][hr] = int64(h[d][hr])
		}
	}
	return heatRowsFmt(v, func(n int64) (string, string) { s := strconv.FormatInt(n, 10); return s, s })
}

// heatRowsFmt builds a weekday × hour heatmap; label returns the cell text
// and its tooltip for non-zero values.
func heatRowsFmt(h [7][24]int64, label func(int64) (string, string)) []heatRow {
	var maxV int64
	for _, row := range h {
		for _, v := range row {
			if v > maxV {
				maxV = v
			}
		}
	}
	names := []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}
	rows := make([]heatRow, 7)
	for d := 0; d < 7; d++ {
		rows[d].Day = names[d]
		for hr := 0; hr < 24; hr++ {
			c := heatCell{Value: h[d][hr]}
			if maxV > 0 && c.Value > 0 {
				c.Alpha = 0.12 + 0.88*float64(c.Value)/float64(maxV)
				c.Label, c.Title = label(c.Value)
			}
			rows[d].Cells[hr] = c
		}
	}
	return rows
}

func weekdayIndex(date string) (int, bool) {
	t, err := time.Parse(dateLayout, date)
	if err != nil {
		return 0, false
	}
	return (int(t.Weekday()) + 6) % 7, true // Monday first
}

// radioStats summarizes transmissions over the range, alongside the
// configured stream inventory (streams with no traffic still appear).
func (a *analyticsStore) radioStats(agg []dayAgg, days int, inventory inventoryFunc) radioData {
	var rd radioData
	a.mu.Lock()
	rd.ArchiveNote = a.archiveNote
	a.mu.Unlock()
	streams := map[string]*radioRow{}
	configured := map[string]bool{}
	if inventory != nil {
		states, forests := map[string]bool{}, map[string]bool{}
		for _, s := range inventory() {
			key := s.displayName()
			configured[key] = true
			states[s.StateName] = true
			forests[s.StateName+"\x00"+s.GroupName] = true
			streams[key] = &radioRow{Name: s.StreamName, State: s.StateName, Group: s.GroupName}
		}
		rd.States, rd.Forests, rd.Streams = len(states), len(forests), len(configured)
	}

	var heatCount, heatAir [7][24]int64
	var hourAir [24]int64
	var lengths [txLengthBuckets]int
	var totalMs int64
	firstDate := ""
	for _, d := range agg {
		if len(d.Radio) == 0 {
			continue
		}
		if firstDate == "" {
			firstDate = d.Date
		}
		for key, t := range d.Radio {
			r := streams[key]
			if r == nil {
				r = &radioRow{Name: t.Name, State: t.State, Group: t.Group, Retired: inventory != nil}
				streams[key] = r
			}
			r.Count += t.Count
			r.AirMs += t.Ms
			if t.Last > r.Last {
				r.Last = t.Last
			}
			rd.Count += t.Count
			totalMs += t.Ms
		}
		for i, n := range d.TxLengths {
			lengths[i] += n
		}
		wd, ok := weekdayIndex(d.Date)
		for h := 0; h < 24; h++ {
			hourAir[h] += d.TxHourMs[h]
			if ok {
				heatCount[wd][h] += int64(d.TxHour[h])
				heatAir[wd][h] += d.TxHourMs[h]
			}
		}
	}
	if inventory == nil {
		states, forests := map[string]bool{}, map[string]bool{}
		for _, r := range streams {
			states[r.State] = true
			forests[r.State+"\x00"+r.Group] = true
		}
		rd.States, rd.Forests, rd.Streams = len(states), len(forests), len(streams)
	}

	// Per-day rates and duty cycle cover the part of the range that has
	// radio data, ending now.
	now := a.now()
	spanStart := firstDate
	if from, _ := a.rangeDates(days); from > spanStart {
		spanStart = from
	}
	spanDays, elapsed := 0.0, time.Duration(0)
	if t, err := time.ParseInLocation(dateLayout, spanStart, now.Location()); err == nil && firstDate != "" {
		elapsed = now.Sub(t)
		spanDays = elapsed.Hours() / 24
		if spanDays < 1 {
			spanDays = 1
		}
	}
	perDay := func(n int) string {
		if spanDays == 0 {
			return "–"
		}
		return fmt.Sprintf("%.1f", float64(n)/spanDays)
	}

	rd.Air = fmtDuration(totalMs)
	rd.Avg, rd.PerDay, rd.DutyPct, rd.BusiestHour = "–", perDay(rd.Count), "–", "–"
	if rd.Count > 0 {
		rd.Avg = fmtDuration(totalMs / int64(rd.Count))
	}
	if rd.Streams > 0 && elapsed > 0 {
		rd.DutyPct = fmt.Sprintf("%.2f%%", 100*float64(totalMs)/(float64(rd.Streams)*float64(elapsed.Milliseconds())))
	}
	if busiest := maxIndex(hourAir[:]); hourAir[busiest] > 0 {
		rd.BusiestHour = fmt.Sprintf("%02d:00–%02d:00", busiest, (busiest+1)%24)
	}

	forests, states := map[string]*radioRow{}, map[string]*radioRow{}
	group := func(m map[string]*radioRow, key string, proto radioRow, r *radioRow) {
		g := m[key]
		if g == nil {
			g = &proto
			m[key] = g
		}
		if !r.Retired {
			g.Streams++
		}
		if r.Count > 0 {
			g.Active++
		}
		g.Count += r.Count
		g.AirMs += r.AirMs
		if r.Last > g.Last {
			g.Last = r.Last
		}
	}
	for _, r := range streams {
		if r.Count > 0 {
			rd.ActiveStreams++
		}
		group(forests, r.State+"\x00"+r.Group, radioRow{Name: r.Group, State: r.State, sortName: r.State + "\x00" + r.Group}, r)
		group(states, r.State, radioRow{Name: r.State, sortName: r.State}, r)
		r.sortName = r.State + "\x00" + r.Group + "\x00" + r.Name
	}
	finish := func(m map[string]*radioRow) []radioRow {
		out := make([]radioRow, 0, len(m))
		var maxMs int64
		for _, r := range m {
			if r.AirMs > maxMs {
				maxMs = r.AirMs
			}
		}
		for _, r := range m {
			r.Air, r.PerDay, r.Avg = fmtDuration(r.AirMs), perDay(r.Count), "–"
			if r.Count > 0 {
				r.Avg = fmtDuration(r.AirMs / int64(r.Count))
			}
			if maxMs > 0 {
				r.Pct = 100 * float64(r.AirMs) / float64(maxMs)
			}
			if r.Last == "" {
				r.Last = "–"
			} else {
				r.Last = shortStamp(r.Last)
			}
			out = append(out, *r)
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].AirMs != out[j].AirMs {
				return out[i].AirMs > out[j].AirMs
			}
			if out[i].Count != out[j].Count {
				return out[i].Count > out[j].Count
			}
			return out[i].sortName < out[j].sortName
		})
		return out
	}
	rd.ByStream, rd.ByForest, rd.ByState = finish(streams), finish(forests), finish(states)

	maxLen := 0
	for _, n := range lengths {
		if n > maxLen {
			maxLen = n
		}
	}
	for i, n := range lengths {
		row := kv{Key: txLengthLabels[i], Value: n}
		if maxLen > 0 {
			row.Pct = 100 * float64(n) / float64(maxLen)
		}
		rd.Lengths = append(rd.Lengths, row)
	}
	rd.HeatCount = heatRowsFmt(heatCount, func(n int64) (string, string) {
		s := strconv.FormatInt(n, 10)
		return s, s + " transmissions"
	})
	rd.HeatAir = heatRowsFmt(heatAir, func(ms int64) (string, string) {
		label := strconv.FormatInt((ms+30000)/60000, 10)
		if ms < 30000 {
			label = "<1"
		}
		return label, fmtDuration(ms) + " of airtime"
	})
	return rd
}

func maxIndex(v []int64) int {
	best := 0
	for i, x := range v {
		if x > v[best] {
			best = i
		}
	}
	return best
}

// exportRadio writes per-stream transmission totals for the range.
func (a *analyticsStore) exportRadio(w http.ResponseWriter, days int, inventory inventoryFunc) {
	from, to := a.rangeDates(days)
	rd := a.radioStats(a.withRadioArchive(a.daysInRange(days), days), days, inventory)
	rows := rd.ByStream
	sort.Slice(rows, func(i, j int) bool { return rows[i].sortName < rows[j].sortName })
	cw := csvAttachment(w, "radio-"+strings.TrimSpace(from+"_"+to)+".csv")
	_ = cw.Write([]string{"state", "forest", "stream", "configured", "transmissions", "airtime_minutes", "average_seconds", "per_day", "last_heard"})
	for _, r := range rows {
		avg := ""
		if r.Count > 0 {
			avg = strconv.FormatFloat(float64(r.AirMs)/float64(r.Count)/1000, 'f', 1, 64)
		}
		last := r.Last
		if last == "–" {
			last = ""
		}
		_ = cw.Write([]string{
			r.State, r.Group, r.Name, strconv.FormatBool(!r.Retired), strconv.Itoa(r.Count),
			strconv.FormatFloat(float64(r.AirMs)/60000, 'f', 1, 64), avg, r.PerDay, last,
		})
	}
	cw.Flush()
}

func addMap(dst, src map[string]int) {
	for k, v := range src {
		dst[k] += v
	}
}

func mapToKV(m map[string]int) []kv {
	out := make([]kv, 0, len(m))
	for k, v := range m {
		if k == "" {
			k = "(none)"
		}
		out = append(out, kv{Key: k, Value: v})
	}
	return out
}

// rank sorts descending, keeps the top n (0 = all), and sets bar widths.
func rank(list []kv, n int) []kv {
	sort.Slice(list, func(i, j int) bool {
		if list[i].Value != list[j].Value {
			return list[i].Value > list[j].Value
		}
		return list[i].Key < list[j].Key
	})
	if n > 0 && len(list) > n {
		list = list[:n]
	}
	if len(list) > 0 && list[0].Value > 0 {
		for i := range list {
			list[i].Pct = 100 * float64(list[i].Value) / float64(list[0].Value)
		}
	}
	return list
}

func median(v []int64) int64 {
	if len(v) == 0 {
		return -1
	}
	s := append([]int64(nil), v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

func fmtSeconds(sec int64) string {
	if sec < 0 {
		return "–"
	}
	return fmtDuration(sec * 1000)
}

func fmtDuration(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}

func shortStamp(s string) string {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Format("2006-01-02 15:04")
	}
	return s
}

func csvAttachment(w http.ResponseWriter, name string) *csv.Writer {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	return csv.NewWriter(w)
}

// exportEvents streams raw events in the range (only days still within the
// raw retention period are available).
func (a *analyticsStore) exportEvents(w http.ResponseWriter, days int) {
	from, to := a.rangeDates(days)
	cw := csvAttachment(w, "analytics-events-"+strings.TrimSpace(from+"_"+to)+".csv")
	_ = cw.Write([]string{"time", "type", "network", "visitor_day_hash", "page", "referrer", "browser", "os", "device", "stream", "duration_ms", "detail", "count", "state", "forest", "stream_name"})
	for _, date := range a.rawDates() {
		if (from != "" && date < from) || date > to {
			continue
		}
		_ = a.readEvents(date, func(ev analyticsEvent) {
			_ = cw.Write([]string{
				ev.Time.Format(time.RFC3339), ev.Type, ev.Net, ev.Visitor, ev.Page, ev.Referrer,
				ev.Browser, ev.OS, ev.Device, ev.Stream, strconv.FormatInt(ev.DurationMs, 10), ev.Detail, strconv.Itoa(ev.Count),
				ev.State, ev.Group, ev.Name,
			})
		})
	}
	cw.Flush()
}

// exportNetworks writes per-network totals with any location data, so raw
// networks can be geolocated with other sources.
func (a *analyticsStore) exportNetworks(w http.ResponseWriter, days int) {
	from, to := a.rangeDates(days)
	nets := mergeNetworks(a.daysInRange(days))
	keys := make([]string, 0, len(nets))
	for k := range nets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	cw := csvAttachment(w, "analytics-networks-"+strings.TrimSpace(from+"_"+to)+".csv")
	_ = cw.Write([]string{"network", "location_override", "geo_city", "geo_region", "geo_country", "visitors", "pageviews", "listens", "listen_minutes", "recording_plays", "first_seen", "last_seen"})
	for _, k := range keys {
		n, g := nets[k], a.geo.Lookup(k)
		_ = cw.Write([]string{
			k, g.Override, g.City, g.Region, g.Country,
			strconv.Itoa(n.Visitors), strconv.Itoa(n.Pageviews), strconv.Itoa(n.Listens),
			strconv.FormatFloat(float64(n.ListenMs)/60000, 'f', 1, 64), strconv.Itoa(n.Plays), n.FirstSeen, n.LastSeen,
		})
	}
	cw.Flush()
}
