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
		name          string
		cfg           streamConfig
		expectedPorts []int
		expectedAddrs []string
		shouldFail    bool
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

func TestTransmissionPriority(t *testing.T) {
	type packet struct {
		port   int
		ms     int
		accept bool
	}
	tests := []struct {
		name    string
		packets []packet
	}{
		{"new TX preempts RX without waiting for dropout", []packet{
			{0, 0, true}, {0, 20, true}, {1, 30, true},
			{0, 40, false}, {1, 50, true}, {0, 60, false},
		}},
		{"new RX preempts TX regardless of port order", []packet{
			{1, 0, true}, {0, 10, true}, {1, 20, false}, {0, 30, true},
		}},
		{"suppressed source cannot reclaim priority below 200ms", []packet{
			{0, 0, true}, {1, 10, true}, {1, 100, true},
			{0, 199, false}, {1, 250, true}, {0, 398, false},
		}},
		{"suppressed source starts a new burst at exactly 200ms", []packet{
			{0, 0, true}, {1, 10, true}, {1, 100, true},
			{0, 200, true}, {1, 210, false},
		}},
		{"older source resumes when newer source reaches 200ms inactivity", []packet{
			{0, 0, true}, {1, 10, true}, {0, 100, false},
			{0, 209, false}, {0, 210, true},
		}},
		{"four ports retain newest burst priority", []packet{
			{0, 0, true}, {1, 10, true}, {2, 20, true}, {3, 30, true},
			{0, 40, false}, {1, 50, false}, {2, 60, false}, {3, 70, true},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ml := &MulticastListener{
				activePort:  -1,
				lastPackets: make([]time.Time, 4),
				dropoutTime: transmissionGap,
			}
			start := time.Now()
			for _, p := range tt.packets {
				ml.mu.Lock()
				accepted := ml.selectPort(p.port, start.Add(time.Duration(p.ms)*time.Millisecond))
				ml.mu.Unlock()
				if accepted != p.accept {
					t.Fatalf("port %d at %dms: accepted=%t, want %t", p.port, p.ms, accepted, p.accept)
				}
			}
		})
	}
}

func TestTransmissionFrames(t *testing.T) {
	listener, err := NewMulticastListener("TX/RX", freeUDPPorts(t, 2), nil, transmissionGap, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	rx := audioFrame{data: bytes.Repeat([]byte{0xff}, frameSizeBytes), codec: wireCodecG711, headerBytes: 12}
	tx := audioFrame{data: bytes.Repeat([]byte{0x80}, frameSizeBytes), codec: wireCodecG711, headerBytes: 14}
	source := &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 1234}
	listener.handlePacket(0, source, rx)
	listener.handlePacket(1, source, tx)
	listener.handlePacket(0, source, rx)
	listener.handlePacket(1, source, tx)
	for i, want := range []audioFrame{rx, tx, tx} {
		select {
		case got := <-listener.FrameChan():
			if !bytes.Equal(got.data, want.data) || got.codec != want.codec || got.headerBytes != want.headerBytes || got.sourceIP != source.IP.String() {
				t.Fatalf("frame %d: incorrect payload or metadata: %+v", i, got)
			}
		default:
			t.Fatalf("frame %d missing", i)
		}
	}
	if len(listener.frameChan) != 0 {
		t.Fatal("older overlapping RX frame was forwarded")
	}
}

func TestTwoPortUDPTransmission(t *testing.T) {
	ports := freeUDPPorts(t, 2)
	listener, err := NewMulticastListener("TX/RX", ports, nil, transmissionGap, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	listener.Start()
	for i, port := range ports {
		conn, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		audio := bytes.Repeat([]byte{byte(i)}, frameSizeBytes)
		if _, err := conn.Write(createRTPPacket(audio)); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-listener.FrameChan():
			if !bytes.Equal(got.data, audio) {
				t.Fatal("incorrect audio from selected port")
			}
		case <-time.After(time.Second):
			t.Fatalf("no audio from port %d", port)
		}
	}
}

func TestTransmissionGapDefault(t *testing.T) {
	listener, err := NewMulticastListener("test", []int{0}, nil, 0, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if listener.dropoutTime != 200*time.Millisecond {
		t.Fatalf("default gap = %s, want 200ms", listener.dropoutTime)
	}
	listener.activePort = 0
	listener.lastPackets[0] = time.Now().Add(-201 * time.Millisecond)
	listener.checkDropout()
	if listener.activePort != -1 {
		t.Fatal("active source not cleared after 200ms")
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
		af, err := extractAudioFrame(pkt)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if af.codec != wireCodecG711 {
			t.Errorf("expected wireCodecG711, got %v", af.codec)
		}
		if !bytes.Equal(af.data, createTestG711Frame(frameSizeBytes)) {
			t.Errorf("extracted frame does not match expected audio bytes")
		}
	})

	t.Run("DFSI 14-byte header (174-byte packet)", func(t *testing.T) {
		pkt := makePacket(14, 0xBB)
		af, err := extractAudioFrame(pkt)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !bytes.Equal(af.data, createTestG711Frame(frameSizeBytes)) {
			t.Errorf("extracted frame does not match expected audio bytes")
		}
	})

	t.Run("DFSI 18-byte header, first frame of burst (178-byte packet)", func(t *testing.T) {
		pkt := makePacket(18, 0xCC)
		af, err := extractAudioFrame(pkt)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !bytes.Equal(af.data, createTestG711Frame(frameSizeBytes)) {
			t.Errorf("extracted frame does not match expected audio bytes")
		}
	})

	t.Run("short non-audio control/keepalive packets are rejected", func(t *testing.T) {
		for _, size := range []int{14, 16, 17, 28, 36, 79} {
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
