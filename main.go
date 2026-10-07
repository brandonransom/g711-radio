package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"embed"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
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

// nextStreamID returns a randomly generated, globally unique stream
// identifier. IDs must never be derived from a stream's position in the
// config file: inserting, removing, or reordering a stream entry would then
// silently reassign another stream's old ID to a different stream on the
// next restart, so a stale ID cached anywhere (a bookmarked link, a client's
// in-memory state) would silently attach to the wrong stream. Random IDs are
// re-generated fresh on every server start, so nothing should assume a
// stream's ID is stable across restarts — transcriptHub.History and
// HasRecentActivity key off the stream name for that reason.
func nextStreamID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand failing is effectively unheard of on real systems; fall
		// back to a counter so a transient entropy failure can't stop startup.
		return fmt.Sprintf("stream-fallback-%d", globalStreamID.Add(1))
	}
	return "stream-" + hex.EncodeToString(buf[:])
}

// shouldAutoTranscribe reports whether a finished clip should be queued for
// transcription automatically. A clip qualifies when its duration falls in
// [AutoTranscribeMinClipMs, AutoTranscribeMaxClipMs]. The minimum must be set
// for any automatic transcription to happen at all; the maximum is optional
// and zero means "no upper bound". Clips outside the window are still
// recorded and can be transcribed on demand from the web UI.
func shouldAutoTranscribe(cfg *whisperConfig, durationMs int) bool {
	if cfg == nil || cfg.AutoTranscribeMinClipMs <= 0 {
		return false
	}
	if durationMs < cfg.AutoTranscribeMinClipMs {
		return false
	}
	if cfg.AutoTranscribeMaxClipMs > 0 && durationMs > cfg.AutoTranscribeMaxClipMs {
		return false
	}
	return true
}

const (
	frameSizeBytes = 160 // G.711 µ-law audio payload size (8 bits/sample, 160 samples/20ms)
	sampleRateHz   = 8000
	skipBytes      = 12 // legacy/default header size; used only for startup diagnostics — actual header length is auto-detected per packet, see extractAudioFrame
	maxHeaderBytes = 64 // sanity cap on the auto-detected header length; guards against unrelated traffic being misread as audio
	configPath     = "config.json"
	secretsPath    = "config.secrets.json"
)

// wireCodec identifies which audio codec produced a UDP packet's payload.
// G.711 is currently the only supported wire codec (an earlier "Telex 32k"
// vocoder was attempted and removed after extensive reverse-engineering
// failed to produce usable audio quality — see git history. Source devices
// using that hardware should be reconfigured to transmit G.711 instead).
type wireCodec int

const (
	wireCodecG711 wireCodec = iota
)

func (c wireCodec) String() string {
	switch c {
	case wireCodecG711:
		return "G.711"
	default:
		return "unknown"
	}
}

// wireFrameSizes lists the known fixed audio-payload sizes that a packet's
// header length is derived from (see extractAudioFrame). Devices may be
// misconfigured; since detection is purely a function of packet size (not
// content), every packet on every stream is classified independently and
// automatically.
var wireFrameSizes = []struct {
	codec      wireCodec
	frameBytes int
}{
	{wireCodecG711, frameSizeBytes},
}

//go:embed web/*
var webFiles embed.FS

type appConfig struct {
	HTTPPort         int                                  `json:"httpPort"`
	HTTPRedirectPort int                                  `json:"httpRedirectPort"`
	EnableHTTP       bool                                 `json:"enableHttp"`
	DebugMulticast   bool                                 `json:"debugMulticast"`
	States           map[string]map[string][]streamConfig `json:"states"`
	// LegacyRegions is the pre-rename spelling of States, still accepted so
	// existing config files keep working. Setting both is an error.
	LegacyRegions map[string]map[string][]streamConfig `json:"regions"`
	Whisper       *whisperConfig                       `json:"whisper"` // all transcription settings (local and remote); see whisperConfig in whisper.go

	// AudioLogDir is the primary, user-facing audio archive: every
	// recorded clip is written here (see saveAudioClip), served over HTTP
	// at /audio/ for in-browser playback, and referenced by the
	// clip/transcript history. Point this at wherever the "live" copy
	// should live — a local path today, but nothing here assumes that; any
	// directory that behaves like a normal filesystem (including a mapped
	// network drive) works. Audio and transcripts are kept indefinitely —
	// nothing in this codebase ever deletes them (see the removed
	// pruneOldFiles/pruneTranscriptLogs in git history for the retention
	// logic this replaced).
	AudioLogDir string `json:"audioLogDir"`

	// AudioBackupDir, when non-empty, makes every recorded clip also get
	// written, byte-for-byte, to this second directory (same relative
	// state/group/stream path, same filename) — a redundant copy for
	// disaster recovery, not served over HTTP or referenced anywhere in
	// the UI. The copy is made at the moment each clip is saved, not by a
	// periodic sync. A write failure here (e.g. a temporarily unreachable
	// network mount) is logged but never blocks the primary write, clip
	// finalization, or transcription. Leave empty to disable.
	AudioBackupDir string `json:"audioBackupDir"`

	// AudioBackupDir2 is an optional third archive that works exactly like
	// AudioBackupDir. Either backup may be set without the other.
	AudioBackupDir2 string `json:"audioBackupDir2"`

	// RecordingIndex controls how the Recordings & Transcripts history is
	// served; see recordingIndexConfig in recording_index.go. Omit for the
	// default ("full").
	RecordingIndex recordingIndexConfig `json:"recordingIndex"`

	// UsageLogFile is obsolete (the usage CSV was removed). It is still
	// accepted so existing configs keep loading under DisallowUnknownFields,
	// but it is ignored.
	UsageLogFile string `json:"usageLogFile"`

	// Analytics enables anonymized visitor statistics and the unlinked
	// dashboard; see analyticsConfig in analytics.go. Omit to disable.
	Analytics *analyticsConfig `json:"analytics"`

	CertFile       string            `json:"certFile"`
	KeyFile        string            `json:"keyFile"`
	PFXFile        string            `json:"pfxFile"`
	PFXPassword    string            `json:"pfxPassword"`
	PFXKeyPassword string            `json:"pfxKeyPassword"`
	ICEServers     []iceServerConfig `json:"iceServers"`

	// AudioDumpDir, when non-empty, makes every station write raw
	// pipeline-stage dumps for offline diagnosis: <dir>/<streamName>_wire.bin
	// (the codec-native bytes exactly as received, pre-transcode),
	// <dir>/<streamName>_live_mulaw.bin (the exact µ-law bytes broadcast to
	// WebRTC — the "live" audio), and <dir>/<streamName>_timing.csv (a
	// per-packet log of arrival time and inter-packet gaps, to catch
	// real-time pacing issues that a batch-written WAV wouldn't reveal). Use
	// cmd/mulaw-to-wav to turn either .bin file into a WAV for listening.
	// Leave empty in normal operation — this is a diagnostic-only feature.
	AudioDumpDir string `json:"audioDumpDir"`

	streamGroups []configuredState
	totalStreams int
}

// iceServerConfig mirrors webrtc.ICEServer for JSON configuration. Without at
// least a STUN server, clients behind NAT (very commonly cellular/CGNAT
// connections) can only offer host candidates, so their connection attempts
// repeatedly gather, connect, and then fail.
type iceServerConfig struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username"`
	Credential string   `json:"credential"`
}

// defaultICEServers is used when the config omits "iceServers" entirely, so
// NAT traversal works out of the box instead of silently only working for
// clients on the same network as the server.
var defaultICEServers = []webrtc.ICEServer{
	{URLs: []string{"stun:stun.l.google.com:19302"}},
}

func (c appConfig) webrtcICEServers() []webrtc.ICEServer {
	if len(c.ICEServers) == 0 {
		return defaultICEServers
	}
	servers := make([]webrtc.ICEServer, 0, len(c.ICEServers))
	for _, s := range c.ICEServers {
		servers = append(servers, webrtc.ICEServer{
			URLs:       s.URLs,
			Username:   s.Username,
			Credential: s.Credential,
		})
	}
	return servers
}

type streamConfig struct {
	StreamName     string   `json:"streamName"`
	UDPPort        int      `json:"udpPort"`        // deprecated: use UDPPorts for multicast
	UDPPorts       []int    `json:"udpPorts"`       // list of UDP ports to listen on (supports multicast)
	MulticastAddr  string   `json:"multicastAddr"`  // single multicast group (if any) — deprecated
	MulticastAddrs []string `json:"multicastAddrs"` // list of multicast group addresses
	DebugMulticast bool     `json:"debugMulticast"`
	// DisableAutoTranscribe stops this stream's clips from being queued for
	// transcription automatically — e.g. when it carries the same audio as
	// another stream that is already transcribed. Clips are still recorded
	// and can be transcribed on demand from the web UI.
	DisableAutoTranscribe bool `json:"disableAutoTranscribe"`
	// TimeZone is the IANA zone the stream's radios are in (e.g.
	// "America/Anchorage"), used for the analytics time-of-day stats.
	// Empty means the server's local time zone.
	TimeZone string `json:"timeZone"`
}

type streamInfo struct {
	StateName  string `json:"stateName"`
	GroupName  string `json:"groupName"`
	ForestName string `json:"forestName"`
	ID         string `json:"id"`
	StreamName string `json:"streamName"`
	UDPPort    int    `json:"udpPort"`
	TimeZone   string `json:"timeZone,omitempty"`
}

func (s streamInfo) displayName() string {
	return fmt.Sprintf("%s / %s / %s", s.StateName, s.GroupName, s.StreamName)
}

type subGroup struct {
	GroupName string       `json:"groupName"`
	Streams   []streamInfo `json:"streams"`
}

type stateGroup struct {
	StateName string     `json:"stateName"`
	SubGroups []subGroup `json:"subGroups"`
}

type configuredSubGroup struct {
	GroupName string
	Streams   []streamConfig
}

type configuredState struct {
	StateName string
	SubGroups []configuredSubGroup
}

// audioFrame is one extracted, codec-tagged audio payload from a UDP packet.
type audioFrame struct {
	data        []byte
	codec       wireCodec
	headerBytes int
}

type station struct {
	info           streamInfo
	codec          webrtc.RTPCodecCapability
	frameDuration  time.Duration
	logger         *log.Logger
	debugMulticast bool
	audioLogDir    string

	// whisperPool is non-nil when transcription is enabled.
	whisperPool *whisperPool
	recorder    *recorderState

	// audioDumpDir/dump* support the diagnostic pipeline dump feature (see
	// appConfig.AudioDumpDir). Only touched by this station's single ingest
	// goroutine, opened lazily on first use. Writes go through a buffered
	// writer (flushed periodically, not per-packet) so the dump feature
	// itself doesn't introduce synchronous per-packet disk I/O on the ingest
	// hot path — that would confound exactly the kind of real-time pacing
	// measurement this feature exists to make.
	audioDumpDir     string
	dumpWireFile     *os.File
	dumpMulawFile    *os.File
	dumpTimingFile   *os.File
	dumpWireBuf      *bufio.Writer
	dumpMulawBuf     *bufio.Writer
	dumpTimingBuf    *bufio.Writer
	dumpLastPacketAt time.Time
	dumpPacketCount  int

	nextID      atomic.Uint64
	mu          sync.RWMutex
	subscribers map[string]*subscriber

	// broadcastChan decouples subscriber RTP writes from packet ingest; see
	// enqueueBroadcast/runBroadcaster.
	broadcastChan chan media.Sample

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
	pc    *webrtc.PeerConnection
	track *webrtc.TrackLocalStaticSample

	// ready is false until the underlying PeerConnection reports it has
	// actually connected (ICE + DTLS complete). Broadcasting audio to a
	// brand-new subscriber before then wastes writes on a transport that
	// isn't ready to send, and produces exactly the kind of bursty/delayed
	// delivery a real-time audio jitter buffer struggles with — see the
	// connection warm-up investigation in git history for measurements.
	ready atomic.Bool
}

type offerRequest struct {
	StreamID string                    `json:"streamId"`
	Offer    webrtc.SessionDescription `json:"offer"`
}

type webrtcServer struct {
	api         *webrtc.API
	logger      *log.Logger
	analytics   *analyticsStore
	streams     map[string]*station
	stateGroups []stateGroup
	hub         *transcriptHub
	clips       map[string]clipRecord
	clipMu      sync.RWMutex
	whisperPool *whisperPool
	feedback    *feedbackStore
	audioLogDir string
	iceServers  []webrtc.ICEServer

	// leaveTokens maps the random token handed to each listener in the
	// X-Listen-Token response header to its subscriber, so the page can say
	// "I'm leaving" via POST /listen-leave. Random (not the sequential peer
	// ID) so nobody can disconnect other listeners by guessing.
	leaveTokens sync.Map // string -> listenLease

	// clipJobs offloads recorder clip finalization (WAV file write, hub
	// publish, whisper submission) off each station's ingest goroutine.
	// Without this, that synchronous disk I/O blocks UDP packet reads for
	// the duration of the write, causing packets to queue up in the kernel
	// socket buffer and then get drained/broadcast in a burst — which live
	// WebRTC playback (unlike a batch-written WAV) is very sensitive to.
	clipJobs chan func()
}

// startClipWorkers launches the background workers that drain clipJobs.
// Call once at startup; workers run for the life of the process.
func (s *webrtcServer) startClipWorkers(n int) {
	for i := 0; i < n; i++ {
		go func() {
			for job := range s.clipJobs {
				job()
			}
		}()
	}
}

func (s *webrtcServer) storeClip(rec clipRecord) {
	s.clipMu.Lock()
	s.clips[rec.clipID] = rec
	s.clipMu.Unlock()
}

// manualTranscriptJob resolves a listener's transcription request to a job.
// Clips recorded by this server run are found in the registry; older ones
// (from before a restart) are located on disk from their audio URL. source
// names which path matched, for usage logging.
func (s *webrtcServer) manualTranscriptJob(clipID, audioURL string) (job transcriptJob, source string, ok bool) {
	if s.whisperPool == nil {
		return transcriptJob{}, "", false
	}
	s.clipMu.RLock()
	rec, found := s.clips[clipID]
	s.clipMu.RUnlock()
	if found && rec.wavPath != "" {
		return transcriptJob{
			info:     rec.info,
			clipID:   rec.clipID,
			wavPath:  rec.wavPath,
			audioURL: rec.audioURL,
			start:    rec.start,
			manual:   true,
		}, "registry", true
	}
	if audioURL == "" || s.audioLogDir == "" {
		return transcriptJob{}, "", false
	}
	// audioUrl is /audio/<state>/<group>/<stream>/<file>.wav
	// Strip the leading /audio/ prefix and convert to a local path.
	info, known := s.streamForAudioURL(audioURL)
	if !known || !strings.HasPrefix(audioURL, "/audio/") {
		s.logger.Printf("transcribe: clip %q audioUrl %q does not belong to a configured stream", clipID, audioURL)
		return transcriptJob{}, "", false
	}
	wavPath := filepath.Join(s.audioLogDir, filepath.FromSlash(strings.TrimPrefix(audioURL, "/audio/")))
	if _, err := os.Stat(wavPath); err != nil {
		s.logger.Printf("transcribe: clip %q audioUrl fallback wav=%s not found on disk", clipID, wavPath)
		return transcriptJob{}, "", false
	}
	// The stream identity and recording time must match what the history
	// endpoint served for this row: the stream page filters SSE by stream
	// ID, the multi-stream page keys rows by it, and the per-stream log is
	// chosen by stream name.
	start, ok := recordingTimestamp(filepath.Base(wavPath))
	if !ok {
		start = time.Now()
	}
	return transcriptJob{
		info:     info,
		clipID:   clipID,
		wavPath:  wavPath,
		audioURL: audioURL,
		start:    start,
		manual:   true,
	}, "audiourl_fallback", true
}

// streamForAudioURL finds the configured stream whose recording directory an
// /audio/<state>/<group>/<stream>/<file>.wav URL points into, using the same
// sanitized names RecordingHistory builds those URLs from. Matching against
// configured streams also confines the fallback to real recording folders.
func (s *webrtcServer) streamForAudioURL(audioURL string) (streamInfo, bool) {
	parts := strings.Split(strings.TrimPrefix(audioURL, "/audio/"), "/")
	if len(parts) != 4 || parts[3] == "" || parts[3] == ".." {
		return streamInfo{}, false
	}
	safe := func(v string) string { return unsafeChars.ReplaceAllString(v, "_") }
	for _, st := range s.streams {
		if safe(st.info.StateName) == parts[0] && safe(st.info.GroupName) == parts[1] && safe(st.info.StreamName) == parts[2] {
			return st.info, true
		}
	}
	return streamInfo{}, false
}

func (s *webrtcServer) handleTranscriptRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ClipID   string `json:"clipId"`
		AudioURL string `json:"audioUrl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ClipID == "" {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	job, source, ok := s.manualTranscriptJob(req.ClipID, req.AudioURL)
	if !ok {
		http.Error(w, "clip not found or transcription unavailable", http.StatusNotFound)
		return
	}
	if source != "registry" {
		s.logger.Printf("transcribe: clip %q not in registry, using audioUrl fallback wav=%s stream=%s", req.ClipID, job.wavPath, job.info.StreamName)
	}
	// A clip already waiting in the queue is not added twice; its pending
	// transcript is published to every listener, this one included.
	s.whisperPool.Submit(job)
	s.analytics.Record(r, analyticsEvent{Type: evTranscriptReq, Stream: job.info.StreamName})
	w.WriteHeader(http.StatusAccepted)
}

// maxBulkTranscriptRequest caps how many clips one "transcribe all filtered
// audio" request may name.
const maxBulkTranscriptRequest = 10000

// handleBulkTranscriptRequest queues every listed clip at low priority (see
// whisperPool.SubmitBulk) and reports which were accepted, so the page can
// mark those rows as queued.
func (s *webrtcServer) handleBulkTranscriptRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.whisperPool == nil {
		http.Error(w, "transcription is not enabled", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Clips []struct {
			ClipID   string `json:"clipId"`
			AudioURL string `json:"audioUrl"`
		} `json:"clips"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Clips) == 0 {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if len(req.Clips) > maxBulkTranscriptRequest {
		http.Error(w, fmt.Sprintf("at most %d clips per request", maxBulkTranscriptRequest), http.StatusRequestEntityTooLarge)
		return
	}

	resp := struct {
		Queued        []string `json:"queued"`
		AlreadyQueued []string `json:"alreadyQueued"`
		NotFound      int      `json:"notFound"`
		QueueFull     int      `json:"queueFull"`
	}{Queued: []string{}, AlreadyQueued: []string{}}
	for _, c := range req.Clips {
		if c.ClipID == "" {
			resp.NotFound++
			continue
		}
		job, _, ok := s.manualTranscriptJob(c.ClipID, c.AudioURL)
		if !ok {
			resp.NotFound++
			continue
		}
		switch s.whisperPool.SubmitBulk(job) {
		case bulkQueued:
			resp.Queued = append(resp.Queued, c.ClipID)
		case bulkDuplicate:
			resp.AlreadyQueued = append(resp.AlreadyQueued, c.ClipID)
		case bulkFull:
			resp.QueueFull++
		}
	}
	s.logger.Printf("transcribe: bulk request from %s: %d queued, %d already queued, %d not found, %d over capacity (queue depth: %d)",
		getClientIP(r), len(resp.Queued), len(resp.AlreadyQueued), resp.NotFound, resp.QueueFull, s.whisperPool.queueDepth())
	s.analytics.Record(r, analyticsEvent{Type: evTranscriptBulk, Count: len(req.Clips)})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(resp)
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
			af := audioFrame{data: packet.data, codec: packet.codec, headerBytes: packet.headerBytes}
			frame, err := s.toMulaw(af)
			if err != nil {
				s.logger.Printf("%s: %v", s.info.StreamName, err)
				continue
			}
			s.dumpPipelineStage(af, frame)

			now := time.Now()
			s.mu.Lock()
			s.lastPacketAt = now
			s.mu.Unlock()

			if packetsSeen == 0 {
				s.logger.Printf(
					"%s: first multicast packet received from %s, codec=%v, audio_bytes=%d, skip_bytes=%d",
					s.info.StreamName,
					packet.sourceIP,
					packet.codec,
					len(packet.data),
					packet.headerBytes,
				)
			}
			packetsSeen++

			// Feed the recorder (whenever transcription/audio logging is
			// enabled) so it can bridge/finalize clips based purely on
			// packet presence — see recorderState in vad.go.
			if s.recorder != nil {
				s.recorder.Push(DecodePCMU(frame), time.Now())
			}

			s.enqueueBroadcast(media.Sample{
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

	if config.UsageLogFile != "" {
		logger.Printf("NOTE: usageLogFile is obsolete and ignored; remove it from %s", configPath)
	}

	var analytics *analyticsStore
	if config.Analytics != nil {
		analytics, err = openAnalyticsStore(*config.Analytics, logger)
		if err != nil {
			logger.Printf("WARNING: analytics disabled: %v", err)
			analytics = nil
		} else {
			logger.Printf("analytics: recording to %s (raw events kept %d days; negative = forever)", config.Analytics.Dir, config.Analytics.RetentionDays)
			defer analytics.Close()
		}
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

	// The permanent, whisper-only transcript CSV lives inside the primary
	// audio archive directory itself (see AudioLogDir's doc comment) —
	// there's no separate config knob for its location, since "make it
	// part of the audio archive folder" is the whole point: wherever the
	// audio clips end up (including a future network mount), the CSV
	// travels with them. If AudioLogDir isn't configured, there's no audio
	// archive folder to put it in, so it's simply not written.
	var transcriptArchivePath string
	if config.AudioLogDir != "" {
		transcriptArchivePath = filepath.Join(config.AudioLogDir, "transcripts.csv")
		if abs, err := filepath.Abs(transcriptArchivePath); err == nil {
			transcriptArchivePath = abs
		}
		logger.Printf("transcript archive file: %s (CSV, permanent, never pruned)", transcriptArchivePath)
	}

	hub := newTranscriptHub("transcripts", transcriptArchivePath, logger)
	hub.index = newRecordingIndex(config.RecordingIndex, config.AudioLogDir, "transcripts", logger)
	if hub.index == nil {
		logger.Printf("recording index: off (history requests scan the audio folders)")
	} else if config.RecordingIndex.Mode == recordingIndexWindow {
		logger.Printf("recording index: window (last %d days; older ranges scan the audio folders)", config.RecordingIndex.WindowDays)
	} else {
		logger.Printf("recording index: %s", config.RecordingIndex.Mode)
	}

	// Listener corrections live beside the transcript archive for the same
	// reason: they are only useful paired with the WAVs they describe.
	var feedbackPath string
	if config.AudioLogDir != "" {
		feedbackPath = filepath.Join(config.AudioLogDir, "transcript-feedback.csv")
		if abs, err := filepath.Abs(feedbackPath); err == nil {
			feedbackPath = abs
		}
		logger.Printf("transcript feedback file: %s (CSV, permanent, never pruned)", feedbackPath)
	}
	feedback := newFeedbackStore(feedbackPath, logger)

	var pool *whisperPool
	if config.Whisper.enabled() {
		config.Whisper.setDefaults()
		pool = newWhisperPool(*config.Whisper, hub, logger)
		pool.analytics = analytics
		if err := pool.Start(); err != nil {
			// Transcription is optional: keep streaming and recording.
			logger.Printf("WARNING: whisper transcription disabled: %v", err)
			pool.Close()
			pool = nil
		} else if config.Whisper.isRemote() {
			logger.Printf("whisper transcription enabled (remote): servers=%v timeoutMs=%d", config.Whisper.RemoteServers, config.Whisper.TimeoutMs)
		} else {
			logger.Printf("whisper transcription enabled (local): model=%s instances=%d timeoutMs=%d", config.Whisper.ModelPath, config.Whisper.Workers, config.Whisper.TimeoutMs)
		}
	}
	if pool != nil {
		if len(config.Whisper.InferenceParams) > 0 {
			logger.Printf("whisper inference params: %v", pool.formFields)
		}
		switch {
		case config.Whisper.AutoTranscribeMinClipMs <= 0:
			logger.Printf("automatic transcription disabled (autoTranscribeMinClipMs is unset); clips can still be transcribed on demand")
		case config.Whisper.AutoTranscribeMaxClipMs > 0 && config.Whisper.AutoTranscribeMaxClipMs < config.Whisper.AutoTranscribeMinClipMs:
			logger.Printf("WARNING: autoTranscribeMaxClipMs (%d ms) is below autoTranscribeMinClipMs (%d ms) — no clip can satisfy both, so nothing will be transcribed automatically",
				config.Whisper.AutoTranscribeMaxClipMs, config.Whisper.AutoTranscribeMinClipMs)
		case config.Whisper.AutoTranscribeMaxClipMs > 0:
			logger.Printf("automatic transcription clip length window: %d-%d ms", config.Whisper.AutoTranscribeMinClipMs, config.Whisper.AutoTranscribeMaxClipMs)
		default:
			logger.Printf("automatic transcription clip length window: %d ms and longer (no maximum)", config.Whisper.AutoTranscribeMinClipMs)
		}
	}

	server := &webrtcServer{
		api:         api,
		logger:      logger,
		analytics:   analytics,
		streams:     make(map[string]*station, config.totalStreams),
		stateGroups: make([]stateGroup, 0, len(config.streamGroups)),
		hub:         hub,
		clips:       make(map[string]clipRecord),
		whisperPool: pool,
		feedback:    feedback,
		audioLogDir: config.AudioLogDir,
		iceServers:  config.webrtcICEServers(),
		clipJobs:    make(chan func(), 256),
	}
	server.startClipWorkers(4)
	if len(config.ICEServers) == 0 {
		logger.Printf("no iceServers configured; using default STUN server (%s) for NAT traversal", defaultICEServers[0].URLs[0])
	} else {
		for _, ice := range server.iceServers {
			if ice.Username != "" {
				logger.Printf("ICE server configured: %v (username=%s)", ice.URLs, ice.Username)
			} else {
				logger.Printf("ICE server configured: %v", ice.URLs)
			}
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if analytics != nil {
		// Before any recorder starts, so archive and live counts can't overlap.
		var inventory []streamInfo
		for _, state := range config.streamGroups {
			for _, sg := range state.SubGroups {
				for _, cfg := range sg.Streams {
					inventory = append(inventory, streamInfo{StateName: state.StateName, GroupName: sg.GroupName, StreamName: cfg.StreamName, TimeZone: cfg.TimeZone})
				}
			}
		}
		analytics.BackfillRadio(config.AudioLogDir, inventory, nil)
	}
	for _, dir := range audioBackupDirs(&config) {
		logger.Printf("audio backup directory: %s (every clip is copied here when saved)", dir)
	}

	for _, state := range config.streamGroups {
		apiState := stateGroup{
			StateName: state.StateName,
			SubGroups: make([]subGroup, 0, len(state.SubGroups)),
		}

		for _, sg := range state.SubGroups {
			apiSubGroup := subGroup{
				GroupName: sg.GroupName,
				Streams:   make([]streamInfo, 0, len(sg.Streams)),
			}

			for _, cfg := range sg.Streams {
				info := streamInfo{
					StateName:  state.StateName,
					GroupName:  sg.GroupName,
					ForestName: sg.GroupName,
					ID:         nextStreamID(),
					StreamName: cfg.StreamName,
					UDPPort:    cfg.UDPPort,
					TimeZone:   cfg.TimeZone,
				}

				st := &station{
					info:           info,
					codec:          codec,
					frameDuration:  frameDuration,
					logger:         logger,
					debugMulticast: cfg.DebugMulticast,
					audioLogDir:    config.AudioLogDir,
					audioDumpDir:   config.AudioDumpDir,
					subscribers:    make(map[string]*subscriber),
					whisperPool:    pool,
					broadcastChan:  make(chan media.Sample, 64),
				}
				hub.index.register(info)
				go st.runBroadcaster()

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
					captureAudioBackupDirs := audioBackupDirs(&config)
					autoTranscribe := !cfg.DisableAutoTranscribe
					if pool != nil && !autoTranscribe {
						logger.Printf("%s: automatic transcription disabled by config (disableAutoTranscribe)", info.displayName())
					}
					st.recorder = newRecorderState(
						time.Duration(wCfg.GapMs)*time.Millisecond,
						time.Duration(wCfg.MaxClipMs)*time.Millisecond,
						func(samples []int16, start time.Time) {
							// The work below includes synchronous disk I/O
							// (WAV file write). recorder.Push() calls this
							// callback inline from the station's ingest
							// goroutine, so doing that work here would block
							// UDP packet reads for its duration — letting
							// packets queue up in the kernel socket buffer
							// and then get drained/broadcast in a burst,
							// which live WebRTC playback is much more
							// sensitive to than a batch-written WAV.
							// Dispatch it to a background worker instead so
							// this callback returns immediately.
							job := func() {
								var wavPath, audioURL string
								durationMs := len(samples) * 1000 / recSampleRate
								server.analytics.Transmission(captureInfo, start, durationMs)
								if captureAudioLogDir != "" {
									var err error

									wavPath, audioURL, err = saveAudioClip(captureAudioLogDir, captureAudioBackupDirs, captureInfo, samples, start, logger)
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
									wav, _ := encodePCM16WAV(samples, recSampleRate)
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
								// The WAV filename is the clip's identity, so live
								// rows and history rows share one key across
								// restarts. Only unsaved clips need a synthetic ID.
								clipID := wavBaseName(requestWavPath)
								if clipID == "" {
									clipID = nextClipID()
								}
								// Publish clip event immediately so the UI shows the recording.
								hub.Publish(transcriptEvent{
									Type:        "clip",
									ClipID:      clipID,
									StreamID:    captureInfo.ID,
									StreamName:  captureInfo.StreamName,
									StateName:   captureInfo.StateName,
									GroupName:   captureInfo.GroupName,
									AudioURL:    audioURL,
									DurationMs:  durationMs,
									Timestamp:   start,
									WAVFilename: wavBaseName(requestWavPath),
								})
								server.storeClip(clipRecord{
									clipID:   clipID,
									info:     captureInfo,
									wavPath:  requestWavPath,
									audioURL: audioURL,
									start:    start,
									duration: durationMs,
								})
								if pool != nil && autoTranscribe && shouldAutoTranscribe(wCfg, durationMs) {
									if job, _, ok := server.manualTranscriptJob(clipID, ""); ok {
										pool.Submit(job)
									}
								}
							}
							select {
							case server.clipJobs <- job:
							default:
								logger.Printf("%s: clip job queue full; dropping clip finalization (disk/whisper backlog)", captureInfo.StreamName)
								// Still count the transmission, off the ingest goroutine.
								go server.analytics.Transmission(captureInfo, start, len(samples)*1000/recSampleRate)
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
						"configured stream %q in state %q, forest %q on UDP ports %v, codec=PCMU, frame_size=%d bytes, skip_bytes=%d, frame_duration=%s",
						info.StreamName,
						state.StateName,
						sg.GroupName,
						ports,
						frameSizeBytes,
						skipBytes,
						frameDuration,
					)

					ml.Start()

					go func(st *station, ml *MulticastListener) {
						<-ctx.Done()
						ml.Close()
						st.closeSubscribers()
						close(st.broadcastChan)
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
						"configured stream %q in state %q, forest %q on UDP %d, codec=PCMU, frame_size=%d bytes, skip_bytes=%d, frame_duration=%s",
						info.StreamName,
						state.StateName,
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
						close(st.broadcastChan)
					}(st, conn)

					go func(st *station, conn net.PacketConn) {
						if err := st.ingest(ctx, conn); err != nil {
							logger.Printf("%s ingest stopped: %v", st.info.StreamName, err)
							stop()
						}
					}(st, conn)
				}
			}

			apiState.SubGroups = append(apiState.SubGroups, apiSubGroup)
		}

		server.stateGroups = append(server.stateGroups, apiState)
	}
	go hub.index.build()

	staticFS, err := fs.Sub(webFiles, "web")
	if err != nil {
		logger.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/", analytics.PageviewMiddleware(http.FileServer(http.FS(staticFS))))
	mux.HandleFunc("/streams", server.handleStreams)
	mux.HandleFunc("/stream-status", server.handleStreamStatus)
	mux.HandleFunc("/stream-activity", server.handleStreamActivity)
	mux.HandleFunc("/offer", server.handleOffer)
	mux.HandleFunc("/listen-leave", server.handleListenLeave)
	mux.HandleFunc("/transcripts/request", server.handleTranscriptRequest)
	mux.HandleFunc("/transcripts/request-bulk", server.handleBulkTranscriptRequest)
	mux.HandleFunc("/transcripts/feedback", server.handleTranscriptFeedback)
	mux.Handle("/transcripts", hub)
	mux.Handle("/recordings/download", analytics.EventMiddleware(http.MethodPost, evDownload, recordingDownloadHandler(config.AudioLogDir, logger)))
	mux.HandleFunc("/transcripts/history", func(w http.ResponseWriter, r *http.Request) {
		streamID := r.URL.Query().Get("streamId")
		if streamID == "" {
			http.Error(w, "streamId required", http.StatusBadRequest)
			return
		}
		st, ok := server.streams[streamID]
		if !ok {
			http.Error(w, "unknown streamId", http.StatusNotFound)
			return
		}
		since, until, err := parseHistoryRange(r.URL.Query())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		events, err := hub.RecordingHistory(config.AudioLogDir, st.info, since, until)
		if err != nil {
			http.Error(w, "failed to read history", http.StatusInternalServerError)
			logger.Printf("transcript history: %v", err)
			return
		}
		if events == nil {
			events = []transcriptEvent{}
		}
		feedback.ApplyToHistory(events)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(events)
	})
	if config.AudioLogDir != "" {
		audioHandler := http.StripPrefix("/audio/", http.FileServer(http.Dir(config.AudioLogDir)))
		mux.Handle("/audio/", analytics.AudioMiddleware(audioHandler))
		logger.Printf("audio log directory: %s (served at /audio/)", config.AudioLogDir)
	}
	if analytics != nil {
		go analytics.Run(ctx)
		if p := config.Analytics.DashboardPath; p != "" {
			analytics.RegisterDashboard(mux, p, server.liveListenerCounts, server.streamInventory, pool.transcriptionSnapshot)
			logger.Printf("analytics: dashboard enabled at the dashboardPath set in %s", configPath)
		} else {
			logger.Printf("analytics: dashboard disabled; set analytics.dashboardPath in %s, e.g. \"/%s\"", configPath, randomToken())
		}
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

	// Background cleanup: prune only the server's own diagnostic log file
	// (entries older than 90 days). Audio clips and transcripts (both the
	// per-stream JSON logs and the permanent whisper CSV archive) are kept
	// indefinitely — nothing in this codebase ever deletes them.
	const logRetentionPeriod = 90 * 24 * time.Hour
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
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
	if config.Whisper != nil {
		if err := config.Whisper.validate(); err != nil {
			return appConfig{}, fmt.Errorf("%s: whisper: %w", path, err)
		}
	}
	if err := config.RecordingIndex.normalize(); err != nil {
		return appConfig{}, fmt.Errorf("%s: recordingIndex: %w", path, err)
	}
	if config.Analytics != nil {
		if err := config.Analytics.normalize(); err != nil {
			return appConfig{}, fmt.Errorf("%s: analytics: %w", path, err)
		}
	}

	// Overlay config.secrets.json if present (passwords and other sensitive values).
	if sf, err := os.Open(secretsPath); err == nil {
		defer sf.Close()
		var secrets struct {
			PFXPassword    string            `json:"pfxPassword"`
			PFXKeyPassword string            `json:"pfxKeyPassword"`
			CertFile       string            `json:"certFile"`
			KeyFile        string            `json:"keyFile"`
			ICEServers     []iceServerConfig `json:"iceServers"`
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
		if len(secrets.ICEServers) > 0 {
			// TURN credentials are sensitive; let config.secrets.json fully
			// replace the (public, non-secret) STUN-only list from config.json.
			config.ICEServers = secrets.ICEServers
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

	if len(config.LegacyRegions) > 0 {
		if len(config.States) > 0 {
			return appConfig{}, fmt.Errorf("%s sets both \"states\" and the legacy \"regions\"; use only \"states\"", path)
		}
		config.States = config.LegacyRegions
		config.LegacyRegions = nil
	}

	states, totalStreams, err := normalizeStates(path, config.States)
	if err != nil {
		return appConfig{}, err
	}

	config.streamGroups = states
	config.totalStreams = totalStreams

	return config, nil
}

func normalizeStates(path string, rawStates map[string]map[string][]streamConfig) ([]configuredState, int, error) {
	if len(rawStates) == 0 {
		return nil, 0, fmt.Errorf("%s has no states configured", path)
	}

	stateNames := make([]string, 0, len(rawStates))
	for stateName := range rawStates {
		stateNames = append(stateNames, stateName)
	}
	sort.Strings(stateNames)

	seenStateNames := make(map[string]struct{}, len(stateNames))
	seenGroupNames := make(map[string]struct{})
	seenPorts := make(map[int]struct{})
	states := make([]configuredState, 0, len(stateNames))
	totalStreams := 0

	for _, sourceStateName := range stateNames {
		stateName := strings.TrimSpace(sourceStateName)
		if stateName == "" {
			return nil, 0, fmt.Errorf("%s has an empty state name", path)
		}
		if _, exists := seenStateNames[stateName]; exists {
			return nil, 0, fmt.Errorf("%s has duplicate state name %q after trimming whitespace", path, stateName)
		}
		seenStateNames[stateName] = struct{}{}

		rawSubGroups := rawStates[sourceStateName]
		if len(rawSubGroups) == 0 {
			return nil, 0, fmt.Errorf("%s state %q has no groups configured", path, stateName)
		}

		state := configuredState{
			StateName: stateName,
			SubGroups: make([]configuredSubGroup, 0, len(rawSubGroups)),
		}

		groupNames := make([]string, 0, len(rawSubGroups))
		for groupName := range rawSubGroups {
			groupNames = append(groupNames, groupName)
		}
		sort.Strings(groupNames)

		for _, sourceGroupName := range groupNames {
			groupName := strings.TrimSpace(sourceGroupName)
			if groupName == "" {
				return nil, 0, fmt.Errorf("%s state %q has an empty group name", path, stateName)
			}

			compositeKey := stateName + "/" + groupName
			if _, exists := seenGroupNames[compositeKey]; exists {
				return nil, 0, fmt.Errorf("%s state %q has duplicate group name %q", path, stateName, groupName)
			}
			seenGroupNames[compositeKey] = struct{}{}

			rawStreams := rawSubGroups[sourceGroupName]
			if len(rawStreams) == 0 {
				return nil, 0, fmt.Errorf("%s state %q group %q has no streams configured", path, stateName, groupName)
			}

			subGroup := configuredSubGroup{
				GroupName: groupName,
				Streams:   make([]streamConfig, 0, len(rawStreams)),
			}

			for i, stream := range rawStreams {
				streamName := strings.TrimSpace(stream.StreamName)
				if streamName == "" {
					return nil, 0, fmt.Errorf("%s state %q group %q entry %d is missing streamName", path, stateName, groupName, i)
				}

				// Validate ports: UDPPorts (plural) takes precedence
				stream.TimeZone = strings.TrimSpace(stream.TimeZone)
				if stream.TimeZone != "" {
					if _, err := time.LoadLocation(stream.TimeZone); err != nil {
						return nil, 0, fmt.Errorf("%s state %q group %q entry %d has unknown timeZone %q (use an IANA name such as \"America/Denver\")", path, stateName, groupName, i, stream.TimeZone)
					}
				}

				var portsToValidate []int
				if len(stream.UDPPorts) > 0 {
					portsToValidate = stream.UDPPorts
				} else if stream.UDPPort > 0 {
					portsToValidate = []int{stream.UDPPort}
				} else {
					return nil, 0, fmt.Errorf("%s state %q group %q entry %d has no ports configured (udpPort or udpPorts)", path, stateName, groupName, i)
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
					return nil, 0, fmt.Errorf("%s state %q group %q entry %d has %d multicast addrs for %d ports; provide one address or one per port", path, stateName, groupName, i, len(stream.MulticastAddrs), len(portsToValidate))
				}

				if len(portsToValidate) > 4 {
					return nil, 0, fmt.Errorf("%s state %q group %q entry %d has %d ports; maximum is 4", path, stateName, groupName, i, len(portsToValidate))
				}

				// Validate each port
				for _, port := range portsToValidate {
					if port < 1 || port > 65535 {
						return nil, 0, fmt.Errorf("%s state %q group %q entry %d has invalid UDP port %d", path, stateName, groupName, i, port)
					}
					if _, exists := seenPorts[port]; exists {
						return nil, 0, fmt.Errorf("%s has duplicate UDP port %d", path, port)
					}
					seenPorts[port] = struct{}{}
				}

				subGroup.Streams = append(subGroup.Streams, stream)
			}

			state.SubGroups = append(state.SubGroups, subGroup)
			totalStreams += len(subGroup.Streams)
		}

		states = append(states, state)
	}

	return states, totalStreams, nil
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

		af, err := extractAudioFrame(buffer[:n])
		if err != nil {
			// Non-audio control/keepalive packets are expected on some device
			// types (e.g. DFSI gateways cycling their channel ports); only log
			// when debugging this stream so normal operation stays quiet.
			if s.debugMulticast {
				s.logger.Printf("%s: dropping UDP packet from %s: %v", s.info.StreamName, remoteAddr, err)
			}
			continue
		}

		frame, err := s.toMulaw(af)
		if err != nil {
			s.logger.Printf("%s: %v", s.info.StreamName, err)
			continue
		}
		s.dumpPipelineStage(af, frame)

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
				if s.recorder != nil {
					s.recorder.Push(DecodePCMU(frame), time.Now())
				}
				s.enqueueBroadcast(media.Sample{Data: frame, Duration: s.frameDuration})
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
				"%s: first UDP packet from %s, packet_bytes=%d, codec=%v, audio_bytes=%d, skip_bytes=%d",
				s.info.StreamName,
				remoteAddr,
				n,
				af.codec,
				len(af.data),
				af.headerBytes,
			)
		}
		packetsSeen++

		// Feed the recorder (whenever transcription/audio logging is
		// enabled) so it can bridge/finalize clips based purely on packet
		// presence — see recorderState in vad.go.
		if s.recorder != nil {
			s.recorder.Push(DecodePCMU(frame), time.Now())
		}

		s.enqueueBroadcast(media.Sample{
			Data:     frame,
			Duration: s.frameDuration,
		})
	}
}

// audioBackupDirs lists the configured backup archives, in order.
func audioBackupDirs(config *appConfig) []string {
	var dirs []string
	for _, d := range []string{config.AudioBackupDir, config.AudioBackupDir2} {
		if d != "" {
			dirs = append(dirs, d)
		}
	}
	return dirs
}

// saveAudioClip writes a recorded clip as an 8kHz mono WAV file under
// audioLogDir (the primary, user-facing archive — served over HTTP and
// referenced by clip playback). Returns the absolute file path and the
// relative URL path for browser playback.
// Path: <audioLogDir>/<state>/<group>/<streamName>/<streamName>_<ISO8601Z>.wav
//
// When backupDirs are given, the identical WAV bytes are also written to
// the same relative path under each one — redundant copies for disaster
// recovery. They're never served over HTTP or referenced by the returned
// path or URL, and a failure writing one (e.g. an unreachable network
// mount) is logged but does not fail this call or affect the other copies.
func saveAudioClip(audioLogDir string, backupDirs []string, info streamInfo, samples []int16, start time.Time, logger *log.Logger) (string, string, error) {
	safe := func(s string) string {
		return unsafeChars.ReplaceAllString(s, "_")
	}
	relDir := filepath.Join(safe(info.StateName), safe(info.GroupName), safe(info.StreamName))
	// ISO 8601 UTC — colons replaced with underscores for Windows filename safety.
	ts := start.UTC().Format("2006-01-02T15_04_05Z")
	filename := fmt.Sprintf("%s_%s.wav", safe(info.StreamName), ts)

	wav, err := encodePCM16WAV(samples, recSampleRate)
	if err != nil {
		return "", "", fmt.Errorf("encode %s/%s: %w", relDir, filename, err)
	}

	absDir := filepath.Join(audioLogDir, relDir)
	if err := os.MkdirAll(absDir, 0755); err != nil {
		return "", "", fmt.Errorf("mkdir %s: %w", absDir, err)
	}
	absPath := filepath.Join(absDir, filename)
	if err := os.WriteFile(absPath, wav, 0644); err != nil {
		return "", "", fmt.Errorf("write %s: %w", absPath, err)
	}

	for _, backupDir := range backupDirs {
		if backupDir == "" {
			continue
		}
		backupAbsDir := filepath.Join(backupDir, relDir)
		if err := os.MkdirAll(backupAbsDir, 0755); err != nil {
			logger.Printf("audio backup: mkdir %s: %v", backupAbsDir, err)
			continue
		}
		backupAbsPath := filepath.Join(backupAbsDir, filename)
		if err := os.WriteFile(backupAbsPath, wav, 0644); err != nil {
			logger.Printf("audio backup: write %s: %v", backupAbsPath, err)
		}
	}

	// Build a URL-style relative path using forward slashes.
	relURL := "/audio/" + safe(info.StateName) + "/" + safe(info.GroupName) + "/" + safe(info.StreamName) + "/" + filename
	return absPath, relURL, nil
}

type recordingDownloadRequest struct {
	AudioURLs []string `json:"audioUrls"`
}

type recordingDownloadFile struct {
	path string
	name string
	info os.FileInfo
}

func recordingDownloadHandler(audioLogDir string, logger *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if audioLogDir == "" {
			http.Error(w, "recording archive is disabled", http.StatusServiceUnavailable)
			return
		}

		var req recordingDownloadRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20))
		if err := decoder.Decode(&req); err != nil {
			http.Error(w, "invalid download request", http.StatusBadRequest)
			return
		}

		files, err := resolveRecordingDownloadFiles(audioLogDir, req.AudioURLs)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(files) == 0 {
			http.Error(w, "no recordings matched the active filters", http.StatusBadRequest)
			return
		}

		filename := "filtered-recordings-" + time.Now().UTC().Format("20060102T150405Z") + ".zip"
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))

		zw := zip.NewWriter(w)
		for _, file := range files {
			header, err := zip.FileInfoHeader(file.info)
			if err != nil {
				logger.Printf("recording download: header %s: %v", file.path, err)
				continue
			}
			header.Name = file.name
			header.Method = zip.Deflate
			entry, err := zw.CreateHeader(header)
			if err != nil {
				logger.Printf("recording download: create %s: %v", file.name, err)
				continue
			}
			src, err := os.Open(file.path)
			if err != nil {
				logger.Printf("recording download: open %s: %v", file.path, err)
				continue
			}
			_, copyErr := io.Copy(entry, src)
			closeErr := src.Close()
			if copyErr != nil {
				logger.Printf("recording download: copy %s: %v", file.path, copyErr)
			}
			if closeErr != nil {
				logger.Printf("recording download: close %s: %v", file.path, closeErr)
			}
		}
		if err := zw.Close(); err != nil {
			logger.Printf("recording download: close zip: %v", err)
		}
	}
}

func resolveRecordingDownloadFiles(audioLogDir string, audioURLs []string) ([]recordingDownloadFile, error) {
	root, err := filepath.Abs(audioLogDir)
	if err != nil {
		return nil, fmt.Errorf("resolve audio archive: %w", err)
	}

	seen := make(map[string]struct{}, len(audioURLs))
	files := make([]recordingDownloadFile, 0, len(audioURLs))
	for _, audioURL := range audioURLs {
		parsed, err := url.ParseRequestURI(audioURL)
		if err != nil || parsed.RawQuery != "" || parsed.Fragment != "" || !strings.HasPrefix(parsed.Path, "/audio/") {
			return nil, fmt.Errorf("invalid recording URL %q", audioURL)
		}
		escapedRelative := strings.TrimPrefix(parsed.EscapedPath(), "/audio/")
		relativeURL, err := url.PathUnescape(escapedRelative)
		if err != nil {
			return nil, fmt.Errorf("invalid recording URL %q", audioURL)
		}
		relative := filepath.Clean(filepath.FromSlash(relativeURL))
		if relative == "." || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("invalid recording path %q", audioURL)
		}
		if !strings.EqualFold(filepath.Ext(relative), ".wav") {
			return nil, fmt.Errorf("recording is not a WAV file: %q", audioURL)
		}

		fullPath := filepath.Join(root, relative)
		contained, err := filepath.Rel(root, fullPath)
		if err != nil || contained == ".." || strings.HasPrefix(contained, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("recording path escapes the audio archive: %q", audioURL)
		}
		if _, duplicate := seen[fullPath]; duplicate {
			continue
		}
		info, err := os.Stat(fullPath)
		if err != nil {
			return nil, fmt.Errorf("recording unavailable: %q", audioURL)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("recording is not a regular file: %q", audioURL)
		}
		seen[fullPath] = struct{}{}
		files = append(files, recordingDownloadFile{
			path: fullPath,
			name: filepath.ToSlash(relative),
			info: info,
		})
	}
	return files, nil
}

// parseHistoryRange parses the optional "since" and "until" RFC3339 query
// parameters accepted by /transcripts/history. Both are optional and, left
// unset, impose no bound in that direction: History treats a zero since as
// "from the beginning of recorded history" and a zero until as "up to now",
// so an unfiltered request returns everything on disk for that stream
// (recordings and transcripts are kept indefinitely — see AudioLogDir).
// Callers wanting a bounded window (e.g. the web UI's default "last 8
// days" view) pass an explicit since.
func parseHistoryRange(q url.Values) (since, until time.Time, err error) {
	if s := q.Get("since"); s != "" {
		since, err = time.Parse(time.RFC3339, s)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid since: %w", err)
		}
	}
	if u := q.Get("until"); u != "" {
		until, err = time.Parse(time.RFC3339, u)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid until: %w", err)
		}
	}
	return since, until, nil
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

// streamInventory lists the configured radio streams for the analytics
// dashboard, in configuration order.
func (s *webrtcServer) streamInventory() []streamInfo {
	var out []streamInfo
	for _, sg := range s.stateGroups {
		for _, g := range sg.SubGroups {
			out = append(out, g.Streams...)
		}
	}
	return out
}

// liveListenerCounts reports connected listeners per stream for the
// analytics dashboard.
func (s *webrtcServer) liveListenerCounts() map[string]int {
	out := map[string]int{}
	for _, st := range s.streams {
		st.mu.RLock()
		n := 0
		for _, sub := range st.subscribers {
			if sub.ready.Load() {
				n++
			}
		}
		st.mu.RUnlock()
		if n > 0 {
			out[st.info.displayName()] = n
		}
	}
	return out
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

// broadcast performs the actual RTP writes to every ready subscriber. It
// must only be called from runBroadcaster — never directly from an ingest
// goroutine — since WriteSample() does real per-packet work (RTP
// packetization, SRTP encryption, the underlying socket send) whose latency
// would otherwise block UDP packet reads on every single frame, not just
// occasionally like the recorder's disk-I/O work already moved off that path.
func (s *station) broadcast(sample media.Sample) {
	s.mu.RLock()
	targets := make(map[string]*subscriber, len(s.subscribers))
	for id, sub := range s.subscribers {
		if sub.ready.Load() {
			targets[id] = sub
		}
	}
	s.mu.RUnlock()

	for id, sub := range targets {
		if err := sub.track.WriteSample(sample); err != nil {
			s.logger.Printf("%s: dropping %s after track write failure: %v", s.info.StreamName, id, err)
			s.removeSubscriber(id)
		}
	}
}

// enqueueBroadcast hands a decoded audio frame off to this station's
// dedicated broadcaster goroutine (see runBroadcaster) instead of writing to
// subscribers inline. Called from the ingest goroutine; must never block it.
func (s *station) enqueueBroadcast(sample media.Sample) {
	select {
	case s.broadcastChan <- sample:
	default:
		s.logger.Printf("%s: broadcast queue full; dropping a frame rather than blocking packet ingest", s.info.StreamName)
	}
}

// runBroadcaster drains broadcastChan and performs the actual subscriber
// writes, decoupled from packet ingest. Run once per station for its
// lifetime; exits when broadcastChan is closed.
func (s *station) runBroadcaster() {
	for sample := range s.broadcastChan {
		s.broadcast(sample)
	}
}

// markSubscriberReady flags a subscriber as ready to receive broadcast audio.
// Called once its PeerConnection reports it has actually connected.
func (s *station) markSubscriberReady(id string) {
	s.mu.RLock()
	sub, ok := s.subscribers[id]
	s.mu.RUnlock()
	if ok {
		sub.ready.Store(true)
	}
}

// markSubscriberNotReady stops broadcasting audio to a subscriber whose
// connection has left the Connected state (disconnecting, failed, or
// closed), symmetric with markSubscriberReady.
func (s *station) markSubscriberNotReady(id string) {
	s.mu.RLock()
	sub, ok := s.subscribers[id]
	s.mu.RUnlock()
	if ok {
		sub.ready.Store(false)
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
	if err := json.NewEncoder(w).Encode(s.stateGroups); err != nil {
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
			s.hub.HasRecentActivity(streamName, 24*time.Hour)

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

// streamActivityWindow is how recently a stream must have received audio
// packets to count as "active" for the UI's activity lights. Packets only
// flow while a radio is keyed, so this tracks live transmissions.
const streamActivityWindow = 3 * time.Second

// handleStreamActivity returns the IDs of streams currently receiving audio.
// It only reads in-memory timestamps, so pages can poll it every few seconds.
func (s *webrtcServer) handleStreamActivity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.Header().Set("Content-Type", "application/json")

	cutoff := time.Now().Add(-streamActivityWindow)
	active := make([]string, 0)
	for id, st := range s.streams {
		st.mu.RLock()
		last := st.lastPacketAt
		st.mu.RUnlock()
		if last.After(cutoff) {
			active = append(active, id)
		}
	}
	sort.Strings(active)

	json.NewEncoder(w).Encode(struct {
		Active []string `json:"active"`
	}{active})
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

	pc, err := s.api.NewPeerConnection(webrtc.Configuration{
		ICEServers: s.iceServers,
	})
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

	s.analytics.ListenStart(r, station.info.displayName(), station.info.TimeZone, peerID)
	leaveToken := randomToken()

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		s.logger.Printf("%s %s state: %s", station.info.StreamName, peerID, state.String())
		switch state {
		case webrtc.PeerConnectionStateConnected:
			station.markSubscriberReady(peerID)
			s.analytics.ListenConnected(peerID)
		case webrtc.PeerConnectionStateDisconnected:
			// Stop broadcasting the instant the connection leaves the
			// Connected state — symmetric with markSubscriberReady, so a
			// subscriber that's disconnecting doesn't keep receiving writes
			// into a transport that's no longer reliably delivering them
			// (the teardown-side counterpart to the connection warm-up
			// issue: bursty/delayed delivery at the very start of a
			// connection).
			station.markSubscriberNotReady(peerID)
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			station.markSubscriberNotReady(peerID)
			station.removeSubscriber(peerID)
			s.leaveTokens.Delete(leaveToken)
			s.analytics.ListenEnded(peerID, state == webrtc.PeerConnectionStateFailed)
		}
	})
	s.leaveTokens.Store(leaveToken, listenLease{station: station, peerID: peerID})

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
	w.Header().Set("X-Listen-Token", leaveToken)
	if err := json.NewEncoder(w).Encode(pc.LocalDescription()); err != nil {
		station.removeSubscriber(peerID)
	}
}

type listenLease struct {
	station *station
	peerID  string
}

// handleListenLeave is the page's "I'm leaving" beacon: it records why the
// listener ended (tab closed/navigated vs. pressed Disconnect) and closes the
// peer connection right away instead of waiting ~30s for ICE to time out.
// Connections that end without this beacon are counted as dropped.
func (s *webrtcServer) handleListenLeave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	reason := r.PostForm.Get("reason")
	if reason != endLeft && reason != endStopped {
		http.Error(w, "invalid reason", http.StatusBadRequest)
		return
	}
	if v, ok := s.leaveTokens.LoadAndDelete(r.PostForm.Get("token")); ok {
		lease := v.(listenLease)
		s.analytics.ListenLeaving(lease.peerID, reason)
		lease.station.markSubscriberNotReady(lease.peerID)
		lease.station.removeSubscriber(lease.peerID)
	}
	w.WriteHeader(http.StatusNoContent)
}

// extractAudioFrame extracts a fixed-size audio payload from a UDP packet and
// identifies which codec produced it (see wireFrameSizes).
//
// Different source device types prepend headers of different lengths before
// the audio — e.g. 12 bytes for legacy analog encoders, 14 bytes for
// DFSI-style gateways, and 18 bytes on a DFSI gateway's first frame of a
// transmission (which carries an extra start-of-stream marker). Header
// contents are never used elsewhere in this codebase, and every audio-bearing
// packet observed so far places the fixed-size audio payload at the very end
// of the packet, so the header length is derived from the packet length
// instead of assumed to be a fixed constant. This lets multiple device
// formats and codecs share the same ingest path without per-stream
// configuration.
//
// A packet's total length alone identifies its codec: G.711 payload size
// (160 bytes), combined with maxHeaderBytes, is used to derive the header
// length. Packets shorter than the payload size, or whose implied header
// exceeds maxHeaderBytes, are rejected as non-audio control/keepalive
// packets (observed as short as 14 bytes from DFSI gateways cycling through
// their channel ports) so callers can discard them quietly.
func extractAudioFrame(payload []byte) (audioFrame, error) {
	for _, wf := range wireFrameSizes {
		if len(payload) < wf.frameBytes {
			continue
		}
		header := len(payload) - wf.frameBytes
		if header <= maxHeaderBytes {
			return audioFrame{
				data:        bytes.Clone(payload[header : header+wf.frameBytes]),
				codec:       wf.codec,
				headerBytes: header,
			}, nil
		}
	}
	return audioFrame{}, fmt.Errorf(
		"packet is %d bytes; does not match any known audio frame format (need %d bytes for G.711, plus a header up to %d bytes)",
		len(payload), frameSizeBytes, maxHeaderBytes)
}

// toMulaw returns the raw wire-format audio frame unchanged. G.711 is the
// only supported wire codec, so this is currently an identity pass-through;
// it remains a named step so downstream code (broadcast/recording) doesn't
// need to know the wire format directly, and so a future codec could be
// added here without touching callers.
func (s *station) toMulaw(af audioFrame) ([]byte, error) {
	switch af.codec {
	case wireCodecG711:
		return af.data, nil
	default:
		return nil, fmt.Errorf("unsupported wire codec %v", af.codec)
	}
}

// dumpPipelineStage writes raw wire bytes and the resulting broadcast µ-law
// frame to disk for offline diagnosis, along with a per-packet timing log,
// when audioDumpDir is configured. Only called from this station's single
// ingest goroutine, so file handles need no locking. Writes are buffered and
// flushed only periodically so this diagnostic feature doesn't itself add
// synchronous per-packet disk I/O to the ingest hot path.
func (s *station) dumpPipelineStage(af audioFrame, mulawFrame []byte) {
	if s.audioDumpDir == "" {
		return
	}
	safe := unsafeChars.ReplaceAllString(s.info.StreamName, "_")
	if s.dumpWireFile == nil {
		if err := os.MkdirAll(s.audioDumpDir, 0755); err != nil {
			s.logger.Printf("%s: audio dump: mkdir %s: %v", s.info.StreamName, s.audioDumpDir, err)
			s.audioDumpDir = "" // disable further attempts for this station
			return
		}
		// Timestamp the filenames (once, at first use) so each run/restart
		// produces its own distinct set of files instead of silently
		// appending onto whatever was left from a previous test session.
		ts := time.Now().UTC().Format("2006-01-02T15_04_05Z")
		prefix := fmt.Sprintf("%s_%s", safe, ts)
		var err error
		s.dumpWireFile, err = os.OpenFile(filepath.Join(s.audioDumpDir, prefix+"_wire.bin"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			s.logger.Printf("%s: audio dump: %v", s.info.StreamName, err)
			s.audioDumpDir = ""
			return
		}
		s.dumpMulawFile, err = os.OpenFile(filepath.Join(s.audioDumpDir, prefix+"_live_mulaw.bin"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			s.logger.Printf("%s: audio dump: %v", s.info.StreamName, err)
			s.audioDumpDir = ""
			return
		}
		s.dumpTimingFile, err = os.OpenFile(filepath.Join(s.audioDumpDir, prefix+"_timing.csv"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			s.logger.Printf("%s: audio dump: %v", s.info.StreamName, err)
			s.audioDumpDir = ""
			return
		}
		s.dumpWireBuf = bufio.NewWriterSize(s.dumpWireFile, 64*1024)
		s.dumpMulawBuf = bufio.NewWriterSize(s.dumpMulawFile, 64*1024)
		s.dumpTimingBuf = bufio.NewWriterSize(s.dumpTimingFile, 64*1024)
		if fi, statErr := s.dumpTimingFile.Stat(); statErr == nil && fi.Size() == 0 {
			fmt.Fprintln(s.dumpTimingBuf, "unix_nano,codec,wire_bytes,mulaw_bytes,gap_ms_since_prev_packet")
		}
		s.logger.Printf("%s: audio pipeline dump enabled: %s", s.info.StreamName, s.audioDumpDir)
	}

	now := time.Now()
	gapMs := -1.0
	if !s.dumpLastPacketAt.IsZero() {
		gapMs = float64(now.Sub(s.dumpLastPacketAt).Microseconds()) / 1000.0
	}
	s.dumpLastPacketAt = now

	_, _ = s.dumpWireBuf.Write(af.data)
	_, _ = s.dumpMulawBuf.Write(mulawFrame)
	fmt.Fprintf(s.dumpTimingBuf, "%d,%v,%d,%d,%.3f\n", now.UnixNano(), af.codec, len(af.data), len(mulawFrame), gapMs)

	// Flush roughly once a second (at 20ms/frame, ~50 packets) rather than
	// every packet, so the periodic flush's I/O cost is amortized and can't
	// itself masquerade as the jitter this feature is meant to diagnose.
	s.dumpPacketCount++
	if s.dumpPacketCount%50 == 0 {
		_ = s.dumpWireBuf.Flush()
		_ = s.dumpMulawBuf.Flush()
		_ = s.dumpTimingBuf.Flush()
	}
}

func drainRTCP(sender *webrtc.RTPSender) {
	buffer := make([]byte, 1500)
	for {
		if _, _, err := sender.Read(buffer); err != nil {
			return
		}
	}
}
