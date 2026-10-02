package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStreamTimeZoneStats(t *testing.T) {
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2025, 3, 10, 9, 15, 0, 0, time.Local) // a Monday, server time
	clock := &fakeClock{t: start}
	dir := t.TempDir()
	a := newTestStore(t, dir, clock)
	jp := streamInfo{StateName: "Japan", GroupName: "G", StreamName: "Tokyo Net", TimeZone: "Asia/Tokyo"}
	local := streamInfo{StateName: "Utah", GroupName: "G", StreamName: "Server Net"}

	a.Transmission(jp, start, 90*60*1000) // 90 minutes: spans two local hours
	a.Transmission(local, start, 1000)
	a.ListenStart(testRequest(http.MethodPost, "/offer", "203.0.113.9"), jp.displayName(), jp.TimeZone, "p1")
	a.ListenConnected("p1")

	d := a.days[start.Format(dateLayout)]
	jwd, jh := weekHour(start.In(tokyo))
	swd, sh := weekHour(start)
	if d.TxLocal[jwd][jh] != 1 || d.TxLocal[swd][sh] < 1 {
		t.Fatalf("tokyo cell %d/%d and server cell %d/%d: %v", jwd, jh, swd, sh, d.TxLocal)
	}
	if got := d.TxLocalMs[jwd][jh]; got != 45*60*1000 {
		t.Fatalf("first local hour airtime = %d, want 45m", got)
	}
	nwd, nh := weekHour(start.In(tokyo).Add(time.Hour))
	if got := d.TxLocalMs[nwd][nh]; got != 45*60*1000 {
		t.Fatalf("second local hour airtime = %d, want 45m", got)
	}
	if d.ListenLocal[jwd][jh] != 1 {
		t.Fatalf("listen not filed in Tokyo time: %v", d.ListenLocal)
	}
	// Server-time hour totals are still kept.
	if d.TxHour[start.Hour()] != 2 {
		t.Fatalf("server TxHour = %v", d.TxHour)
	}

	rd := a.radioStats(a.daysInRange(1), 1, nil)
	if rd.HeatCount[jwd].Cells[jh].Value != 1 {
		t.Fatalf("heatmap not in local time: %+v", rd.HeatCount[jwd].Cells[jh])
	}
	data := a.dashboard(1, nil, nil)
	if data.HeatListens[jwd].Cells[jh].Value != 1 {
		t.Fatalf("listen heatmap not in local time")
	}

	// Replay after a restart files events the same way.
	a.Close()
	b := newTestStore(t, dir, clock)
	if got := b.days[start.Format(dateLayout)]; got.TxLocal != d.TxLocal || got.ListenLocal != d.ListenLocal {
		t.Fatal("replay disagrees with live aggregation")
	}
}

func TestLegacyDayFallsBackToServerTime(t *testing.T) {
	now := time.Date(2025, 3, 12, 12, 0, 0, 0, time.Local)
	a := newTestStore(t, t.TempDir(), &fakeClock{t: now})
	old := newDayAgg("2025-03-10") // a Monday rolled up before local times existed
	old.Radio["k"] = &txAgg{Name: "n", Count: 3, Ms: 3000}
	old.TxHour[7] = 3
	old.TxHourMs[7] = 3000
	old.HourListens[8] = 2
	a.mu.Lock()
	a.days[old.Date] = old
	a.mu.Unlock()

	rd := a.radioStats(a.daysInRange(7), 7, nil)
	if rd.HeatCount[0].Cells[7].Value != 3 || rd.BusiestHour != "07:00–08:00" {
		t.Fatalf("legacy radio fallback: cell=%+v busiest=%s", rd.HeatCount[0].Cells[7], rd.BusiestHour)
	}
	if data := a.dashboard(7, nil, nil); data.HeatListens[0].Cells[8].Value != 2 {
		t.Fatal("legacy listen fallback missing")
	}
}

func TestSpreadByWeekHourWrapsWeek(t *testing.T) {
	var cells [7][24]int64
	sunday := time.Date(2025, 3, 16, 23, 30, 0, 0, time.UTC)
	spreadByWeekHour(&cells, sunday, 60*60*1000)
	if cells[6][23] != 30*60*1000 || cells[0][0] != 30*60*1000 {
		t.Fatalf("Sunday 23h=%d Monday 0h=%d", cells[6][23], cells[0][0])
	}
}

func TestStreamTimeZoneConfigValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	good := map[string]map[string][]streamConfig{"Alaska": {"G": {{StreamName: "S", UDPPort: 5000, TimeZone: " America/Anchorage "}}}}
	states, _, err := normalizeStates(path, good)
	if err != nil {
		t.Fatal(err)
	}
	if tz := states[0].SubGroups[0].Streams[0].TimeZone; tz != "America/Anchorage" {
		t.Fatalf("timeZone = %q", tz)
	}
	bad := map[string]map[string][]streamConfig{"Alaska": {"G": {{StreamName: "S", UDPPort: 5000, TimeZone: "Alaska/Nowhere"}}}}
	if _, _, err := normalizeStates(path, bad); err == nil || !strings.Contains(err.Error(), "timeZone") {
		t.Fatalf("bad timeZone accepted: %v", err)
	}
}

func TestRadioArchiveRejectsOldVersion(t *testing.T) {
	now := time.Date(2025, 3, 12, 12, 0, 0, 0, time.Local)
	a := newTestStore(t, t.TempDir(), &fakeClock{t: now})
	old := `{"cutoff":"2025-03-12T12:00:00Z","days":{}}`
	if err := os.WriteFile(a.radioArchivePath(), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := a.readRadioArchive(); err == nil {
		t.Fatal("version-1 archive accepted; it would never be rescanned with time zones")
	}
	if err := a.writeRadioArchive(&radioArchiveFile{Version: radioArchiveVersion, Cutoff: now, Days: map[string]radioDay{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.readRadioArchive(); err != nil {
		t.Fatalf("current archive rejected: %v", err)
	}
}
