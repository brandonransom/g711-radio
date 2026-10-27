package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/ipv4"
)

const (
	defaultMpingPeerLog        = "mping-peers.log"
	defaultMpingPeerStatus     = "mping-peers.txt"
	defaultMpingPeerTimeoutSec = 300
	mpingPeerTick              = 2 * time.Second
)

// mpingPeer is one remote device heard pinging a multicast group.
type mpingPeer struct {
	first, last time.Time
	count       int
	stale       bool
}

// mpingTracker records which peers have been heard on which groups.
type mpingTracker struct {
	mu      sync.Mutex
	groups  map[string]map[string]*mpingPeer // group -> peer IP -> state
	timeout time.Duration
	log     *log.Logger
}

func newMpingTracker(timeout time.Duration, logger *log.Logger) *mpingTracker {
	return &mpingTracker{groups: make(map[string]map[string]*mpingPeer), timeout: timeout, log: logger}
}

func (t *mpingTracker) heard(group, ip string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	peers := t.groups[group]
	if peers == nil {
		peers = make(map[string]*mpingPeer)
		t.groups[group] = peers
	}
	p := peers[ip]
	switch {
	case p == nil:
		p = &mpingPeer{first: now}
		peers[ip] = p
		t.log.Printf("NEW      group %s peer %s", group, ip)
	case p.stale:
		t.log.Printf("RESUMED  group %s peer %s after %s of silence", group, ip, now.Sub(p.last).Round(time.Second))
		p.stale = false
	}
	p.last = now
	p.count++
}

// sweep marks peers not heard within the timeout as stale, logging each once.
func (t *mpingTracker) sweep(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for group, peers := range t.groups {
		for ip, p := range peers {
			if !p.stale && now.Sub(p.last) > t.timeout {
				p.stale = true
				t.log.Printf("SILENT   group %s peer %s last heard %s ago", group, ip, now.Sub(p.last).Round(time.Second))
			}
		}
	}
}

// render lists the active groups, most recently heard first, each with its
// peers most recently heard first. Groups with no peers come last.
func (t *mpingTracker) render(now time.Time, active []string) string {
	t.mu.Lock()
	defer t.mu.Unlock()

	type groupView struct {
		addr  string
		last  time.Time
		peers []string
		ips   map[string]*mpingPeer
	}
	views := make([]groupView, 0, len(active))
	for _, g := range active {
		v := groupView{addr: g, ips: t.groups[g]}
		for ip, p := range v.ips {
			v.peers = append(v.peers, ip)
			if p.last.After(v.last) {
				v.last = p.last
			}
		}
		sort.Slice(v.peers, func(i, j int) bool {
			a, b := v.ips[v.peers[i]], v.ips[v.peers[j]]
			if !a.last.Equal(b.last) {
				return a.last.After(b.last)
			}
			return v.peers[i] < v.peers[j]
		})
		views = append(views, v)
	}
	sort.Slice(views, func(i, j int) bool {
		if !views[i].last.Equal(views[j].last) {
			return views[i].last.After(views[j].last)
		}
		return views[i].addr < views[j].addr
	})

	var b strings.Builder
	fmt.Fprintf(&b, "mping peers - updated %s - peers silent for more than %s are marked STALE\n\n",
		now.Format("2006-01-02 15:04:05"), t.timeout)
	for _, v := range views {
		fmt.Fprintf(&b, "%s\n", v.addr)
		if len(v.peers) == 0 {
			b.WriteString("    no peers heard\n\n")
			continue
		}
		for _, ip := range v.peers {
			p := v.ips[ip]
			state := "ok"
			if p.stale {
				state = "STALE"
			}
			fmt.Fprintf(&b, "    %-39s last heard %-9s ago (%s)  pings %-6d first heard %s  %s\n",
				ip, now.Sub(p.last).Round(time.Second), p.last.Format("15:04:05"),
				p.count, p.first.Format("2006-01-02 15:04:05"), state)
		}
		b.WriteString("\n")
	}
	if len(views) == 0 {
		b.WriteString("no multicast groups configured\n")
	}
	return b.String()
}

func localIPSet() map[string]bool {
	set := make(map[string]bool)
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			set[ipn.IP.String()] = true
		}
	}
	return set
}

func openMpingListener(ctx context.Context, group string, port int) (net.PacketConn, error) {
	lc := net.ListenConfig{Control: func(network, address string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) { serr = setMpingSockopts(fd) }); err != nil {
			return err
		}
		return serr
	}}
	pc, err := lc.ListenPacket(ctx, "udp4", fmt.Sprintf(":%d", port))
	if err != nil {
		return nil, fmt.Errorf("listen on UDP port %d: %w", port, err)
	}
	p := ipv4.NewPacketConn(pc)
	if err := p.JoinGroup(nil, &net.UDPAddr{IP: net.ParseIP(group)}); err != nil {
		pc.Close()
		return nil, fmt.Errorf("join %s: %w", group, err)
	}
	return pc, nil
}

// runMulticastPingPeers listens on the ping port for every multicast group
// in stations(), tracks which peers ping each group, writes events to the
// peer log and a periodically rewritten status file, and optionally opens a
// console window showing that status file.
func runMulticastPingPeers(ctx context.Context, cfg multicastPingConfig, stations func() []*station, logger *log.Logger) {
	logFile, err := os.OpenFile(cfg.PeerLogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		logger.Printf("multicast ping: peer tracking disabled, cannot open %s: %v", cfg.PeerLogFile, err)
		return
	}
	defer logFile.Close()
	tracker := newMpingTracker(time.Duration(cfg.PeerTimeoutSeconds)*time.Second,
		log.New(logFile, "", log.Ldate|log.Ltime))
	statusPath, err := filepath.Abs(cfg.PeerStatusFile)
	if err != nil {
		statusPath = cfg.PeerStatusFile
	}
	logger.Printf("multicast ping: tracking peers on port %d; log %s, status %s", cfg.Port, cfg.PeerLogFile, statusPath)

	local := localIPSet()
	conns := make(map[string]net.PacketConn)
	failing := make(map[string]bool)
	var wg sync.WaitGroup
	defer func() {
		for _, c := range conns {
			c.Close()
		}
		wg.Wait()
	}()

	read := func(group string, conn net.PacketConn) {
		defer wg.Done()
		buf := make([]byte, 1500)
		for {
			_, src, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			if ua, ok := src.(*net.UDPAddr); ok && !local[ua.IP.String()] {
				tracker.heard(group, ua.IP.String(), time.Now())
			}
		}
	}

	write := func(active []string) {
		_ = os.WriteFile(statusPath, []byte(tracker.render(time.Now(), active)), 0o644)
	}

	var windowStarted bool
	update := func() {
		targets := multicastPingTargets(stations())
		want := make(map[string]bool, len(targets))
		for _, g := range targets {
			want[g] = true
			if _, ok := conns[g]; ok {
				continue
			}
			conn, err := openMpingListener(ctx, g, cfg.Port)
			if err != nil {
				if !failing[g] {
					logger.Printf("multicast ping: cannot receive peer pings for %s: %v", g, err)
					failing[g] = true
				}
				continue
			}
			delete(failing, g)
			conns[g] = conn
			wg.Add(1)
			go read(g, conn)
		}
		for g, c := range conns {
			if !want[g] {
				c.Close()
				delete(conns, g)
			}
		}
		tracker.sweep(time.Now())
		write(targets)
		if !windowStarted && cfg.openPeerWindow() {
			windowStarted = true
			if err := openPeerWindow(ctx, statusPath); err != nil {
				logger.Printf("multicast ping: cannot open peer window: %v", err)
			}
		}
	}

	update()
	ticker := time.NewTicker(mpingPeerTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			update()
		}
	}
}

// openPeerWindow reports whether to open the console window: an explicit
// peerWindow setting wins, otherwise it opens on Windows only.
func (c multicastPingConfig) openPeerWindow() bool {
	if c.PeerWindow != nil {
		return *c.PeerWindow
	}
	return runtime.GOOS == "windows"
}
