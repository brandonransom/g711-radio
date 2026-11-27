package main

import (
	"fmt"
	"testing"
	"time"
)

func findIssue(t *testing.T, list []issueView, id string) issueView {
	t.Helper()
	for _, v := range list {
		if v.ID == id {
			return v
		}
	}
	t.Fatalf("issue %q not found in %+v", id, list)
	return issueView{}
}

func TestAnalyzerFlagsKnownProblems(t *testing.T) {
	a := newLogAnalyzer()
	for _, line := range []string{
		`2026/11/27 12:52:50 archive sync: error: archive sync: mkdir B:\audio: mkdir B:\audio: The system cannot find the path specified.`,
		`2026/11/27 12:52:38 transcript log replication: transcripts\Sitka.log -> B:\audio\transcript-logs\Sitka.log: mkdir B:\audio\transcript-logs: mkdir B:\audio: The system cannot find the path specified.`,
		`2026/11/27 12:52:38 transcript log replication: transcripts\West.log -> B:\audio\transcript-logs\West.log: mkdir B:\audio\transcript-logs: mkdir B:\audio: The system cannot find the path specified.`,
		`2026/11/27 12:53:00 audio backup: write \\199.131.68.16\Audio02\audio\x.wav: Access is denied.`,
		`2026/11/27 12:54:00 whisper server#2 http://166.3.222.222:8091: unreachable, requeueing clip abc: dial tcp: connection refused`,
		`2026/11/27 12:54:01 WARNING: whisper server server#2 http://166.3.222.222:8091 is not ready: timeout`,
		`2026/11/27 12:55:00 Sitka: clip job queue full; dropping clip finalization (disk/whisper backlog)`,
		`2026/11/27 12:56:00 ERROR: stream "Yampa" in state "CO", forest "WRNF" not started: listen udp :2250: bind: address already in use`,
		`2026/11/27 12:57:00 log migration: could not move service-out.log: The process cannot access the file because it is being used by another process.`,
	} {
		a.Observe(line)
	}
	list := a.Snapshot()

	arch := findIssue(t, list, "archive-unreachable|B:")
	if arch.Count != 3 || arch.Severity != severityError || !arch.Active || arch.Hint == "" || arch.Filter != "B:" {
		t.Errorf("archive B: = %+v", arch)
	}
	findIssue(t, list, `archive-unreachable|\\199.131.68.16\Audio02`)
	if w := findIssue(t, list, "whisper-down|server#2 http://166.3.222.222:8091"); w.Count != 2 {
		t.Errorf("whisper = %+v", w)
	}
	findIssue(t, list, "clip-queue-full|Sitka")
	findIssue(t, list, "stream-start|Yampa")
	findIssue(t, list, "log-move|service-out.log")
}

func TestAnalyzerIgnoresRoutineLines(t *testing.T) {
	a := newLogAnalyzer()
	for _, line := range []string{
		"2026/11/27 12:00:00 stream config applied: 1 added, 0 removed, 0 restarted with changes, 0 failed, 12 unchanged",
		"2026/11/27 12:00:01 Sitka: dropping abc after track write failure: io: closed pipe",
		"2026/11/27 12:00:02 archive sync: copied 2026/a.wav -> B:\\audio",
		"2026/11/27 12:00:03 loaded 40 stream(s) from streams.json",
		"2026/11/27 12:00:04 analytics: dashboard disabled; set analytics.dashboardPath in config.json",
		"",
	} {
		a.Observe(line)
	}
	if list := a.Snapshot(); len(list) != 0 {
		t.Fatalf("expected no issues, got %+v", list)
	}
	a.Observe("2026/11/27 12:00:05 stream config applied: 1 added, 0 removed, 0 restarted with changes, 2 failed, 12 unchanged")
	findIssue(t, a.Snapshot(), "stream-config-failed|")
}

func TestAnalyzerGroupsGenericProblems(t *testing.T) {
	a := newLogAnalyzer()
	a.Observe("2026/11/27 12:00:00 uploader: request 17 failed: EOF")
	a.Observe("2026/11/27 12:00:01 uploader: request 18 failed: EOF")
	a.Observe("2026/11/27 12:00:02 ERROR: something exploded")
	list := a.Snapshot()
	if len(list) != 2 {
		t.Fatalf("got %+v", list)
	}
	if list[0].Severity != severityError || list[0].Filter != "ERROR: something exploded" {
		t.Errorf("errors should sort first with a usable filter: %+v", list[0])
	}
	if list[1].Count != 2 || list[1].Severity != severityWarn || list[1].Hint != "" {
		t.Errorf("repeats should group: %+v", list[1])
	}
}

func TestAnalyzerRecurringRealWorldMessages(t *testing.T) {
	a := newLogAnalyzer()
	for _, line := range []string{
		"2026/09/01 20:22:43 Revillagigedo Net stream-11-peer-4 state: failed",
		"2026/09/01 20:23:16 Revillagigedo Net stream-11-peer-5 state: failed",
		"2026/09/01 20:23:27 Juneau Net - Multicast stream-5-peer-7 state: failed",
		"2026/09/01 20:23:30 Juneau Net - Multicast stream-5-peer-8 state: failed",
		"2026/09/01 20:23:31 Juneau Net - Multicast stream-5-peer-9 state: disconnected",
		"2026/09/02 14:46:04 Revilla Net - DFSI Multicast: dropping UDP packet from 199.131.93.210:41018 on listener 0: packet is 16 bytes, need at least 172",
		"2026/09/02 14:46:20 Revilla Net - DFSI Multicast: dropping UDP packet from 199.131.93.210:41018 on listener 0: packet is 16 bytes, need at least 172",
		"2026/09/02 15:00:00 WARNING: Silence (UDP 9999) is receiving packets from multiple sources: expected 10.0.0.1, also receiving from 10.0.0.2 — this will cause audio problems",
		"2026/09/02 15:01:00 whisper pool: queued clip abc from Forest Net (queue depth: 3, servers: 3)",
		"2026/09/02 15:01:01 whisper pool: queued clip def from Forest Net (queue depth: 14, servers: 3)",
		"2026/09/02 15:02:00 whisper server#1 http://166.3.222.222:8090: transcribe Forest Net (clip def): whisper returned HTTP 500",
		"2026/09/02 15:03:00 transcribe: bulk request from 1.2.3.4: 50 queued, 0 already queued, 0 not found, 0 over capacity (queue depth: 50)",
	} {
		a.Observe(line)
	}
	list := a.Snapshot()
	if len(list) != 5 {
		t.Fatalf("expected 5 issues, got %d: %+v", len(list), list)
	}

	peers := findIssue(t, list, "listener-connection-failed|")
	if peers.Count != 4 || peers.SubjectTotal != 2 || len(peers.Subjects) != 2 || peers.Filter != "state: failed" {
		t.Errorf("peer failures should fold into one issue listing the streams: %+v", peers)
	}
	names := map[string]int{}
	for _, s := range peers.Subjects {
		names[s.Name] = s.Count
	}
	if names["Revillagigedo Net"] != 2 || names["Juneau Net - Multicast"] != 2 {
		t.Errorf("subjects = %+v", peers.Subjects)
	}

	udp := findIssue(t, list, "udp-packet-dropped|Revilla Net - DFSI Multicast \u00B7 199.131.93.210")
	if udp.Count != 2 || udp.Filter != "Revilla Net - DFSI Multicast" {
		t.Errorf("udp drop = %+v", udp)
	}
	if m := findIssue(t, list, "multiple-sources|Silence (UDP 9999)"); m.Severity != severityError {
		t.Errorf("multiple sources = %+v", m)
	}
	if b := findIssue(t, list, "transcription-backlog|"); b.Count != 1 {
		t.Errorf("only a queue depth of 10+ is a backlog: %+v", b)
	}
	findIssue(t, list, "whisper-transcribe-failed|server#1 http://166.3.222.222:8090")
}

func TestAnalyzerActivityAndEviction(t *testing.T) {
	a := newLogAnalyzer()
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	a.Observe(`archive sync: error: mkdir B:\audio: The system cannot find the path specified.`)
	now = now.Add(issueActiveWindow + time.Minute)
	a.Observe("ERROR: fresh problem")
	list := a.Snapshot()
	if !list[0].Active || list[0].Title != "Other problem logged" || list[1].Active {
		t.Fatalf("recent issue should be active and sort first: %+v", list)
	}

	before := a.Version()
	for i := 0; i < maxIssues+20; i++ {
		a.Observe(fmt.Sprintf("failed to frobnicate widget%c%c", 'a'+byte(i/26%26), 'a'+byte(i%26)))
	}
	if n := len(a.Snapshot()); n != maxIssues {
		t.Fatalf("issue table should be capped at %d, got %d", maxIssues, n)
	}
	if a.Version() == before {
		t.Fatal("version should advance as issues are recorded")
	}
}
