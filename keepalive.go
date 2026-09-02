package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"time"
)

type keepaliveConfig struct {
	Enabled    bool   `json:"enabled"`
	Port       int    `json:"port"`
	IntervalMs int    `json:"intervalMs"`
	BurstMs    int    `json:"burstMs"`
}

func (k *keepaliveConfig) setDefaults() {
	if k.Port == 0 {
		k.Port = 2600
	}
	if k.IntervalMs == 0 {
		k.IntervalMs = 115000
	}
	if k.BurstMs == 0 {
		k.BurstMs = 5000
	}
}

type keepaliveManager struct {
	config  *keepaliveConfig
	logger  *log.Logger
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	runners map[string]context.CancelFunc
}

func newKeepaliveManager(config *keepaliveConfig, logger *log.Logger) *keepaliveManager {
	if config == nil || !config.Enabled {
		return nil
	}
	config.setDefaults()
	ctx, cancel := context.WithCancel(context.Background())
	return &keepaliveManager{
		config:  config,
		logger:  logger,
		ctx:     ctx,
		cancel:  cancel,
		runners: make(map[string]context.CancelFunc),
	}
}

func (m *keepaliveManager) startForAddresses(addresses []string) error {
	if m == nil {
		return nil
	}
	seen := make(map[string]struct{})
	for _, addr := range addresses {
		if addr == "" {
			continue
		}
		if _, ok := seen[addr]; ok {
			continue
		}
		seen[addr] = struct{}{}
		m.start(addr)
	}
	return nil
}

func (m *keepaliveManager) start(addr string) {
	m.mu.Lock()
	if _, exists := m.runners[addr]; exists {
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.runners[addr] = cancel
	m.mu.Unlock()

	go m.run(ctx, addr)
}

func (m *keepaliveManager) run(ctx context.Context, addr string) {
	target := net.JoinHostPort(addr, fmt.Sprintf("%d", m.config.Port))
	dest, err := net.ResolveUDPAddr("udp4", target)
	if err != nil {
		m.logger.Printf("keepalive: resolve %s: %v", target, err)
		return
	}

	conn, err := net.DialUDP("udp4", nil, dest)
	if err != nil {
		m.logger.Printf("keepalive: dial %s: %v", target, err)
		return
	}
	defer conn.Close()

	payload := make([]byte, 160)
	burst := time.Duration(m.config.BurstMs) * time.Millisecond
	interval := time.Duration(m.config.IntervalMs) * time.Millisecond
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	m.logger.Printf("keepalive: started for %s on port %d (burst=%s interval=%s)", addr, m.config.Port, burst, interval)
	sendBurst := func() {
		deadline := time.Now().Add(burst)
		for time.Now().Before(deadline) {
			_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
			if _, err := conn.Write(payload); err != nil {
				m.logger.Printf("keepalive: write to %s failed: %v", target, err)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	sendBurst()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sendBurst()
		}
	}
}

func (m *keepaliveManager) close() {
	if m == nil {
		return
	}
	m.cancel()
	m.mu.Lock()
	m.runners = map[string]context.CancelFunc{}
	m.mu.Unlock()
}
