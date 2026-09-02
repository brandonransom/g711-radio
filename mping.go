package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
)

// mpingConfig holds configuration for multicast route keep-alive using mping
type mpingConfig struct {
	Enabled           bool   `json:"enabled"`
	ExecutablePath    string `json:"executablePath"`    // Path to mping.exe
	CommandPath       string `json:"commandPath"`        // Working directory for the mping process
	Port              int    `json:"port"`               // Port for mping (typically 5750 or similar)
	TTL               int    `json:"ttl"`                // Time to live (default 32)
	IntervalMs        int    `json:"intervalMs"`         // Milliseconds between pings (default 1000)
}

// setDefaults applies default values for mping configuration
func (m *mpingConfig) setDefaults() {
	if m.ExecutablePath == "" {
		m.ExecutablePath = "mping.exe"
	}
	if m.CommandPath == "" {
		m.CommandPath = filepath.Dir(m.ExecutablePath)
	}
	if m.Port == 0 {
		m.Port = 5750
	}
	if m.TTL == 0 {
		m.TTL = 32
	}
	if m.IntervalMs == 0 {
		m.IntervalMs = 1000
	}
}

// mpingManager maintains mping processes for multicast addresses
type mpingManager struct {
	config    *mpingConfig
	logger    *log.Logger
	processes map[string]*exec.Cmd // keyed by multicast address
	mu        sync.Mutex
	ctx       context.Context
	cancel    context.CancelFunc
}

// newMpingManager creates a new mping manager
func newMpingManager(config *mpingConfig, logger *log.Logger) *mpingManager {
	if config == nil || !config.Enabled {
		return nil
	}

	config.setDefaults()

	ctx, cancel := context.WithCancel(context.Background())
	return &mpingManager{
		config:    config,
		logger:    logger,
		processes: make(map[string]*exec.Cmd),
		ctx:       ctx,
		cancel:    cancel,
	}
}

// startMping starts an mping process for a multicast address
func (m *mpingManager) startMping(multicastAddr string) error {
	if m == nil || multicastAddr == "" {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if already running
	if _, exists := m.processes[multicastAddr]; exists {
		return nil
	}

	// Build mping command line
	// Default to sender mode (-s flag)
	// Format: mping.exe -s -a <multicast_addr> -p <port> -t <ttl> -i <interval>
	args := []string{
		"-s", // Sender mode
		"-a", multicastAddr,
		"-p", strconv.Itoa(m.config.Port),
		"-t", strconv.Itoa(m.config.TTL),
		"-i", strconv.Itoa(m.config.IntervalMs),
	}

	exeDir := m.config.CommandPath
	if exeDir == "" {
		exeDir = "."
	}
	if _, err := os.Stat(m.config.ExecutablePath); err != nil {
		return fmt.Errorf("mping executable not found at %s: %w", m.config.ExecutablePath, err)
	}
	if info, err := os.Stat(exeDir); err != nil || !info.IsDir() {
		return fmt.Errorf("mping command path is not a directory: %s", exeDir)
	}
	m.logger.Printf("starting mping as a process in %s: %s", exeDir, m.config.ExecutablePath)
	cmd := exec.CommandContext(m.ctx, m.config.ExecutablePath, args...)
	cmd.Dir = exeDir
	cmd.Stdout = nil // Suppress stdout
	cmd.Stderr = nil // Suppress stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start mping for %s: %w", multicastAddr, err)
	}

	m.processes[multicastAddr] = cmd
	m.logger.Printf("started mping keep-alive for multicast %s on port %d (interval %dms, ttl %d)",
		multicastAddr, m.config.Port, m.config.IntervalMs, m.config.TTL)

	// Monitor process in background
	go func(addr string, process *exec.Cmd) {
		_ = process.Wait()
		m.mu.Lock()
		delete(m.processes, addr)
		m.mu.Unlock()
		m.logger.Printf("mping process for %s has exited", addr)
	}(multicastAddr, cmd)

	return nil
}

// stopMping stops an mping process for a multicast address
func (m *mpingManager) stopMping(multicastAddr string) {
	if m == nil || multicastAddr == "" {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if cmd, exists := m.processes[multicastAddr]; exists {
		// Kill the process
		if err := cmd.Process.Kill(); err != nil {
			m.logger.Printf("error killing mping for %s: %v", multicastAddr, err)
		} else {
			m.logger.Printf("stopped mping keep-alive for multicast %s", multicastAddr)
		}
		delete(m.processes, multicastAddr)
	}
}

// startMulticastKeepAlive starts mping keep-alive for all unique multicast addresses in a stream
func (m *mpingManager) startMulticastKeepAlive(addresses []string) error {
	if m == nil {
		return nil
	}

	// Track unique non-empty multicast addresses
	seen := make(map[string]bool)
	var errs []error

	for _, addr := range addresses {
		if addr == "" {
			// Empty string means unicast, skip
			continue
		}
		if !seen[addr] {
			seen[addr] = true
			if err := m.startMping(addr); err != nil {
				errs = append(errs, err)
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("errors starting mping: %v", errs)
	}
	return nil
}

// stopAllMping stops all mping processes
func (m *mpingManager) stopAllMping() {
	if m == nil {
		return
	}

	m.mu.Lock()
	addrs := make([]string, 0, len(m.processes))
	for addr := range m.processes {
		addrs = append(addrs, addr)
	}
	m.mu.Unlock()

	for _, addr := range addrs {
		m.stopMping(addr)
	}
}

// close shuts down the mping manager
func (m *mpingManager) close() {
	if m == nil {
		return
	}

	m.stopAllMping()
	m.cancel()
}

// isEnabled returns true if mping is enabled and configured
func (m *mpingManager) isEnabled() bool {
	return m != nil && m.config != nil && m.config.Enabled
}
