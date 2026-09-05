package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"golang.org/x/net/ipv4"
)

// MulticastListener manages multiple UDP ports and optional multicast groups for a single stream.
// It binds to all configured endpoints and routes packets from the first active port.
// If the active source goes quiet long enough, the next packet from any port becomes active.
type MulticastListener struct {
	streamName  string
	ports       []int
	addresses   []string // multicast group addresses (empty string = unicast)
	listeners   []net.PacketConn
	frameChan   chan multicastFrame
	logger      *log.Logger
	debug       bool
	dropoutTime time.Duration

	mu             sync.Mutex
	activePort     int // -1 = no active port (listening), 0-based index
	lastPacketTime time.Time
	portAddr       map[int]string // track which port:addr pair each listener handles
	ctx            context.Context
	cancel         context.CancelFunc
	wg             sync.WaitGroup
}

type multicastFrame struct {
	data        []byte
	sourceIP    string
	headerBytes int // header length auto-detected for this packet (packet length minus audio frame size)
	codec       wireCodec
}

// NewMulticastListener creates a listener for multiple UDP ports/multicast addresses.
// ports: list of UDP port numbers to bind to
// addresses: list of multicast group addresses (or empty for unicast). Can be empty.
func NewMulticastListener(streamName string, ports []int, addresses []string, dropoutTime time.Duration, logger *log.Logger, debug bool) (*MulticastListener, error) {
	if len(ports) == 0 {
		return nil, fmt.Errorf("no ports specified for multicast listener")
	}
	if len(ports) > 4 {
		return nil, fmt.Errorf("too many ports (%d); maximum is 4", len(ports))
	}
	if dropoutTime < 100*time.Millisecond {
		dropoutTime = 1 * time.Second
	}

	ml := &MulticastListener{
		streamName:     streamName,
		ports:          ports,
		addresses:      addresses,
		listeners:      make([]net.PacketConn, 0, len(ports)),
		frameChan:      make(chan multicastFrame, 256), // buffered channel for frames
		logger:         logger,
		debug:          debug,
		dropoutTime:    dropoutTime,
		activePort:     -1,
		portAddr:       make(map[int]string),
		lastPacketTime: time.Now(),
	}

	ml.ctx, ml.cancel = context.WithCancel(context.Background())

	if err := ml.bindListeners(); err != nil {
		ml.Close()
		return nil, err
	}

	return ml, nil
}

// bindListeners creates and configures UDP socket listeners for all ports.
func (ml *MulticastListener) bindListeners() error {
	for i, port := range ml.ports {
		var addr string
		var isMulticast bool

		if i < len(ml.addresses) && ml.addresses[i] != "" {
			// Multicast
			addr = ml.addresses[i]
			isMulticast = true
		} else {
			// Unicast — bind to all interfaces
			addr = ""
		}

		conn, err := net.ListenPacket("udp", fmt.Sprintf(":%d", port))
		if err != nil {
			return fmt.Errorf("listen on UDP port %d: %w", port, err)
		}

		if isMulticast {
			// Join multicast group
			udpConn := conn.(*net.UDPConn)

			// Parse multicast group IP
			group := net.ParseIP(addr)
			if group == nil {
				conn.Close()
				return fmt.Errorf("invalid multicast address %q", addr)
			}

			// Join multicast group on all interfaces
			p := ipv4.NewPacketConn(udpConn)
			if err := p.JoinGroup(nil, &net.UDPAddr{IP: group, Port: 0}); err != nil {
				conn.Close()
				return fmt.Errorf("join multicast group %s on port %d: %w", addr, port, err)
			}

			// Disable multicast loopback (don't receive our own packets)
			_ = p.SetMulticastLoopback(false)
			ml.portAddr[i] = fmt.Sprintf("%s:%d", addr, port)
		} else {
			ml.portAddr[i] = fmt.Sprintf(":%d", port)
		}

		if ml.debug {
			ml.logger.Printf("%s: bound listener %d on %s (multicast=%t)", ml.streamName, i, ml.portAddr[i], isMulticast)
		}

		ml.listeners = append(ml.listeners, conn)
	}

	return nil
}

// Start begins listening on all ports. Run this in a goroutine.
func (ml *MulticastListener) Start() {
	for i, conn := range ml.listeners {
		i := i
		conn := conn
		ml.wg.Add(1)
		go ml.readFrom(i, conn)
	}
}

// readFrom is a goroutine that reads from one listener and routes packets.
func (ml *MulticastListener) readFrom(listenerIdx int, conn net.PacketConn) {
	defer ml.wg.Done()

	buffer := make([]byte, 64*1024)
	ticker := time.NewTicker(100 * time.Millisecond) // periodic dropout check
	defer ticker.Stop()

	for {
		select {
		case <-ml.ctx.Done():
			return
		case <-ticker.C:
			// Periodic dropout check
			ml.checkDropout()

		default:
			// Set short read deadline to allow periodic dropout checks
			_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))

			n, remoteAddr, err := conn.ReadFrom(buffer)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return
				}
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					// Timeout is expected; continue
					continue
				}
				if errors.Is(err, net.ErrClosed) {
					return
				}
				ml.logger.Printf("read error on listener %d (%s): %v", listenerIdx, ml.portAddr[listenerIdx], err)
				continue
			}

			if ml.debug {
				ml.logger.Printf("%s: packet on listener %d (%s) from %s, bytes=%d",
					ml.streamName, listenerIdx, ml.portAddr[listenerIdx], remoteAddr, n)
			}

			af, err := extractAudioFrame(buffer[:n])
			if err != nil {
				// Non-audio control/keepalive packets are expected on some device
				// types (e.g. DFSI gateways cycling their channel ports); only log
				// when debugging this stream so normal operation stays quiet.
				if ml.debug {
					ml.logger.Printf("%s: dropping UDP packet from %s on listener %d: %v",
						ml.streamName, remoteAddr, listenerIdx, err)
				}
				continue
			}

			ml.handlePacket(listenerIdx, remoteAddr, af)
		}
	}
}

// handlePacket is called when a valid frame is received on a listener.
// It implements port priority: first port to send a packet wins until dropout.
func (ml *MulticastListener) handlePacket(listenerIdx int, remoteAddr net.Addr, af audioFrame) {
	ml.mu.Lock()
	now := time.Now()
	prevActive := ml.activePort

	// Check for dropout on the active port
	if ml.activePort >= 0 {
		if now.Sub(ml.lastPacketTime) > ml.dropoutTime {
			// Stream dropped — accept from any port
			if ml.debug {
				ml.logger.Printf("%s: active listener %d (%s) timed out after %s; accepting any port again",
					ml.streamName, ml.activePort, ml.portAddr[ml.activePort], ml.dropoutTime)
			}
			ml.activePort = -1
		}
	}

	// If no active port or this packet is from the active port, process it
	if ml.activePort < 0 {
		// No active port — this packet wins
		ml.activePort = listenerIdx
		ml.lastPacketTime = now
		if ml.debug {
			ml.logger.Printf("%s: listener %d (%s) became active (previous=%d)",
				ml.streamName, listenerIdx, ml.portAddr[listenerIdx], prevActive)
		}
		ml.mu.Unlock()

		// Make a copy of frame and send it (non-blocking)
		frameCopy := make([]byte, len(af.data))
		copy(frameCopy, af.data)
		sourceIP := ""
		if udpAddr, ok := remoteAddr.(*net.UDPAddr); ok && udpAddr != nil {
			sourceIP = udpAddr.IP.String()
		}

		select {
		case ml.frameChan <- multicastFrame{data: frameCopy, sourceIP: sourceIP, headerBytes: af.headerBytes, codec: af.codec}:
		case <-ml.ctx.Done():
		default:
			ml.logger.Printf("%s: frame queue full on listener %d", ml.streamName, listenerIdx)
		}
	} else if ml.activePort == listenerIdx {
		// Packet from active port — process it
		ml.lastPacketTime = now
		if ml.debug {
			ml.logger.Printf("%s: packet accepted from active listener %d (%s)",
				ml.streamName, listenerIdx, ml.portAddr[listenerIdx])
		}
		ml.mu.Unlock()

		frameCopy := make([]byte, len(af.data))
		copy(frameCopy, af.data)
		sourceIP := ""
		if udpAddr, ok := remoteAddr.(*net.UDPAddr); ok && udpAddr != nil {
			sourceIP = udpAddr.IP.String()
		}

		select {
		case ml.frameChan <- multicastFrame{data: frameCopy, sourceIP: sourceIP, headerBytes: af.headerBytes, codec: af.codec}:
		case <-ml.ctx.Done():
		default:
			ml.logger.Printf("%s: frame queue full on active listener %d", ml.streamName, listenerIdx)
		}
	} else {
		if ml.debug {
			ml.logger.Printf("%s: packet ignored from listener %d (%s); active listener is %d (%s)",
				ml.streamName, listenerIdx, ml.portAddr[listenerIdx], ml.activePort, ml.portAddr[ml.activePort])
		}
		ml.mu.Unlock()
	}
}

// checkDropout is called periodically to detect stream silence.
func (ml *MulticastListener) checkDropout() {
	ml.mu.Lock()
	defer ml.mu.Unlock()

	if ml.activePort >= 0 && time.Now().Sub(ml.lastPacketTime) > ml.dropoutTime {
		if ml.debug {
			ml.logger.Printf("%s: dropout check cleared active listener %d (%s)",
				ml.streamName, ml.activePort, ml.portAddr[ml.activePort])
		}
		ml.activePort = -1
	}
}

// FrameChan returns the channel on which audio frames arrive.
func (ml *MulticastListener) FrameChan() <-chan multicastFrame {
	return ml.frameChan
}

// Close shuts down all listeners and the frame channel.
func (ml *MulticastListener) Close() {
	ml.cancel()

	for _, conn := range ml.listeners {
		if conn != nil {
			_ = conn.Close()
		}
	}

	// Wait for all goroutines to finish
	ml.wg.Wait()

	close(ml.frameChan)
}

// Stop gracefully stops the listener and returns remaining frames.
func (ml *MulticastListener) Stop() {
	ml.Close()
}
