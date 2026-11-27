package main

import (
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// issueActiveWindow is how recently a problem must have been logged to
	// count as active rather than historical.
	issueActiveWindow = 10 * time.Minute
	maxIssues         = 200
	maxIssueSample    = 300
	maxIssueKey       = 100
	// maxSubjects bounds the per-issue list of affected items, and
	// topSubjects is how many of them are sent to the page.
	maxSubjects = 500
	topSubjects = 8
)

type issueSeverity string

const (
	severityError issueSeverity = "error"
	severityWarn  issueSeverity = "warn"
)

// issueRule recognizes one kind of logged problem. Lines matching an
// ignore rule are never flagged, which keeps routine lines that merely
// contain a word like "failed" (e.g. "0 failed") out of the list.
type issueRule struct {
	id       string
	severity issueSeverity
	title    string
	hint     string
	match    *regexp.Regexp
	// key, when set, names what is affected (a drive, a server, a stream): its
	// first non-empty capture group, or the whole match. Problems with the
	// same rule and key are counted together.
	key    *regexp.Regexp
	ignore bool
	// aggregate folds every subject the key names into one issue that lists
	// them, instead of one issue per subject. filter is what "Show in log"
	// searches for in that case.
	aggregate bool
	filter    string
}

var logTimestampPrefix = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}(?:\.\d+)? `)

// pathRoot finds the drive letter or UNC share in a message, e.g. "B:" or
// `\\host\share`.
var pathRoot = regexp.MustCompile(`\b([A-Za-z]:)[\\/]|(\\\\+[^\\\s:]+\\+[^\\\s:]+)`)

var genericIssue = regexp.MustCompile(`(?i)\b(error|errors|failed|failure|fatal|panic|cannot|could not|unable to|timed out|unreachable|warning)\b`)

var severeIssue = regexp.MustCompile(`(?i)\b(ERROR:|fatal|panic)`)

var issueDigits = regexp.MustCompile(`\d+`)

var issueRules = []issueRule{
	{id: "stream-config-ok", ignore: true, match: regexp.MustCompile(`stream config applied: .*\b0 failed\b`)},
	{id: "track-write", ignore: true, match: regexp.MustCompile(`dropping .* after track write failure`)},
	{
		id: "listener-connection-failed", severity: severityWarn, title: "Listeners failing to connect",
		hint: "Browsers could not set up the WebRTC audio connection (connection state \"failed\") for the listed streams. A few failures are normal for individual viewers on restrictive networks, hotel/guest Wi-Fi or VPNs. If many listeners fail across several streams, the server side is usually the cause: allow inbound UDP on webrtcUdpPort through the firewall, set webrtcPublicIps when the server is behind NAT, and add a TURN server under iceServers for networks that block UDP.",
		match: regexp.MustCompile(`-peer-\d+ state: failed$`),
		key:   regexp.MustCompile(`^(.+?) \S+-peer-\d+ state: failed$`),

		aggregate: true,
		filter:    "state: failed",
	},
	{
		id: "udp-packet-dropped", severity: severityWarn, title: "UDP packets rejected as too short",
		hint:  "Packets reaching this stream's UDP port are smaller than one audio frame, so they are discarded. The sender is probably sending something other than the expected audio to this port (control or signalling traffic), or the stream's codec and skip_bytes settings do not match what the sender transmits. Check what the sender at that address transmits and the stream's codec settings. Correctly sized audio packets are unaffected.",
		match: regexp.MustCompile(`: dropping UDP packet from `),
		key:   regexp.MustCompile(`^(.+?): dropping UDP packet from (\[[^\]]+\]|[^\s:]+)`),
	},
	{
		id: "multiple-sources", severity: severityError, title: "Several senders on one UDP port",
		hint:  "Two or more devices are transmitting to this stream's UDP port, so their audio is interleaved and sounds garbled. Give each sender its own port, or stop the unexpected sender named in the message.",
		match: regexp.MustCompile(`is receiving packets from multiple sources`),
		key:   regexp.MustCompile(`WARNING: (.+? \(UDP \d+\)) is receiving`),
	},
	{
		id: "transcription-backlog", severity: severityWarn, title: "Transcription backlog",
		hint:  "Clips are waiting for whisper faster than they are transcribed (the queue is normally under 5). A large bulk transcription request does this too and clears by itself. Otherwise check that every whisper server is up (see Whisper server not responding) and not overloaded; add whisper workers or servers if it persists. If the queue fills, recordings are dropped.",
		match: regexp.MustCompile(`^whisper pool: queued clip .*\(queue depth: [1-9]\d+`),
	},
	{
		id: "whisper-transcribe-failed", severity: severityWarn, title: "Whisper failed on a clip",
		hint:  "The whisper server returned an error for a clip; the reason is in the message below. Occasional failures from bad audio are normal. The same error on every clip means the server is misconfigured, out of memory, or running the wrong model.",
		match: regexp.MustCompile(`^whisper \S.*: transcribe .+ \(clip [^)]+\): `),
		key:   regexp.MustCompile(`(?i)whisper ((?:server|local)#\d+(?: https?://[^\s/:]+(?::\d+)?)?)`),
	},
	{
		id: "whisper-restarting", severity: severityError, title: "Local whisper-server keeps stopping",
		hint:  "A whisper-server instance started by this program exited and is being restarted. Check the whisper.modelPath and serverArgs settings, free memory, and the message for the exit reason.",
		match: regexp.MustCompile(`^whisper local#\d+: .*; restarting in`),
		key:   regexp.MustCompile(`^whisper (local#\d+):`),
	},
	{
		id: "stream-config-failed", severity: severityWarn, title: "Some stream changes failed to apply",
		hint:  "Look just above this line for ERROR: lines naming the stream. Fix streams.json (ports, addresses) and the change is retried on the next reload.",
		match: regexp.MustCompile(`stream config applied: .*\b[1-9]\d* failed\b`),
	},
	{
		id: "disk-full", severity: severityError, title: "Disk full",
		hint:  "Free space on the drive named in the message. Recordings and transcripts are kept forever, so archive drives eventually fill.",
		match: regexp.MustCompile(`(?i)not enough space on the disk|no space left on device|disk is full`),
		key:   pathRoot,
	},
	{
		id: "archive-unreachable", severity: severityError, title: "Archive location unreachable or not writable",
		hint: `The account running the server cannot reach this drive or share. Mapped drive letters exist only in the logon session that created them, so a service or another user never sees them. Use a UNC path (\\host\share\audio) for audioBackupDir / audioBackupDir2 and store credentials for the server's account (cmdkey /add:host /user:... /pass), then restart. The archive sync fills in anything missed once the location is reachable.`,
		match: regexp.MustCompile(`(?i)(archive sync|transcript log replication|audio backup|logging to file)[^\n]*(cannot find the (path|drive|file) specified|network path was not found|network name is no longer available|logon failure|access is denied|permission denied|no such file or directory|not a directory)`),
		key:   pathRoot,
	},
	{
		id: "log-move", severity: severityWarn, title: "Old log file could not be moved",
		hint:  `Another process has the file open: a second copy of the server, or NSSM redirecting output into the app folder. Point NSSM's AppStdout/AppStderr into the logs folder (for example nssm set <service> AppStderr <app folder>\logs\service-err.log) or stop the other process, then restart.`,
		match: regexp.MustCompile(`log migration: could not move`),
		key:   regexp.MustCompile(`could not move (\S+?): `),
	},
	{
		id: "whisper-down", severity: severityError, title: "Whisper server not responding",
		hint:  "Check that the whisper-server at that address is running and reachable (whisper.remoteServers in config.json). Transcription is delayed until it recovers.",
		match: regexp.MustCompile(`(?i)whisper[^\n]*(unreachable|health check failed|is not ready|connection refused|no such host|i/o timeout)`),
		key:   regexp.MustCompile(`(?i)whisper (?:server )?((?:server|local)#\d+(?: https?://[^\s/:]+(?::\d+)?)?)`),
	},
	{
		id: "clip-queue-full", severity: severityError, title: "Recordings being dropped",
		hint:  "The clip queue is full, so finished clips are discarded. Usually the whisper servers are down or too slow, or an archive drive is slow or unreachable. Fix those first.",
		match: regexp.MustCompile(`clip job queue full`),
		key:   regexp.MustCompile(`^(.+?): clip job queue full`),
	},
	{
		id: "stream-start", severity: severityError, title: "Stream failed to start",
		hint:  "Most often the UDP port is already in use, for example another copy of g711-radio running (the service and a manual run at the same time), or the multicast address or interface is invalid. Check the ports in streams.json.",
		match: regexp.MustCompile(`ERROR: stream .* not started`),
		key:   regexp.MustCompile(`ERROR: stream "([^"]+)"`),
	},
	{
		id: "listener-read", severity: severityWarn, title: "UDP/multicast receive errors",
		hint:  "The network interface may be down or the multicast group membership lost. Check the NIC, cabling, multicast routing and firewall.",
		match: regexp.MustCompile(`read error on listener`),
		key:   regexp.MustCompile(`read error on listener \d+ \(([^)]+)\)`),
	},
	{
		id: "uplink-down", severity: severityWarn, title: "Edge uplink down",
		hint:  "This origin cannot reach its edge server. Check the uplink URL and token in config.json and that the edge is running; it keeps retrying.",
		match: regexp.MustCompile(`uplink \S+: .*(retrying every|disconnected after)`),
		key:   regexp.MustCompile(`uplink (\S+):`),
	},
	{
		id: "edge-sync", severity: severityWarn, title: "Edge stream list sync failing",
		hint:  "The edge cannot fetch the stream list from the origin. Check the uplink and network connectivity between them.",
		match: regexp.MustCompile(`edge: stream list sync failed`),
	},
	{
		id: "mping", severity: severityWarn, title: "Multicast ping problem",
		hint:  "Ping and peer tracking are not working. Check permissions, the network interface and that the logs folder is writable. Streaming and recording are unaffected.",
		match: regexp.MustCompile(`multicast ping: .*(disabled|cannot|could not|failed)`),
	},
	{
		id: "streams-file", severity: severityError, title: "Streams file problem",
		hint:  "The streams file could not be read or parsed, so the current streams stay active. Check its path and JSON syntax.",
		match: regexp.MustCompile(`stream config: cannot read`),
	},
}

// issue is one distinct problem seen in the log, with how often and when.
type issue struct {
	rule     *issueRule
	key      string
	filter   string
	severity issueSeverity
	title    string
	count    int
	first    time.Time
	last     time.Time
	sample   string
	// subjects counts, for aggregate rules, how often each affected item
	// (for example a stream) was named.
	subjects map[string]int
}

type issueSubject struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// issueView is the JSON form sent to the console page.
type issueView struct {
	ID       string        `json:"id"`
	Severity issueSeverity `json:"severity"`
	Title    string        `json:"title"`
	Key      string        `json:"key,omitempty"`
	Hint     string        `json:"hint,omitempty"`
	Count    int           `json:"count"`
	AgoSecs  int64         `json:"ago"`
	Active   bool          `json:"active"`
	Filter   string        `json:"filter"`
	Sample   string        `json:"sample"`
	// Subjects lists the most affected items of an aggregate issue;
	// SubjectTotal is how many distinct items there are.
	Subjects     []issueSubject `json:"subjects,omitempty"`
	SubjectTotal int            `json:"subjectTotal,omitempty"`
}

// logAnalyzer watches log lines as they are written and keeps a table of the
// distinct problems seen since the program started.
type logAnalyzer struct {
	now func() time.Time

	mu      sync.Mutex
	issues  map[string]*issue
	version uint64
}

func newLogAnalyzer() *logAnalyzer {
	return &logAnalyzer{now: time.Now, issues: map[string]*issue{}}
}

// Observe examines one log line (without its newline).
func (a *logAnalyzer) Observe(line string) {
	body := strings.TrimSpace(logTimestampPrefix.ReplaceAllString(line, ""))
	if body == "" {
		return
	}
	var rule *issueRule
	for i := range issueRules {
		r := &issueRules[i]
		if r.match.MatchString(body) {
			if r.ignore {
				return
			}
			rule = r
			break
		}
	}
	var (
		id, key, filter, title string
		sev                    issueSeverity
	)
	switch {
	case rule != nil:
		id, title, sev = rule.id, rule.title, rule.severity
		key, filter = issueKey(rule.key, body)
	case genericIssue.MatchString(body):
		id, title, sev = "other", "Other problem logged", severityWarn
		if severeIssue.MatchString(body) {
			sev = severityError
		}
		key = normalizeIssueText(body)
	default:
		return
	}
	if len(key) > maxIssueKey {
		key = key[:maxIssueKey]
	}
	subject := ""
	if rule != nil && rule.aggregate {
		subject, key, filter = key, "", rule.filter
	}
	if len(body) > maxIssueSample {
		body = body[:maxIssueSample] + "..."
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	mapKey := id + "|" + key
	it := a.issues[mapKey]
	if it == nil {
		if len(a.issues) >= maxIssues {
			a.evictOldestLocked()
		}
		it = &issue{rule: rule, key: key, filter: filter, severity: sev, title: title, first: now}
		a.issues[mapKey] = it
	}
	it.count++
	it.last = now
	it.sample = body
	if sev == severityError {
		it.severity = severityError
	}
	if subject != "" {
		if it.subjects == nil {
			it.subjects = map[string]int{}
		}
		if _, known := it.subjects[subject]; known || len(it.subjects) < maxSubjects {
			it.subjects[subject]++
		}
	}
	a.version++
}

func (a *logAnalyzer) evictOldestLocked() {
	var oldestKey string
	var oldest time.Time
	for k, it := range a.issues {
		if oldestKey == "" || it.last.Before(oldest) {
			oldestKey, oldest = k, it.last
		}
	}
	delete(a.issues, oldestKey)
}

// Version changes whenever the issue table does.
func (a *logAnalyzer) Version() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.version
}

// Snapshot lists the issues, active ones first, then by severity and how
// recently each was seen.
func (a *logAnalyzer) Snapshot() []issueView {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	out := make([]issueView, 0, len(a.issues))
	for mapKey, it := range a.issues {
		ago := now.Sub(it.last)
		v := issueView{
			ID:       mapKey,
			Severity: it.severity,
			Title:    it.title,
			Key:      it.key,
			Count:    it.count,
			AgoSecs:  int64(ago.Seconds()),
			Active:   ago <= issueActiveWindow,
			Filter:   it.filter,
			Sample:   it.sample,
		}
		if it.rule != nil {
			v.Hint = it.rule.hint
		}
		if v.Filter == "" {
			v.Filter = firstWords(it.sample, 40)
		}
		if len(it.subjects) > 0 {
			subs := make([]issueSubject, 0, len(it.subjects))
			for name, n := range it.subjects {
				subs = append(subs, issueSubject{name, n})
			}
			sort.Slice(subs, func(i, j int) bool {
				if subs[i].Count != subs[j].Count {
					return subs[i].Count > subs[j].Count
				}
				return subs[i].Name < subs[j].Name
			})
			v.SubjectTotal = len(subs)
			if len(subs) > topSubjects {
				subs = subs[:topSubjects]
			}
			v.Subjects = subs
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Active != out[j].Active {
			return out[i].Active
		}
		if out[i].Severity != out[j].Severity {
			return out[i].Severity == severityError
		}
		if out[i].AgoSecs != out[j].AgoSecs {
			return out[i].AgoSecs < out[j].AgoSecs
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func issueKey(re *regexp.Regexp, body string) (key, filter string) {
	if re == nil {
		return "", ""
	}
	m := re.FindStringSubmatch(body)
	if m == nil {
		return "", ""
	}
	var parts []string
	for _, g := range m[1:] {
		if g = strings.TrimSpace(g); g != "" {
			parts = append(parts, g)
		}
	}
	if len(parts) == 0 {
		whole := strings.TrimSpace(m[0])
		return whole, whole
	}
	return strings.Join(parts, " \u00B7 "), parts[0]
}

// normalizeIssueText makes repeats of the same message compare equal by
// masking the numbers in it.
func normalizeIssueText(s string) string {
	return strings.Join(strings.Fields(issueDigits.ReplaceAllString(s, "#")), " ")
}

func firstWords(s string, n int) string {
	if len(s) > n {
		s = s[:n]
	}
	return strings.TrimSpace(s)
}
