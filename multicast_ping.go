package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/ipv4"
)

const (
	defaultMulticastPingSeconds = 30
	defaultMulticastPingTTL     = 16
)

// multicastPingConfig is the optional "multicastPing" block in config.json.
// When present, the server periodically sends a small UDP datagram to every
// distinct multicast group used by the configured streams, so PIM/IGMP state
// (mroutes) along the path stays alive even when a group is quiet.
type multicastPingConfig struct {
	Port            int `json:"port"`            // destination UDP port of the pings (required)
	IntervalSeconds int `json:"intervalSeconds"` // 0 means the default (30s)
	TTL             int `json:"ttl"`             // IP multicast TTL; 0 means the default (16)

	// Peer tracking: pings from other devices on Port are logged and listed.
	PeerLogFile        string `json:"peerLogFile"`        // event log; default mping-peers.log
	PeerStatusFile     string `json:"peerStatusFile"`     // peers by group, newest first, rewritten every 2s; default mping-peers.txt
	PeerTimeoutSeconds int    `json:"peerTimeoutSeconds"` // silence after which a peer is marked stale; 0 means 300
	PeerWindow         *bool  `json:"peerWindow"`         // open a console window showing PeerStatusFile; default true on Windows only
}

func (c *multicastPingConfig) normalize() error {
	if c.PeerTimeoutSeconds < 0 {
		return fmt.Errorf("invalid peerTimeoutSeconds %d", c.PeerTimeoutSeconds)
	}
	if c.PeerTimeoutSeconds == 0 {
		c.PeerTimeoutSeconds = defaultMpingPeerTimeoutSec
	}
	if c.PeerLogFile == "" {
		c.PeerLogFile = defaultMpingPeerLog
	}
	if c.PeerStatusFile == "" {
		c.PeerStatusFile = defaultMpingPeerStatus
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("invalid port %d", c.Port)
	}
	if c.IntervalSeconds < 0 {
		return fmt.Errorf("invalid intervalSeconds %d", c.IntervalSeconds)
	}
	if c.IntervalSeconds == 0 {
		c.IntervalSeconds = defaultMulticastPingSeconds
	}
	if c.TTL < 0 || c.TTL > 255 {
		return fmt.Errorf("invalid ttl %d; must be 1-255", c.TTL)
	}
	if c.TTL == 0 {
		c.TTL = defaultMulticastPingTTL
	}
	return nil
}

// multicastPingTargets returns the distinct multicast group addresses used by
// the stations, sorted. Duplicates across streams and ports are dropped, and
// entries that are not IPv4 multicast addresses are ignored.
func multicastPingTargets(stations []*station) []string {
	seen := make(map[string]struct{})
	for _, st := range stations {
		addrs := st.cfg.MulticastAddrs
		if len(addrs) == 0 && st.cfg.MulticastAddr != "" {
			addrs = []string{st.cfg.MulticastAddr}
		}
		for _, raw := range addrs {
			ip := net.ParseIP(strings.TrimSpace(raw))
			if ip4 := ip.To4(); ip4 != nil && ip4.IsMulticast() {
				seen[ip4.String()] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for a := range seen {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// runMulticastPing sends a ping to every group in stations() immediately and
// then every configured interval, until ctx ends. stations is re-evaluated
// each round, so streams added or removed by a reload are picked up.
func runMulticastPing(ctx context.Context, cfg multicastPingConfig, stations func() []*station, logger *log.Logger) {
	conn, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		logger.Printf("multicast ping: disabled, cannot open socket: %v", err)
		return
	}
	defer conn.Close()
	pc := ipv4.NewPacketConn(conn)
	if err := pc.SetMulticastTTL(cfg.TTL); err != nil {
		logger.Printf("multicast ping: disabled, cannot set ttl %d: %v", cfg.TTL, err)
		return
	}
	_ = pc.SetMulticastLoopback(false)

	interval := time.Duration(cfg.IntervalSeconds) * time.Second
	logger.Printf("multicast ping: every %s to port %d, ttl %d", interval, cfg.Port, cfg.TTL)

	payload := []byte("g711-radio mping")
	failing := make(map[string]bool)
	send := func() {
		for _, addr := range multicastPingTargets(stations()) {
			dst := &net.UDPAddr{IP: net.ParseIP(addr), Port: cfg.Port}
			_, err := conn.WriteTo(payload, dst)
			switch {
			case err != nil && !failing[addr]:
				logger.Printf("multicast ping: send to %s failed: %v", dst, err)
				failing[addr] = true
			case err == nil && failing[addr]:
				logger.Printf("multicast ping: send to %s recovered", dst)
				delete(failing, addr)
			}
		}
	}

	send()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			send()
		}
	}
}
