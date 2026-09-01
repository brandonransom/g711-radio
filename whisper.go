package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// transcriptJob is submitted by VAD when a transmission clip is ready.
type transcriptJob struct {
	info     streamInfo
	clipID   string    // unique ID correlating the clip event with its later transcript
	wavPath  string    // path to the already-saved 8kHz WAV file on disk
	audioURL string    // relative URL served to browsers (e.g. /audio/...)
	start    time.Time
	manual   bool
}

// whisperConfig holds runtime configuration for the Whisper worker pool.
type whisperConfig struct {
	BinaryPath              string  `json:"binaryPath"`
	ModelPath               string  `json:"modelPath"`
	RemoteHost              string  `json:"remoteHost"`
	Workers                 int     `json:"workers"`
	VADThreshold            float64 `json:"vadThreshold"`
	SilenceMs               int     `json:"silenceMs"`
	MinClipMs               int     `json:"minClipMs"`
	MaxClipMs               int     `json:"maxClipMs"`
	TimeoutMs               int     `json:"timeoutMs"`
	AutoTranscribeMinClipMs int     `json:"autoTranscribeMinClipMs"`
}

func (c *whisperConfig) setDefaults() {
	if c.Workers <= 0 {
		c.Workers = 2
	}
	if c.VADThreshold <= 0 {
		c.VADThreshold = 0.02
	}
	if c.SilenceMs <= 0 {
		c.SilenceMs = 600
	}
	if c.MinClipMs <= 0 {
		c.MinClipMs = 300
	}
	if c.MaxClipMs <= 0 {
		c.MaxClipMs = 30000
	}
	if c.TimeoutMs <= 0 {
		c.TimeoutMs = 60000
	}
	if c.AutoTranscribeMinClipMs < 0 {
		c.AutoTranscribeMinClipMs = 0
	}
}

// whisperPool is defined below with its unbounded queue fields.

// whisperPool manages an unbounded FIFO job queue and a fixed number of worker
// goroutines that each call whisper.cpp to transcribe audio clips.
type whisperPool struct {
	cfg        whisperConfig
	mu         sync.Mutex
	queue      []transcriptJob
	ready      chan struct{}
	hub        *transcriptHub
	logger     *log.Logger
	done       chan struct{}
	httpClient *http.Client
}

func newWhisperPool(cfg whisperConfig, hub *transcriptHub, logger *log.Logger) *whisperPool {
	return &whisperPool{
		cfg:    cfg,
		ready:  make(chan struct{}, 1),
		hub:    hub,
		logger: logger,
		done:   make(chan struct{}),
		httpClient: &http.Client{
			// Safety margin above the per-request context timeout used in
			// transcribeRemote, so the context (not the client) is what
			// normally cancels a slow request.
			Timeout: time.Duration(cfg.TimeoutMs+5000) * time.Millisecond,
		},
	}
}

// Start launches worker goroutines. If a remote whisper server is configured,
// it first performs a one-time, non-fatal reachability check so operators get
// early feedback in the logs without blocking or crashing startup.
func (p *whisperPool) Start() {
	if p.cfg.RemoteHost != "" {
		p.checkRemoteReachability()
	}
	for i := 0; i < p.cfg.Workers; i++ {
		go p.worker(i)
	}
}

// checkRemoteReachability performs a best-effort GET against the remote
// whisper server's /healthz endpoint purely for diagnostic logging at
// startup. It never returns an error and never blocks startup for long —
// failures are logged as warnings only.
func (p *whisperPool) checkRemoteReachability() {
	base := remoteBaseURL(p.cfg.RemoteHost)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(base + "/healthz")
	if err != nil {
		p.logger.Printf("WARNING: remote whisper server at %s unreachable: %v", base, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		p.logger.Printf("WARNING: remote whisper server at %s unreachable: status %s", base, resp.Status)
		return
	}
	p.logger.Printf("remote whisper server at %s is reachable", base)
}

// remoteBaseURL normalizes a configured RemoteHost into a base URL: it
// defaults to the http:// scheme when none is given and strips any trailing
// slash so callers can safely append a path like "/transcribe".
func remoteBaseURL(remoteHost string) string {
	base := remoteHost
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	return strings.TrimRight(base, "/")
}

// Submit enqueues a clip for transcription. Never drops — unbounded FIFO.
func (p *whisperPool) Submit(job transcriptJob) {
	p.mu.Lock()
	p.queue = append(p.queue, job)
	qlen := len(p.queue)
	p.mu.Unlock()
	if qlen == 1 {
		select {
		case p.ready <- struct{}{}:
		default:
		}
	}
	p.logger.Printf("whisper pool: queued clip from %s (queue depth: %d)", job.info.StreamName, qlen)
}

func (p *whisperPool) dequeue() (transcriptJob, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.queue) == 0 {
		return transcriptJob{}, false
	}
	job := p.queue[0]
	p.queue = p.queue[1:]
	return job, true
}

// Close signals workers to stop.
func (p *whisperPool) Close() {
	close(p.done)
}

func (p *whisperPool) worker(id int) {
	for {
		select {
		case <-p.done:
			return
		case <-p.ready:
		}
		for {
			job, ok := p.dequeue()
			if !ok {
				break
			}
			text, err := p.transcribe(job.wavPath)
			if err != nil {
				p.logger.Printf("whisper worker %d: transcribe %s: %v", id, job.info.StreamName, err)
				continue
			}
			text = strings.TrimSpace(text)
			if text == "" {
				continue
			}
			p.hub.Publish(transcriptEvent{
				Type:       "transcript",
				ClipID:     job.clipID,
				StreamID:   job.info.ID,
				StreamName: job.info.StreamName,
				RegionName: job.info.RegionName,
				GroupName:  job.info.GroupName,
				Text:       text,
				AudioURL:   job.audioURL,
				Timestamp:  job.start,
			})
			p.logger.Printf("whisper worker %d: [%s] %s", id, job.info.StreamName, text)
		}
		// Signal other workers there may still be items.
		p.mu.Lock()
		remaining := len(p.queue)
		p.mu.Unlock()
		if remaining > 0 {
			select {
			case p.ready <- struct{}{}:
			default:
			}
		}
	}
}

// transcribeLocal passes the saved WAV file directly to whisper-cli with --convert,
// letting whisper handle any resampling. This avoids a fragile re-encode step.
func (p *whisperPool) transcribeLocal(wavPath string) (string, error) {
	binary := p.cfg.BinaryPath
	if binary == "" {
		binary = "WhisperCLI.exe"
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(p.cfg.TimeoutMs)*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(
		ctx,
		binary,
		"-m", p.cfg.ModelPath,
		"-f", wavPath,
		"--convert",
		"-nt",
		"-np",
		"--language", "en",
	)

	var out bytes.Buffer
	var errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("whisper-cli: %w\nstderr: %s", err, errBuf.String())
	}

	return cleanWhisperOutput(out.String()), nil
}

// transcribeRemote sends the saved WAV file to a remote whisper-server
// instance (see cmd/whisper-server) over HTTP instead of running whisper-cli
// locally. The remote host has no authentication and is expected to run on a
// trusted private network.
func (p *whisperPool) transcribeRemote(wavPath string) (string, error) {
	data, err := os.ReadFile(wavPath)
	if err != nil {
		return "", fmt.Errorf("remote whisper: read %s: %w", wavPath, err)
	}

	url := remoteBaseURL(p.cfg.RemoteHost) + "/transcribe"

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(p.cfg.TimeoutMs)*time.Millisecond)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("remote whisper %s: build request: %w", url, err)
	}
	req.Header.Set("Content-Type", "audio/wav")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("remote whisper %s: %w", url, err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		var errResp struct {
			Error string `json:"error"`
		}
		if readErr == nil && json.Unmarshal(body, &errResp) == nil && errResp.Error != "" {
			return "", fmt.Errorf("remote whisper %s: %s", url, errResp.Error)
		}
		return "", fmt.Errorf("remote whisper %s: unexpected status %s", url, resp.Status)
	}
	if readErr != nil {
		return "", fmt.Errorf("remote whisper %s: read response: %w", url, readErr)
	}

	var okResp struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body, &okResp); err != nil {
		return "", fmt.Errorf("remote whisper %s: decode response: %w", url, err)
	}

	return okResp.Text, nil
}

// transcribe dispatches to a remote whisper-server instance when RemoteHost
// is configured, otherwise it runs whisper-cli locally as a subprocess.
func (p *whisperPool) transcribe(wavPath string) (string, error) {
	if p.cfg.RemoteHost != "" {
		return p.transcribeRemote(wavPath)
	}
	return p.transcribeLocal(wavPath)
}

// encodePCM16WAV encodes int16 samples into a standard WAV byte slice.
func encodePCM16WAV(samples []int16, sampleRate int) ([]byte, error) {
	numSamples := len(samples)
	dataSize := numSamples * 2 // int16 = 2 bytes per sample
	fileSize := 36 + dataSize

	var buf bytes.Buffer
	buf.Grow(fileSize + 8)

	write := func(v any) {
		_ = binary.Write(&buf, binary.LittleEndian, v)
	}

	// RIFF header
	buf.WriteString("RIFF")
	write(uint32(fileSize))
	buf.WriteString("WAVE")

	// fmt chunk
	buf.WriteString("fmt ")
	write(uint32(16))         // chunk size
	write(uint16(1))          // PCM
	write(uint16(1))          // mono
	write(uint32(sampleRate)) // sample rate
	write(uint32(sampleRate * 2)) // byte rate
	write(uint16(2))          // block align
	write(uint16(16))         // bits per sample

	// data chunk
	buf.WriteString("data")
	write(uint32(dataSize))
	for _, s := range samples {
		write(s)
	}

	return buf.Bytes(), nil
}

// cleanWhisperOutput strips bracketed timestamps and whitespace from whisper output.
func cleanWhisperOutput(raw string) string {
	var lines []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Strip lines that are pure timestamp markers like [00:00:00.000 --> 00:00:05.000]
		if strings.HasPrefix(line, "[") && strings.Contains(line, "-->") {
			continue
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, " ")
}
