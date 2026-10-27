package main

import (
	"io"
	"log"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMulticastPingTargetsDedupes(t *testing.T) {
	stations := []*station{
		{cfg: streamConfig{MulticastAddrs: []string{"239.1.1.1", " 239.1.1.2", "239.1.1.1"}}},
		{cfg: streamConfig{MulticastAddr: "239.1.1.2"}},
		{cfg: streamConfig{MulticastAddrs: []string{"", "10.0.0.1", "not-an-ip", "239.1.1.3"}}},
		{cfg: streamConfig{UDPPort: 5000}},
	}
	got := multicastPingTargets(stations)
	want := []string{"239.1.1.1", "239.1.1.2", "239.1.1.3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("targets = %v, want %v", got, want)
	}
}

func TestMpingTrackerOrdersByLastHeard(t *testing.T) {
	tr := newMpingTracker(5*time.Minute, log.New(io.Discard, "", 0))
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	tr.heard("239.1.1.1", "10.0.0.1", t0)
	tr.heard("239.1.1.1", "10.0.0.2", t0.Add(10*time.Second))
	tr.heard("239.1.1.2", "10.0.0.3", t0.Add(20*time.Second))
	now := t0.Add(30 * time.Second)
	out := tr.render(now, []string{"239.1.1.1", "239.1.1.2", "239.1.1.9"})

	order := []string{"239.1.1.2", "10.0.0.3", "239.1.1.1", "10.0.0.2", "10.0.0.1", "239.1.1.9"}
	pos := -1
	for _, s := range order {
		i := strings.Index(out, s)
		if i <= pos {
			t.Fatalf("%q out of order in:\n%s", s, out)
		}
		pos = i
	}

	tr.sweep(t0.Add(10 * time.Minute))
	if out := tr.render(t0.Add(10*time.Minute), []string{"239.1.1.1"}); !strings.Contains(out, "STALE") {
		t.Errorf("expected STALE in:\n%s", out)
	}
}

func TestMulticastPingConfigNormalize(t *testing.T) {
	c := multicastPingConfig{Port: 9999}
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	if c.IntervalSeconds != defaultMulticastPingSeconds || c.TTL != defaultMulticastPingTTL {
		t.Errorf("defaults not applied: %+v", c)
	}
	for _, bad := range []multicastPingConfig{{}, {Port: 70000}, {Port: 1, IntervalSeconds: -1}, {Port: 1, TTL: 256}} {
		if err := bad.normalize(); err == nil {
			t.Errorf("%+v: expected error", bad)
		}
	}
}
