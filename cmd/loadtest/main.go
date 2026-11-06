// Command loadtest drives a g711-radio server with synthetic traffic: it
// feeds G.711 UDP frames to every configured stream and connects many
// WebRTC listeners, then reports how much audio they actually receive.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
)

type stream struct {
	ID      string
	UDPPort int
}

func collectStreams(v any, out *[]stream) {
	switch t := v.(type) {
	case map[string]any:
		id, hasID := t["id"].(string)
		port, hasPort := t["udpPort"].(float64)
		if hasID && hasPort {
			*out = append(*out, stream{ID: id, UDPPort: int(port)})
		}
		for _, c := range t {
			collectStreams(c, out)
		}
	case []any:
		for _, c := range t {
			collectStreams(c, out)
		}
	}
}

func main() {
	base := flag.String("url", "https://127.0.0.1:8443", "server base URL")
	listeners := flag.Int("listeners", 100, "simultaneous WebRTC listeners")
	rampPerSec := flag.Int("ramp", 20, "new listeners per second")
	hold := flag.Duration("hold", 30*time.Second, "how long to hold after ramp completes")
	activePct := flag.Int("active", 100, "percent of streams carrying audio (worst case 100)")
	sendHost := flag.String("send-host", "127.0.0.1", "host receiving the synthetic UDP radio audio")
	iface := flag.String("iface", "", "only gather ICE candidates on this interface name (default: all non-loopback)")
	flag.Parse()

	httpClient := &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, MaxIdleConnsPerHost: 64},
	}

	resp, err := httpClient.Get(*base + "/streams")
	if err != nil {
		log.Fatal(err)
	}
	var raw any
	err = json.NewDecoder(resp.Body).Decode(&raw)
	resp.Body.Close()
	if err != nil {
		log.Fatal(err)
	}
	var streams []stream
	collectStreams(raw, &streams)
	if len(streams) == 0 {
		log.Fatal("no streams found")
	}
	log.Printf("found %d streams", len(streams))

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	activeN := len(streams) * *activePct / 100
	go sendAudio(ctx, *sendHost, streams[:activeN])

	var connected, failed, rtpPackets, rtpBytes atomic.Int64
	se := webrtc.SettingEngine{}
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	if *iface != "" {
		se.SetInterfaceFilter(func(name string) bool { return name == *iface })
	}
	me := &webrtc.MediaEngine{}
	if err := me.RegisterDefaultCodecs(); err != nil {
		log.Fatal(err)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithSettingEngine(se))

	var wg sync.WaitGroup
	var pcsMu sync.Mutex
	var pcs []*webrtc.PeerConnection
	sem := make(chan struct{}, 128) // bound simultaneous handshakes

	start := time.Now()
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		var lastPk, lastB int64
		last := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				pk, b := rtpPackets.Load(), rtpBytes.Load()
				dt := now.Sub(last).Seconds()
				c := connected.Load()
				rate := float64(pk-lastPk) / dt
				exp := float64(c) * 50 * float64(activeN) / float64(len(streams))
				pct := 0.0
				if exp > 0 {
					pct = 100 * rate / exp
				}
				log.Printf("t=%3.0fs connected=%d failed=%d rtp=%.0f pkt/s (expected ~%.0f, %.1f%%) payload=%.1f kbit/s",
					now.Sub(start).Seconds(), c, failed.Load(), rate, exp, pct, float64(b-lastB)*8/1000/dt)
				lastPk, lastB, last = pk, b, now
			}
		}
	}()

	ramp := time.NewTicker(time.Second / time.Duration(*rampPerSec))
	defer ramp.Stop()
rampLoop:
	for i := 0; i < *listeners; i++ {
		select {
		case <-ctx.Done():
			break rampLoop
		case <-ramp.C:
		}
		st := streams[i%len(streams)]
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			pc, err := listen(ctx, api, httpClient, *base, st.ID, &connected, &rtpPackets, &rtpBytes)
			if pc != nil {
				pcsMu.Lock()
				pcs = append(pcs, pc)
				pcsMu.Unlock()
			}
			if err != nil {
				failed.Add(1)
				log.Printf("listener failed: %v", err)
			}
		}()
	}
	wg.Wait()
	log.Printf("ramp complete: %d connected, %d failed; holding %s", connected.Load(), failed.Load(), *hold)
	select {
	case <-ctx.Done():
	case <-time.After(*hold):
	}
	cancel()
	pcsMu.Lock()
	for _, pc := range pcs {
		_ = pc.Close()
	}
	pcsMu.Unlock()
}

func listen(ctx context.Context, api *webrtc.API, hc *http.Client, base, streamID string,
	connected, rtpPackets, rtpBytes *atomic.Int64) (*webrtc.PeerConnection, error) {
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, err
	}
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		return pc, err
	}
	up := make(chan struct{})
	var once sync.Once
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		switch s {
		case webrtc.PeerConnectionStateConnected:
			connected.Add(1)
			once.Do(func() { close(up) })
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateDisconnected:
			if ctx.Err() == nil {
				connected.Add(-1)
			}
		}
	})
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for {
			pkt, _, err := tr.ReadRTP()
			if err != nil {
				return
			}
			rtpPackets.Add(1)
			rtpBytes.Add(int64(len(pkt.Payload)))
		}
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return pc, err
	}
	gather := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		return pc, err
	}
	<-gather

	body, _ := json.Marshal(map[string]any{"streamId": streamID, "offer": pc.LocalDescription()})
	resp, err := hc.Post(base+"/offer", "application/json", bytes.NewReader(body))
	if err != nil {
		return pc, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return pc, fmt.Errorf("offer: %s %s", resp.Status, bytes.TrimSpace(data))
	}
	var answer webrtc.SessionDescription
	if err := json.Unmarshal(data, &answer); err != nil {
		return pc, err
	}
	if err := pc.SetRemoteDescription(answer); err != nil {
		return pc, err
	}
	select {
	case <-up:
	case <-time.After(20 * time.Second):
		return pc, fmt.Errorf("stream %s: connect timeout", streamID)
	case <-ctx.Done():
	}
	return pc, nil
}

// sendAudio emits one 20 ms G.711 frame (12-byte header + 160 bytes) per
// stream port every 20 ms, like a radio gateway with continuous traffic.
func sendAudio(ctx context.Context, host string, streams []stream) {
	conn, err := net.ListenUDP("udp", nil)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	addrs := make([]*net.UDPAddr, len(streams))
	for i, s := range streams {
		addrs[i] = &net.UDPAddr{IP: net.ParseIP(host), Port: s.UDPPort}
	}
	pkt := make([]byte, 172)
	pkt[0] = 0x80
	for i := 12; i < len(pkt); i++ {
		pkt[i] = byte(0x80 + (i*7)%64)
	}
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	var seq uint16
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			seq++
			pkt[2], pkt[3] = byte(seq>>8), byte(seq)
			for _, a := range addrs {
				_, _ = conn.WriteToUDP(pkt, a)
			}
		}
	}
}

