package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

// transcriptJob is submitted by VAD when a transmission clip is ready.
type transcriptJob struct {
	info    streamInfo
	samples []int16 // 8kHz mono PCM16
	start   time.Time
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
}

// whisperPool manages a job queue and a fixed number of worker goroutines
// that each call whisper.cpp to transcribe audio clips.
type whisperPool struct {
	cfg    whisperConfig
	jobs   chan transcriptJob
	hub    *transcriptHub
	logger *log.Logger
}

func newWhisperPool(cfg whisperConfig, hub *transcriptHub, logger *log.Logger) *whisperPool {
	return &whisperPool{
		cfg:    cfg,
		jobs:   make(chan transcriptJob, 128),
		hub:    hub,
		logger: logger,
	}
}

// Start launches worker goroutines. They run until ctx is cancelled via the
// jobs channel being closed.
func (p *whisperPool) Start() {
	for i := 0; i < p.cfg.Workers; i++ {
		go p.worker(i)
	}
}

// Submit enqueues a clip for transcription. Non-blocking: drops if queue full.
func (p *whisperPool) Submit(job transcriptJob) {
	p.logger.Printf("whisper pool: submitting clip from %s (%d samples, %.1fs)", job.info.StreamName, len(job.samples), float64(len(job.samples))/8000.0)
	select {
	case p.jobs <- job:
	default:
		p.logger.Printf("whisper pool: queue full, dropping clip from %s", job.info.StreamName)
	}
}

// Close signals workers to stop after draining the queue.
func (p *whisperPool) Close() {
	close(p.jobs)
}

func (p *whisperPool) worker(id int) {
	for job := range p.jobs {
		text, err := p.transcribe(job.samples)
		if err != nil {
			p.logger.Printf("whisper worker %d: transcribe %s: %v", id, job.info.StreamName, err)
			continue
		}
		text = strings.TrimSpace(text)
		if text == "" {
			p.logger.Printf("whisper worker %d: empty result for %s (clip len=%d samples)", id, job.info.StreamName, len(job.samples))
			continue
		}
		p.hub.Publish(transcriptEvent{
			StreamID:   job.info.ID,
			StreamName: job.info.StreamName,
			RegionName: job.info.RegionName,
			GroupName:  job.info.GroupName,
			Text:       text,
			Timestamp:  job.start,
		})
		p.logger.Printf("whisper worker %d: [%s] %s", id, job.info.StreamName, text)
	}
}

// transcribe writes the audio to a temp WAV file, runs whisper.cpp, and returns the transcript.
func (p *whisperPool) transcribe(samples []int16) (string, error) {
	// whisper.cpp expects 16kHz mono WAV. Upsample from 8kHz by duplicating each sample.
	upsampled := upsample8to16kHz(samples)

	wav, err := encodePCM16WAV(upsampled, 16000)
	if err != nil {
		return "", fmt.Errorf("encode wav: %w", err)
	}

	tmp, err := os.CreateTemp("", "g711-whisper-*.wav")
	if err != nil {
		return "", fmt.Errorf("create temp wav: %w", err)
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(wav); err != nil {
		tmp.Close()
		return "", fmt.Errorf("write temp wav: %w", err)
	}
	tmp.Close()

	// Run whisper.cpp main binary with a 60-second timeout.
	// -m  model path
	// -f  input file
	// -nt no timestamps in output
	// -np no progress
	// --language en
	binary := p.cfg.BinaryPath
	if binary == "" {
		binary = "whisper-cli"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(
		ctx,
		binary,
		"-m", p.cfg.ModelPath,
		"-f", tmp.Name(),
		"-nt",
		"-np",
		"--language", "en",
		"--no-prints",
	)

	var out bytes.Buffer
	var errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf

	p.logger.Printf("whisper: running %s on %d samples", binary, len(samples))
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("whisper-cli: %w\nstderr: %s", err, errBuf.String())
	}
	p.logger.Printf("whisper: stdout=%q stderr=%q", strings.TrimSpace(out.String()), strings.TrimSpace(errBuf.String()))

	return cleanWhisperOutput(out.String()), nil
}

// upsample8to16kHz duplicates every sample to double the sample rate.
func upsample8to16kHz(in []int16) []int16 {
	out := make([]int16, len(in)*2)
	for i, s := range in {
		out[i*2] = s
		out[i*2+1] = s
	}
	return out
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
