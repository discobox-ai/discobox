package shim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/obot-platform/discobox/sandbox-agent/execs"
	"github.com/obot-platform/discobox/sandbox-agent/shimruntime"
	"github.com/obot-platform/discobox/sandbox-agent/terminal"
	"github.com/obot-platform/discobox/sandbox-agent/terminal/frame"
)

type Config struct {
	TerminalID  string
	AgentID     string
	Unit        string
	Command     []string
	Workdir     string
	SocketPath  string
	RuntimePath string
	LogDir      string
	Rows        uint16
	Cols        uint16
	Env         map[string]string
	User        *execs.User
}

type Runtime struct {
	cfg      Config
	cmd      *exec.Cmd
	tty      *os.File
	logger   *terminal.AsyncLogger
	server   *http.Server
	listener net.Listener
	done     chan struct{}
	ptyDone  chan struct{}
	doneOnce sync.Once
	startMu  sync.Mutex
	stream   *shimruntime.Runtime
	mu       sync.Mutex
	status   terminal.Terminal
}

func Run(ctx context.Context, cfg Config) error {
	r := &Runtime{cfg: cfg, done: make(chan struct{})}
	r.stream = shimruntime.New("discobox-agent-terminal", r.done, r.handleAttachFrame)
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

func (r *Runtime) start(ctx context.Context) error {
	if len(r.cfg.Command) == 0 || strings.TrimSpace(r.cfg.Command[0]) == "" {
		return fmt.Errorf("command is required")
	}
	if strings.TrimSpace(r.cfg.TerminalID) == "" {
		return fmt.Errorf("terminal id is required")
	}
	if strings.TrimSpace(r.cfg.Workdir) == "" {
		return fmt.Errorf("workdir is required")
	}
	logger, err := terminal.NewAsyncLogger(r.cfg.LogDir, r.cfg.TerminalID)
	if err != nil {
		return err
	}
	r.logger = logger
	now := time.Now().UTC()
	r.status = terminal.Terminal{
		ID:          r.cfg.TerminalID,
		AgentID:     r.cfg.AgentID,
		Status:      terminal.StatusStarting,
		Command:     append([]string{}, r.cfg.Command...),
		Workdir:     r.cfg.Workdir,
		Unit:        r.cfg.Unit,
		CreatedAt:   now,
		SocketPath:  r.cfg.SocketPath,
		RuntimePath: r.cfg.RuntimePath,
	}
	if err := r.writeStatus(); err != nil {
		return err
	}
	ln, err := shimruntime.ListenUnix(ctx, r.cfg.SocketPath)
	if err != nil {
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

func (r *Runtime) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", r.handleStatus)
	mux.HandleFunc("POST /attach", r.handleAttach)
	mux.HandleFunc("POST /start", r.handleStart)
	return mux
}

func (r *Runtime) serve() {
	if err := r.server.Serve(r.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		r.finish()
	}
}

func (r *Runtime) handleStatus(w http.ResponseWriter, _ *http.Request) {
	r.mu.Lock()
	status := r.status
	status.Command = append([]string{}, status.Command...)
	status.Metadata = cloneMap(status.Metadata)
	r.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

func (r *Runtime) handleAttach(w http.ResponseWriter, req *http.Request) {
	var replay shimruntime.ReplayFunc
	if wantsReplay(req) {
		replay = r.replayHistory
	}
	r.stream.HandleAttach(w, replay)
}

func wantsReplay(req *http.Request) bool {
	replay, _ := strconv.ParseBool(req.URL.Query().Get("replay"))
	return replay
}

// replayHistory streams the recorded output up to the cutover offset the shim
// captured when the attacher registered. It first waits for the async logger to
// persist the cutover bytes, then reads them back from disk in broadcast order.
func (r *Runtime) replayHistory(ctx context.Context, cutover int64, writeOutput func([]byte) error) error {
	r.logger.WaitForFlush(ctx, cutover)
	return terminal.StreamOutput(ctx, r.cfg.LogDir, r.cfg.TerminalID, cutover, writeOutput)
}

func (r *Runtime) handleStart(w http.ResponseWriter, _ *http.Request) {
	if err := r.startCommand(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	r.mu.Lock()
	status := r.status
	r.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

func (r *Runtime) startCommand() error {
	r.startMu.Lock()
	defer r.startMu.Unlock()

	r.mu.Lock()
	if r.cmd != nil {
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()

	cmd := exec.Command(r.cfg.Command[0], r.cfg.Command[1:]...) //nolint:gosec // command is injected sandbox agent config.
	cmd.Dir = r.cfg.Workdir
	cmd.Env = append(os.Environ(), "DISCOBOX_AGENT_TERMINAL_ID="+r.cfg.TerminalID)
	userEnv, err := execs.UserEnvDefaults(r.cfg.User)
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
	sysProcAttr, err := execs.AgentSysProcAttr(r.cfg.User)
	if err != nil {
		r.markStartFailed(err)
		return err
	}
	cmd.SysProcAttr = sysProcAttr
	if r.stream.HasAttachers() {
		resizeCtx, cancelResize := context.WithTimeout(context.Background(), 100*time.Millisecond)
		r.stream.WaitForResize(resizeCtx)
		cancelResize()
	}
	tty, err := pty.StartWithSize(cmd, r.stream.InitialWinsize(r.cfg.Rows, r.cfg.Cols))
	if err != nil {
		r.markStartFailed(err)
		return err
	}
	now := time.Now().UTC()
	r.mu.Lock()
	r.cmd = cmd
	r.tty = tty
	r.ptyDone = make(chan struct{})
	r.status.Status = terminal.StatusRunning
	r.status.PID = int64(cmd.Process.Pid)
	r.status.StartedAt = &now
	ptyDone := r.ptyDone
	r.mu.Unlock()
	if err := r.writeStatus(); err != nil {
		r.terminate()
		return err
	}
	go r.readPTY(ptyDone)
	go r.wait()
	return nil
}

func (r *Runtime) markStartFailed(err error) {
	now := time.Now().UTC()
	r.mu.Lock()
	r.status.Status = terminal.StatusFailed
	r.status.Error = err.Error()
	r.status.ExitedAt = &now
	status := r.status
	r.mu.Unlock()
	_ = r.writeStatusValue(status)
}

func (r *Runtime) readPTY(done chan struct{}) {
	defer close(done)
	buf := make([]byte, 32*1024)
	for {
		n, err := r.tty.Read(buf)
		if n > 0 {
			r.logger.Record(terminal.LogStreamOutput, buf[:n])
			r.stream.Broadcast(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

func (r *Runtime) handleAttachFrame(attach *shimruntime.Attacher, next frame.Frame) {
	switch next.Type {
	case frame.Input:
		if _, err := r.tty.Write(next.Payload); err != nil {
			_ = attach.WriteFrame(frame.Error, []byte(err.Error()))
			attach.Close()
			return
		}
		r.logger.Record(terminal.LogStreamInput, next.Payload)
	case frame.Resize:
		resize, err := frame.DecodeResize(next.Payload)
		if err != nil {
			_ = attach.WriteFrame(frame.Error, []byte(err.Error()))
			attach.Close()
			return
		}
		r.mu.Lock()
		tty := r.tty
		r.mu.Unlock()
		r.stream.ApplyResize(tty, resize)
	case frame.Signal:
		if err := signalProcess(r.cmd.Process, string(next.Payload)); err != nil {
			_ = attach.WriteFrame(frame.Error, []byte(err.Error()))
			attach.Close()
			return
		}
	default:
		_ = attach.WriteFrame(frame.Error, []byte("unknown frame type"))
		attach.Close()
		return
	}
}

func (r *Runtime) wait() {
	if r.cmd == nil {
		return
	}
	err := r.cmd.Wait()
	now := time.Now().UTC()
	r.mu.Lock()
	r.status.ExitedAt = &now
	r.status.Status = terminal.StatusExited
	if err != nil {
		r.status.Status = terminal.StatusFailed
		r.status.Error = err.Error()
	}
	if state := r.cmd.ProcessState; state != nil {
		code := int64(state.ExitCode())
		r.status.ExitCode = &code
	}
	status := r.status
	ptyDone := r.ptyDone
	attachers := r.stream.Attachers()
	r.mu.Unlock()
	if ptyDone != nil {
		<-ptyDone
	}
	_ = r.writeStatusValue(status)
	payload, _ := frame.EncodeExit(string(status.Status), status.ExitCode, status.Error)
	for _, attach := range attachers {
		_ = attach.WriteFrame(frame.Exit, payload)
		attach.Close()
	}
	r.finish()
}

func (r *Runtime) terminate() {
	if r.cmd != nil && r.cmd.Process != nil {
		_ = terminateProcessGroup(r.cmd.Process)
	}
}

func (r *Runtime) close() {
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
	if r.logger != nil {
		r.logger.Close()
	}
}

func (r *Runtime) finish() {
	r.doneOnce.Do(func() {
		close(r.done)
	})
}

func (r *Runtime) writeStatus() error {
	r.mu.Lock()
	status := r.status
	r.mu.Unlock()
	return r.writeStatusValue(status)
}

func (r *Runtime) writeStatusValue(status terminal.Terminal) error {
	if strings.TrimSpace(r.cfg.RuntimePath) == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(r.cfg.RuntimePath), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(r.cfg.RuntimePath, data, 0o600)
}

func signalProcess(process *os.Process, name string) error {
	if process == nil {
		return fmt.Errorf("process is unavailable")
	}
	sig, err := parseSignal(name)
	if err != nil {
		return err
	}
	return process.Signal(sig)
}

func parseSignal(name string) (os.Signal, error) {
	trimmed := strings.TrimSpace(strings.ToUpper(name))
	if trimmed == "" {
		return nil, fmt.Errorf("signal is required")
	}
	trimmed = strings.TrimPrefix(trimmed, "SIG")
	switch trimmed {
	case "INT":
		return interruptSignal(), nil
	case "TERM":
		return terminateSignal(), nil
	case "KILL":
		return killSignal(), nil
	case "HUP":
		return hangupSignal(), nil
	case "QUIT":
		return quitSignal(), nil
	default:
		return nil, fmt.Errorf("unsupported signal %q", name)
	}
}

func cloneMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
