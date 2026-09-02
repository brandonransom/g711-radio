package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"embed"
	"encoding/csv"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
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

	"golang.org/x/crypto/pkcs12"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

var globalClipID atomic.Uint64
var globalStreamID atomic.Uint64

func nextClipID() string {
	return fmt.Sprintf("clip-%d", globalClipID.Add(1))
}

func nextStreamID() string {
	return fmt.Sprintf("stream-%d", globalStreamID.Add(1))
}

func shouldAutoTranscribe(cfg *whisperConfig, durationMs int) bool {
	return cfg != nil && cfg.AutoTranscribeMinClipMs > 0 && durationMs >= cfg.AutoTranscribeMinClipMs
}

const (
	frameSizeBytes = 160
	sampleRateHz   = 8000
	skipBytes      = 12
	configPath     = "config.json"
	secretsPath    = "config.secrets.json"
)

//go:embed web/*
var webFiles embed.FS

type appConfig struct {
	HTTPPort         int                                  `json:"httpPort"`
	HTTPRedirectPort int                                  `json:"httpRedirectPort"`
	EnableHTTP       bool                                 `json:"enableHttp"`
	DebugMulticast   bool                                 `json:"debugMulticast"`
	Regions          map[string]map[string][]streamConfig `json:"regions"`
	Whisper          *whisperConfig                       `json:"whisper"`
	Keepalive        *keepaliveConfig                      `json:"keepalive"`
	AudioLogDir      string                               `json:"audioLogDir"`
	UsageLogFile     string                               `json:"usageLogFile"`
	CertFile         string                               `json:"certFile"`
	KeyFile          string                               `json:"keyFile"`
	PFXFile          string                               `json:"pfxFile"`
	PFXPassword      string                               `json:"pfxPassword"`
	PFXKeyPassword   string                               `json:"pfxKeyPassword"`

	streamGroups []configuredRegion
	totalStreams  int
}

type streamConfig struct {
	StreamName    string   `json:"streamName"`
	UDPPort       int      `json:"udpPort"`       // deprecated: use UDPPorts for multicast
	UDPPorts      []int    `json:"udpPorts"`      // list of UDP ports to listen on (supports multicast)
	MulticastAddr string   `json:"multicastAddr"` // single multicast group (if any) — deprecated
	MulticastAddrs []string `json:"multicastAddrs"` // list of multicast group addresses
	DebugMulticast bool     `json:"debugMulticast"`
}

type streamInfo struct {
	RegionName string `json:"regionName"`
	GroupName  string `json:"groupName"`
	ForestName string `json:"forestName"`
	ID         string `json:"id"`
	StreamName string `json:"streamName"`
	UDPPort    int    `json:"udpPort"`
}

func (s streamInfo) displayName() string {
	return fmt.Sprintf("%s / %s / %s", s.RegionName, s.GroupName, s.StreamName)
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
	debugMulticast bool
	usageLogger   *usageLogger
	audioLogDir   string

	// whisperPool is non-nil when transcription is enabled.
	whisperPool *whisperPool
	vad         *vadState

	nextID      atomic.Uint64
	mu          sync.RWMutex
	subscribers map[string]*subscriber

	// multicast listener (non-nil if using multiple ports/multicast addresses)
	multicastListener *MulticastListener

	// packet health tracking (protected by mu)
	lastPacketAt       time.Time
	sourceAddr         string // first/expected source IP (no port)
	conflictAddr       string // non-empty when a second source IP is detected
	conflictClearCount int    // consecutive packets from a single source seen after a conflict
	conflictSingleSrc  string // which IP the consecutive run is from (primary or conflict addr)
}

type clipRecord struct {
	clipID   string
	info     streamInfo
	wavPath  string
	audioURL string
	start    time.Time
	duration int
}

type subscriber struct {
	pc            *webrtc.PeerConnection
	track         *webrtc.TrackLocalStaticSample
	clientIP      string
	connectionAt  time.Time
}

type usageLogger struct {
	mu     sync.Mutex
	file   *os.File
	writer *csv.Writer
	close  chan struct{}
}

type offerRequest struct {
	StreamID string                    `json:"streamId"`
	Offer    webrtc.SessionDescription `json:"offer"`
}

type webrtcServer struct {
	api          *webrtc.API
	logger       *log.Logger
	usageLogger  *usageLogger
	streams      map[string]*station
	regionGroups []regionGroup
	hub          *transcriptHub
	clips        map[string]clipRecord
	clipMu       sync.RWMutex
	whisperPool  *whisperPool
	audioLogDir  string
	keepaliveManager *keepaliveManager
}

func (s *webrtcServer) storeClip(rec clipRecord) {
	s.clipMu.Lock()
	s.clips[rec.clipID] = rec
	s.clipMu.Unlock()
}

func (s *webrtcServer) requestClipTranscription(clipID string) bool {
	s.clipMu.RLock()
	rec, ok := s.clips[clipID]
	s.clipMu.RUnlock()
	if !ok || s.whisperPool == nil {
		return false
	}
	if rec.wavPath == "" {
		return false
	}
	s.whisperPool.Submit(transcriptJob{
		info:     rec.info,
		clipID:   rec.clipID,
		wavPath:  rec.wavPath,
		audioURL: rec.audioURL,
		start:    rec.start,
		manual:   true,
	})
	return true
}

func (s *webrtcServer) handleTranscriptRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	clientIP := getClientIP(r)

	var req struct {
		ClipID   string `json:"clipId"`
		AudioURL string `json:"audioUrl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ClipID == "" {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	// Try registry first (clips from current server run).
	if s.requestClipTranscription(req.ClipID) {
		s.usageLogger.logUsage("transcript_request", map[string]string{
			"client_ip": clientIP,
			"clip_id":   req.ClipID,
			"source":    "registry",
		})
		w.WriteHeader(http.StatusAccepted)
		return
	}
	// Fallback: derive wavPath from audioUrl for clips from before a server restart.
	if req.AudioURL != "" && s.audioLogDir != "" && s.whisperPool != nil {
		// audioUrl is /audio/<region>/<group>/<stream>/<file>.wav
		// Strip the leading /audio/ prefix and convert to a local path.
		rel := strings.TrimPrefix(req.AudioURL, "/audio/")
		wavPath := filepath.Join(s.audioLogDir, filepath.FromSlash(rel))
		if _, err := os.Stat(wavPath); err == nil {
			s.logger.Printf("transcribe: clip %q not in registry, using audioUrl fallback wav=%s", req.ClipID, wavPath)
			s.whisperPool.Submit(transcriptJob{
				clipID:   req.ClipID,
				wavPath:  wavPath,
				audioURL: req.AudioURL,
				start:    time.Now(),
				manual:   true,
			})
			s.usageLogger.logUsage("transcript_request", map[string]string{
				"client_ip": clientIP,
				"clip_id":   req.ClipID,
				"source":    "audiourl_fallback",
			})
			w.WriteHeader(http.StatusAccepted)
			return
		}
		s.logger.Printf("transcribe: clip %q audioUrl fallback wav=%s not found on disk", req.ClipID, wavPath)
	}
	http.Error(w, "clip not found or transcription unavailable", http.StatusNotFound)
}

// getListenerConfig extracts UDP ports and multicast addresses from streamConfig.
// Returns (ports, addresses, error).
// Ports are ordered: if UDPPorts is present, use it; otherwise use UDPPort.
// Addresses are ordered: if MulticastAddrs is present, use it; otherwise use MulticastAddr.
// If not enough addresses are provided for the ports, empty strings fill the gaps (indicating unicast).
func getListenerConfig(cfg streamConfig) ([]int, []string, error) {
	var ports []int
	var addresses []string

	// Extract ports
	if len(cfg.UDPPorts) > 0 {
		ports = cfg.UDPPorts
	} else if cfg.UDPPort > 0 {
		ports = []int{cfg.UDPPort}
	}

	if len(ports) == 0 {
		return nil, nil, fmt.Errorf("stream has no ports configured")
	}
	if len(ports) > 4 {
		return nil, nil, fmt.Errorf("stream has %d ports; maximum is 4", len(ports))
	}

	// Extract addresses
	if len(cfg.MulticastAddrs) > 0 {
		addresses = cfg.MulticastAddrs
	} else if cfg.MulticastAddr != "" {
		addresses = []string{cfg.MulticastAddr}
	}

	// Pad addresses with empty strings (for unicast ports)
	for len(addresses) < len(ports) {
		addresses = append(addresses, "")
	}

	return ports, addresses, nil
}

// ingestMulticast is called when a stream uses multicast listener.
func (s *station) ingestMulticast(ctx context.Context) error {
	if s.multicastListener == nil {
		return fmt.Errorf("multicast listener not initialized")
	}

	packetsSeen := 0
	for {
		select {
		case <-ctx.Done():
			return nil
		case packet, ok := <-s.multicastListener.FrameChan():
			if !ok {
				// Channel closed
				return nil
			}
			frame := packet.data

			now := time.Now()
			s.mu.Lock()
			s.lastPacketAt = now
			s.mu.Unlock()

			if packetsSeen == 0 {
				s.logger.Printf(
					"%s: first multicast packet received from %s, audio_bytes=%d, skip_bytes=%d",
					s.info.StreamName,
					packet.sourceIP,
					len(frame),
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
}

func main() {
	// Determine log file path (same directory as the executable).
	logFilePath, logFileErr := resolveLogFilePath()

	var logWriter io.Writer = os.Stdout
	var logFile *os.File
	if logFileErr == nil {
		f, err := os.OpenFile(logFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err == nil {
			logFile = f
			logWriter = io.MultiWriter(os.Stdout, f)
		} else {
			logFileErr = err
		}
	}
	logger := log.New(logWriter, "", log.LstdFlags)
	if logFileErr != nil {
		logger.Printf("WARNING: could not open log file: %v", logFileErr)
	} else {
		logger.Printf("logging to file: %s", logFilePath)
		defer logFile.Close()
	}

	config, err := loadConfig(configPath)
	if err != nil {
		logger.Fatal(err)
	}

	usageLog, err := newUsageLogger(config.UsageLogFile)
	if err != nil {
		logger.Printf("WARNING: could not open usage log file: %v", err)
		usageLog, _ = newUsageLogger("")
	}
	if config.UsageLogFile != "" {
		logger.Printf("usage log file: %s", config.UsageLogFile)
		defer usageLog.Close()
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
	if config.Whisper != nil && (config.Whisper.ModelPath != "" || config.Whisper.RemoteHost != "") {
		config.Whisper.setDefaults()
		if config.Whisper.RemoteHost == "" && config.Whisper.BinaryPath == "" {
			config.Whisper.BinaryPath = "WhisperCLI.exe"
		}
		pool = newWhisperPool(*config.Whisper, hub, logger)
		pool.Start()
		if config.Whisper.RemoteHost != "" {
			logger.Printf("whisper transcription enabled: remote host=%s workers=%d", config.Whisper.RemoteHost, config.Whisper.Workers)
		} else {
			logger.Printf("whisper transcription enabled: model=%s workers=%d", config.Whisper.ModelPath, config.Whisper.Workers)
		}
	}

	server := &webrtcServer{
		api:          api,
		logger:       logger,
		usageLogger:  usageLog,
		streams:      make(map[string]*station, config.totalStreams),
		regionGroups: make([]regionGroup, 0, len(config.streamGroups)),
		hub:          hub,
		clips:        make(map[string]clipRecord),
		whisperPool:  pool,
		audioLogDir:  config.AudioLogDir,
		keepaliveManager: newKeepaliveManager(config.Keepalive, logger),
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
					ForestName: sg.GroupName,
					ID:         nextStreamID(),
					StreamName: cfg.StreamName,
					UDPPort:    cfg.UDPPort,
				}

				st := &station{
					info:          info,
					codec:         codec,
					frameDuration: frameDuration,
					logger:        logger,
					usageLogger:   server.usageLogger,
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
								clipID := nextClipID()
								var wavPath, audioURL string
								durationMs := len(samples) * 1000 / vadSampleRate
								if captureAudioLogDir != "" {
									var err error

									wavPath, audioURL, err = saveAudioClip(captureAudioLogDir, captureInfo, samples, start, logger)
									if err != nil {
										logger.Printf("audio log: %v", err)
									}
								}
								// requestWavPath is the file used for on-demand transcription.
								// Prefer the persisted audio log file; fall back to a temp file.
								var requestWavPath string
								if wavPath != "" {
									requestWavPath = wavPath
								} else if pool != nil {
									wav, _ := encodePCM16WAV(samples, vadSampleRate)
									tmp, err := os.CreateTemp("", "g711-whisper-*.wav")
									if err == nil {
										if _, err := tmp.Write(wav); err == nil {
											_ = tmp.Close()
											requestWavPath = tmp.Name()
										} else {
											_ = tmp.Close()
											_ = os.Remove(tmp.Name())
										}
								}
								}
								// Publish clip event immediately so the UI shows the recording.
								hub.Publish(transcriptEvent{
									Type:       "clip",
									ClipID:     clipID,
									StreamID:   captureInfo.ID,
									StreamName: captureInfo.StreamName,
									RegionName: captureInfo.RegionName,
									GroupName:  captureInfo.GroupName,
									AudioURL:   audioURL,
									DurationMs: durationMs,
									Timestamp:  start,
								})
								server.storeClip(clipRecord{
									clipID:   clipID,
									info:     captureInfo,
									wavPath:  requestWavPath,
									audioURL: audioURL,
									start:    start,
									duration: durationMs,
								})
								if pool != nil && shouldAutoTranscribe(wCfg, durationMs) {
									server.requestClipTranscription(clipID)
								}
							},
						)
					}

				// Extract ports and addresses from config
				ports, addresses, err := getListenerConfig(cfg)
				if err != nil {
					logger.Fatalf("invalid stream config for %q: %v", cfg.StreamName, err)
				}

				// Use the multicast listener whenever any configured address is multicast.
				useMulticast := false
				for _, addr := range addresses {
					if addr != "" {
						useMulticast = true
						break
					}
				}

				if useMulticast {
					// Use multicast listener for multiple ports
					ml, err := NewMulticastListener(cfg.StreamName, ports, addresses, 1*time.Second, logger, cfg.DebugMulticast)
					if err != nil {
						logger.Fatalf("failed to create multicast listener for %q: %v", cfg.StreamName, err)
					}
					st.multicastListener = ml

					server.streams[info.ID] = st
					apiSubGroup.Streams = append(apiSubGroup.Streams, info)

					logger.Printf(
						"configured stream %q in region %q, forest %q on UDP ports %v, codec=PCMU, frame_size=%d bytes, skip_bytes=%d, frame_duration=%s",
						info.StreamName,
						region.RegionName,
						sg.GroupName,
						ports,
						frameSizeBytes,
						skipBytes,
						frameDuration,
					)

					// Start integrated keepalive bursts for multicast addresses.
					if server.keepaliveManager != nil && server.keepaliveManager.config.Enabled {
						if err := server.keepaliveManager.startForAddresses(addresses); err != nil {
							logger.Printf("warning: failed to start keepalive for %q: %v", cfg.StreamName, err)
						}
					}

					ml.Start()

					go func(st *station, ml *MulticastListener) {
						<-ctx.Done()
						ml.Close()
						st.closeSubscribers()
					}(st, ml)

					go func(st *station) {
						if err := st.ingestMulticast(ctx); err != nil {
							logger.Printf("%s ingest stopped: %v", st.info.StreamName, err)
							stop()
						}
					}(st)
				} else {
					// Use single UDP port (original behavior)
					conn, err := net.ListenPacket("udp", fmt.Sprintf(":%d", ports[0]))
					if err != nil {
						logger.Fatalf("listen on UDP %d for %q: %v", ports[0], cfg.StreamName, err)
					}

					server.streams[info.ID] = st
					apiSubGroup.Streams = append(apiSubGroup.Streams, info)

					logger.Printf(
						"configured stream %q in region %q, forest %q on UDP %d, codec=PCMU, frame_size=%d bytes, skip_bytes=%d, frame_duration=%s",
						info.StreamName,
						region.RegionName,
						sg.GroupName,
						ports[0],
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
	mux.HandleFunc("/stream-status", server.handleStreamStatus)
	mux.HandleFunc("/offer", server.handleOffer)
	mux.HandleFunc("/transcripts/request", server.handleTranscriptRequest)
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
		audioHandler := http.StripPrefix("/audio/", http.FileServer(http.Dir(config.AudioLogDir)))
		mux.Handle("/audio/", audioLoggingMiddleware(audioHandler, usageLog))
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
		if server.keepaliveManager != nil {
			server.keepaliveManager.close()
		}
	}()

	// Background cleanup: delete audio WAV files, prune transcript logs older than 72h,
	// and prune the server log file (entries older than 90 days).
	const retentionPeriod = 72 * time.Hour
	const logRetentionPeriod = 90 * 24 * time.Hour
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
				if logFileErr == nil {
					pruneLogFile(logFilePath, logRetentionPeriod, logger)
				}
			}
		}
	}()

	logger.Printf("loaded %d stream(s) from %s", config.totalStreams, configPath)

	redirectMux := http.NewServeMux()
	redirectMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		target := "https://" + r.Host + r.URL.RequestURI()
		http.Redirect(w, r, target, http.StatusMovedPermanently)
	})
	redirectServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", config.HTTPRedirectPort),
		Handler: redirectMux,
	}

	if !config.EnableHTTP {
		logger.Printf("HTTP listener disabled by config (enableHttp=false)")
		<-ctx.Done()
		return
	}

	if config.HTTPRedirectPort != 0 {
		go func() {
			logger.Printf("redirecting http://localhost:%d to https://", config.HTTPRedirectPort)
			if err := redirectServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Fatal(err)
			}
		}()
	}

	if config.PFXFile != "" {
		tlsCert, tlsErr := buildTLSCertFromPFX(config.PFXFile, config.PFXPassword, config.PFXKeyPassword, logger)
		if tlsErr != nil {
			logger.Fatal(tlsErr)
		} else {
			httpServer.TLSConfig = &tls.Config{
				Certificates: []tls.Certificate{tlsCert},
				MinVersion:   tls.VersionTLS12,
			}
			logger.Printf("serving WebRTC client on https://localhost:%d (PFX: %s)", config.HTTPPort, config.PFXFile)
			if err := httpServer.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Fatal(err)
			}
		}
	} else if config.CertFile != "" && config.KeyFile != "" {
		logger.Printf("serving WebRTC client on https://localhost:%d", config.HTTPPort)
		if err := httpServer.ListenAndServeTLS(config.CertFile, config.KeyFile); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatal(err)
		}
	} else {
		logger.Fatal("HTTPS is required but no certificate configuration was provided")
	}
}

func buildTLSCertFromPFX(pfxFile, pfxPassword, keyPassword string, logger *log.Logger) (tls.Certificate, error) {
	pfxData, err := os.ReadFile(pfxFile)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("read PFX: %w", err)
	}
	
	// Try to decode with the PFX password first. If that fails and a separate key password
	// is provided, try again with the key password (some CAs use separate passwords).
	blocks, err := pkcs12.ToPEM(pfxData, pfxPassword)
	if err != nil && keyPassword != "" {
		logger.Printf("TLS: initial PFX decode failed, retrying with keyPassword")
		blocks, err = pkcs12.ToPEM(pfxData, keyPassword)
	}
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("decode PFX: %w", err)
	}
	
	var keyPEM []byte
	var certPEMs [][]byte
	for _, b := range blocks {
		switch b.Type {
		case "PRIVATE KEY", "RSA PRIVATE KEY", "EC PRIVATE KEY":
			keyPEM = pem.EncodeToMemory(b)
		case "CERTIFICATE":
			certPEMs = append(certPEMs, pem.EncodeToMemory(b))
		}
	}
	if keyPEM == nil {
		return tls.Certificate{}, fmt.Errorf("no private key found in PFX")
	}
	var allCertDER [][]byte
	for _, pemBytes := range certPEMs {
		p, _ := pem.Decode(pemBytes)
		if p != nil {
			allCertDER = append(allCertDER, p.Bytes)
		}
	}
	var tlsCert tls.Certificate
	var leafIdx int
	for i, certBlock := range certPEMs {
		c, e := tls.X509KeyPair(certBlock, keyPEM)
		if e == nil {
			tlsCert = c
			leafIdx = i
			break
		}
	}
	if tlsCert.PrivateKey == nil {
		return tls.Certificate{}, fmt.Errorf("no certificate in PFX matches the private key")
	}
	// Build chain in order: leaf → issuing CA → ... → root.
	leaf, _ := x509.ParseCertificate(allCertDER[leafIdx])
	bySubject := make(map[string][]byte)
	for i, der := range allCertDER {
		if i == leafIdx {
			continue
		}
		if c, e := x509.ParseCertificate(der); e == nil {
			bySubject[c.Subject.String()] = der
		}
	}
	tlsCert.Certificate = [][]byte{allCertDER[leafIdx]}
	current := leaf
	for {
		issuerDER, ok := bySubject[current.Issuer.String()]
		if !ok {
			break
		}
		tlsCert.Certificate = append(tlsCert.Certificate, issuerDER)
		next, err := x509.ParseCertificate(issuerDER)
		if err != nil || next.Subject.String() == next.Issuer.String() {
			break
		}
		current = next
	}
	logger.Printf("TLS chain: %d cert(s) loaded", len(tlsCert.Certificate))
	for idx, der := range tlsCert.Certificate {
		if c, e := x509.ParseCertificate(der); e == nil {
			logger.Printf("  [%d] Subject=%s  Issuer=%s  IsCA=%v", idx, c.Subject.CommonName, c.Issuer.CommonName, c.IsCA)
		}
	}
	return tlsCert, nil
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

	// Overlay config.secrets.json if present (passwords and other sensitive values).
	if sf, err := os.Open(secretsPath); err == nil {
		defer sf.Close()
		var secrets struct {
			PFXPassword    string `json:"pfxPassword"`
			PFXKeyPassword string `json:"pfxKeyPassword"`
			CertFile       string `json:"certFile"`
			KeyFile        string `json:"keyFile"`
		}
		if err := json.NewDecoder(sf).Decode(&secrets); err != nil {
			return appConfig{}, fmt.Errorf("decode %s: %w", secretsPath, err)
		}
		if secrets.PFXPassword != "" {
			config.PFXPassword = secrets.PFXPassword
		}
		if secrets.PFXKeyPassword != "" {
			config.PFXKeyPassword = secrets.PFXKeyPassword
		}
		if secrets.CertFile != "" {
			config.CertFile = secrets.CertFile
		}
		if secrets.KeyFile != "" {
			config.KeyFile = secrets.KeyFile
		}
	}
	// Default to enabled for backward compatibility with existing configs.
	// If enableHttp is omitted from config, it decodes as false; treat omission as true.
	var hasEnableHTTPField bool
	if cfgBytes, readErr := os.ReadFile(path); readErr == nil {
		var raw map[string]json.RawMessage
		if unmarshalErr := json.Unmarshal(cfgBytes, &raw); unmarshalErr == nil {
			_, hasEnableHTTPField = raw["enableHttp"]
		}
	}
	if !hasEnableHTTPField {
		config.EnableHTTP = true
	}
	if config.HTTPRedirectPort == 0 {
		config.HTTPRedirectPort = 80
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
			
				// Validate ports: UDPPorts (plural) takes precedence
				var portsToValidate []int
				if len(stream.UDPPorts) > 0 {
					portsToValidate = stream.UDPPorts
				} else if stream.UDPPort > 0 {
					portsToValidate = []int{stream.UDPPort}
				} else {
					return nil, 0, fmt.Errorf("%s region %q group %q entry %d has no ports configured (udpPort or udpPorts)", path, regionName, groupName, i)
				}

				if stream.MulticastAddr != "" && len(stream.MulticastAddrs) == 0 {
					stream.MulticastAddrs = []string{stream.MulticastAddr}
				}
				if len(stream.MulticastAddrs) == 1 && len(portsToValidate) > 1 {
					addr := strings.TrimSpace(stream.MulticastAddrs[0])
					stream.MulticastAddrs = make([]string, len(portsToValidate))
					for idx := range stream.MulticastAddrs {
						stream.MulticastAddrs[idx] = addr
					}
				}
				if len(stream.MulticastAddrs) > 1 && len(stream.MulticastAddrs) != len(portsToValidate) {
					return nil, 0, fmt.Errorf("%s region %q group %q entry %d has %d multicast addrs for %d ports; provide one address or one per port", path, regionName, groupName, i, len(stream.MulticastAddrs), len(portsToValidate))
				}
			
				if len(portsToValidate) > 4 {
					return nil, 0, fmt.Errorf("%s region %q group %q entry %d has %d ports; maximum is 4", path, regionName, groupName, i, len(portsToValidate))
				}
			
				// Validate each port
				for _, port := range portsToValidate {
					if port < 1 || port > 65535 {
						return nil, 0, fmt.Errorf("%s region %q group %q entry %d has invalid UDP port %d", path, regionName, groupName, i, port)
					}
					if _, exists := seenPorts[port]; exists {
						return nil, 0, fmt.Errorf("%s has duplicate UDP port %d", path, port)
					}
					seenPorts[port] = struct{}{}
				}

				subGroup.Streams = append(subGroup.Streams, stream)
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

		addrStr := remoteAddr.String()
		// Extract just the IP for conflict detection — port changes on the same
		// encoder are normal and should not be treated as a conflict.
		sourceIP, _, ipErr := net.SplitHostPort(addrStr)
		if ipErr != nil {
			sourceIP = addrStr // fallback: use full string if parsing fails
		}
		now := time.Now()

		s.mu.Lock()
		s.lastPacketAt = now
		if packetsSeen == 0 {
			s.sourceAddr = sourceIP
		} else if sourceIP != s.sourceAddr && s.conflictAddr != sourceIP {
			if packetsSeen < 500 {
				s.mu.Unlock()
				packetsSeen++
				if s.vad != nil {
					s.vad.Push(DecodePCMU(frame), time.Now())
				}
				s.broadcast(media.Sample{Data: frame, Duration: s.frameDuration})
				continue
			}
			// New conflicting source IP detected.
			s.conflictAddr = sourceIP
			s.conflictClearCount = 0
			s.conflictSingleSrc = ""
			s.mu.Unlock()
			s.logger.Printf(
				"WARNING: %s (UDP %d) is receiving packets from multiple sources: expected %s, also receiving from %s — this will cause audio problems",
				s.info.StreamName, s.info.UDPPort, s.sourceAddr, sourceIP,
			)
			s.mu.Lock()
		} else if s.conflictAddr != "" {
			// Conflict is active. Count consecutive packets from a single source
			// (either the primary or the conflicting addr). If 500 consecutive
			// packets arrive from only one IP, treat the conflict as resolved and
			// promote that IP as the new primary. This handles both the normal
			// case (original encoder returns) and the case where the encoder was
			// replaced by the "conflicting" source and the old one is gone.
			if sourceIP == s.conflictSingleSrc {
				s.conflictClearCount++
				if s.conflictClearCount >= 500 {
							oldConflict := s.conflictAddr
							if sourceIP == s.conflictAddr {
								// The "conflicting" source won — promote it as the new primary.
								s.sourceAddr = sourceIP
							}
							s.conflictAddr = ""
							s.conflictClearCount = 0
							s.conflictSingleSrc = ""
							newPrimary := s.sourceAddr
							s.mu.Unlock()
							s.logger.Printf(
								"INFO: %s (UDP %d) UDP source conflict resolved — packets now arriving only from %s (was also %s)",
								s.info.StreamName, s.info.UDPPort, newPrimary, oldConflict,
							)
							s.mu.Lock()
						}
			} else if sourceIP == s.sourceAddr || sourceIP == s.conflictAddr {
				// Switched to the other known source — start/reset the consecutive run.
				s.conflictSingleSrc = sourceIP
				s.conflictClearCount = 1
			} else {
				// A third unexpected source — reset.
				s.conflictSingleSrc = ""
				s.conflictClearCount = 0
			}
		}
		s.mu.Unlock()

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

// audioLoggingMiddleware wraps an http.Handler to log audio file downloads
func audioLoggingMiddleware(handler http.Handler, usageLogger *usageLogger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientIP := getClientIP(r)
		filePath := r.URL.Path
		// Log audio download requests
		usageLogger.logUsage("audio_download", map[string]string{
			"client_ip": clientIP,
			"path":      filePath,
		})
		handler.ServeHTTP(w, r)
	})
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

// getClientIP extracts the client IP address from an HTTP request.
// Checks X-Forwarded-For header first (for proxied connections), then falls back to RemoteAddr.
func getClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// X-Forwarded-For can contain multiple IPs; use the first one
		if idx := strings.Index(xff, ","); idx != -1 {
			return strings.TrimSpace(xff[:idx])
		}
		return strings.TrimSpace(xff)
	}
	// Extract IP from RemoteAddr (format: "IP:port")
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// newUsageLogger creates a CSV logger for usage events. If path is empty, no logging occurs.
func newUsageLogger(path string) (*usageLogger, error) {
	if path == "" {
		return &usageLogger{}, nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	ul := &usageLogger{file: f, writer: csv.NewWriter(f), close: make(chan struct{})}
	// Write header if file is empty
	info, _ := f.Stat()
	if info.Size() == 0 {
		_ = ul.writer.Write([]string{"timestamp", "action", "client_ip", "stream", "peer_id", "clip_id", "source", "duration_ms", "path"})
		ul.writer.Flush()
	}
	return ul, nil
}

// logUsage writes a usage event to the CSV file with flexible fields.
func (ul *usageLogger) logUsage(action string, fields map[string]string) {
	if ul.file == nil {
		return
	}
	ul.mu.Lock()
	defer ul.mu.Unlock()
	
	timestamp := time.Now().Format(time.RFC3339)
	row := []string{
		timestamp,
		action,
		fields["client_ip"],
		fields["stream"],
		fields["peer_id"],
		fields["clip_id"],
		fields["source"],
		fields["duration_ms"],
		fields["path"],
	}
	_ = ul.writer.Write(row)
	ul.writer.Flush()
}

// Close closes the usage logger file.
func (ul *usageLogger) Close() error {
	if ul.file != nil {
		ul.mu.Lock()
		if ul.writer != nil {
			ul.writer.Flush()
		}
		ul.mu.Unlock()
		return ul.file.Close()
	}
	return nil
}

// resolveLogFilePath returns the path to the server log file (g711-radio.log)
// in the current working directory, so it works consistently with both
// "go run ." and running the compiled binary.
func resolveLogFilePath() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "g711-radio.log"), nil
}

// pruneLogFile removes lines from the plain-text log file that are older than maxAge.
// Log lines are expected to start with the standard log prefix: "YYYY/MM/DD HH:MM:SS ".
func pruneLogFile(path string, maxAge time.Duration, logger *log.Logger) {
	f, err := os.OpenFile(path, os.O_RDWR, 0644)
	if err != nil {
		return
	}
	defer f.Close()

	cutoff := time.Now().Add(-maxAge)
	var kept []byte
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Bytes()
		// Standard log prefix is "2006/01/02 15:04:05 " (20 chars).
		if len(line) >= 20 {
			t, err := time.ParseInLocation("2006/01/02 15:04:05", string(line[:19]), time.Local)
			if err == nil && t.Before(cutoff) {
				continue // drop old line
			}
		}
		kept = append(kept, line...)
		kept = append(kept, '\n')
	}

	if err := f.Truncate(0); err != nil {
		logger.Printf("pruning log file %s (truncate): %v", path, err)
		return
	}
	if _, err := f.WriteAt(kept, 0); err != nil {
		logger.Printf("pruning log file %s (write): %v", path, err)
	}
}

func (s *station) addSubscriber(pc *webrtc.PeerConnection, clientIP string) (string, error) {
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
		pc:           pc,
		track:        track,
		clientIP:     clientIP,
		connectionAt: time.Now(),
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

	// Log connection duration
	if ok && !sub.connectionAt.IsZero() {
		duration := time.Since(sub.connectionAt)
		s.usageLogger.logUsage("disconnect", map[string]string{
			"stream":      s.info.StreamName,
			"peer_id":     id,
			"client_ip":   sub.clientIP,
			"duration_ms": fmt.Sprintf("%d", duration.Milliseconds()),
		})
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

	// Disable caching — browser must always fetch fresh stream list from server
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(s.regionGroups); err != nil {
		http.Error(w, "failed to encode streams", http.StatusInternalServerError)
	}
}

type streamStatus struct {
	ID            string `json:"id"`
	HeardToday    bool   `json:"heardToday"`
	HasConflict   bool   `json:"hasConflict"`
	ConflictAddr  string `json:"conflictAddr,omitempty"`
	StatusMessage string `json:"statusMessage"`
}

func (s *webrtcServer) handleStreamStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Disable caching — browser must always fetch fresh status from server
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Header().Set("Content-Type", "application/json")

	cutoff := time.Now().Add(-24 * time.Hour)
	statuses := make([]streamStatus, 0, len(s.streams))
	for id, st := range s.streams {
		st.mu.RLock()
		last := st.lastPacketAt
		conflict := st.conflictAddr
		streamName := st.info.StreamName
		st.mu.RUnlock()

		// Consider a stream "heard today" if live packets arrived in the last 24h,
		// OR if the transcript/recording log has a clip event in that window
		// (covers cases where the server was restarted and lastPacketAt reset).
		heardToday := (!last.IsZero() && last.After(cutoff)) ||
			s.hub.HasRecentActivity(id, 24*time.Hour)

		// Generate server-side status message — decision logic stays on server
		statusMessage := ""
		if conflict != "" {
			statusMessage = "⚠ Multiple audio sources detected for this stream — this will cause audio problems"
		} else if !heardToday {
			statusMessage = fmt.Sprintf(`Nothing heard from "%s" today`, streamName)
		}

		statuses = append(statuses, streamStatus{
			ID:            id,
			HeardToday:    heardToday,
			HasConflict:   conflict != "",
			ConflictAddr:  conflict,
			StatusMessage: statusMessage,
		})
	}

	json.NewEncoder(w).Encode(statuses)
}

func (s *webrtcServer) handleOffer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	defer r.Body.Close()

	clientIP := getClientIP(r)

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

	peerID, err := station.addSubscriber(pc, clientIP)
	if err != nil {
		_ = pc.Close()
		http.Error(w, "failed to add audio track", http.StatusInternalServerError)
		return
	}

	// Log new connection
	s.usageLogger.logUsage("connect", map[string]string{
		"stream":    station.info.StreamName,
		"peer_id":   peerID,
		"client_ip": clientIP,
	})

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
