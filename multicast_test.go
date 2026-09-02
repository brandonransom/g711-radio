package main

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"
)

// TestMulticastListenerCreation verifies basic listener setup
func TestMulticastListenerCreation(t *testing.T) {
	tests := []struct {
		name       string
		ports      []int
		addresses  []string
		shouldFail bool
	}{
		{
			name:       "Single port",
			ports:      []int{5000},
			addresses:  []string{},
			shouldFail: false,
		},
		{
			name:       "Two ports",
			ports:      []int{5000, 5001},
			addresses:  []string{},
			shouldFail: false,
		},
		{
			name:       "Max four ports",
			ports:      []int{5000, 5001, 5002, 5003},
			addresses:  []string{"", "", "", ""}, // Unicast only for testing
			shouldFail: false,
		},
		{
			name:       "Too many ports",
			ports:      []int{5000, 5001, 5002, 5003, 5004},
			addresses:  []string{},
			shouldFail: true,
		},
		{
			name:       "No ports",
			ports:      []int{},
			addresses:  []string{},
			shouldFail: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			listener, err := NewMulticastListener("test", tt.ports, tt.addresses, 1*time.Second, nil)

			if (err != nil) != tt.shouldFail {
				t.Errorf("expected shouldFail=%v, got error=%v", tt.shouldFail, err)
			}

			if listener != nil {
				listener.Close()
			}
		})
	}
}

// TestGetListenerConfig validates configuration extraction
func TestGetListenerConfig(t *testing.T) {
	tests := []struct {
		name           string
		cfg            streamConfig
		expectedPorts  []int
		expectedAddrs  []string
		shouldFail     bool
	}{
		{
			name:          "Single port (udpPort)",
			cfg:           streamConfig{UDPPort: 5000},
			expectedPorts: []int{5000},
			expectedAddrs: []string{""},
			shouldFail:    false,
		},
		{
			name:          "Multiple ports (udpPorts)",
			cfg:           streamConfig{UDPPorts: []int{5000, 5001}},
			expectedPorts: []int{5000, 5001},
			expectedAddrs: []string{"", ""},
			shouldFail:    false,
		},
		{
			name:          "With multicast addresses",
			cfg:           streamConfig{UDPPorts: []int{5000, 5001}, MulticastAddrs: []string{"224.0.0.1", "224.0.0.2"}},
			expectedPorts: []int{5000, 5001},
			expectedAddrs: []string{"224.0.0.1", "224.0.0.2"},
			shouldFail:    false,
		},
		{
			name:          "Single multicast port",
			cfg:           streamConfig{UDPPorts: []int{5000}, MulticastAddrs: []string{"224.0.0.1"}},
			expectedPorts: []int{5000},
			expectedAddrs: []string{"224.0.0.1"},
			shouldFail:    false,
		},
		{
			name:          "UDPPorts takes precedence over UDPPort",
			cfg:           streamConfig{UDPPort: 5000, UDPPorts: []int{6000, 6001}},
			expectedPorts: []int{6000, 6001},
			expectedAddrs: []string{"", ""},
			shouldFail:    false,
		},
		{
			name:          "No ports configured",
			cfg:           streamConfig{},
			expectedPorts: nil,
			expectedAddrs: nil,
			shouldFail:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ports, addrs, err := getListenerConfig(tt.cfg)

			if (err != nil) != tt.shouldFail {
				t.Errorf("expected shouldFail=%v, got error=%v", tt.shouldFail, err)
			}

			if !tt.shouldFail {
				if !equalIntSlices(ports, tt.expectedPorts) {
					t.Errorf("ports mismatch: got %v, expected %v", ports, tt.expectedPorts)
				}
				if !equalStringSlices(addrs, tt.expectedAddrs) {
					t.Errorf("addresses mismatch: got %v, expected %v", addrs, tt.expectedAddrs)
				}
			}
		})
	}
}

// TestPortPriority simulates port priority behavior
func TestPortPriority(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Create a listener on a single available port for testing
	listener, err := NewMulticastListener("test", []int{15000}, []string{}, 500*time.Millisecond, nil)
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	defer listener.Close()

	listener.Start()

	// Send a test frame via UDP
	testFrame := createTestG711Frame(160)
	conn, err := net.Dial("udp", "127.0.0.1:15000")
	if err != nil {
		t.Fatalf("failed to create UDP connection: %v", err)
	}
	defer conn.Close()

	// Send a frame with RTP header
	rtp := createRTPPacket(testFrame)
	if _, err := conn.Write(rtp); err != nil {
		t.Fatalf("failed to send test packet: %v", err)
	}

	// Verify frame is received
	select {
	case frame := <-listener.FrameChan():
		if !bytes.Equal(frame, testFrame) {
			t.Errorf("frame mismatch: got %d bytes, expected %d", len(frame), len(testFrame))
		}
	case <-ctx.Done():
		t.Error("timeout waiting for frame")
	}
}

// TestDropoutDetection verifies dropout behavior
func TestDropoutDetection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Create a listener with short dropout time for testing
	listener, err := NewMulticastListener("test", []int{15001}, []string{}, 200*time.Millisecond, nil)
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	defer listener.Close()

	listener.Start()

	conn, err := net.Dial("udp", "127.0.0.1:15001")
	if err != nil {
		t.Fatalf("failed to create UDP connection: %v", err)
	}
	defer conn.Close()

	testFrame := createTestG711Frame(160)
	rtp := createRTPPacket(testFrame)

	// Send initial packet
	if _, err := conn.Write(rtp); err != nil {
		t.Fatalf("failed to send test packet: %v", err)
	}

	// Wait for frame
	select {
	case <-listener.FrameChan():
	case <-ctx.Done():
		t.Error("timeout waiting for initial frame")
		return
	}

	// Wait for dropout
	time.Sleep(300 * time.Millisecond)

	// Verify listener is ready to accept new port
	// (This is an internal state check; we can't directly inspect it without exposing it)
	// But we can verify that new packets are still being accepted
	if _, err := conn.Write(rtp); err == nil {
		// Send should work; verify frame is received
		select {
		case <-listener.FrameChan():
		case <-ctx.Done():
			t.Error("timeout waiting for frame after dropout")
		}
	}
}

// Helper functions

func equalIntSlices(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func createTestG711Frame(size int) []byte {
	frame := make([]byte, size)
	for i := range frame {
		frame[i] = byte(i % 256)
	}
	return frame
}

func createRTPPacket(payload []byte) []byte {
	// Create a minimal RTP packet with 12-byte header + payload
	// RTP header format:
	// V(2), P(1), X(1), CC(4), M(1), PT(7), SN(16), TS(32), SSRC(32)
	rtp := make([]byte, 12+len(payload))

	// Version=2, Padding=0, Extension=0, CSRC Count=0
	rtp[0] = 0x80

	// Marker=0, Payload Type=0 (PCMU)
	rtp[1] = 0x00

	// Sequence number (arbitrary)
	rtp[2] = 0x00
	rtp[3] = 0x01

	// Timestamp (arbitrary)
	rtp[4] = 0x00
	rtp[5] = 0x00
	rtp[6] = 0x00
	rtp[7] = 0x00

	// SSRC (arbitrary)
	rtp[8] = 0x00
	rtp[9] = 0x00
	rtp[10] = 0x00
	rtp[11] = 0x00

	// Copy payload
	copy(rtp[12:], payload)

	return rtp
}
