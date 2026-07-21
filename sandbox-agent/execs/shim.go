package execs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/obot-platform/discobox/sandbox-agent/shimruntime"
	"github.com/obot-platform/discobox/sandbox-agent/terminal/frame"
)

type ShimConfig struct {
	ExecID      string
	Unit        string
	Command     []string
	Workdir     string
	SocketPath  string
	RuntimePath string
	LogDir      string
	Rows        uint16
	Cols        uint16
	TTY         bool
	Env         map[string]string
	User        *User
	Metadata    map[string]string
}

type shimRuntime struct {
	cfg       ShimConfig
	cmd       *exec.Cmd
	tty       *os.File
	stdin     io.WriteCloser
	logger    *AsyncLogger
	server    *http.Server
	listener  net.Listener
	done      chan struct{}
	doneOnce  sync.Once
	closeOnce sync.Once
	outputWG  sync.WaitGroup
	startMu   sync.Mutex
	stream    *shimruntime.Runtime
	mu        sync.Mutex
	status    Exec
}

func RunShim(ctx context.Context, cfg ShimConfig) error {
	r := &shimRuntime{cfg: cfg, done: make(chan struct{})}
	r.stream = shimruntime.New("discobox-sandbox-exec", r.done, r.handleAttachFrame)
	if err := r.start(ctx); err != nil {
		return err
	}
	defer r.close()
	go r.serve()
	select {
	case <-ctx.Done():
		r.terminate()
		return ctx.Err()
	case <-r.done:
		return nil
	}
}

func (r *shimRuntime) start(ctx context.Context) error {
	if len(r.cfg.Command) == 0 || strings.TrimSpace(r.cfg.Command[0]) == "" {
		return fmt.Errorf("command is required")
	}
	if strings.TrimSpace(r.cfg.ExecID) == "" {
		return fmt.Errorf("exec id is required")
	}
	if strings.TrimSpace(r.cfg.Workdir) == "" {
		return fmt.Errorf("workdir is required")
	}
	logger, err := NewAsyncLogger(r.cfg.LogDir, r.cfg.ExecID)
	if err != nil {
		return err
	}
	r.logger = logger

	now := time.Now().UTC()
	r.status = Exec{
		ID:          r.cfg.ExecID,
		Status:      StatusStarting,
		Command:     append([]string{}, r.cfg.Command...),
		Workdir:     r.cfg.Workdir,
		Env:         cloneMap(r.cfg.Env),
		User:        cloneUser(r.cfg.User),
		TTY:         r.cfg.TTY,
		Unit:        r.cfg.Unit,
		CreatedAt:   now,
		Metadata:    cloneMap(r.cfg.Metadata),
		SocketPath:  r.cfg.SocketPath,
		RuntimePath: r.cfg.RuntimePath,
	}
	if err := r.writeStatus(); err != nil {
		logger.Close()
		return err
	}
	ln, err := shimruntime.ListenUnix(ctx, r.cfg.SocketPath)
	if err != nil {
		r.terminate()
		logger.Close()
		return err
	}
	r.listener = ln
	r.server = &http.Server{
		Handler:           r.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	return nil
}

func (r *shimRuntime) startPTY(cmd *exec.Cmd) error {
	if r.stream.HasAttachers() {
		resizeCtx, cancelResize := context.WithTimeout(context.Background(), 100*time.Millisecond)
		r.stream.WaitForResize(resizeCtx)
		cancelResize()
	}
	winsize := r.stream.InitialWinsize(r.cfg.Rows, r.cfg.Cols)
	tty, err := pty.StartWithSize(cmd, winsize)
	if err != nil {
		return err
	}
	r.tty = tty
	r.stdin = tty
	// Track the screen in memory at the PTY's real size so a late attacher can be
	// repainted with the current screen and recent scrollback, and so the
	// program's terminal queries are answered at the PTY while no client is
	// attached. Only TTY execs have a screen; plain (pipe) execs attach without
	// a repaint. Output cannot race this: broadcasting starts with copyOutput
	// below.
	r.stream.EnableScreen(winsize.Rows, winsize.Cols, shimruntime.DefaultScrollbackLines, tty)
	r.status.PID = int64(cmd.Process.Pid)
	r.outputWG.Add(1)
	go r.copyOutput(LogStreamStdout, tty)
	return nil
}

func (r *shimRuntime) startPipes(cmd *exec.Cmd) error {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	r.stdin = stdin
	r.status.PID = int64(cmd.Process.Pid)
	r.outputWG.Add(2)
	go r.copyOutput(LogStreamStdout, stdout)
	go r.copyOutput(LogStreamStderr, stderr)
	return nil
}

func (r *shimRuntime) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", r.handleStatus)
	mux.HandleFunc("POST /attach", r.handleAttach)
	mux.HandleFunc("POST /start", r.handleStart)
	return mux
}

func (r *shimRuntime) handleStatus(w http.ResponseWriter, _ *http.Request) {
	r.mu.Lock()
	status := r.status
	r.mu.Unlock()
	writeJSON(w, status)
}

func writeJSON(w http.ResponseWriter, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func (r *shimRuntime) serve() {
	if err := r.server.Serve(r.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		r.finish()
	}
}

func (r *shimRuntime) handleAttach(w http.ResponseWriter, req *http.Request) {
	repaint, _ := strconv.ParseBool(req.URL.Query().Get("replay"))
	r.stream.HandleAttach(w, repaint)
}

func (r *shimRuntime) handleStart(w http.ResponseWriter, _ *http.Request) {
	if err := r.startCommand(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	r.mu.Lock()
	status := r.status
	r.mu.Unlock()
	writeJSON(w, status)
}

func (r *shimRuntime) startCommand() error {
	r.startMu.Lock()
	defer r.startMu.Unlock()

	r.mu.Lock()
	if r.cmd != nil {
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()

	// The exec's lifetime is managed by the shim itself (stop/kill via the
	// runtime API), not by a request context.
	cmd := exec.CommandContext(context.Background(), r.cfg.Command[0], r.cfg.Command[1:]...) //nolint:gosec // command is caller-supplied for sandbox exec.
	cmd.Dir = r.cfg.Workdir
	cmd.Env = append(os.Environ(), "DISCOBOX_EXEC_ID="+r.cfg.ExecID)
	userEnv, err := userEnvDefaults(r.cfg.User)
	if err != nil {
		r.markStartFailed(err)
		return err
	}
	for key, value := range userEnv {
		if strings.TrimSpace(key) != "" {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	for key, value := range r.cfg.Env {
		if strings.TrimSpace(key) != "" {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	sysProcAttr, err := agentSysProcAttr(r.cfg.User)
	if err != nil {
		r.markStartFailed(err)
		return err
	}
	cmd.SysProcAttr = sysProcAttr

	if r.cfg.TTY {
		if err := r.startPTY(cmd); err != nil {
			r.markStartFailed(err)
			return err
		}
	} else if err := r.startPipes(cmd); err != nil {
		r.markStartFailed(err)
		return err
	}
	now := time.Now().UTC()
	r.mu.Lock()
	r.cmd = cmd
	r.status.Status = StatusRunning
	r.status.StartedAt = &now
	r.mu.Unlock()
	if err := r.writeStatus(); err != nil {
		r.terminate()
		return err
	}
	go r.wait()
	return nil
}

func (r *shimRuntime) markStartFailed(err error) {
	now := time.Now().UTC()
	r.mu.Lock()
	r.status.Status = StatusFailed
	r.status.Error = err.Error()
	r.status.ExitedAt = &now
	status := r.status
	r.mu.Unlock()
	_ = r.writeStatusValue(status)
}

func (r *shimRuntime) copyOutput(stream LogStream, reader io.Reader) {
	defer r.outputWG.Done()
	frameType := attachFrameType(stream)
	buf := make([]byte, 32*1024)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			r.logger.Record(stream, chunk)
			r.stream.Broadcast(frameType, chunk)
		}
		if err != nil {
			return
		}
	}
}

// attachFrameType is the attach frame a log stream is broadcast as, keeping the
// live stream and the audit log labeled identically. A TTY exec's single merged
// stream is stdout in both.
func attachFrameType(stream LogStream) byte {
	if stream == LogStreamStderr {
		return frame.Stderr
	}
	return frame.Stdout
}

func (r *shimRuntime) handleAttachFrame(attach *shimruntime.Attacher, next frame.Frame) {
	switch next.Type {
	case frame.Input:
		if err := r.writeInput(next.Payload); err != nil {
			_ = attach.WriteFrame(frame.Error, []byte(err.Error()))
			attach.Close()
			return
		}
	case frame.Resize:
		resize, err := frame.DecodeResize(next.Payload)
		if err != nil {
			_ = attach.WriteFrame(frame.Error, []byte(err.Error()))
			attach.Close()
			return
		}
		r.stream.ApplyResize(resize)
	case frame.Signal:
		if err := signalProcess(r.cmd, string(next.Payload)); err != nil {
			_ = attach.WriteFrame(frame.Error, []byte(err.Error()))
			attach.Close()
			return
		}
	case frame.CloseInput:
		r.closeInput()
	default:
		_ = attach.WriteFrame(frame.Error, []byte("unknown frame type"))
		attach.Close()
		return
	}
}

func (r *shimRuntime) writeInput(payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	r.mu.Lock()
	stdin := r.stdin
	r.mu.Unlock()
	if stdin == nil {
		return fmt.Errorf("exec stdin is closed")
	}
	if _, err := stdin.Write(payload); err != nil {
		return err
	}
	r.logger.Record(LogStreamInput, payload)
	return nil
}

func (r *shimRuntime) closeInput() {
	r.closeOnce.Do(func() {
		r.mu.Lock()
		if r.tty != nil {
			r.mu.Unlock()
			return
		}
		stdin := r.stdin
		r.stdin = nil
		r.mu.Unlock()
		if stdin != nil {
			_ = stdin.Close()
		}
	})
}

func (r *shimRuntime) wait() {
	if r.cmd == nil {
		return
	}
	err := r.cmd.Wait()
	r.closeInput()
	now := time.Now().UTC()
	r.mu.Lock()
	r.status.ExitedAt = &now
	r.status.Status = StatusExited
	if err != nil {
		r.status.Status = StatusFailed
		r.status.Error = err.Error()
	}
	if state := r.cmd.ProcessState; state != nil {
		code := int64(state.ExitCode())
		r.status.ExitCode = &code
	}
	status := r.status
	attachers := r.stream.Attachers()
	r.mu.Unlock()
	// Do not publish terminal status until every byte has been read from the
	// PTY/pipes and the asynchronous audit log has drained. Callers use terminal
	// status as the signal that output and logs are complete.
	r.outputWG.Wait()
	r.logger.Close()
	_ = r.writeStatusValue(status)
	payload, _ := frame.EncodeExit(string(status.Status), status.ExitCode, status.Error)
	// Retain the exit frame so a client attaching after this point still receives
	// the final screen replay and exit code, then notify current attachers.
	r.stream.MarkExited(payload)
	for _, attach := range attachers {
		_ = attach.WriteFrame(frame.Exit, payload)
		attach.Close()
	}
	// Linger so a late attacher (e.g. something that died immediately after start,
	// before its client attached) can still replay the buffer and read the exit
	// code, then shut the shim down.
	go r.lingerThenFinish()
}

// exitLingerTimeout is how long the shim keeps serving after the process exits
// so a late attacher can replay the final output and read the exit code.
const exitLingerTimeout = 60 * time.Second

func (r *shimRuntime) lingerThenFinish() {
	select {
	case <-time.After(exitLingerTimeout):
	case <-r.done:
	}
	r.finish()
}

func (r *shimRuntime) terminate() {
	if r.cmd != nil {
		terminateProcessGroup(r.cmd)
	}
}

func (r *shimRuntime) close() {
	r.terminate()
	if r.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = r.server.Shutdown(ctx)
		cancel()
	}
	if r.listener != nil {
		_ = r.listener.Close()
	}
	if r.tty != nil {
		_ = r.tty.Close()
	}
	r.closeInput()
	if r.logger != nil {
		r.logger.Close()
	}
}

func (r *shimRuntime) finish() {
	r.doneOnce.Do(func() {
		close(r.done)
	})
}

func (r *shimRuntime) writeStatus() error {
	r.mu.Lock()
	status := r.status
	r.mu.Unlock()
	return r.writeStatusValue(status)
}

func (r *shimRuntime) writeStatusValue(status Exec) error {
	return writeRuntime(r.cfg.RuntimePath, status)
}
