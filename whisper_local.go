package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// startLocalInstances validates the local-mode settings and launches one
// supervised whisper-server per worker on consecutive loopback ports.
func (p *whisperPool) startLocalInstances() error {
	binary, err := exec.LookPath(p.cfg.ServerBinaryPath)
	if err != nil {
		return fmt.Errorf("serverBinaryPath %q: %w", p.cfg.ServerBinaryPath, err)
	}
	if _, err := os.Stat(p.cfg.ModelPath); err != nil {
		return fmt.Errorf("modelPath: %w", err)
	}
	for i := 0; i < p.cfg.Workers; i++ {
		port := p.cfg.LocalBasePort + i
		ep := newWhisperEndpoint(
			fmt.Sprintf("local#%d", i+1),
			fmt.Sprintf("http://127.0.0.1:%d", port),
			true,
		)
		ep.model = filepath.Base(p.cfg.ModelPath) + " (configured)"
		p.endpoints = append(p.endpoints, ep)
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.superviseLocal(ep, binary, port)
		}()
	}
	p.logger.Printf("whisper: launching %d local whisper-server instance(s) on 127.0.0.1:%d-%d (binary=%s args=%q)",
		p.cfg.Workers, p.cfg.LocalBasePort, p.cfg.LocalBasePort+p.cfg.Workers-1, binary, p.cfg.ServerArgs)
	return nil
}

// localServerArgs builds the whisper-server command line for one instance.
// The user's serverArgs come last; validate() forbids them from overriding
// the model, host, port, or request paths set here.
func (p *whisperPool) localServerArgs(port int) []string {
	args := []string{
		"-m", p.cfg.ModelPath,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(port),
	}
	return append(args, p.cfg.ServerArgs...)
}

// superviseLocal keeps one local whisper-server running until the pool is
// closed: it starts the process, marks the endpoint ready once /health
// succeeds, and restarts the process with backoff whenever it exits.
func (p *whisperPool) superviseLocal(ep *whisperEndpoint, binary string, port int) {
	backoff := time.Second
	for {
		if p.ctx.Err() != nil {
			return
		}
		err := p.runLocalInstance(ep, binary, port)
		ep.setReady(false)
		if p.ctx.Err() != nil {
			return
		}
		p.logger.Printf("whisper %s: %v; restarting in %s", ep.label, err, backoff)
		select {
		case <-p.ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// runLocalInstance runs one whisper-server process to completion and
// returns why it stopped.
func (p *whisperPool) runLocalInstance(ep *whisperEndpoint, binary string, port int) error {
	// Fail fast with a clear message rather than letting whisper-server
	// exit on a bind error — or worse, mistaking another program already
	// listening there for our instance.
	if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err != nil {
		return fmt.Errorf("port %d is unavailable (change localBasePort, or stop whatever is using it): %w", port, err)
	} else {
		ln.Close()
	}

	output := &tailBuffer{max: 8 << 10}
	cmd := exec.CommandContext(p.ctx, binary, p.localServerArgs(port)...)
	cmd.Stdout = output
	cmd.Stderr = output
	prepareChildProcess(cmd)
	started := time.Now()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	if err := bindChildProcess(cmd); err != nil {
		p.logger.Printf("WARNING: whisper %s: could not tie whisper-server to this process's lifetime (it may outlive a crash): %v", ep.label, err)
	}

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	if err := p.waitLocalHealthy(ep, exited); err != nil {
		return fmt.Errorf("%w\n--- whisper-server output ---\n%s", err, output.String())
	}
	p.logger.Printf("whisper %s: ready on %s (pid %d, model loaded in %s)",
		ep.label, ep.baseURL, cmd.Process.Pid, time.Since(started).Round(time.Millisecond))
	ep.setReady(true)

	err := <-exited
	return fmt.Errorf("whisper-server exited: %v\n--- whisper-server output ---\n%s", err, output.String())
}

// waitLocalHealthy polls /health until the model is loaded, the process
// exits, or the pool closes. Model loading can legitimately take a while,
// so there's no deadline — just periodic log messages.
func (p *whisperPool) waitLocalHealthy(ep *whisperEndpoint, exited <-chan error) error {
	poll := time.NewTicker(250 * time.Millisecond)
	defer poll.Stop()
	started := time.Now()
	nextNotice := 30 * time.Second
	for {
		select {
		case err := <-exited:
			return fmt.Errorf("whisper-server exited during startup: %v", err)
		case <-p.ctx.Done():
			// Wait for CommandContext's kill to reap the process.
			<-exited
			return p.ctx.Err()
		case <-poll.C:
		}
		if checkWhisperHealth(p.ctx, ep.baseURL, 2*time.Second) == nil {
			ep.markChecked()
			return nil
		}
		if waited := time.Since(started); waited >= nextNotice {
			p.logger.Printf("whisper %s: still waiting for whisper-server to load the model (%s)", ep.label, waited.Round(time.Second))
			nextNotice += 30 * time.Second
		}
	}
}

// tailBuffer keeps the last max bytes written to it, so a crashed
// whisper-server's final output can be logged without retaining all of it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tailBuffer) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, b...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(b), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}
