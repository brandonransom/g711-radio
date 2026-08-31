// Command whisper-server is a standalone HTTP server that accepts recorded
// WAV audio clips and transcribes them using a locally installed
// whisper.cpp (whisper-cli) binary. It is meant to run on a separate, more
// powerful host than the g711-radio WebRTC server: point the main app's
// whisper.remoteHost config field at this server's address to offload
// transcription here instead of running whisper-cli on the WebRTC host.
//
// There is no authentication. This is intended to run on a trusted
// internal/private network only — firewall the configured port if this
// host is otherwise reachable.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// maxUploadBytes bounds how much a single /transcribe request body may
// contain, to avoid unbounded memory/disk usage from a misbehaving client.
const maxUploadBytes int64 = 64 << 20 // 64MB

// serverConfig holds runtime configuration for the remote whisper server.
type serverConfig struct {
	Port       int    `json:"port"`
	BinaryPath string `json:"binaryPath"`
	ModelPath  string `json:"modelPath"`
	Workers    int    `json:"workers"`
	TimeoutMs  int    `json:"timeoutMs"`
}

func (c *serverConfig) setDefaults() {
	if c.Port <= 0 {
		c.Port = 8090
	}
	if c.BinaryPath == "" {
		c.BinaryPath = "whisper-cli"
	}
	if c.Workers <= 0 {
		c.Workers = 2
	}
	if c.TimeoutMs <= 0 {
		c.TimeoutMs = 60000
	}
}

func loadConfig(path string) (serverConfig, error) {
	var cfg serverConfig
	file, err := os.Open(path)
	if err != nil {
		return cfg, fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("decode %s: %w", path, err)
	}
	cfg.setDefaults()
	return cfg, nil
}

// transcriptionServer holds shared state for handling /transcribe and
// /healthz requests.
type transcriptionServer struct {
	cfg            serverConfig
	logger         *log.Logger
	sem            chan struct{} // buffered semaphore bounding concurrent whisper-cli runs
	inFlight       atomic.Int64
	totalProcessed atomic.Int64
}

func newTranscriptionServer(cfg serverConfig, logger *log.Logger) *transcriptionServer {
	return &transcriptionServer{
		cfg:    cfg,
		logger: logger,
		sem:    make(chan struct{}, cfg.Workers),
	}
}

func main() {
	configPath := flag.String("config", "whisper-server.config.json", "path to the server config JSON file")
	flag.Parse()

	logger := log.New(os.Stdout, "", log.LstdFlags)

	cfg, err := loadConfig(*configPath)
	if err != nil {
		logger.Fatalf("load config: %v", err)
	}
	if cfg.ModelPath == "" {
		logger.Fatalf("config error: %q is missing a required \"modelPath\" value", *configPath)
	}

	srv := newTranscriptionServer(cfg, logger)

	mux := http.NewServeMux()
	mux.HandleFunc("/transcribe", srv.handleTranscribe)
	mux.HandleFunc("/healthz", srv.handleHealthz)

	httpServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Port),
		Handler: mux,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		logger.Printf("shutdown signal received, draining connections...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	logger.Printf("whisper-server listening on :%d (binary=%s model=%s workers=%d timeoutMs=%d)",
		cfg.Port, cfg.BinaryPath, cfg.ModelPath, cfg.Workers, cfg.TimeoutMs)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Fatal(err)
	}
	logger.Printf("server stopped")
}

// handleTranscribe accepts a raw WAV file body, runs it through whisper-cli,
// and responds with the cleaned transcript text.
func (s *transcriptionServer) handleTranscribe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed, use POST")
		return
	}

	start := time.Now()
	streamName := r.Header.Get("X-Stream-Name")
	remoteAddr := r.RemoteAddr

	body, err := io.ReadAll(io.LimitReader(r.Body, maxUploadBytes+1))
	if err != nil {
		s.logger.Printf("transcribe: stream=%q addr=%s: read body: %v", streamName, remoteAddr, err)
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	if len(body) == 0 {
		writeError(w, http.StatusBadRequest, "request body is empty")
		return
	}
	if int64(len(body)) > maxUploadBytes {
		writeError(w, http.StatusBadRequest, "request body exceeds maximum allowed size")
		return
	}

	tmpFile, err := os.CreateTemp("", "whisper-server-*.wav")
	if err != nil {
		s.logger.Printf("transcribe: stream=%q addr=%s: create temp file: %v", streamName, remoteAddr, err)
		writeError(w, http.StatusInternalServerError, "failed to stage upload")
		return
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	_, writeErr := tmpFile.Write(body)
	closeErr := tmpFile.Close()
	if writeErr != nil || closeErr != nil {
		s.logger.Printf("transcribe: stream=%q addr=%s: write temp file: write=%v close=%v", streamName, remoteAddr, writeErr, closeErr)
		writeError(w, http.StatusInternalServerError, "failed to stage upload")
		return
	}

	// Acquire a worker slot, but give up with 408 instead of blocking forever
	// if the client's request context is cancelled while we wait (e.g. it
	// disconnected or its own client-side timeout elapsed).
	select {
	case s.sem <- struct{}{}:
	case <-r.Context().Done():
		writeError(w, http.StatusRequestTimeout, "request cancelled while waiting for a free worker")
		return
	}
	defer func() { <-s.sem }()

	s.inFlight.Add(1)
	defer s.inFlight.Add(-1)

	text, err := s.runWhisperCLI(r.Context(), tmpPath)
	elapsed := time.Since(start)
	if err != nil {
		s.logger.Printf("transcribe: stream=%q addr=%s bytes=%d elapsed=%s failed: %v", streamName, remoteAddr, len(body), elapsed, err)
		status := http.StatusInternalServerError
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			status = http.StatusGatewayTimeout
		}
		writeError(w, status, err.Error())
		return
	}

	s.totalProcessed.Add(1)
	preview := text
	if len(preview) > 200 {
		preview = preview[:200] + "..."
	}
	s.logger.Printf("transcribe: stream=%q addr=%s bytes=%d elapsed=%s text=%q", streamName, remoteAddr, len(body), elapsed, preview)

	writeJSON(w, http.StatusOK, map[string]string{"text": text})
}

// runWhisperCLI invokes whisper-cli against the given WAV file, bounding
// execution by cfg.TimeoutMs derived from the request context so that
// client-side cancellation propagates down to the subprocess.
func (s *transcriptionServer) runWhisperCLI(reqCtx context.Context, wavPath string) (string, error) {
	ctx, cancel := context.WithTimeout(reqCtx, time.Duration(s.cfg.TimeoutMs)*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(
		ctx,
		s.cfg.BinaryPath,
		"-m", s.cfg.ModelPath,
		"-f", wavPath,
		"--convert",
		"-nt",
		"-np",
		"--language", "en",
	)

	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("whisper-cli timed out or was cancelled: %w", ctx.Err())
		}
		if stderr := strings.TrimSpace(errBuf.String()); stderr != "" {
			return "", fmt.Errorf("whisper-cli failed: %v: %s", err, stderr)
		}
		return "", fmt.Errorf("whisper-cli failed: %w", err)
	}

	return cleanWhisperOutput(out.String()), nil
}

// handleHealthz reports basic liveness and load information.
func (s *transcriptionServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "ok",
		"workers":        s.cfg.Workers,
		"inFlight":       s.inFlight.Load(),
		"totalProcessed": s.totalProcessed.Load(),
	})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// cleanWhisperOutput strips bracketed timestamps and blank lines from
// whisper-cli's stdout. This mirrors the root package's helper of the same
// name; it's duplicated here because this is a separate "main" package and
// cannot import code from the repo's root package.
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
