package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// transcriptJob is submitted when a recorded clip is ready.
type transcriptJob struct {
	info     streamInfo
	clipID   string // unique ID correlating the clip event with its later transcript
	wavPath  string // path to the already-saved 8kHz WAV file on disk
	audioURL string // relative URL served to browsers (e.g. /audio/...)
	start    time.Time
	manual   bool
	// retries counts how many times this job was handed back to the queue
	// because its server was unreachable (see maxJobRetries).
	retries int
}

// maxJobRetries bounds how often one clip is requeued after a connection
// failure, so a clip that crashes a whisper-server can't crash-loop it.
const maxJobRetries = 2

const (
	defaultWhisperWorkers = 2
	defaultLocalBasePort  = 18910
	defaultServerBinary   = "whisper-server"
)

// whisperConfig is the "whisper" block of config.json — the only place
// transcription is configured. loadConfig (main.go) decodes it into
// appConfig.Whisper and calls validate(); main() then calls setDefaults()
// and passes it to newWhisperPool.
//
// Transcription always goes through whisper.cpp's own HTTP server
// (whisper-server), which keeps the model loaded between clips instead of
// reloading it for every clip as whisper-cli did. Each whisper-server
// instance transcribes one clip at a time, so parallelism comes from
// running several instances — one pool worker is bound to each.
//
//   - Remote mode (RemoteServers set): POST to whisper-server instances that
//     you run yourself on another host.
//   - Local mode (ModelPath set): this process launches Workers instances of
//     ServerBinaryPath on 127.0.0.1 and restarts them if they exit.
type whisperConfig struct {
	// RemoteServers lists base URLs of whisper.cpp whisper-server instances,
	// e.g. "http://gpu-box:8080". Setting it selects remote mode.
	RemoteServers []string `json:"remoteServers"`

	// ServerBinaryPath is the whisper.cpp whisper-server executable (local mode).
	ServerBinaryPath string `json:"serverBinaryPath"`
	// ModelPath is the GGML model file loaded by each local instance.
	ModelPath string `json:"modelPath"`
	// Workers is the number of local whisper-server instances to launch.
	// In remote mode concurrency is len(RemoteServers) and this is ignored.
	Workers int `json:"workers"`
	// LocalBasePort is the loopback port of the first local instance; the
	// rest use the following consecutive ports.
	LocalBasePort int `json:"localBasePort"`
	// ServerArgs are extra command-line flags appended when launching each
	// local instance (load-time settings such as "-t", "-fa", "-ng", "-p").
	ServerArgs []string `json:"serverArgs"`

	// InferenceParams are extra form fields sent with every /inference
	// request in both modes (per-request decoding settings such as
	// "beam_size", "best_of", "temperature", "prompt", "no_speech_thold").
	// Values may be JSON strings, numbers, or booleans.
	InferenceParams map[string]any `json:"inferenceParams"`
	// TimeoutMs bounds a single /inference request.
	TimeoutMs int `json:"timeoutMs"`

	// GapMs and MaxClipMs drive the presence-based recorder (see recorder.go).
	GapMs     int `json:"gapMs"`
	MaxClipMs int `json:"maxClipMs"`
	// AutoTranscribeMinClipMs and AutoTranscribeMaxClipMs bound which clips
	// are queued for transcription automatically. A clip shorter than the
	// minimum is usually a key-up blip with no speech; one longer than the
	// maximum is usually a stuck transmitter or open carrier, and is
	// expensive to transcribe. Zero disables that bound, and either way a
	// clip outside the window can still be transcribed on demand from the
	// web UI (see requestClipTranscription).
	AutoTranscribeMinClipMs int `json:"autoTranscribeMinClipMs"`
	AutoTranscribeMaxClipMs int `json:"autoTranscribeMaxClipMs"`

	// HallucinationFilter removes text whisper invents from silence or
	// noise (subtitle credits and the like); see hallucination.go. On by
	// default with a built-in phrase list.
	HallucinationFilter *hallucinationFilterConfig `json:"hallucinationFilter"`

	// Removed settings. They are still decoded so validate() can explain
	// how to migrate instead of failing with a bare "unknown field" error.
	LegacyBinaryPath string `json:"binaryPath"`
	LegacyRemoteHost string `json:"remoteHost"`
}

// reservedServerArgs are flags g711-radio sets itself when launching a local
// whisper-server; overriding them would break how it reaches the instance.
var reservedServerArgs = map[string]bool{
	"-m": true, "--model": true,
	"--host": true, "--port": true,
	"--request-path": true, "--inference-path": true,
}

// reservedInferenceParams are form fields g711-radio sets itself.
var reservedInferenceParams = map[string]bool{
	"file":            true, // the clip itself
	"response_format": true, // responses are parsed as JSON
}

// enabled reports whether transcription is configured at all.
func (c *whisperConfig) enabled() bool {
	return c != nil && (len(c.RemoteServers) > 0 || c.ModelPath != "")
}

func (c *whisperConfig) isRemote() bool {
	return len(c.RemoteServers) > 0
}

// validate rejects settings that cannot work. It is called by loadConfig so
// mistakes surface as config errors at startup.
func (c *whisperConfig) validate() error {
	if c.LegacyBinaryPath != "" {
		return errors.New(`"binaryPath" (whisper-cli) is no longer supported: set "serverBinaryPath" to whisper.cpp's whisper-server executable, which is built next to whisper-cli`)
	}
	if c.LegacyRemoteHost != "" {
		return errors.New(`"remoteHost" (cmd/whisper-server) is no longer supported: run whisper.cpp's whisper-server on the remote host and list its URL(s) in "remoteServers"`)
	}
	for _, raw := range c.RemoteServers {
		if _, err := whisperServerURL(raw); err != nil {
			return fmt.Errorf("remoteServers: %w", err)
		}
	}
	for _, arg := range c.ServerArgs {
		flagName, _, _ := strings.Cut(arg, "=")
		if reservedServerArgs[flagName] {
			return fmt.Errorf("serverArgs: %q is set by g711-radio and cannot be overridden", flagName)
		}
	}
	for key, value := range c.InferenceParams {
		if reservedInferenceParams[key] {
			return fmt.Errorf("inferenceParams: %q is set by g711-radio and cannot be overridden", key)
		}
		if _, err := formatInferenceParam(value); err != nil {
			return fmt.Errorf("inferenceParams.%s: %w", key, err)
		}
	}
	if c.LocalBasePort < 0 || c.LocalBasePort > 65535 {
		return fmt.Errorf("localBasePort %d is not a valid port", c.LocalBasePort)
	}
	if c.LocalBasePort > 0 && c.Workers > 0 && c.LocalBasePort+c.Workers-1 > 65535 {
		return fmt.Errorf("localBasePort %d + workers %d runs past port 65535", c.LocalBasePort, c.Workers)
	}
	if _, err := newHallucinationFilter(c.HallucinationFilter); err != nil {
		return fmt.Errorf("hallucinationFilter: %w", err)
	}
	return nil
}

func (c *whisperConfig) setDefaults() {
	if c.Workers <= 0 {
		c.Workers = defaultWhisperWorkers
	}
	if c.LocalBasePort <= 0 {
		c.LocalBasePort = defaultLocalBasePort
	}
	if c.ServerBinaryPath == "" {
		c.ServerBinaryPath = defaultServerBinary
	}
	if c.GapMs <= 0 {
		c.GapMs = 4000
	}
	if c.MaxClipMs <= 0 {
		c.MaxClipMs = 600000
	}
	if c.TimeoutMs <= 0 {
		c.TimeoutMs = 60000
	}
	if c.AutoTranscribeMinClipMs < 0 {
		c.AutoTranscribeMinClipMs = 0
	}
	if c.AutoTranscribeMaxClipMs < 0 {
		c.AutoTranscribeMaxClipMs = 0
	}
}

// ignoredLocalFields names local-mode settings that remote mode ignores, so
// startup can say so instead of silently dropping them.
func (c *whisperConfig) ignoredLocalFields() []string {
	var fields []string
	if c.ModelPath != "" {
		fields = append(fields, "modelPath")
	}
	if c.ServerBinaryPath != "" && c.ServerBinaryPath != defaultServerBinary {
		fields = append(fields, "serverBinaryPath")
	}
	if len(c.ServerArgs) > 0 {
		fields = append(fields, "serverArgs")
	}
	if c.LocalBasePort != 0 && c.LocalBasePort != defaultLocalBasePort {
		fields = append(fields, "localBasePort")
	}
	return fields
}

// formatInferenceParam converts a JSON config value to a form field value.
func formatInferenceParam(value any) (string, error) {
	switch v := value.(type) {
	case string:
		return v, nil
	case bool:
		return strconv.FormatBool(v), nil
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	case int:
		return strconv.Itoa(v), nil
	default:
		return "", fmt.Errorf("value must be a string, number, or boolean, got %T", value)
	}
}

// whisperServerURL normalizes a whisper-server base URL: it defaults to
// http:// and strips any trailing slash so "/inference" and "/health" can be
// appended. A path is kept, which matches whisper-server's --request-path.
func whisperServerURL(raw string) (string, error) {
	base := strings.TrimSpace(raw)
	if base == "" {
		return "", errors.New("empty server URL")
	}
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("invalid server URL %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("server URL %q must use http or https", raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("server URL %q has no host", raw)
	}
	return strings.TrimRight(base, "/"), nil
}

// whisperEndpoint is one whisper-server instance. Exactly one pool worker is
// bound to each endpoint, because whisper-server handles one request at a
// time and sending it more would only queue them server-side.
type whisperEndpoint struct {
	label   string
	baseURL string
	// managed endpoints are local instances whose readiness is driven by
	// superviseLocal; unmanaged (remote) endpoints are probed by their worker.
	managed bool

	mu      sync.Mutex
	ready   bool
	readyCh chan struct{} // closed while ready
}

func newWhisperEndpoint(label, baseURL string, managed bool) *whisperEndpoint {
	e := &whisperEndpoint{label: label, baseURL: baseURL, managed: managed, readyCh: make(chan struct{})}
	if !managed {
		// Remote servers are assumed up until a request proves otherwise.
		e.setReady(true)
	}
	return e
}

func (e *whisperEndpoint) setReady(ready bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if ready == e.ready {
		return
	}
	e.ready = ready
	if ready {
		close(e.readyCh)
	} else {
		e.readyCh = make(chan struct{})
	}
}

func (e *whisperEndpoint) isReady() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.ready
}

func (e *whisperEndpoint) readyChan() <-chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.readyCh
}

// whisperPool manages an unbounded FIFO job queue and one worker goroutine
// per whisper-server endpoint.
type whisperPool struct {
	cfg   whisperConfig
	mu    sync.Mutex
	queue []transcriptJob
	// bulkQueue holds "transcribe all filtered" requests. Workers drain it
	// only when queue is empty, so a large backfill never delays live clips
	// or a listener's single on-demand request.
	bulkQueue []transcriptJob
	// queued is the set of WAV paths waiting in either queue, so the same
	// clip requested twice (e.g. a bulk request overlapping an earlier one)
	// is only transcribed once.
	queued     map[string]struct{}
	ready      chan struct{}
	hub        *transcriptHub
	logger     *log.Logger
	done       chan struct{}
	closeOnce  sync.Once
	ctx        context.Context // cancelled by Close; kills local instances
	cancel     context.CancelFunc
	wg         sync.WaitGroup // local instance supervisors
	httpClient *http.Client
	endpoints  []*whisperEndpoint
	formFields map[string]string
	filter     *hallucinationFilter
}

func newWhisperPool(cfg whisperConfig, hub *transcriptHub, logger *log.Logger) *whisperPool {
	ctx, cancel := context.WithCancel(context.Background())
	filter, err := newHallucinationFilter(cfg.HallucinationFilter)
	if err != nil {
		// validate() rejects bad filters at startup; this only guards
		// callers that skip it.
		logger.Printf("whisper: hallucination filter disabled: %v", err)
	}
	p := &whisperPool{
		cfg:        cfg,
		ready:      make(chan struct{}, 1),
		hub:        hub,
		logger:     logger,
		done:       make(chan struct{}),
		ctx:        ctx,
		cancel:     cancel,
		httpClient: &http.Client{}, // per-request deadlines come from TimeoutMs contexts
		formFields: inferenceFormFields(cfg.InferenceParams),
		filter:     filter,
	}
	return p
}

// inferenceFormFields builds the form fields sent with every request:
// defaults matching the old whisper-cli invocation ("-nt --language en",
// plus whisper-cli's beam search defaults, which whisper-server does not
// share — it defaults to greedy decoding with best_of=2), then the user's
// inferenceParams, then the fields the response parser relies on.
func inferenceFormFields(params map[string]any) map[string]string {
	fields := map[string]string{
		"language":      "en",
		"no_timestamps": "true",
		"beam_size":     "5",
		"best_of":       "5",
	}
	for key, value := range params {
		if s, err := formatInferenceParam(value); err == nil {
			fields[key] = s
		}
	}
	fields["response_format"] = "json"
	return fields
}

// Start resolves the configured endpoints and launches the workers. In local
// mode it also launches and supervises the whisper-server instances. An
// error means transcription cannot work with this config.
func (p *whisperPool) Start() error {
	if p.cfg.isRemote() {
		if ignored := p.cfg.ignoredLocalFields(); len(ignored) > 0 {
			p.logger.Printf("whisper: remote mode — ignoring local-mode settings: %s", strings.Join(ignored, ", "))
		}
		for i, raw := range p.cfg.RemoteServers {
			base, err := whisperServerURL(raw)
			if err != nil {
				return err
			}
			p.endpoints = append(p.endpoints, newWhisperEndpoint(
				fmt.Sprintf("server#%d %s", i+1, base), base, false))
		}
		p.checkRemoteReachability()
	} else {
		if err := p.startLocalInstances(); err != nil {
			return err
		}
	}
	for _, ep := range p.endpoints {
		go p.worker(ep)
	}
	return nil
}

// checkRemoteReachability logs whether each remote server answers /health.
// It is diagnostic only and never blocks startup for long.
func (p *whisperPool) checkRemoteReachability() {
	for _, ep := range p.endpoints {
		if err := checkWhisperHealth(p.ctx, ep.baseURL, 5*time.Second); err != nil {
			p.logger.Printf("WARNING: whisper server %s is not ready: %v", ep.label, err)
			continue
		}
		p.logger.Printf("whisper server %s is ready", ep.label)
	}
}

// checkWhisperHealth calls whisper-server's GET /health, which returns 200
// once the model is loaded (503 while still loading).
func checkWhisperHealth(ctx context.Context, baseURL string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %s", resp.Status)
	}
	return nil
}

// maxBulkQueue caps how many bulk-transcription jobs may wait at once, so
// repeated "transcribe all" clicks can't queue an unbounded backlog.
const maxBulkQueue = 10000

// bulkSubmitResult reports what SubmitBulk did with one job.
type bulkSubmitResult int

const (
	bulkQueued bulkSubmitResult = iota
	bulkDuplicate
	bulkFull
)

// markQueuedLocked records job as waiting. It reports false if the same WAV
// is already queued. Callers hold p.mu.
func (p *whisperPool) markQueuedLocked(job transcriptJob) bool {
	if job.wavPath == "" {
		return true
	}
	if p.queued == nil {
		p.queued = make(map[string]struct{})
	}
	if _, dup := p.queued[job.wavPath]; dup {
		return false
	}
	p.queued[job.wavPath] = struct{}{}
	return true
}

// Submit queues a clip at normal priority. The normal queue is an unbounded
// FIFO and never drops. A clip whose WAV is already
// waiting is not queued twice; it reports false in that case, and the
// waiting job's transcript will still be published to every listener.
func (p *whisperPool) Submit(job transcriptJob) bool {
	p.mu.Lock()
	if !p.markQueuedLocked(job) {
		// A clip waiting in the low-priority bulk queue is promoted, so a
		// listener's explicit request isn't stuck behind a backfill.
		for i, waiting := range p.bulkQueue {
			if waiting.wavPath == job.wavPath {
				p.bulkQueue = append(p.bulkQueue[:i], p.bulkQueue[i+1:]...)
				p.queue = append(p.queue, waiting)
				break
			}
		}
		p.mu.Unlock()
		p.signal()
		return false
	}
	p.queue = append(p.queue, job)
	qlen := len(p.queue) + len(p.bulkQueue)
	p.mu.Unlock()
	p.signal()
	p.logger.Printf("whisper pool: queued clip %s from %s (queue depth: %d, servers: %d)",
		job.clipID, job.info.StreamName, qlen, len(p.endpoints))
	return true
}

// SubmitBulk queues a clip at low priority (see bulkQueue).
func (p *whisperPool) SubmitBulk(job transcriptJob) bulkSubmitResult {
	p.mu.Lock()
	if len(p.bulkQueue) >= maxBulkQueue {
		p.mu.Unlock()
		return bulkFull
	}
	if !p.markQueuedLocked(job) {
		p.mu.Unlock()
		return bulkDuplicate
	}
	p.bulkQueue = append(p.bulkQueue, job)
	p.mu.Unlock()
	p.signal()
	return bulkQueued
}

// queueDepth reports how many clips are waiting for a free server.
func (p *whisperPool) queueDepth() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.queue) + len(p.bulkQueue)
}

// requeueFront puts a job back at the head of the queue so another endpoint
// can pick it up without losing its place in line.
func (p *whisperPool) requeueFront(job transcriptJob) {
	p.mu.Lock()
	p.markQueuedLocked(job)
	p.queue = append([]transcriptJob{job}, p.queue...)
	p.mu.Unlock()
	p.signal()
}

func (p *whisperPool) signal() {
	select {
	case p.ready <- struct{}{}:
	default:
	}
}

// nextJob blocks until a job is available or the pool is closed. It also
// reports the queue depth left behind after this job was taken. Normal jobs
// always go first; bulk jobs only run when nothing else is waiting.
func (p *whisperPool) nextJob() (transcriptJob, int, bool) {
	for {
		p.mu.Lock()
		var job transcriptJob
		found := true
		switch {
		case len(p.queue) > 0:
			job = p.queue[0]
			p.queue = p.queue[1:]
		case len(p.bulkQueue) > 0:
			job = p.bulkQueue[0]
			p.bulkQueue = p.bulkQueue[1:]
		default:
			found = false
		}
		if found {
			delete(p.queued, job.wavPath)
			remaining := len(p.queue) + len(p.bulkQueue)
			p.mu.Unlock()
			if remaining > 0 {
				// Wake another worker for the rest.
				p.signal()
			}
			return job, remaining, true
		}
		p.mu.Unlock()
		select {
		case <-p.done:
			return transcriptJob{}, 0, false
		case <-p.ready:
		}
	}
}

// Close stops the workers and shuts down any local whisper-server instances.
func (p *whisperPool) Close() {
	p.closeOnce.Do(func() {
		close(p.done)
		p.cancel()
		stopped := make(chan struct{})
		go func() {
			p.wg.Wait()
			close(stopped)
		}()
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			p.logger.Printf("whisper: timed out waiting for local whisper-server instances to stop")
		}
	})
}

// awaitEndpoint blocks until ep can take a request. Local instances become
// ready when their supervisor sees /health succeed; a remote server that
// failed is polled with backoff until it answers /health again.
func (p *whisperPool) awaitEndpoint(ep *whisperEndpoint) bool {
	if ep.managed {
		select {
		case <-p.done:
			return false
		case <-ep.readyChan():
			return true
		}
	}
	backoff := time.Second
	for !ep.isReady() {
		select {
		case <-p.done:
			return false
		case <-time.After(backoff):
		}
		if err := checkWhisperHealth(p.ctx, ep.baseURL, 5*time.Second); err == nil {
			p.logger.Printf("whisper server %s is reachable again", ep.label)
			ep.setReady(true)
			break
		}
		backoff = min(backoff*2, 30*time.Second)
	}
	return true
}

func (p *whisperPool) worker(ep *whisperEndpoint) {
	for {
		if !p.awaitEndpoint(ep) {
			return
		}
		job, waiting, ok := p.nextJob()
		if !ok {
			return
		}
		started := time.Now()
		p.logger.Printf("whisper %s: transcribing clip %s from %s (queue depth: %d)",
			ep.label, job.clipID, job.info.StreamName, waiting)
		// Tell the UI a server has picked this clip up, so the row stops
		// reading "Recording received." while whisper works on it.
		p.hub.Publish(transcriptEvent{
			Type:        "transcribing",
			ClipID:      job.clipID,
			StreamID:    job.info.ID,
			StreamName:  job.info.StreamName,
			StateName:   job.info.StateName,
			GroupName:   job.info.GroupName,
			AudioURL:    job.audioURL,
			Timestamp:   job.start,
			WAVFilename: wavBaseName(job.wavPath),
		})
		text, err := p.transcribe(ep, job.wavPath, job.info.StreamName)
		if err != nil {
			var unreachable *endpointUnreachableError
			if errors.As(err, &unreachable) && job.retries < maxJobRetries {
				// The server itself is down, not the clip: take this
				// endpoint out of rotation and let another (or this one,
				// once healthy again) retry the clip.
				p.logger.Printf("whisper %s: unreachable, requeueing clip %s: %v", ep.label, job.clipID, err)
				job.retries++
				p.requeueFront(job)
				if ep.managed {
					// The supervisor owns local readiness; give it a moment
					// to notice the instance exited before taking more work.
					select {
					case <-p.done:
						return
					case <-time.After(time.Second):
					}
				} else {
					ep.setReady(false)
				}
				continue
			}
			p.logger.Printf("whisper %s: transcribe %s (clip %s): %v", ep.label, job.info.StreamName, job.clipID, err)
			// Publish a visible failure marker instead of silently dropping
			// the job: otherwise the clip stays stuck showing "Recording
			// received." in the UI, indistinguishable from one still in
			// progress. The full error is server-log-only (see above).
			p.publish(job, "[transcription failed]")
			continue
		}
		text = strings.TrimSpace(text)
		if filtered, changed := p.filter.Apply(text); changed {
			p.logger.Printf("whisper %s: clip %s from %s: filtered hallucination %q -> %q",
				ep.label, job.clipID, job.info.StreamName, text, filtered)
			text = filtered
		}
		if text == "" {
			// No speech detected — a normal outcome (e.g. a keyed-up but
			// silent transmission), published so the clip isn't left pending.
			p.logger.Printf("whisper %s: clip %s from %s produced no speech in %s (queue depth: %d)",
				ep.label, job.clipID, job.info.StreamName, time.Since(started).Round(time.Millisecond), p.queueDepth())
			p.publish(job, noSpeechMarker)
			continue
		}
		p.publish(job, text)
		p.logger.Printf("whisper %s: clip %s from %s done in %s (queue depth: %d): %s",
			ep.label, job.clipID, job.info.StreamName, time.Since(started).Round(time.Millisecond), p.queueDepth(), text)
	}
}

func (p *whisperPool) publish(job transcriptJob, text string) {
	p.hub.Publish(transcriptEvent{
		Type:        "transcript",
		ClipID:      job.clipID,
		StreamID:    job.info.ID,
		StreamName:  job.info.StreamName,
		StateName:   job.info.StateName,
		GroupName:   job.info.GroupName,
		Text:        text,
		AudioURL:    job.audioURL,
		Timestamp:   job.start,
		WAVFilename: wavBaseName(job.wavPath),
	})
}

// endpointUnreachableError marks a failure to reach the server at all (as
// opposed to the server rejecting or failing on this particular clip).
type endpointUnreachableError struct{ err error }

func (e *endpointUnreachableError) Error() string { return e.err.Error() }
func (e *endpointUnreachableError) Unwrap() error { return e.err }

// transcribe POSTs the WAV file to the endpoint's /inference as
// multipart/form-data. whisper-server decodes WAV at any sample rate and
// resamples it internally, so the 8kHz clips are sent unmodified.
func (p *whisperPool) transcribe(ep *whisperEndpoint, wavPath, streamName string) (string, error) {
	data, err := os.ReadFile(wavPath)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", wavPath, err)
	}

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	keys := make([]string, 0, len(p.formFields))
	for key := range p.formFields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := form.WriteField(key, p.formFields[key]); err != nil {
			return "", fmt.Errorf("build request: %w", err)
		}
	}
	part, err := form.CreateFormFile("file", filepath.Base(wavPath))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	if err := form.Close(); err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}

	endpointURL := ep.baseURL + "/inference"
	ctx, cancel := context.WithTimeout(p.ctx, time.Duration(p.cfg.TimeoutMs)*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL, &body)
	if err != nil {
		return "", fmt.Errorf("%s: build request: %w", endpointURL, err)
	}
	req.Header.Set("Content-Type", form.FormDataContentType())
	// Ignored by whisper-server; useful in proxy/access logs.
	req.Header.Set("X-Stream-Name", streamName)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("%s: timed out after %dms: %w", endpointURL, p.cfg.TimeoutMs, err)
		}
		return "", &endpointUnreachableError{fmt.Errorf("%s: %w", endpointURL, err)}
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("%s: read response: %w", endpointURL, err)
	}
	// whisper-server reports some errors as {"error": "..."} with a 200
	// status, so the body is checked for an error regardless of status.
	var result struct {
		Text  string `json:"text"`
		Error string `json:"error"`
	}
	decodeErr := json.Unmarshal(respBody, &result)
	if result.Error != "" {
		return "", fmt.Errorf("%s: %s", endpointURL, result.Error)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: unexpected status %s", endpointURL, resp.Status)
	}
	if decodeErr != nil {
		return "", fmt.Errorf("%s: decode response: %w", endpointURL, decodeErr)
	}
	return cleanWhisperOutput(result.Text), nil
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
	write(uint32(16))             // chunk size
	write(uint16(1))              // PCM
	write(uint16(1))              // mono
	write(uint32(sampleRate))     // sample rate
	write(uint32(sampleRate * 2)) // byte rate
	write(uint16(2))              // block align
	write(uint16(16))             // bits per sample

	// data chunk
	buf.WriteString("data")
	write(uint32(dataSize))
	for _, s := range samples {
		write(s)
	}

	return buf.Bytes(), nil
}

// cleanWhisperOutput strips bracketed timestamps and joins segment lines.
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
