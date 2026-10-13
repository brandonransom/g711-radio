package main

// Edge server: a public-facing copy of the web page (e.g. an Azure
// Government VM) that has no radios of its own. The origin — the internal
// ingest server — connects out to it (see uplink.go); everything the edge
// shows comes through that one connection:
//
//   - the stream list is mirrored from the origin's /streams;
//   - live audio is pulled per stream from the origin only while at least one
//     edge listener is connected, and re-broadcast to edge listeners over the
//     edge's own WebRTC connections;
//   - live transcript/clip events are relayed from the origin's /transcripts
//     into a local hub, so every edge page shares one upstream stream;
//   - status/activity polls are answered from a short cache;
//   - history, recordings, downloads and transcription requests are proxied.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"golang.org/x/net/http2"
)

// edgeConfig turns a server into an edge. See README "Edge server".
type edgeConfig struct {
	// Token is the shared secret origins present (uplinks[].token). Keep it
	// in config.secrets.json as "edgeToken".
	Token string `json:"token"`
	// StreamsRefreshSeconds is how often the stream list is re-read from the
	// origin. 0 means the default (15).
	StreamsRefreshSeconds int `json:"streamsRefreshSeconds"`
	// FeedLingerSeconds keeps a stream's audio feed from the origin open this
	// long after its last edge listener leaves, so quick reconnects and
	// channel flipping don't churn the uplink. 0 means the default (30).
	FeedLingerSeconds int `json:"feedLingerSeconds"`
}

const (
	defaultEdgeStreamsRefresh = 15 * time.Second
	defaultEdgeFeedLinger     = 30 * time.Second
	edgeRetryDelay            = 2 * time.Second
	edgeUpstreamTimeout       = 15 * time.Second
	// edgeStallTimeout ends a streaming read from the origin that has gone
	// silent; the origin sends keepalives well inside it.
	edgeStallTimeout = 45 * time.Second
	// edgeOriginBase is a placeholder; requests travel over the uplink, not DNS.
	edgeOriginBase = "http://origin"
)

func (c edgeConfig) streamsRefresh() time.Duration {
	if c.StreamsRefreshSeconds > 0 {
		return time.Duration(c.StreamsRefreshSeconds) * time.Second
	}
	return defaultEdgeStreamsRefresh
}

func (c edgeConfig) feedLinger() time.Duration {
	if c.FeedLingerSeconds > 0 {
		return time.Duration(c.FeedLingerSeconds) * time.Second
	}
	return defaultEdgeFeedLinger
}

// validateEdgeConfig rejects origin-only settings on an edge, so a config
// mistake can't silently do nothing.
func validateEdgeConfig(c *appConfig) error {
	originOnly := []struct {
		name string
		set  bool
	}{
		{"states", len(c.States) > 0},
		{"regions", len(c.LegacyRegions) > 0},
		{"streamsFile", strings.TrimSpace(c.StreamsFile) != ""},
		{"whisper", c.Whisper != nil},
		{"audioLogDir", c.AudioLogDir != ""},
		{"audioBackupDir", c.AudioBackupDir != ""},
		{"audioBackupDir2", c.AudioBackupDir2 != ""},
		{"audioDumpDir", c.AudioDumpDir != ""},
		{"uplinks", len(c.Uplinks) > 0},
	}
	for _, o := range originOnly {
		if o.set {
			return fmt.Errorf("%q is an origin setting and cannot be used with \"edge\"; an edge gets streams, audio and transcripts from its origin", o.name)
		}
	}
	if len(strings.TrimSpace(c.Edge.Token)) < minUplinkTokenLen {
		return fmt.Errorf("edge.token must be at least %d characters (set \"edgeToken\" in config.secrets.json)", minUplinkTokenLen)
	}
	if c.Edge.StreamsRefreshSeconds < 0 || c.Edge.FeedLingerSeconds < 0 {
		return errors.New("edge.streamsRefreshSeconds and edge.feedLingerSeconds must not be negative")
	}
	return nil
}

// edgeTunnel is the edge's end of the uplink: an HTTP/2 client connection
// to the origin. A new origin connection replaces the old one.
type edgeTunnel struct {
	mu        sync.Mutex
	cc        *http2.ClientConn
	conn      net.Conn
	remote    string
	since     time.Time
	onAttach  func()
	transport *http2.Transport
}

func newEdgeTunnel(onAttach func()) *edgeTunnel {
	return &edgeTunnel{
		onAttach: onAttach,
		transport: &http2.Transport{
			ReadIdleTimeout: 30 * time.Second,
			PingTimeout:     15 * time.Second,
		},
	}
}

func (t *edgeTunnel) attach(conn net.Conn) error {
	cc, err := t.transport.NewClientConn(conn)
	if err != nil {
		return err
	}
	t.mu.Lock()
	old := t.conn
	t.cc, t.conn = cc, conn
	t.remote = conn.RemoteAddr().String()
	t.since = time.Now()
	t.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	if t.onAttach != nil {
		t.onAttach()
	}
	return nil
}

// RoundTrip sends req to the origin over the current uplink.
func (t *edgeTunnel) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	cc := t.cc
	t.mu.Unlock()
	if cc == nil {
		return nil, errNoUplink
	}
	if st := cc.State(); st.Closed || st.Closing {
		return nil, errNoUplink
	}
	return cc.RoundTrip(req)
}

// status reports whether an origin is connected, from where and since when.
func (t *edgeTunnel) status() (bool, string, time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cc == nil {
		return false, "", time.Time{}
	}
	if st := t.cc.State(); st.Closed || st.Closing {
		return false, "", time.Time{}
	}
	return true, t.remote, t.since
}

func (t *edgeTunnel) close() {
	t.mu.Lock()
	conn := t.conn
	t.cc, t.conn = nil, nil
	t.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

type edgeServer struct {
	cfg           edgeConfig
	ctx           context.Context
	logger        *log.Logger
	web           *webrtcServer
	hub           *transcriptHub
	analytics     *analyticsStore
	tunnel        *edgeTunnel
	client        *http.Client
	proxy         *httputil.ReverseProxy
	codec         webrtc.RTPCodecCapability
	frameDuration time.Duration
	syncNow       chan struct{}
	status        *cachedUpstream
	activity      *cachedUpstream
}

func newEdgeServer(ctx context.Context, config appConfig, api *webrtc.API, codec webrtc.RTPCodecCapability, frameDuration time.Duration, analytics *analyticsStore, logger *log.Logger) *edgeServer {
	// No log directory: transcripts are persisted on the origin only.
	hub := newTranscriptHub("", "", logger)
	e := &edgeServer{
		cfg:           *config.Edge,
		ctx:           ctx,
		logger:        logger,
		hub:           hub,
		analytics:     analytics,
		codec:         codec,
		frameDuration: frameDuration,
		syncNow:       make(chan struct{}, 1),
	}
	e.web = &webrtcServer{
		api:          api,
		logger:       logger,
		analytics:    analytics,
		hub:          hub,
		streams:      make(map[string]*station),
		streamsByKey: make(map[string]*station),
		clips:        make(map[string]clipRecord),
		iceServers:   config.webrtcICEServers(),
		runtime:      stationRuntime{ctx: ctx, codec: codec, frameDuration: frameDuration},
	}
	e.tunnel = newEdgeTunnel(e.requestSync)
	e.client = &http.Client{Transport: e.tunnel}

	origin, _ := url.Parse(edgeOriginBase)
	e.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(origin)
			// Lets the origin's analytics see the real visitor address.
			pr.SetXForwarded()
		},
		Transport:     e.tunnel,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if !errors.Is(err, context.Canceled) {
				logger.Printf("edge: %s %s: %v", r.Method, r.URL.Path, err)
			}
			http.Error(w, "radio origin unavailable", http.StatusBadGateway)
		},
	}
	e.status = &cachedUpstream{edge: e, path: "/stream-status", ttl: 5 * time.Second, maxStale: 30 * time.Second}
	e.activity = &cachedUpstream{edge: e, path: "/stream-activity", ttl: time.Second, maxStale: 10 * time.Second}
	return e
}

func (e *edgeServer) requestSync() {
	select {
	case e.syncNow <- struct{}{}:
	default:
	}
}

// Handler returns the edge's public HTTP routes.
func (e *edgeServer) Handler(staticFS fs.FS, dashboardPath string) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", e.analytics.PageviewMiddleware(http.FileServer(http.FS(staticFS))))
	mux.HandleFunc("/streams", e.web.handleStreams)
	mux.HandleFunc("/offer", e.web.handleOffer)
	mux.HandleFunc("/listen-leave", e.web.handleListenLeave)
	mux.Handle("/transcripts", e.hub)
	mux.Handle("/stream-status", e.status)
	mux.Handle("/stream-activity", e.activity)
	for _, p := range []string{"/transcripts/history", "/transcripts/request", "/transcripts/request-bulk", "/transcripts/feedback", "/recordings/download", edgeAudioPrefix} {
		mux.Handle(p, e.proxy)
	}
	mux.HandleFunc(uplinkPath, e.handleUplink)
	mux.HandleFunc("/healthz", e.handleHealth)
	if e.analytics != nil && dashboardPath != "" {
		e.analytics.RegisterDashboard(mux, dashboardPath, e.web.liveListenerCounts, e.web.streamInventory)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Visitors connect directly, so a client-supplied X-Forwarded-For
		// is never trustworthy here (getClientIP would believe it).
		r.Header.Del("X-Forwarded-For")
		mux.ServeHTTP(w, r)
	})
}

// Run starts the background loops that mirror the origin. They stop with ctx.
func (e *edgeServer) Run() {
	go e.syncLoop()
	go e.relayTranscripts()
}

func tokenMatches(got, want string) bool {
	g := sha256.Sum256([]byte(got))
	w := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(g[:], w[:]) == 1 && want != ""
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// handleUplink accepts an origin's connection and makes it the tunnel.
func (e *edgeServer) handleUplink(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") || !tokenMatches(strings.TrimPrefix(auth, "Bearer "), e.cfg.Token) {
		e.logger.Printf("edge: rejected uplink from %s: bad or missing token", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet || r.ProtoMajor != 1 ||
		!headerHasToken(r.Header, "Connection", "upgrade") ||
		!strings.EqualFold(r.Header.Get("Upgrade"), uplinkProtocol) {
		http.Error(w, "expected an "+uplinkProtocol+" upgrade over HTTP/1.1", http.StatusBadRequest)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "connection cannot be upgraded", http.StatusInternalServerError)
		return
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		e.logger.Printf("edge: uplink hijack: %v", err)
		return
	}
	_ = conn.SetDeadline(time.Time{})
	if _, err := rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + uplinkProtocol + "\r\n\r\n"); err == nil {
		err = rw.Flush()
	}
	if err != nil {
		_ = conn.Close()
		return
	}
	if err := e.tunnel.attach(&bufferedConn{Conn: conn, r: rw.Reader}); err != nil {
		e.logger.Printf("edge: uplink from %s: %v", conn.RemoteAddr(), err)
		_ = conn.Close()
		return
	}
	e.logger.Printf("edge: origin connected from %s", conn.RemoteAddr())
}

func (e *edgeServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	connected, _, since := e.tunnel.status()
	resp := struct {
		OriginConnected bool       `json:"originConnected"`
		ConnectedSince  *time.Time `json:"connectedSince,omitempty"`
		Streams         int        `json:"streams"`
	}{OriginConnected: connected, Streams: len(e.web.stationList())}
	if connected {
		resp.ConnectedSince = &since
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if !connected {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// originGet performs a GET to the origin through the uplink.
func (e *edgeServer) originGet(ctx context.Context, pathAndQuery string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, edgeOriginBase+pathAndQuery, nil)
	if err != nil {
		return nil, err
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		_ = resp.Body.Close()
		return nil, &originStatusError{code: resp.StatusCode, msg: strings.TrimSpace(string(body))}
	}
	return resp, nil
}

type originStatusError struct {
	code int
	msg  string
}

func (e *originStatusError) Error() string {
	return fmt.Sprintf("origin answered %d %s", e.code, e.msg)
}

// syncLoop mirrors the origin's stream list, on a timer and whenever the
// origin (re)connects.
func (e *edgeServer) syncLoop() {
	ticker := time.NewTicker(e.cfg.streamsRefresh())
	defer ticker.Stop()
	lastErr := ""
	for {
		if connected, _, _ := e.tunnel.status(); connected {
			err := e.syncStreams()
			msg := ""
			if err != nil {
				msg = err.Error()
			}
			if msg != lastErr && msg != "" {
				e.logger.Printf("edge: stream list sync failed: %v", err)
			}
			lastErr = msg
		}
		select {
		case <-e.ctx.Done():
			return
		case <-ticker.C:
		case <-e.syncNow:
		}
	}
}

func (e *edgeServer) syncStreams() error {
	ctx, cancel := context.WithTimeout(e.ctx, edgeUpstreamTimeout)
	defer cancel()
	resp, err := e.originGet(ctx, "/streams")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var groups []stateGroup
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&groups); err != nil {
		return fmt.Errorf("decode /streams: %w", err)
	}
	e.applyOriginStreams(groups)
	return nil
}

// applyOriginStreams makes the edge's stations match the origin's list.
// Stations are keyed by the origin's stream ID, which listeners use.
func (e *edgeServer) applyOriginStreams(groups []stateGroup) {
	s := e.web
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()

	desired := make(map[string]streamInfo)
	for gi := range groups {
		for si := range groups[gi].SubGroups {
			streams := groups[gi].SubGroups[si].Streams
			for i := range streams {
				// The origin's UDP ports are internal details.
				streams[i].UDPPort = 0
				desired[streams[i].ID] = streams[i]
			}
		}
	}

	s.streamsMu.Lock()
	var removed []*station
	for id, st := range s.streams {
		if info, ok := desired[id]; !ok || info != st.info {
			removed = append(removed, st)
			delete(s.streams, id)
			delete(s.streamsByKey, id)
		}
	}
	added := 0
	for id, info := range desired {
		if _, ok := s.streams[id]; ok {
			continue
		}
		st := e.startStation(info)
		s.streams[id] = st
		s.streamsByKey[id] = st
		added++
	}
	s.stateGroups = groups
	s.streamsMu.Unlock()

	for _, st := range removed {
		s.stopStation(st)
	}
	if added > 0 || len(removed) > 0 {
		e.logger.Printf("edge: stream list from origin: %d stream(s), %d added, %d removed", len(desired), added, len(removed))
	}
}

// startStation creates an edge station: a broadcaster for its WebRTC
// listeners, fed from the origin only while someone is listening.
func (e *edgeServer) startStation(info streamInfo) *station {
	ctx, cancel := context.WithCancel(e.ctx)
	st := &station{
		info:          info,
		codec:         e.codec,
		frameDuration: e.frameDuration,
		logger:        e.logger,
		subscribers:   make(map[string]*subscriber),
		broadcastChan: make(chan media.Sample, 64),
		subsChanged:   make(chan struct{}, 1),
		cancel:        cancel,
		done:          make(chan struct{}),
	}
	go st.runBroadcaster()
	go e.superviseStation(ctx, st)
	return st
}

// superviseStation starts the station's origin feed when its first
// listener joins and stops it feedLinger after the last one leaves. When
// ctx ends it tears the station down.
func (e *edgeServer) superviseStation(ctx context.Context, st *station) {
	var (
		feedCancel context.CancelFunc
		feedDone   chan struct{}
		linger     *time.Timer
		lingerC    <-chan time.Time
	)
	stopFeed := func() {
		if feedCancel != nil {
			feedCancel()
			<-feedDone
			feedCancel, feedDone = nil, nil
		}
	}
	stopLinger := func() {
		if linger != nil {
			linger.Stop()
			linger, lingerC = nil, nil
		}
	}
	defer func() {
		stopLinger()
		stopFeed()
		st.closeSubscribers()
		close(st.broadcastChan)
		close(st.done)
	}()

	for {
		if st.subscriberCount() > 0 {
			stopLinger()
			if feedCancel == nil {
				feedCtx, cancelFeed := context.WithCancel(ctx)
				feedCancel = cancelFeed
				feedDone = make(chan struct{})
				go func(done chan struct{}) {
					defer close(done)
					e.runFeed(feedCtx, st)
				}(feedDone)
			}
		} else if feedCancel != nil && linger == nil {
			linger = time.NewTimer(e.cfg.feedLinger())
			lingerC = linger.C
		}

		select {
		case <-ctx.Done():
			return
		case <-st.subsChanged:
		case <-lingerC:
			linger, lingerC = nil, nil
			if st.subscriberCount() == 0 {
				stopFeed()
			}
		}
	}
}

// runFeed pulls a station's live audio from the origin until ctx ends,
// reconnecting as needed.
func (e *edgeServer) runFeed(ctx context.Context, st *station) {
	failing := false
	for ctx.Err() == nil {
		err := e.pullFeed(ctx, st, func() {
			if failing {
				e.logger.Printf("edge: live feed for %s restored", st.info.displayName())
				failing = false
			}
		})
		if ctx.Err() != nil {
			return
		}
		if !failing {
			e.logger.Printf("edge: live feed for %s interrupted: %v; retrying", st.info.displayName(), err)
			failing = true
		}
		var se *originStatusError
		if errors.As(err, &se) && se.code == http.StatusNotFound {
			// The origin no longer has this stream (e.g. it restarted).
			e.requestSync()
		}
		if !sleepCtx(ctx, edgeRetryDelay) {
			return
		}
	}
}

func (e *edgeServer) pullFeed(ctx context.Context, st *station, onData func()) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	resp, err := e.originGet(ctx, edgeLivePath+"?streamId="+url.QueryEscape(st.info.ID))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	stall := time.AfterFunc(edgeStallTimeout, cancel)
	defer stall.Stop()
	r := bufio.NewReader(resp.Body)
	first := true
	for {
		frame, err := readLiveFrame(r)
		if err != nil {
			if ctx.Err() != nil && e.ctx.Err() == nil {
				return errors.New("origin feed stalled")
			}
			if err == io.EOF {
				return errors.New("origin ended the feed")
			}
			return err
		}
		stall.Reset(edgeStallTimeout)
		if first {
			onData()
			first = false
		}
		if len(frame) == 0 {
			continue
		}
		st.enqueueBroadcast(media.Sample{Data: frame, Duration: st.frameDuration})
	}
}

// relayTranscripts republishes the origin's live transcript/clip events to
// the edge's pages.
func (e *edgeServer) relayTranscripts() {
	failing := false
	for e.ctx.Err() == nil {
		err := e.pullTranscripts(func() {
			if failing {
				e.logger.Printf("edge: live transcript relay restored")
				failing = false
			}
		})
		if e.ctx.Err() != nil {
			return
		}
		if !failing && !errors.Is(err, errNoUplink) {
			e.logger.Printf("edge: live transcript relay interrupted: %v; retrying", err)
			failing = true
		}
		if !sleepCtx(e.ctx, edgeRetryDelay) {
			return
		}
	}
}

func (e *edgeServer) pullTranscripts(onConnected func()) error {
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	resp, err := e.originGet(ctx, "/transcripts")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	onConnected()

	// The origin pings every few seconds; silence means a dead link.
	stall := time.AfterFunc(edgeStallTimeout, cancel)
	defer stall.Stop()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		stall.Reset(edgeStallTimeout)
		line := sc.Text()
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var ev transcriptEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		e.hub.Publish(ev)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return errors.New("origin closed the transcript stream")
}

// cachedUpstream answers frequent polls (stream status/activity) from a
// short-lived copy of the origin's response, so page polling doesn't
// multiply across the uplink. Origin handlers ignore the query string.
type cachedUpstream struct {
	edge     *edgeServer
	path     string
	ttl      time.Duration
	maxStale time.Duration

	fetchMu sync.Mutex
	mu      sync.Mutex
	body    []byte
	fetched time.Time
}

func (c *cachedUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, ok := c.get()
	if !ok {
		http.Error(w, "radio origin unavailable", http.StatusBadGateway)
		return
	}
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func (c *cachedUpstream) cached(maxAge time.Duration) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.body != nil && time.Since(c.fetched) < maxAge {
		return c.body, true
	}
	return nil, false
}

func (c *cachedUpstream) get() ([]byte, bool) {
	if body, ok := c.cached(c.ttl); ok {
		return body, true
	}
	// One fetch at a time; waiters reuse its result.
	c.fetchMu.Lock()
	defer c.fetchMu.Unlock()
	if body, ok := c.cached(c.ttl); ok {
		return body, true
	}
	ctx, cancel := context.WithTimeout(c.edge.ctx, 5*time.Second)
	defer cancel()
	resp, err := c.edge.originGet(ctx, c.path)
	if err == nil {
		var body []byte
		body, err = io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		_ = resp.Body.Close()
		if err == nil {
			c.mu.Lock()
			c.body, c.fetched = body, time.Now()
			c.mu.Unlock()
			return body, true
		}
	}
	return c.cached(c.maxStale)
}

// runEdge runs this process as an edge server until ctx ends.
func runEdge(ctx context.Context, config appConfig, api *webrtc.API, codec webrtc.RTPCodecCapability, frameDuration time.Duration, analytics *analyticsStore, logger *log.Logger) {
	e := newEdgeServer(ctx, config, api, codec, frameDuration, analytics, logger)
	logICEServers(config, e.web.iceServers, logger)

	staticFS, err := fs.Sub(webFiles, "web")
	if err != nil {
		logger.Fatal(err)
	}
	dashboardPath := ""
	if analytics != nil {
		go analytics.Run(ctx)
		dashboardPath = config.Analytics.DashboardPath
		if dashboardPath != "" {
			logger.Printf("analytics: edge dashboard enabled at the dashboardPath set in %s (counts this edge's visitors only)", configPath)
		}
	}
	handler := e.Handler(staticFS, dashboardPath)
	e.Run()
	logger.Printf("edge mode: waiting for an origin to connect to %s (stream list refreshed every %s)", uplinkPath, e.cfg.streamsRefresh())
	serveHTTPS(ctx, config, handler, logger, e.tunnel.close)
}
