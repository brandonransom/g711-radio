package main

// Origin side of the origin↔edge link.
//
// The origin (the internal ingest server) dials OUT to each configured edge
// over HTTPS, so the internal network needs no inbound firewall rule. After
// an HTTP/1.1 Upgrade the roles reverse: the origin serves HTTP/2 on that
// connection and the edge sends requests through it. Only the paths in
// originEdgePaths are reachable that way, plus edgeLivePath, which streams a
// station's live µ-law frames for the edge to re-broadcast over WebRTC.

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"golang.org/x/net/http2"
)

const (
	uplinkPath     = "/_uplink"
	uplinkProtocol = "g711-uplink/1"
	edgeLivePath   = "/_edge/live"

	// minUplinkTokenLen keeps the shared secret long enough to be unguessable.
	minUplinkTokenLen = 24

	liveKeepaliveInterval = 15 * time.Second
	uplinkMaxBackoff      = 60 * time.Second
)

// originEdgePaths are the origin endpoints an edge may call. Everything else
// (notably the analytics dashboard) is unreachable through an edge.
var originEdgePaths = map[string]bool{
	"/streams":                  true,
	"/stream-status":            true,
	"/stream-activity":          true,
	"/transcripts":              true,
	"/transcripts/history":      true,
	"/transcripts/request":      true,
	"/transcripts/request-bulk": true,
	"/transcripts/feedback":     true,
	"/recordings/download":      true,
}

const edgeAudioPrefix = "/audio/"

// originServesEdgePath reports whether path may be requested through an edge.
// Uncleaned paths are refused so "/audio/../x" cannot reach other handlers.
func originServesEdgePath(p string) bool {
	clean := path.Clean(p)
	if clean != p && clean+"/" != p {
		return false
	}
	return originEdgePaths[p] || strings.HasPrefix(p, edgeAudioPrefix)
}

// uplinkConfig is one edge server an origin connects to.
type uplinkConfig struct {
	// URL is the edge's base URL, e.g. "https://radio.example.gov".
	URL string `json:"url"`
	// Token must equal the edge's edge.token. Keep it in config.secrets.json.
	Token string `json:"token"`
	// CAFile optionally names a PEM bundle used instead of the system roots
	// to verify the edge's certificate (e.g. a private CA).
	CAFile string `json:"caFile"`
	// ServerName overrides the TLS name checked on the edge's certificate.
	ServerName string `json:"serverName"`
	// Proxy is an optional "http://[user:pass@]host:port" forward proxy
	// (HTTP CONNECT). When empty, HTTPS_PROXY/NO_PROXY are honored.
	Proxy string `json:"proxy"`
}

func (u uplinkConfig) validate() error {
	target, err := url.Parse(u.URL)
	if err != nil {
		return fmt.Errorf("invalid url %q: %w", u.URL, err)
	}
	if target.Scheme != "https" || target.Host == "" {
		return fmt.Errorf("url %q must be an https:// URL", u.URL)
	}
	if target.Path != "" && target.Path != "/" {
		return fmt.Errorf("url %q must not have a path; use the edge's base URL", u.URL)
	}
	if len(strings.TrimSpace(u.Token)) < minUplinkTokenLen {
		return fmt.Errorf("token must be at least %d characters (keep it in config.secrets.json)", minUplinkTokenLen)
	}
	if u.Proxy != "" {
		p, err := url.Parse(u.Proxy)
		if err != nil || p.Scheme != "http" || p.Host == "" {
			return fmt.Errorf("proxy %q must be an http://host:port URL", u.Proxy)
		}
	}
	if u.CAFile != "" {
		if _, err := os.Stat(u.CAFile); err != nil {
			return fmt.Errorf("caFile: %w", err)
		}
	}
	return nil
}

// bufferedConn is a net.Conn whose reads go through r, so bytes already
// buffered while parsing an HTTP handshake are not lost.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// writeLiveFrame writes one length-prefixed live audio frame. An empty
// frame is a keepalive.
func writeLiveFrame(w io.Writer, frame []byte) error {
	if len(frame) > 0xFFFF {
		return fmt.Errorf("live frame too large: %d bytes", len(frame))
	}
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], uint16(len(frame)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(frame) == 0 {
		return nil
	}
	_, err := w.Write(frame)
	return err
}

// readLiveFrame reads one frame written by writeLiveFrame. The returned
// slice is newly allocated; it is empty for a keepalive.
func readLiveFrame(r io.Reader) ([]byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint16(hdr[:])
	if n == 0 {
		return nil, nil
	}
	frame := make([]byte, n)
	if _, err := io.ReadFull(r, frame); err != nil {
		return nil, err
	}
	return frame, nil
}

type edgeCtxKey struct{}

// viaEdge reports whether r arrived through an edge, i.e. from the internet.
func viaEdge(r *http.Request) bool {
	v, _ := r.Context().Value(edgeCtxKey{}).(bool)
	return v
}

// edgeHandler is what an edge reaches through the uplink: the allowlisted
// public endpoints (served by public) plus the live audio feed.
func (s *webrtcServer) edgeHandler(public http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(context.WithValue(r.Context(), edgeCtxKey{}, true))
		switch {
		case r.URL.Path == edgeLivePath:
			s.handleEdgeLive(w, r)
		case originServesEdgePath(r.URL.Path):
			public.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

// handleEdgeLive streams a station's broadcast frames to an edge until the
// edge hangs up or the station stops.
func (s *webrtcServer) handleEdgeLive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	st, ok := s.stationByID(r.URL.Query().Get("streamId"))
	if !ok {
		http.Error(w, "unknown stream", http.StatusNotFound)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	id, frames := st.addTap()
	defer st.removeTap(id)

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	keepalive := time.NewTicker(liveKeepaliveInterval)
	defer keepalive.Stop()
	for {
		var frame []byte
		select {
		case <-r.Context().Done():
			return
		case <-st.done:
			return
		case frame = <-frames:
		case <-keepalive.C:
		}
		if err := writeLiveFrame(w, frame); err != nil {
			return
		}
		flusher.Flush()
	}
}

// runUplink keeps a connection to one edge open until ctx ends, reconnecting
// with backoff.
func runUplink(ctx context.Context, u uplinkConfig, handler http.Handler, logger *log.Logger) {
	name := u.URL
	backoff := time.Second
	lastErr := ""
	for ctx.Err() == nil {
		conn, err := dialUplink(ctx, u)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// Log each distinct failure once, not every retry.
			if msg := err.Error(); msg != lastErr {
				logger.Printf("uplink %s: %v (retrying every %s or less)", name, err, uplinkMaxBackoff)
				lastErr = msg
			}
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, uplinkMaxBackoff)
			continue
		}
		lastErr = ""
		logger.Printf("uplink %s: connected", name)
		started := time.Now()
		serveUplinkConn(ctx, conn, handler)
		if ctx.Err() != nil {
			return
		}
		logger.Printf("uplink %s: disconnected after %s; reconnecting", name, time.Since(started).Round(time.Second))
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, uplinkMaxBackoff)
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// serveUplinkConn serves HTTP/2 to the edge over conn until it closes.
func serveUplinkConn(ctx context.Context, conn net.Conn, handler http.Handler) {
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-connCtx.Done()
		_ = conn.Close()
	}()
	srv := &http2.Server{
		MaxConcurrentStreams: 1000,
		ReadIdleTimeout:      30 * time.Second,
		PingTimeout:          15 * time.Second,
		WriteByteTimeout:     30 * time.Second,
	}
	srv.ServeConn(conn, &http2.ServeConnOpts{
		Context: connCtx,
		Handler: handler,
	})
}

// dialUplink connects to the edge (through a proxy if configured), verifies
// its certificate, and performs the upgrade handshake.
func dialUplink(ctx context.Context, u uplinkConfig) (net.Conn, error) {
	target, err := url.Parse(u.URL)
	if err != nil {
		return nil, err
	}
	port := target.Port()
	if port == "" {
		port = "443"
	}
	addr := net.JoinHostPort(target.Hostname(), port)

	tlsCfg := &tls.Config{
		ServerName: target.Hostname(),
		// HTTP/1.1 only, so the edge can take over the connection.
		NextProtos: []string{"http/1.1"},
		MinVersion: tls.VersionTLS12,
	}
	if u.ServerName != "" {
		tlsCfg.ServerName = u.ServerName
	}
	if u.CAFile != "" {
		pem, err := os.ReadFile(u.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read caFile: %w", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("caFile %s holds no PEM certificates", u.CAFile)
		}
		tlsCfg.RootCAs = roots
	}

	proxyURL, err := u.proxyFor(target)
	if err != nil {
		return nil, err
	}

	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	dialer := &net.Dialer{KeepAlive: 30 * time.Second}

	var raw net.Conn
	if proxyURL != nil {
		raw, err = dialViaProxy(dialCtx, dialer, proxyURL, addr)
	} else {
		raw, err = dialer.DialContext(dialCtx, "tcp", addr)
	}
	if err != nil {
		return nil, err
	}

	ok := false
	defer func() {
		if !ok {
			_ = raw.Close()
		}
	}()
	if deadline, has := dialCtx.Deadline(); has {
		_ = raw.SetDeadline(deadline)
	}

	tlsConn := tls.Client(raw, tlsCfg)
	if err := tlsConn.HandshakeContext(dialCtx); err != nil {
		return nil, fmt.Errorf("TLS handshake: %w", err)
	}

	host := target.Host
	if _, err := fmt.Fprintf(tlsConn,
		"GET %s HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: %s\r\nAuthorization: Bearer %s\r\nUser-Agent: g711-radio-origin\r\n\r\n",
		uplinkPath, host, uplinkProtocol, u.Token); err != nil {
		return nil, fmt.Errorf("send upgrade: %w", err)
	}
	br := bufio.NewReader(tlsConn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return nil, fmt.Errorf("read upgrade response: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("edge refused uplink: %s %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), uplinkProtocol) {
		return nil, fmt.Errorf("edge answered with unexpected protocol %q", resp.Header.Get("Upgrade"))
	}

	_ = raw.SetDeadline(time.Time{})
	ok = true
	return &bufferedConn{Conn: tlsConn, r: br}, nil
}

// proxyFor returns the forward proxy to reach target, or nil for none.
func (u uplinkConfig) proxyFor(target *url.URL) (*url.URL, error) {
	if u.Proxy != "" {
		return url.Parse(u.Proxy)
	}
	p, err := http.ProxyFromEnvironment(&http.Request{URL: target})
	if err != nil || p == nil {
		return nil, err
	}
	if p.Scheme != "http" {
		return nil, fmt.Errorf("unsupported proxy %s://%s from environment; set uplinks[].proxy to an http:// proxy", p.Scheme, p.Host)
	}
	return p, nil
}

func dialViaProxy(ctx context.Context, dialer *net.Dialer, proxyURL *url.URL, addr string) (net.Conn, error) {
	proxyAddr := proxyURL.Host
	if proxyURL.Port() == "" {
		proxyAddr = net.JoinHostPort(proxyURL.Hostname(), "80")
	}
	conn, err := dialer.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("dial proxy %s: %w", proxyAddr, err)
	}
	if deadline, has := ctx.Deadline(); has {
		_ = conn.SetDeadline(deadline)
	}
	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		Header: make(http.Header),
	}
	if proxyURL.User != nil {
		pass, _ := proxyURL.User.Password()
		cred := base64.StdEncoding.EncodeToString([]byte(proxyURL.User.Username() + ":" + pass))
		req.Header.Set("Proxy-Authorization", "Basic "+cred)
	}
	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("proxy CONNECT: %w", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("proxy CONNECT: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("proxy CONNECT to %s: %s", addr, resp.Status)
	}
	return &bufferedConn{Conn: conn, r: br}, nil
}

var errNoUplink = errors.New("radio origin is not connected")
