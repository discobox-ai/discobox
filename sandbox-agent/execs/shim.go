package execs

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
	attachers map[*attachWriter]struct{}
	mu        sync.Mutex
	status    Exec
}

type attachWriter struct {
	mu        sync.Mutex
	w         *bufio.ReadWriter
	done      chan struct{}
	closeOnce sync.Once
}

func RunShim(ctx context.Context, cfg ShimConfig) error {
	r := &shimRuntime{cfg: cfg, done: make(chan struct{}), attachers: map[*attachWriter]struct{}{}}
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
		TTY:         r.cfg.TTY,
		Unit:        r.cfg.Unit,
		CreatedAt:   now,
		SocketPath:  r.cfg.SocketPath,
		RuntimePath: r.cfg.RuntimePath,
	}
	if err := r.writeStatus(); err != nil {
		logger.Close()
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.cfg.SocketPath), 0o700); err != nil {
		r.terminate()
		logger.Close()
		return err
	}
	if err := prepareSocketPath(r.cfg.SocketPath); err != nil {
		r.terminate()
		logger.Close()
		return err
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "unix", r.cfg.SocketPath)
	if err != nil {
		r.terminate()
		logger.Close()
		return err
	}
	if err := os.Chmod(r.cfg.SocketPath, 0o600); err != nil {
		_ = ln.Close()
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
	r.tty = tty
	r.stdin = tty
	r.status.PID = int64(cmd.Process.Pid)
	r.outputWG.Add(1)
	go r.copyOutput(LogStreamOutput, tty)
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
	mux.HandleFunc("POST /attach", r.handleAttach)
	mux.HandleFunc("POST /start", r.handleStart)
	return mux
}

func (r *shimRuntime) serve() {
	if err := r.server.Serve(r.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		r.finish()
	}
}

func (r *shimRuntime) handleAttach(w http.ResponseWriter, _ *http.Request) {
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer conn.Close()
	_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: discobox-sandbox-exec\r\n\r\n")
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

func (r *shimRuntime) handleStart(w http.ResponseWriter, _ *http.Request) {
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

func (r *shimRuntime) startCommand() error {
	r.startMu.Lock()
	defer r.startMu.Unlock()

	r.mu.Lock()
	if r.cmd != nil {
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()

	cmd := exec.Command(r.cfg.Command[0], r.cfg.Command[1:]...) //nolint:gosec // command is caller-supplied for sandbox exec.
	cmd.Dir = r.cfg.Workdir
	cmd.Env = append(os.Environ(), "DISCOBOX_EXEC_ID="+r.cfg.ExecID)
	for key, value := range r.cfg.Env {
		if strings.TrimSpace(key) != "" {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	cmd.SysProcAttr = agentSysProcAttr()

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
	buf := make([]byte, 32*1024)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			r.logger.Record(stream, chunk)
			r.broadcast(chunk)
		}
		if err != nil {
			return
		}
	}
}

func (r *shimRuntime) readAttachFrames(attach *attachWriter, rw io.Reader) {
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
			if err := r.writeInput(next.Payload); err != nil {
				_ = attach.writeFrame(frame.Error, []byte(err.Error()))
				attach.close()
				return
			}
		case frame.Resize:
			if r.tty == nil {
				continue
			}
			resize, err := frame.DecodeResize(next.Payload)
			if err != nil {
				_ = attach.writeFrame(frame.Error, []byte(err.Error()))
				attach.close()
				return
			}
			_ = pty.Setsize(r.tty, &pty.Winsize{Rows: resize.Rows, Cols: resize.Cols})
		case frame.Signal:
			if err := signalProcess(r.cmd, string(next.Payload)); err != nil {
				_ = attach.writeFrame(frame.Error, []byte(err.Error()))
				attach.close()
				return
			}
		case frame.CloseInput:
			r.closeInput()
		default:
			_ = attach.writeFrame(frame.Error, []byte("unknown frame type"))
			attach.close()
			return
		}
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
	attachers := make([]*attachWriter, 0, len(r.attachers))
	for attach := range r.attachers {
		attachers = append(attachers, attach)
	}
	r.mu.Unlock()
	r.outputWG.Wait()
	_ = r.writeStatusValue(status)
	payload, _ := frame.EncodeExit(string(status.Status), status.ExitCode, status.Error)
	for _, attach := range attachers {
		_ = attach.writeFrame(frame.Exit, payload)
		attach.close()
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

func (r *shimRuntime) addAttacher(attach *attachWriter) {
	r.mu.Lock()
	r.attachers[attach] = struct{}{}
	r.mu.Unlock()
}

func (r *shimRuntime) removeAttacher(attach *attachWriter) {
	r.mu.Lock()
	delete(r.attachers, attach)
	r.mu.Unlock()
}

func (r *shimRuntime) broadcast(payload []byte) {
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

func (r *shimRuntime) writeStatus() error {
	r.mu.Lock()
	status := r.status
	r.mu.Unlock()
	return r.writeStatusValue(status)
}

func (r *shimRuntime) writeStatusValue(status Exec) error {
	return writeRuntime(r.cfg.RuntimePath, status)
}

func (a *attachWriter) writeFrame(typ byte, payload []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.writeFrameLocked(typ, payload)
}

func (a *attachWriter) writeFrameLocked(typ byte, payload []byte) error {
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
