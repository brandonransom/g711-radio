package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"log"
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
	BinaryPath   string  `json:"binaryPath"`
	ModelPath    string  `json:"modelPath"`
	Workers      int     `json:"workers"`
	VADThreshold float64 `json:"vadThreshold"`
	SilenceMs    int     `json:"silenceMs"`
	MinClipMs    int     `json:"minClipMs"`
	MaxClipMs    int     `json:"maxClipMs"`
	TimeoutMs    int     `json:"timeoutMs"`
	AutoTranscribeMinClipMs int `json:"autoTranscribeMinClipMs"`
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
	cfg    whisperConfig
	mu     sync.Mutex
	queue  []transcriptJob
	ready  chan struct{}
	hub    *transcriptHub
	logger *log.Logger
	done   chan struct{}
}

func newWhisperPool(cfg whisperConfig, hub *transcriptHub, logger *log.Logger) *whisperPool {
	return &whisperPool{
		cfg:    cfg,
		ready:  make(chan struct{}, 1),
		hub:    hub,
		logger: logger,
		done:   make(chan struct{}),
	}
}

// Start launches worker goroutines.
func (p *whisperPool) Start() {
	for i := 0; i < p.cfg.Workers; i++ {
		go p.worker(i)
	}
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

// transcribe passes the saved WAV file directly to whisper-cli with --convert,
// letting whisper handle any resampling. This avoids a fragile re-encode step.
func (p *whisperPool) transcribe(wavPath string) (string, error) {
	binary := p.cfg.BinaryPath
	if binary == "" {
		binary = "whisper-cli"
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
