package desktop

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Streamer runs voxl, the process that captures the X screen, encodes it as
// H.264 and carries it, the pointer, the keyboard and the clipboard over one
// WebSocket (ADR 26-11-06-851). The page never reaches it directly: the
// viewer proxies its own /api/stream route onto it, so the desktop stays one
// origin and one port.
//
// voxl is this process's child, started on demand by the first stream request
// and kept while it runs, rather than a unit of its own. Three things decide
// that:
//
//   - **It must start late.** voxl attaches to an X server that is already up
//     and exits when it cannot: a cold display refuses its MIT-SHM attach, and
//     an X server going away ends its capture. So it is started only once the
//     display has answered, by the request that needs it, and started again
//     the next time one does.
//   - **One place serializes it.** Startup, recovery and the retry after a
//     cold refusal all go through Ensure under one lock, so two tabs opening
//     at once start one voxl, and a failure is retried a bounded number of
//     times rather than by whoever asks next racing whoever asked first.
//   - **It runs as the sandbox user, beside the X server.** MIT-SHM needs the
//     same IPC namespace as Xorg, and the clipboard and input it injects are
//     that user's desktop. The viewer already is both.
//
// Its listener is loopback-only and gated by a token made fresh for every
// start, handed to voxl in its environment and to nothing else, so no page a
// browser happens to have open, and no program that only finds the port, can
// use voxl's input, screenshot or recording routes. It is not a boundary
// inside the sandbox: a process of the sandbox user or root can read voxl's
// environment, and could as well type on the desktop through X itself. The
// port watcher never lists it (see DefaultStreamAddr).
//
// Restarting it touches nothing on the desktop: it is a reader of the screen
// and a source of input events, not the parent of anything a person runs.
type Streamer struct {
	// Binary is voxl's executable.
	Binary string
	// Addr is the loopback address voxl serves on.
	Addr string
	// Display is the X display it captures.
	Display string
	// Log receives voxl's own output and what goes wrong starting it.
	Log *slog.Logger

	mu      sync.Mutex
	current *streamProcess
}

// streamProcess is one run of voxl.
type streamProcess struct {
	cmd   *exec.Cmd
	token string
	// done is closed when the process has exited; err is how, read after.
	done chan struct{}
	err  error
	tail *outputTail
}

const (
	// streamStartAttempts bounds how many times one Ensure starts voxl. The
	// first start against a display that has only just come up can be refused
	// its shared-memory attach; the next, a moment later, is not. Three covers
	// that with room to spare and still gives up on a binary that cannot run.
	streamStartAttempts = 3
	// streamRetryDelay is the pause before the second attempt, doubled before
	// the third.
	streamRetryDelay = 400 * time.Millisecond
	// streamReadyTimeout bounds one start: process creation, X connection and
	// encoder setup. voxl answers its health check once all three are done.
	streamReadyTimeout = 10 * time.Second
	// streamStopTimeout is how long a stopped voxl gets before it is killed.
	streamStopTimeout = 2 * time.Second
	// streamTailLines is how much of voxl's output a failed start reports.
	streamTailLines = 12
)

// errStreamExited is a voxl that exited before it was ready.
var errStreamExited = errors.New("desktop: the stream exited before it was ready")

// streamArgs are the flags voxl runs with. Each switches off something the
// desktop page owns or the decision excludes.
func (s *Streamer) streamArgs() ([]string, error) {
	host, port, err := splitHostPort(s.Addr)
	if err != nil {
		return nil, err
	}
	return []string{
		"--addr=" + host,
		"--port=" + port,
		// The page sizes the desktop through /api/display, so voxl must not
		// resize it too: two owners of the geometry race, and the annotation
		// coordinates and the density chain are both measured against it.
		"--enable-resize=false",
		// Audio is built out (no PulseAudio) and the microphone and file
		// transfer are outside the decision.
		"--audio-enabled=false",
		"--microphone-enabled=false",
		"--file-transfers=none",
		// Both directions: what is done with the remote clipboard, and when
		// the local one is read, is the page's decision, not voxl's.
		"--enable-clipboard=true",
		"--stripes=1",
		// The token in VOXL_PASSWORD is required on every route but the
		// health check.
		"--enable-basic-auth=true",
	}, nil
}

// Ensure returns the token of a running voxl, starting one when none is.
//
// A start that fails is retried, after a short pause, up to
// streamStartAttempts times; the error after the last says why the last one
// failed, with voxl's own last words.
func (s *Streamer) Ensure(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.current; p != nil && p.alive() {
		return p.token, nil
	}
	s.current = nil
	var last error
	for attempt := 1; attempt <= streamStartAttempts; attempt++ {
		if attempt > 1 {
			delay := streamRetryDelay << (attempt - 2)
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(delay):
			}
		}
		p, err := s.start(ctx)
		if err == nil {
			s.current = p
			return p.token, nil
		}
		last = err
		s.log().Warn("desktop: start the stream", "attempt", attempt, "error", err)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
	}
	return "", fmt.Errorf("desktop: the stream did not start after %d attempts: %w", streamStartAttempts, last)
}

// Close stops voxl, if it is running.
func (s *Streamer) Close() {
	s.mu.Lock()
	p := s.current
	s.current = nil
	s.mu.Unlock()
	if p != nil {
		p.stop()
	}
}

func (s *Streamer) start(ctx context.Context) (*streamProcess, error) {
	args, err := s.streamArgs()
	if err != nil {
		return nil, err
	}
	token, err := newStreamToken()
	if err != nil {
		return nil, err
	}
	// Detached from the request's cancellation: voxl outlives the request that
	// started it, and is stopped by Close.
	//nolint:gosec // G204: the binary is the desktop feature's voxl, from the viewer's own configuration, and the arguments are the fixed flags above.
	cmd := exec.CommandContext(context.WithoutCancel(ctx), s.Binary, args...)
	cmd.Env = s.env(token)
	output, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("desktop: open the stream's output: %w", err)
	}
	cmd.Stderr = cmd.Stdout
	p := &streamProcess{cmd: cmd, token: token, done: make(chan struct{}), tail: &outputTail{max: streamTailLines}}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("desktop: start the stream: %w", err)
	}
	logged := make(chan struct{})
	go func() {
		defer close(logged)
		p.tail.copy(output, s.log())
	}()
	go func() {
		// The output is drained before Wait, which closes the pipe.
		<-logged
		p.err = cmd.Wait()
		close(p.done)
		s.log().Info("desktop: the stream exited", "error", p.err)
	}()
	if err := s.awaitReady(ctx, p); err != nil {
		p.stop()
		return nil, err
	}
	s.log().Info("desktop: the stream is up", "addr", s.Addr, "pid", cmd.Process.Pid)
	return p, nil
}

// awaitReady polls voxl until it answers as this run of voxl: an
// authenticated request, which only a voxl given this run's token passes. It
// is up once its capture, its encoder and its listener are. Another program
// holding the port answers nothing that passes, and voxl itself then exits on
// the port being taken; a refused capture shows the same way, as an exit.
func (s *Streamer) awaitReady(ctx context.Context, p *streamProcess) error {
	ctx, cancel := context.WithTimeout(ctx, streamReadyTimeout)
	defer cancel()
	client := &http.Client{Timeout: time.Second}
	url := "http://" + s.Addr + "/stats"
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-p.done:
			return fmt.Errorf("%w (%w): %s", errStreamExited, p.err, p.tail.String())
		case <-ctx.Done():
			return fmt.Errorf("desktop: the stream was not ready within %s: %s", streamReadyTimeout, p.tail.String())
		case <-tick.C:
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", "Bearer "+p.token)
		response, err := client.Do(request)
		if err != nil {
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		_ = response.Body.Close()
		if response.StatusCode == http.StatusOK && p.alive() {
			return nil
		}
	}
}

// env is voxl's whole environment: the display it captures, the home its
// clipboard and X authority are resolved against, a PATH, and the token.
func (s *Streamer) env(token string) []string {
	env := []string{
		"DISPLAY=" + s.Display,
		"PATH=/usr/bin:/bin",
		"VOXL_PASSWORD=" + token,
		"RUST_LOG=info",
	}
	if home := os.Getenv("HOME"); home != "" {
		env = append(env, "HOME="+home)
	}
	return env
}

func (s *Streamer) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.New(slog.DiscardHandler)
}

func (p *streamProcess) alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// stop ends voxl: politely, then not.
func (p *streamProcess) stop() {
	if !p.alive() {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(streamStopTimeout):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
}

func newStreamToken() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("desktop: make a stream token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// outputTail logs voxl's output line by line and keeps the last few lines, so
// a start that fails can say why in voxl's own words.
type outputTail struct {
	max   int
	mu    sync.Mutex
	lines []string
}

func (t *outputTail) copy(r io.Reader, log *slog.Logger) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 4096), 64*1024)
	for scanner.Scan() {
		line := scanner.Text()
		log.Info("voxl", "line", line)
		t.mu.Lock()
		t.lines = append(t.lines, line)
		if len(t.lines) > t.max {
			t.lines = t.lines[len(t.lines)-t.max:]
		}
		t.mu.Unlock()
	}
}

func (t *outputTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.lines) == 0 {
		return "(no output)"
	}
	return strings.Join(t.lines, " | ")
}
