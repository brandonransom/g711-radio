package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"reflect"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// streamRetryDelay is how long a reload waits before retrying streams that
// failed to start (e.g. a UDP port still held by another process).
const streamRetryDelay = 30 * time.Second

// stationRuntime holds the process-wide settings every station is started
// with. They come from config.json and are fixed for the life of the
// process; only the stream definitions themselves are reloadable.
type stationRuntime struct {
	ctx             context.Context    // process lifetime
	stop            context.CancelFunc // shuts the whole server down
	codec           webrtc.RTPCodecCapability
	frameDuration   time.Duration
	audioLogDir     string
	audioDumpDir    string
	audioBackupDirs []string
	whisper         *whisperConfig // clip gap/length and auto-transcribe settings
}

// recorderWhisperConfig returns the whisper settings recorders use, with
// defaults applied even when transcription is not configured.
func recorderWhisperConfig(cfg *whisperConfig) *whisperConfig {
	if cfg == nil {
		cfg = &whisperConfig{}
	}
	cfg.setDefaults()
	return cfg
}

func streamKey(stateName, groupName, streamName string) string {
	return stateName + "\x00" + groupName + "\x00" + streamName
}

func (s *webrtcServer) stationByID(id string) (*station, bool) {
	s.streamsMu.RLock()
	defer s.streamsMu.RUnlock()
	st, ok := s.streams[id]
	return st, ok
}

func (s *webrtcServer) stationList() []*station {
	s.streamsMu.RLock()
	defer s.streamsMu.RUnlock()
	out := make([]*station, 0, len(s.streams))
	for _, st := range s.streams {
		out = append(out, st)
	}
	return out
}

// stationsByID returns a snapshot of the running stations keyed by ID.
func (s *webrtcServer) stationsByID() map[string]*station {
	s.streamsMu.RLock()
	defer s.streamsMu.RUnlock()
	out := make(map[string]*station, len(s.streams))
	for id, st := range s.streams {
		out[id] = st
	}
	return out
}

func (s *webrtcServer) stateGroupList() []stateGroup {
	s.streamsMu.RLock()
	defer s.streamsMu.RUnlock()
	return s.stateGroups
}

// applyStreams makes the running stations match states. Streams whose
// definition is unchanged keep running untouched (listeners stay connected);
// changed streams restart under the same ID; removed streams stop after
// finishing any clip in progress; new streams start. Transcription jobs
// already queued are unaffected — each carries its own stream info and WAV
// path. initial is true for the startup call, before the recording index
// is built. Returns how many streams failed to start.
func (s *webrtcServer) applyStreams(states []configuredState, initial bool) int {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()

	type desiredStream struct {
		stateName, groupName string
		cfg                  streamConfig
	}
	desired := make(map[string]desiredStream)
	for _, state := range states {
		for _, sg := range state.SubGroups {
			for _, cfg := range sg.Streams {
				desired[streamKey(state.StateName, sg.GroupName, cfg.StreamName)] = desiredStream{state.StateName, sg.GroupName, cfg}
			}
		}
	}

	s.streamsMu.Lock()
	if s.streamsByKey == nil {
		s.streamsByKey = make(map[string]*station)
	}
	if s.streams == nil {
		s.streams = make(map[string]*station)
	}
	keep := make(map[string]*station)
	reuseID := make(map[string]string)
	var removed, changed []*station
	for key, st := range s.streamsByKey {
		d, ok := desired[key]
		switch {
		case !ok:
			removed = append(removed, st)
		case reflect.DeepEqual(st.cfg, d.cfg):
			keep[key] = st
			continue
		default:
			changed = append(changed, st)
			reuseID[key] = st.info.ID
		}
		// Unlisted before stopping, so no new listener can join it.
		delete(s.streamsByKey, key)
		delete(s.streams, st.info.ID)
	}
	s.streamsMu.Unlock()

	// Stop everything first so a port can move between streams in one edit.
	for _, st := range removed {
		s.stopStation(st)
		s.hub.recordingIndex().forget(st.info)
		s.logger.Printf("removed stream %q in state %q, forest %q", st.info.StreamName, st.info.StateName, st.info.GroupName)
	}
	for _, st := range changed {
		s.stopStation(st)
	}

	added, failed := 0, 0
	groups := make([]stateGroup, 0, len(states))
	for _, state := range states {
		apiState := stateGroup{StateName: state.StateName, SubGroups: make([]subGroup, 0, len(state.SubGroups))}
		for _, sg := range state.SubGroups {
			apiSubGroup := subGroup{GroupName: sg.GroupName, Streams: make([]streamInfo, 0, len(sg.Streams))}
			for _, cfg := range sg.Streams {
				key := streamKey(state.StateName, sg.GroupName, cfg.StreamName)
				st := keep[key]
				if st == nil {
					var err error
					st, err = s.startStation(state.StateName, sg.GroupName, cfg, reuseID[key])
					if err != nil {
						s.logger.Printf("ERROR: stream %q in state %q, forest %q not started: %v", cfg.StreamName, state.StateName, sg.GroupName, err)
						failed++
						continue
					}
					if initial {
						s.hub.recordingIndex().register(st.info)
					} else {
						s.hub.recordingIndex().track(st.info)
						if _, wasChanged := reuseID[key]; !wasChanged {
							added++
						}
					}
					s.streamsMu.Lock()
					s.streams[st.info.ID] = st
					s.streamsByKey[key] = st
					s.streamsMu.Unlock()
				}
				apiSubGroup.Streams = append(apiSubGroup.Streams, st.info)
			}
			if len(apiSubGroup.Streams) > 0 {
				apiState.SubGroups = append(apiState.SubGroups, apiSubGroup)
			}
		}
		if len(apiState.SubGroups) > 0 {
			groups = append(groups, apiState)
		}
	}

	s.streamsMu.Lock()
	s.stateGroups = groups
	s.streamsMu.Unlock()

	if !initial && (added > 0 || len(removed) > 0 || len(changed) > 0 || failed > 0) {
		s.logger.Printf("stream config applied: %d added, %d removed, %d restarted with changes, %d failed, %d unchanged",
			added, len(removed), len(changed), failed, len(keep))
	}
	return failed
}

// recordingIndex returns the hub's recording index, nil-safe for a nil hub.
func (h *transcriptHub) recordingIndex() *recordingIndex {
	if h == nil {
		return nil
	}
	return h.index
}

// stopStation stops a station's ingest and waits until its UDP listeners
// are released. When the process is not shutting down, the clip in
// progress is finished and saved/queued first.
func (s *webrtcServer) stopStation(st *station) {
	if st.cancel == nil {
		return
	}
	st.cancel()
	<-st.done
}

// startStation binds a stream's UDP listener(s) and starts its ingest,
// broadcast and recording. id is reused when restarting a changed stream so
// open pages keep matching it; empty means a new ID.
func (s *webrtcServer) startStation(stateName, groupName string, cfg streamConfig, id string) (*station, error) {
	rt := s.runtime
	logger := s.logger
	pool := s.whisperPool
	hub := s.hub
	if id == "" {
		id = nextStreamID()
	}
	info := streamInfo{
		StateName:  stateName,
		GroupName:  groupName,
		ForestName: groupName,
		ID:         id,
		StreamName: cfg.StreamName,
		UDPPort:    cfg.UDPPort,
		TimeZone:   cfg.TimeZone,
	}

	ports, addresses, err := getListenerConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("invalid stream config: %w", err)
	}
	useMulticast := false
	for _, addr := range addresses {
		if addr != "" {
			useMulticast = true
			break
		}
	}
	var (
		ml   *MulticastListener
		conn net.PacketConn
	)
	if useMulticast || len(ports) > 1 {
		ml, err = NewMulticastListener(cfg.StreamName, ports, addresses, transmissionGap, logger, cfg.DebugMulticast)
		if err != nil {
			return nil, fmt.Errorf("create multicast listener: %w", err)
		}
	} else {
		conn, err = net.ListenPacket("udp", fmt.Sprintf(":%d", ports[0]))
		if err != nil {
			return nil, fmt.Errorf("listen on UDP %d: %w", ports[0], err)
		}
	}

	st := &station{
		info:              info,
		codec:             rt.codec,
		frameDuration:     rt.frameDuration,
		logger:            logger,
		debugMulticast:    cfg.DebugMulticast,
		audioLogDir:       rt.audioLogDir,
		audioDumpDir:      rt.audioDumpDir,
		subscribers:       make(map[string]*subscriber),
		whisperPool:       pool,
		broadcastChan:     make(chan media.Sample, 64),
		multicastListener: ml,
		cfg:               cfg,
		done:              make(chan struct{}),
	}

	if pool != nil || rt.audioLogDir != "" {
		wCfg := rt.whisper
		if wCfg == nil {
			wCfg = recorderWhisperConfig(nil)
		}
		autoTranscribe := !cfg.DisableAutoTranscribe
		if pool != nil && !autoTranscribe {
			logger.Printf("%s: automatic transcription disabled by config (disableAutoTranscribe)", info.displayName())
		}
		st.recorder = newRecorderState(
			time.Duration(wCfg.GapMs)*time.Millisecond,
			time.Duration(wCfg.MaxClipMs)*time.Millisecond,
			func(samples []int16, start time.Time) {
				// The work below includes synchronous disk I/O (WAV file
				// write). recorder.Push() calls this callback inline from
				// the station's ingest goroutine, so doing that work here
				// would block UDP packet reads for its duration — letting
				// packets queue up in the kernel socket buffer and then get
				// drained/broadcast in a burst, which live WebRTC playback
				// is much more sensitive to than a batch-written WAV.
				// Dispatch it to a background worker instead so this
				// callback returns immediately.
				job := func() {
					var wavPath, audioURL string
					durationMs := len(samples) * 1000 / recSampleRate
					s.analytics.Transmission(info, start, durationMs)
					if rt.audioLogDir != "" {
						var err error
						wavPath, audioURL, err = saveAudioClip(rt.audioLogDir, rt.audioBackupDirs, info, samples, start, logger)
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
						StreamID:    info.ID,
						StreamName:  info.StreamName,
						StateName:   info.StateName,
						GroupName:   info.GroupName,
						AudioURL:    audioURL,
						DurationMs:  durationMs,
						Timestamp:   start,
						WAVFilename: wavBaseName(requestWavPath),
					})
					s.storeClip(clipRecord{
						clipID:   clipID,
						info:     info,
						wavPath:  requestWavPath,
						audioURL: audioURL,
						start:    start,
						duration: durationMs,
					})
					if pool != nil && autoTranscribe && shouldAutoTranscribe(wCfg, durationMs) {
						if job, _, ok := s.manualTranscriptJob(clipID, ""); ok {
							pool.Submit(job)
						}
					}
				}
				select {
				case s.clipJobs <- job:
				default:
					logger.Printf("%s: clip job queue full; dropping clip finalization (disk/whisper backlog)", info.StreamName)
					// Still count the transmission, off the ingest goroutine.
					go s.analytics.Transmission(info, start, len(samples)*1000/recSampleRate)
				}
			},
		)
	}

	ctx, cancel := context.WithCancel(rt.ctx)
	st.cancel = cancel
	go st.runBroadcaster()

	if ml != nil {
		logger.Printf(
			"configured stream %q in state %q, forest %q on UDP ports %v, codec=PCMU, frame_size=%d bytes, skip_bytes=%d, frame_duration=%s",
			info.StreamName, stateName, groupName, ports, frameSizeBytes, skipBytes, rt.frameDuration,
		)
		ml.Start()
	} else {
		logger.Printf(
			"configured stream %q in state %q, forest %q on UDP %d, codec=PCMU, frame_size=%d bytes, skip_bytes=%d, frame_duration=%s",
			info.StreamName, stateName, groupName, ports[0], frameSizeBytes, skipBytes, rt.frameDuration,
		)
		// Unblocks the pending ReadFrom when the station is stopped.
		go func() {
			<-ctx.Done()
			_ = conn.Close()
		}()
	}

	go func() {
		var err error
		if ml != nil {
			err = st.ingestMulticast(ctx)
		} else {
			err = st.ingest(ctx, conn)
		}
		if err != nil && rt.ctx.Err() == nil && ctx.Err() == nil {
			logger.Printf("%s ingest stopped: %v", info.StreamName, err)
			if rt.stop != nil {
				rt.stop()
			}
		}
		cancel()
		if ml != nil {
			ml.Close()
		} else {
			_ = conn.Close()
		}
		// Removed or changed by a reload (not a shutdown): keep the clip
		// in progress instead of dropping it.
		if rt.ctx.Err() == nil && st.recorder != nil {
			st.recorder.Flush()
		}
		st.closeDumpFiles()
		st.closeSubscribers()
		close(st.broadcastChan)
		close(st.done)
	}()

	return st, nil
}

// closeDumpFiles flushes and closes the pipeline dump files (see
// appConfig.AudioDumpDir). Called only after ingest has returned.
func (s *station) closeDumpFiles() {
	for _, w := range []*bufio.Writer{s.dumpWireBuf, s.dumpMulawBuf, s.dumpTimingBuf} {
		if w != nil {
			_ = w.Flush()
		}
	}
	for _, f := range []*os.File{s.dumpWireFile, s.dumpMulawFile, s.dumpTimingFile} {
		if f != nil {
			_ = f.Close()
		}
	}
}

// watchStreamConfig polls the stream definitions and applies changes. A
// change is applied only after two consecutive identical reads, so a file
// caught mid-save is not used; an invalid file is rejected as a whole and
// the running streams are kept. Edits to other settings are logged as
// needing a restart.
func (s *webrtcServer) watchStreamConfig(ctx context.Context, src streamSource, interval time.Duration) {
	path := src.streamsFile()
	applied, seen := "", ""
	settings, _ := src.settingsFingerprint()
	missingLogged := false
	var retryAt time.Time

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if fp, err := src.settingsFingerprint(); err == nil {
			if settings != "" && fp != settings {
				s.logger.Printf("WARNING: settings in %s changed; only stream definitions apply without a restart — restart to apply the rest", src.configFile)
			}
			settings = fp
		}

		h, err := fileHash(path)
		if err != nil {
			if !missingLogged {
				s.logger.Printf("stream config: cannot read %s (%v); keeping current streams", path, err)
				missingLogged = true
			}
			continue
		}
		missingLogged = false
		if h != seen {
			seen = h
			continue
		}
		retry := !retryAt.IsZero() && !time.Now().Before(retryAt)
		if h == applied && !retry {
			continue
		}
		applied = h
		retryAt = time.Time{}

		states, _, err := src.load()
		if err != nil {
			s.logger.Printf("stream config: rejected %s, keeping current streams: %v", path, err)
			continue
		}
		if failed := s.applyStreams(states, false); failed > 0 {
			retryAt = time.Now().Add(streamRetryDelay)
		}
	}
}

func fileHash(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
