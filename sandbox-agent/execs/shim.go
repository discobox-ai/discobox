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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/obot-platform/discobox/execstream/frame"
	"github.com/obot-platform/discobox/sandbox-agent/procio"
	"github.com/obot-platform/discobox/sandbox-agent/shimruntime"
)

type ShimConfig struct {
	ExecID         string
	Unit           string
	Command        []string
	StartupCommand []string
	Workdir        string
	SocketPath     string
	RuntimePath    string
	LogDir         string
	Rows           uint16
	Cols           uint16
	TTY            bool
	Env            map[string]string
	User           *User
	Metadata       map[string]string
}

type shimRuntime struct {
	cfg       ShimConfig
	proc      *procio.Process
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
	// inputClosed records that stdin has been closed, so a later write reports
	// it rather than failing on a closed descriptor.
	inputClosed bool
	status      Exec
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
		ID:             r.cfg.ExecID,
		Status:         StatusStarting,
		Command:        append([]string{}, r.cfg.Command...),
		StartupCommand: append([]string{}, r.cfg.StartupCommand...),
		Workdir:        r.cfg.Workdir,
		Env:            cloneMap(r.cfg.Env),
		User:           r.cfg.User.Clone(),
		TTY:            r.cfg.TTY,
		Unit:           r.cfg.Unit,
		CreatedAt:      now,
		Metadata:       cloneMap(r.cfg.Metadata),
		SocketPath:     r.cfg.SocketPath,
		RuntimePath:    r.cfg.RuntimePath,
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

// startProcess launches the command and wires its streams to the audit log and
// the attach stream. Everything about owning the process — PTY versus pipes,
// which descriptors the parent holds, signals, exit status — belongs to procio.
func (r *shimRuntime) startProcess(opts procio.Options) error {
	if opts.TTY {
		// A client that is already attached may have sent its size; starting at
		// the wrong one makes the program paint twice.
		if r.stream.HasAttachers() {
			resizeCtx, cancelResize := context.WithTimeout(context.Background(), 100*time.Millisecond)
			r.stream.WaitForResize(resizeCtx)
			cancelResize()
		}
		opts.Winsize = r.stream.InitialWinsize(r.cfg.Rows, r.cfg.Cols)
	}
	proc, err := procio.Start(opts)
	if err != nil {
		return err
	}
	r.proc = proc
	r.status.PID = proc.PID()

	if tty := proc.TTY(); tty != nil {
		// Track the screen in memory at the PTY's real size so a late attacher
		// can be repainted with the current screen and recent scrollback, and so
		// the program's terminal queries are answered at the PTY while no client
		// is attached. Output cannot race this: broadcasting starts below.
		r.stream.EnableScreen(opts.Winsize.Rows, opts.Winsize.Cols, shimruntime.DefaultScrollbackLines, tty)
		r.outputWG.Add(1)
		go r.copyOutput(LogStreamStdout, proc.Stdout())
		return nil
	}
	r.outputWG.Add(2)
	go r.copyOutput(LogStreamStdout, proc.Stdout())
	go r.copyOutput(LogStreamStderr, proc.Stderr())
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
	if r.proc != nil {
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()

	env := append(os.Environ(), "DISCOBOX_EXEC_ID="+r.cfg.ExecID)
	userEnv, err := userEnvDefaults(r.cfg.User)
	if err != nil {
		r.markStartFailed(err)
		return err
	}
	for key, value := range userEnv {
		if strings.TrimSpace(key) != "" {
			env = append(env, key+"="+value)
		}
	}
	for key, value := range r.cfg.Env {
		if strings.TrimSpace(key) != "" {
			env = append(env, key+"="+value)
		}
	}
	sysProcAttr, err := agentSysProcAttr(r.cfg.User)
	if err != nil {
		r.markStartFailed(err)
		return err
	}

	if err := r.startProcess(procio.Options{
		Command:     r.cfg.Command,
		Dir:         r.cfg.Workdir,
		Env:         env,
		SysProcAttr: sysProcAttr,
		TTY:         r.cfg.TTY,
	}); err != nil {
		r.markStartFailed(err)
		return err
	}
	if err := r.writeStartupCommand(); err != nil {
		r.terminate()
		r.markStartFailed(err)
		return err
	}
	now := time.Now().UTC()
	r.mu.Lock()
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

// writeStartupCommand types the resolved startup command into the process's
// input once, immediately after it starts, exactly as attach input arrives —
// see Exec.StartupCommand for why this is how a harness terminal gets real job
// control.
func (r *shimRuntime) writeStartupCommand() error {
	payload := QuoteShellCommand(r.cfg.StartupCommand)
	if len(payload) == 0 {
		return nil
	}
	if _, err := r.proc.WriteInput(payload); err != nil {
		return err
	}
	r.logger.Record(LogStreamInput, payload)
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

func (r *shimRuntime) handleAttachFrame(next frame.Frame) error {
	switch next.Type {
	case frame.Input:
		return r.writeInput(next.Payload)
	case frame.Resize:
		resize, err := frame.DecodeResize(next.Payload)
		if err != nil {
			return err
		}
		r.stream.ApplyResize(resize)
		return nil
	case frame.Signal:
		r.mu.Lock()
		proc := r.proc
		r.mu.Unlock()
		if proc == nil {
			return fmt.Errorf("exec has not started")
		}
		return proc.Signal(string(next.Payload))
	case frame.CloseInput:
		r.closeInput()
		return nil
	default:
		return fmt.Errorf("unknown frame type %d", next.Type)
	}
}

func (r *shimRuntime) writeInput(payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	r.mu.Lock()
	proc, closed := r.proc, r.inputClosed
	r.mu.Unlock()
	if proc == nil || closed {
		return fmt.Errorf("exec stdin is closed")
	}
	if _, err := proc.WriteInput(payload); err != nil {
		return err
	}
	r.logger.Record(LogStreamInput, payload)
	return nil
}

// closeInput ends the process's stdin so a command reading to EOF terminates.
// procio makes it a no-op for a TTY exec, whose input side is the terminal.
func (r *shimRuntime) closeInput() {
	r.closeOnce.Do(func() {
		r.mu.Lock()
		proc := r.proc
		r.inputClosed = proc != nil && proc.TTY() == nil
		r.mu.Unlock()
		if proc != nil {
			proc.CloseInput()
		}
	})
}

func (r *shimRuntime) wait() {
	if r.proc == nil {
		return
	}
	exit := r.proc.Wait()
	r.closeInput()
	now := time.Now().UTC()
	r.mu.Lock()
	r.status.ExitedAt = &now
	r.status.Status = StatusExited
	if exit.Err != nil {
		r.status.Status = StatusFailed
		r.status.Error = exit.Err.Error()
	}
	code := exit.ExitCode
	r.status.ExitCode = &code
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
	if r.proc != nil {
		r.proc.Terminate()
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
	if r.proc != nil {
		r.proc.Close()
	}
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
