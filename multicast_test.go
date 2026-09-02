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
			listener, err := NewMulticastListener("test", tt.ports, tt.addresses, 1*time.Second, nil, false)

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
	listener, err := NewMulticastListener("test", []int{15000}, []string{}, 500*time.Millisecond, nil, false)
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
	case packet := <-listener.FrameChan():
		if !bytes.Equal(packet.data, testFrame) {
			t.Errorf("frame mismatch: got %d bytes, expected %d", len(packet.data), len(testFrame))
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
	listener, err := NewMulticastListener("test", []int{15001}, []string{}, 200*time.Millisecond, nil, false)
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

// TestExtractAudioFrame verifies audio extraction across multiple device
// packet formats: the legacy 12-byte-header format, and the DFSI-style
// gateway's 14-byte (steady-state) and 18-byte (first frame of a burst,
// carrying an extra start-of-stream marker) headers. It also verifies that
// short non-audio control/keepalive packets (as seen from DFSI gateways
// cycling through their channel ports) are rejected rather than misread as
// audio.
func TestExtractAudioFrame(t *testing.T) {
	makePacket := func(headerLen int, marker byte) []byte {
		payload := make([]byte, headerLen+frameSizeBytes)
		for i := 0; i < headerLen; i++ {
			payload[i] = marker // arbitrary header bytes; content must be ignored
		}
		audio := createTestG711Frame(frameSizeBytes)
		copy(payload[headerLen:], audio)
		return payload
	}

	t.Run("legacy 12-byte header (172-byte packet)", func(t *testing.T) {
		pkt := makePacket(12, 0xAA)
		frame, err := extractAudioFrame(pkt)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !bytes.Equal(frame, createTestG711Frame(frameSizeBytes)) {
			t.Errorf("extracted frame does not match expected audio bytes")
		}
	})

	t.Run("DFSI 14-byte header (174-byte packet)", func(t *testing.T) {
		pkt := makePacket(14, 0xBB)
		frame, err := extractAudioFrame(pkt)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !bytes.Equal(frame, createTestG711Frame(frameSizeBytes)) {
			t.Errorf("extracted frame does not match expected audio bytes")
		}
	})

	t.Run("DFSI 18-byte header, first frame of burst (178-byte packet)", func(t *testing.T) {
		pkt := makePacket(18, 0xCC)
		frame, err := extractAudioFrame(pkt)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !bytes.Equal(frame, createTestG711Frame(frameSizeBytes)) {
			t.Errorf("extracted frame does not match expected audio bytes")
		}
	})

	t.Run("short non-audio control/keepalive packets are rejected", func(t *testing.T) {
		for _, size := range []int{14, 16, 17, 28, 36, 84} {
			if _, err := extractAudioFrame(make([]byte, size)); err == nil {
				t.Errorf("expected error for %d-byte non-audio packet, got nil", size)
			}
		}
	})

	t.Run("implausibly large header is rejected as a sanity check", func(t *testing.T) {
		pkt := makePacket(maxHeaderBytes+1, 0xDD)
		if _, err := extractAudioFrame(pkt); err == nil {
			t.Errorf("expected error for packet with %d-byte header, got nil", maxHeaderBytes+1)
		}
	})
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
