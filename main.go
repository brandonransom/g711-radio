package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

const (
	frameSizeBytes = 160
	sampleRateHz   = 8000
	skipBytes      = 12
	configPath     = "config.local.json"
)

//go:embed web/*
var webFiles embed.FS

type appConfig struct {
	HTTPPort     int                                  `json:"httpPort"`
	Regions      map[string]map[string][]streamConfig `json:"regions"`
	Whisper      *whisperConfig                       `json:"whisper"`
	AudioLogDir  string                               `json:"audioLogDir"`

	streamGroups []configuredRegion
	totalStreams  int
}

type streamConfig struct {
	StreamName string `json:"streamName"`
	UDPPort    int    `json:"udpPort"`
}

type streamInfo struct {
	RegionName string `json:"regionName"`
	GroupName  string `json:"groupName"`
	ID         string `json:"id"`
	StreamName string `json:"streamName"`
	UDPPort    int    `json:"udpPort"`
}

type subGroup struct {
	GroupName string       `json:"groupName"`
	Streams   []streamInfo `json:"streams"`
}

type regionGroup struct {
	RegionName string     `json:"regionName"`
	SubGroups  []subGroup `json:"subGroups"`
}

type configuredSubGroup struct {
	GroupName string
	Streams   []streamConfig
}

type configuredRegion struct {
	RegionName string
	SubGroups  []configuredSubGroup
}

type station struct {
	info          streamInfo
	codec         webrtc.RTPCodecCapability
	frameDuration time.Duration
	logger        *log.Logger
	audioLogDir   string

	// whisperPool is non-nil when transcription is enabled.
	whisperPool *whisperPool
	vad         *vadState

	nextID      atomic.Uint64
	mu          sync.RWMutex
	subscribers map[string]*subscriber
}

type subscriber struct {
	pc    *webrtc.PeerConnection
	track *webrtc.TrackLocalStaticSample
}

type offerRequest struct {
	StreamID string                    `json:"streamId"`
	Offer    webrtc.SessionDescription `json:"offer"`
}

type webrtcServer struct {
	api          *webrtc.API
	logger       *log.Logger
	streams      map[string]*station
	regionGroups []regionGroup
	hub          *transcriptHub
}

func main() {
	logger := log.New(os.Stdout, "", log.LstdFlags)

	config, err := loadConfig(configPath)
	if err != nil {
		logger.Fatal(err)
	}

	codec := webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypePCMU,
		ClockRate: sampleRateHz,
		Channels:  1,
	}

	frameDuration := time.Second * frameSizeBytes / sampleRateHz

	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
		logger.Fatal(err)
	}

	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine))

	hub := newTranscriptHub("transcripts", logger)

	var pool *whisperPool
	if config.Whisper != nil && config.Whisper.ModelPath != "" {
		config.Whisper.setDefaults()
		pool = newWhisperPool(*config.Whisper, hub, logger)
		pool.Start()
		logger.Printf("whisper transcription enabled: model=%s workers=%d", config.Whisper.ModelPath, config.Whisper.Workers)
	}

	server := &webrtcServer{
		api:          api,
		logger:       logger,
		streams:      make(map[string]*station, config.totalStreams),
		regionGroups: make([]regionGroup, 0, len(config.streamGroups)),
		hub:          hub,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	for _, region := range config.streamGroups {
		apiRegion := regionGroup{
			RegionName: region.RegionName,
			SubGroups:  make([]subGroup, 0, len(region.SubGroups)),
		}

		for _, sg := range region.SubGroups {
			apiSubGroup := subGroup{
				GroupName: sg.GroupName,
				Streams:   make([]streamInfo, 0, len(sg.Streams)),
			}

			for _, cfg := range sg.Streams {
				info := streamInfo{
					RegionName: region.RegionName,
					GroupName:  sg.GroupName,
					ID:         fmt.Sprintf("stream-%d", cfg.UDPPort),
					StreamName: cfg.StreamName,
					UDPPort:    cfg.UDPPort,
				}

				st := &station{
					info:          info,
					codec:         codec,
					frameDuration: frameDuration,
					logger:        logger,
						audioLogDir:   config.AudioLogDir,
						subscribers:   make(map[string]*subscriber),
						whisperPool:   pool,
					}

					if pool != nil || config.AudioLogDir != "" {
						wCfg := &whisperConfig{}
						if config.Whisper != nil {
							wCfg = config.Whisper
							wCfg.setDefaults()
						} else {
							wCfg.setDefaults()
						}
						captureInfo := info
						captureAudioLogDir := config.AudioLogDir
						st.vad = newVADState(
							wCfg.VADThreshold,
							time.Duration(wCfg.SilenceMs)*time.Millisecond,
							time.Duration(wCfg.MinClipMs)*time.Millisecond,
							time.Duration(wCfg.MaxClipMs)*time.Millisecond,
							func(samples []int16, start time.Time) {
								var wavPath, audioURL string
								if captureAudioLogDir != "" {
									var err error
									wavPath, audioURL, err = saveAudioClip(captureAudioLogDir, captureInfo, samples, start, logger)
									if err != nil {
										logger.Printf("audio log: %v", err)
									}
								}
								if pool != nil {
									if wavPath == "" {
										// No audio log dir — write a temp file for whisper.
										wav, _ := encodePCM16WAV(samples, vadSampleRate)
										tmp, err := os.CreateTemp("", "g711-whisper-*.wav")
										if err == nil {
											tmp.Write(wav)
											tmp.Close()
											wavPath = tmp.Name()
										}
									}
									if wavPath != "" {
										pool.Submit(transcriptJob{info: captureInfo, wavPath: wavPath, audioURL: audioURL, start: start})
									}
								}
							},
						)
					}

				conn, err := net.ListenPacket("udp", fmt.Sprintf(":%d", cfg.UDPPort))
				if err != nil {
					logger.Fatalf("listen on UDP %d for %q: %v", cfg.UDPPort, cfg.StreamName, err)
				}

				server.streams[info.ID] = st
				apiSubGroup.Streams = append(apiSubGroup.Streams, info)

				logger.Printf(
					"configured stream %q in region %q, group %q on UDP %d, codec=PCMU, frame_size=%d bytes, skip_bytes=%d, frame_duration=%s",
					info.StreamName,
					region.RegionName,
					sg.GroupName,
					info.UDPPort,
					frameSizeBytes,
					skipBytes,
					frameDuration,
				)

				go func(st *station, conn net.PacketConn) {
					<-ctx.Done()
					_ = conn.Close()
					st.closeSubscribers()
				}(st, conn)

				go func(st *station, conn net.PacketConn) {
					if err := st.ingest(ctx, conn); err != nil {
						logger.Printf("%s ingest stopped: %v", st.info.StreamName, err)
						stop()
					}
				}(st, conn)
			}

			apiRegion.SubGroups = append(apiRegion.SubGroups, apiSubGroup)
		}

		server.regionGroups = append(server.regionGroups, apiRegion)
	}

	staticFS, err := fs.Sub(webFiles, "web")
	if err != nil {
		logger.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(staticFS)))
	mux.HandleFunc("/streams", server.handleStreams)
	mux.HandleFunc("/offer", server.handleOffer)
	mux.Handle("/transcripts", hub)
	mux.HandleFunc("/transcripts/history", func(w http.ResponseWriter, r *http.Request) {
		streamID := r.URL.Query().Get("streamId")
		if streamID == "" {
			http.Error(w, "streamId required", http.StatusBadRequest)
			return
		}
		events, err := hub.History(streamID, 72*time.Hour)
		if err != nil {
			http.Error(w, "failed to read history", http.StatusInternalServerError)
			logger.Printf("transcript history: %v", err)
			return
		}
		if events == nil {
			events = []transcriptEvent{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(events)
	})
	if config.AudioLogDir != "" {
		mux.Handle("/audio/", http.StripPrefix("/audio/", http.FileServer(http.Dir(config.AudioLogDir))))
		logger.Printf("audio log directory: %s (served at /audio/)", config.AudioLogDir)
	}

	httpServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", config.HTTPPort),
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
		if pool != nil {
			pool.Close()
		}
	}()

	// Background cleanup: delete audio WAV files and prune transcript logs older than 72h.
	const retentionPeriod = 72 * time.Hour
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if config.AudioLogDir != "" {
					pruneOldFiles(config.AudioLogDir, retentionPeriod, logger)
				}
				pruneTranscriptLogs(hub.logDir, retentionPeriod, logger)
			}
		}
	}()

	logger.Printf("loaded %d stream(s) from %s", config.totalStreams, configPath)
	logger.Printf("serving WebRTC client on http://localhost:%d", config.HTTPPort)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Fatal(err)
	}
}

func loadConfig(path string) (appConfig, error) {
	file, err := os.Open(path)
	if err != nil {
		return appConfig{}, fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()

	var config appConfig
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return appConfig{}, fmt.Errorf("decode %s: %w", path, err)
	}

	if config.HTTPPort < 1 || config.HTTPPort > 65535 {
		return appConfig{}, fmt.Errorf("%s has invalid httpPort %d", path, config.HTTPPort)
	}

	regions, totalStreams, err := normalizeRegions(path, config.Regions)
	if err != nil {
		return appConfig{}, err
	}

	config.streamGroups = regions
	config.totalStreams = totalStreams

	return config, nil
}

func normalizeRegions(path string, rawRegions map[string]map[string][]streamConfig) ([]configuredRegion, int, error) {
	if len(rawRegions) == 0 {
		return nil, 0, fmt.Errorf("%s has no regions configured", path)
	}

	regionNames := make([]string, 0, len(rawRegions))
	for regionName := range rawRegions {
		regionNames = append(regionNames, regionName)
	}
	sort.Strings(regionNames)

	seenRegionNames := make(map[string]struct{}, len(regionNames))
	seenGroupNames := make(map[string]struct{})
	seenPorts := make(map[int]struct{})
	regions := make([]configuredRegion, 0, len(regionNames))
	totalStreams := 0

	for _, sourceRegionName := range regionNames {
		regionName := strings.TrimSpace(sourceRegionName)
		if regionName == "" {
			return nil, 0, fmt.Errorf("%s has an empty region name", path)
		}
		if _, exists := seenRegionNames[regionName]; exists {
			return nil, 0, fmt.Errorf("%s has duplicate region name %q after trimming whitespace", path, regionName)
		}
		seenRegionNames[regionName] = struct{}{}

		rawSubGroups := rawRegions[sourceRegionName]
		if len(rawSubGroups) == 0 {
			return nil, 0, fmt.Errorf("%s region %q has no groups configured", path, regionName)
		}

		region := configuredRegion{
			RegionName: regionName,
			SubGroups:  make([]configuredSubGroup, 0, len(rawSubGroups)),
		}

		groupNames := make([]string, 0, len(rawSubGroups))
		for groupName := range rawSubGroups {
			groupNames = append(groupNames, groupName)
		}
		sort.Strings(groupNames)

		for _, sourceGroupName := range groupNames {
			groupName := strings.TrimSpace(sourceGroupName)
			if groupName == "" {
				return nil, 0, fmt.Errorf("%s region %q has an empty group name", path, regionName)
			}

			compositeKey := regionName + "/" + groupName
			if _, exists := seenGroupNames[compositeKey]; exists {
				return nil, 0, fmt.Errorf("%s region %q has duplicate group name %q", path, regionName, groupName)
			}
			seenGroupNames[compositeKey] = struct{}{}

			rawStreams := rawSubGroups[sourceGroupName]
			if len(rawStreams) == 0 {
				return nil, 0, fmt.Errorf("%s region %q group %q has no streams configured", path, regionName, groupName)
			}

			subGroup := configuredSubGroup{
				GroupName: groupName,
				Streams:   make([]streamConfig, 0, len(rawStreams)),
			}

			for i, stream := range rawStreams {
				streamName := strings.TrimSpace(stream.StreamName)
				if streamName == "" {
					return nil, 0, fmt.Errorf("%s region %q group %q entry %d is missing streamName", path, regionName, groupName, i)
				}
				if stream.UDPPort < 1 || stream.UDPPort > 65535 {
					return nil, 0, fmt.Errorf("%s region %q group %q entry %d has invalid udpPort %d", path, regionName, groupName, i, stream.UDPPort)
				}
				if _, exists := seenPorts[stream.UDPPort]; exists {
					return nil, 0, fmt.Errorf("%s has duplicate udpPort %d", path, stream.UDPPort)
				}
				seenPorts[stream.UDPPort] = struct{}{}

				subGroup.Streams = append(subGroup.Streams, streamConfig{
					StreamName: streamName,
					UDPPort:    stream.UDPPort,
				})
			}

			region.SubGroups = append(region.SubGroups, subGroup)
			totalStreams += len(subGroup.Streams)
		}

		regions = append(regions, region)
	}

	return regions, totalStreams, nil
}

func (s *station) ingest(ctx context.Context, conn net.PacketConn) error {
	buffer := make([]byte, 64*1024)
	packetsSeen := 0

	for {
		n, remoteAddr, err := conn.ReadFrom(buffer)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}

		frame, err := extractAudioFrame(buffer[:n])
		if err != nil {
			s.logger.Printf("%s: dropping UDP packet from %s: %v", s.info.StreamName, remoteAddr, err)
			continue
		}

		if packetsSeen == 0 {
			s.logger.Printf(
				"%s: first UDP packet from %s, packet_bytes=%d, audio_bytes=%d, skip_bytes=%d",
				s.info.StreamName,
				remoteAddr,
				n,
				frameSizeBytes,
				skipBytes,
			)
		}
		packetsSeen++

		// Run VAD if transcription is enabled.
		if s.vad != nil {
			s.vad.Push(DecodePCMU(frame), time.Now())
		}

		s.broadcast(media.Sample{
			Data:     frame,
			Duration: s.frameDuration,
		})
	}
}

// saveAudioClip writes a VAD clip as an 8kHz mono WAV file under audioLogDir.
// Returns the absolute file path and the relative URL path for browser playback.
// Path: <audioLogDir>/<region>/<group>/<streamName>/<streamName>_<ISO8601Z>.wav
func saveAudioClip(audioLogDir string, info streamInfo, samples []int16, start time.Time, logger *log.Logger) (string, string, error) {
	safe := func(s string) string {
		return unsafeChars.ReplaceAllString(s, "_")
	}
	relDir := filepath.Join(safe(info.RegionName), safe(info.GroupName), safe(info.StreamName))
	absDir := filepath.Join(audioLogDir, relDir)
	if err := os.MkdirAll(absDir, 0755); err != nil {
		return "", "", fmt.Errorf("mkdir %s: %w", absDir, err)
	}
	// ISO 8601 UTC — colons replaced with underscores for Windows filename safety.
	ts := start.UTC().Format("2006-01-02T15_04_05Z")
	filename := fmt.Sprintf("%s_%s.wav", safe(info.StreamName), ts)
	absPath := filepath.Join(absDir, filename)

	wav, err := encodePCM16WAV(samples, vadSampleRate)
	if err != nil {
		return "", "", fmt.Errorf("encode %s: %w", absPath, err)
	}
	if err := os.WriteFile(absPath, wav, 0644); err != nil {
		return "", "", fmt.Errorf("write %s: %w", absPath, err)
	}
	// Build a URL-style relative path using forward slashes.
	relURL := "/audio/" + safe(info.RegionName) + "/" + safe(info.GroupName) + "/" + safe(info.StreamName) + "/" + filename
	return absPath, relURL, nil
}

// pruneOldFiles walks dir and removes regular files older than maxAge.
// Empty directories left behind are also removed.
func pruneOldFiles(dir string, maxAge time.Duration, logger *log.Logger) {
	cutoff := time.Now().Add(-maxAge)
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.ModTime().Before(cutoff) {
			if removeErr := os.Remove(path); removeErr == nil {
				logger.Printf("pruned old audio: %s", path)
			}
		}
		return nil
	})
	// Remove empty leaf directories.
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, _ error) error {
		if path == dir || !d.IsDir() {
			return nil
		}
		entries, _ := os.ReadDir(path)
		if len(entries) == 0 {
			os.Remove(path)
		}
		return nil
	})
}

// pruneTranscriptLogs rewrites each .log file in dir, keeping only entries
// newer than maxAge.
func pruneTranscriptLogs(dir string, maxAge time.Duration, logger *log.Logger) {
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".log" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var kept []byte
		for _, line := range bytes.Split(data, []byte("\n")) {
			if len(line) == 0 {
				continue
			}
			var ev struct {
				Timestamp time.Time `json:"timestamp"`
			}
			if err := json.Unmarshal(line, &ev); err != nil || ev.Timestamp.After(cutoff) {
				kept = append(kept, line...)
				kept = append(kept, '\n')
			}
		}
		if err := os.WriteFile(path, kept, 0644); err != nil {
			logger.Printf("pruning transcript log %s: %v", path, err)
		}
	}
}

func (s *station) addSubscriber(pc *webrtc.PeerConnection) (string, error) {
	track, err := webrtc.NewTrackLocalStaticSample(s.codec, "audio", s.info.ID)
	if err != nil {
		return "", err
	}

	sender, err := pc.AddTrack(track)
	if err != nil {
		return "", err
	}

	go drainRTCP(sender)

	id := fmt.Sprintf("%s-peer-%d", s.info.ID, s.nextID.Add(1))

	s.mu.Lock()
	s.subscribers[id] = &subscriber{
		pc:    pc,
		track: track,
	}
	s.mu.Unlock()

	return id, nil
}

func (s *station) removeSubscriber(id string) {
	s.mu.Lock()
	sub, ok := s.subscribers[id]
	if ok {
		delete(s.subscribers, id)
	}
	s.mu.Unlock()

	if ok && sub.pc.ConnectionState() != webrtc.PeerConnectionStateClosed {
		_ = sub.pc.Close()
	}
}

func (s *station) closeSubscribers() {
	s.mu.Lock()
	subscribers := s.subscribers
	s.subscribers = make(map[string]*subscriber)
	s.mu.Unlock()

	for _, sub := range subscribers {
		if sub.pc.ConnectionState() != webrtc.PeerConnectionStateClosed {
			_ = sub.pc.Close()
		}
	}
}

func (s *station) broadcast(sample media.Sample) {
	s.mu.RLock()
	targets := make(map[string]*webrtc.TrackLocalStaticSample, len(s.subscribers))
	for id, sub := range s.subscribers {
		targets[id] = sub.track
	}
	s.mu.RUnlock()

	for id, track := range targets {
		if err := track.WriteSample(sample); err != nil {
			s.logger.Printf("%s: dropping %s after track write failure: %v", s.info.StreamName, id, err)
			s.removeSubscriber(id)
		}
	}
}

func (s *webrtcServer) handleStreams(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(s.regionGroups); err != nil {
		http.Error(w, "failed to encode streams", http.StatusInternalServerError)
	}
}

func (s *webrtcServer) handleOffer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	defer r.Body.Close()

	var request offerRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid offer body", http.StatusBadRequest)
		return
	}

	if request.StreamID == "" {
		http.Error(w, "missing streamId", http.StatusBadRequest)
		return
	}
	if request.Offer.Type != webrtc.SDPTypeOffer {
		http.Error(w, "expected an SDP offer", http.StatusBadRequest)
		return
	}

	station, ok := s.streams[request.StreamID]
	if !ok {
		http.Error(w, "unknown stream", http.StatusNotFound)
		return
	}

	pc, err := s.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		http.Error(w, "failed to create peer connection", http.StatusInternalServerError)
		return
	}

	peerID, err := station.addSubscriber(pc)
	if err != nil {
		_ = pc.Close()
		http.Error(w, "failed to add audio track", http.StatusInternalServerError)
		return
	}

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		s.logger.Printf("%s %s state: %s", station.info.StreamName, peerID, state.String())
		switch state {
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			station.removeSubscriber(peerID)
		}
	})

	if err := pc.SetRemoteDescription(request.Offer); err != nil {
		station.removeSubscriber(peerID)
		http.Error(w, "failed to apply remote description", http.StatusBadRequest)
		return
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		station.removeSubscriber(peerID)
		http.Error(w, "failed to create answer", http.StatusInternalServerError)
		return
	}

	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		station.removeSubscriber(peerID)
		http.Error(w, "failed to set local description", http.StatusInternalServerError)
		return
	}

	<-gatherComplete

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(pc.LocalDescription()); err != nil {
		station.removeSubscriber(peerID)
	}
}

func extractAudioFrame(payload []byte) ([]byte, error) {
	if len(payload) < skipBytes+frameSizeBytes {
		return nil, fmt.Errorf("packet is %d bytes, need at least %d", len(payload), skipBytes+frameSizeBytes)
	}

	return bytes.Clone(payload[skipBytes : skipBytes+frameSizeBytes]), nil
}

func drainRTCP(sender *webrtc.RTPSender) {
	buffer := make([]byte, 1500)
	for {
		if _, _, err := sender.Read(buffer); err != nil {
			return
		}
	}
}
