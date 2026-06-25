package shim

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/obot-platform/discobox/sandbox-agent/terminal"
	"github.com/obot-platform/discobox/sandbox-agent/terminal/frame"
)

type Config struct {
	TerminalID  string
	AgentID     string
	Command     []string
	Workdir     string
	SocketPath  string
	RuntimePath string
	Rows        uint16
	Cols        uint16
	Env         map[string]string
}

type Runtime struct {
	cfg       Config
	cmd       *exec.Cmd
	tty       *os.File
	server    *http.Server
	listener  net.Listener
	done      chan struct{}
	doneOnce  sync.Once
	attachers map[*attachWriter]struct{}
	mu        sync.Mutex
	status    terminal.Terminal
}

type attachWriter struct {
	mu        sync.Mutex
	w         *bufio.ReadWriter
	done      chan struct{}
	closeOnce sync.Once
}

func Run(ctx context.Context, cfg Config) error {
	r := &Runtime{cfg: cfg, done: make(chan struct{}), attachers: map[*attachWriter]struct{}{}}
	if err := r.start(ctx); err != nil {
		return err
	}
	defer r.close()
	go r.serve()
	go r.readPTY()
	go r.wait()
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
	cmd := exec.CommandContext(ctx, r.cfg.Command[0], r.cfg.Command[1:]...) //nolint:gosec // command is injected sandbox agent config.
	cmd.Dir = r.cfg.Workdir
	cmd.Env = append(os.Environ(), "DISCOBOX_AGENT_TERMINAL_ID="+r.cfg.TerminalID)
	for key, value := range r.cfg.Env {
		if strings.TrimSpace(key) != "" {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	cmd.SysProcAttr = agentSysProcAttr()
	size := &pty.Winsize{Rows: r.cfg.Rows, Cols: r.cfg.Cols}
	if size.Rows == 0 {
		size.Rows = 24
	}
	if size.Cols == 0 {
		size.Cols = 80
	}
	tty, err := pty.StartWithSize(cmd, size)
	if err != nil {
		return err
	}
	r.cmd = cmd
	r.tty = tty
	now := time.Now().UTC()
	started := now
	r.status = terminal.Terminal{
		ID:        r.cfg.TerminalID,
		AgentID:   r.cfg.AgentID,
		Status:    terminal.StatusRunning,
		Command:   append([]string{}, r.cfg.Command...),
		Workdir:   r.cfg.Workdir,
		PID:       int64(cmd.Process.Pid),
		CreatedAt: now,
		StartedAt: &started,
	}
	if err := r.writeStatus(); err != nil {
		_ = tty.Close()
		_ = cmd.Process.Kill()
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.cfg.SocketPath), 0o700); err != nil {
		_ = tty.Close()
		_ = cmd.Process.Kill()
		return err
	}
	if err := prepareSocketPath(r.cfg.SocketPath); err != nil {
		_ = tty.Close()
		_ = cmd.Process.Kill()
		return err
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "unix", r.cfg.SocketPath)
	if err != nil {
		_ = tty.Close()
		_ = cmd.Process.Kill()
		return err
	}
	if err := os.Chmod(r.cfg.SocketPath, 0o600); err != nil {
		_ = ln.Close()
		_ = tty.Close()
		_ = cmd.Process.Kill()
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

func (r *Runtime) handleAttach(w http.ResponseWriter, _ *http.Request) {
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer conn.Close()
	_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: discobox-agent-terminal\r\n\r\n")
	if err := rw.Flush(); err != nil {
		return
	}
	attach := &attachWriter{w: rw, done: make(chan struct{})}
	r.addAttacher(attach)
	defer r.removeAttacher(attach)
	go r.readAttachFrames(attach, rw)
	select {
	case <-attach.done:
	case <-r.done:
	}
}

func (r *Runtime) readPTY() {
	buf := make([]byte, 32*1024)
	for {
		n, err := r.tty.Read(buf)
		if n > 0 {
			r.broadcast(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

func (r *Runtime) readAttachFrames(attach *attachWriter, rw io.Reader) {
	for {
		next, err := frame.Read(rw)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				_ = attach.writeFrame(frame.Error, []byte(err.Error()))
			}
			attach.close()
			return
		}
		switch next.Type {
		case frame.Input:
			if _, err := r.tty.Write(next.Payload); err != nil {
				_ = attach.writeFrame(frame.Error, []byte(err.Error()))
				attach.close()
				return
			}
		case frame.Resize:
			resize, err := frame.DecodeResize(next.Payload)
			if err != nil {
				_ = attach.writeFrame(frame.Error, []byte(err.Error()))
				attach.close()
				return
			}
			_ = pty.Setsize(r.tty, &pty.Winsize{Rows: resize.Rows, Cols: resize.Cols})
		case frame.Signal:
			if err := signalProcess(r.cmd.Process, string(next.Payload)); err != nil {
				_ = attach.writeFrame(frame.Error, []byte(err.Error()))
				attach.close()
				return
			}
		default:
			_ = attach.writeFrame(frame.Error, []byte("unknown frame type"))
			attach.close()
			return
		}
	}
}

func (r *Runtime) wait() {
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
	attachers := make([]*attachWriter, 0, len(r.attachers))
	for attach := range r.attachers {
		attachers = append(attachers, attach)
	}
	r.mu.Unlock()
	_ = r.writeStatusValue(status)
	for _, attach := range attachers {
		_ = attach.writeFrame(frame.Error, []byte("agent terminal exited"))
		attach.close()
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
}

func (r *Runtime) finish() {
	r.doneOnce.Do(func() {
		close(r.done)
	})
}

func (r *Runtime) addAttacher(attach *attachWriter) {
	r.mu.Lock()
	r.attachers[attach] = struct{}{}
	r.mu.Unlock()
}

func (r *Runtime) removeAttacher(attach *attachWriter) {
	r.mu.Lock()
	delete(r.attachers, attach)
	r.mu.Unlock()
}

func (r *Runtime) broadcast(payload []byte) {
	r.mu.Lock()
	attachers := make([]*attachWriter, 0, len(r.attachers))
	for attach := range r.attachers {
		attachers = append(attachers, attach)
	}
	r.mu.Unlock()
	for _, attach := range attachers {
		if err := attach.writeFrame(frame.Output, payload); err != nil {
			r.removeAttacher(attach)
		}
	}
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

func (a *attachWriter) writeFrame(typ byte, payload []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := frame.Write(a.w, typ, payload); err != nil {
		a.close()
		return err
	}
	if err := a.w.Flush(); err != nil {
		a.close()
		return err
	}
	return nil
}

func (a *attachWriter) close() {
	a.closeOnce.Do(func() {
		close(a.done)
	})
}

func prepareSocketPath(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing to remove non-socket path %s", path)
	}
	return os.Remove(path)
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
