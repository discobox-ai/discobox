package daemon

import (
	"context"
	"crypto/subtle"
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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	sessions "github.com/obot-platform/discobox/sessions"
	supervisorapigen "github.com/obot-platform/discobox/sessions/api/supervisorgen"
	"github.com/obot-platform/discobox/sessions/store"
)

type SupervisorConfig struct {
	Session     sessions.Session
	SocketPath  string
	RuntimePath string
	Rows        uint16
	Cols        uint16
	Token       string
}

type supervisorRuntime struct {
	cfg            SupervisorConfig
	cmd            *exec.Cmd
	tty            *os.File
	server         *http.Server
	listener       net.Listener
	done           chan struct{}
	doneOnce       sync.Once
	attachers      map[*attachWriter]struct{}
	mu             sync.Mutex
	activeAttaches int64
	exit           store.RuntimeExit
}

func RunSupervisor(ctx context.Context, cfg SupervisorConfig) error {
	if cfg.Token == "" {
		return fmt.Errorf("supervisor token is required")
	}
	r := &supervisorRuntime{cfg: cfg, done: make(chan struct{}), attachers: map[*attachWriter]struct{}{}}
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

func (r *supervisorRuntime) start(ctx context.Context) error {
	if len(r.cfg.Session.Command) == 0 {
		return fmt.Errorf("command is required")
	}
	//nolint:gosec // The sessions module intentionally launches the configured local coding-agent command.
	cmd := exec.CommandContext(ctx, r.cfg.Session.Command[0], r.cfg.Session.Command[1:]...)
	cmd.Dir = r.cfg.Session.Workdir
	cmd.Env = append(os.Environ(), "DISCOBOX_CODING_AGENT_SESSION=1")
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
	if err := os.MkdirAll(filepath.Dir(r.cfg.SocketPath), 0o755); err != nil {
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
	handler, err := r.handler()
	if err != nil {
		_ = tty.Close()
		_ = cmd.Process.Kill()
		_ = ln.Close()
		return err
	}
	r.listener = ln
	r.server = &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	return r.writeRuntime(store.RuntimeExit{SessionID: r.cfg.Session.ID, PID: cmd.Process.Pid})
}

func (r *supervisorRuntime) handler() (http.Handler, error) {
	generated, err := supervisorapigen.NewServer(
		&supervisorGeneratedHandler{r: r},
		supervisorSecurityHandler{token: r.cfg.Token},
		supervisorapigen.WithErrorHandler(supervisorErrorHandler),
	)
	if err != nil {
		return nil, err
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost && req.URL.Path == "/attach" {
			if !r.authorized(req) {
				writeError(w, http.StatusUnauthorized, fmt.Errorf("unauthorized"))
				return
			}
			r.handleAttach(w, req)
			return
		}
		generated.ServeHTTP(w, req)
	}), nil
}

func (r *supervisorRuntime) authorized(req *http.Request) bool {
	got := strings.TrimSpace(req.Header.Get("Authorization"))
	want := "Bearer " + r.cfg.Token
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (r *supervisorRuntime) serve() {
	if err := r.server.Serve(r.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		r.finish()
	}
}

func (r *supervisorRuntime) readPTY() {
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

func (r *supervisorRuntime) wait() {
	err := r.cmd.Wait()
	now := time.Now().UTC()
	exit := store.RuntimeExit{SessionID: r.cfg.Session.ID, PID: r.cmd.Process.Pid, ExitedAt: &now}
	if err != nil {
		exit.Error = err.Error()
	}
	if state := r.cmd.ProcessState; state != nil {
		code := state.ExitCode()
		exit.ExitCode = &code
	}
	r.mu.Lock()
	r.exit = exit
	attachers := make([]*attachWriter, 0, len(r.attachers))
	for attach := range r.attachers {
		attachers = append(attachers, attach)
	}
	r.mu.Unlock()
	_ = r.writeRuntime(exit)
	for _, attach := range attachers {
		_ = attach.writeFrame(sessions.FrameError, []byte("session exited"))
		attach.close()
	}
	r.finish()
}

func (r *supervisorRuntime) finish() {
	r.doneOnce.Do(func() {
		close(r.done)
	})
}

func (r *supervisorRuntime) terminate() {
	if r.cmd != nil && r.cmd.Process != nil {
		_ = signalProcessGroup(r.cmd.Process, syscall.SIGTERM)
	}
}

func (r *supervisorRuntime) close() {
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

func (r *supervisorRuntime) handleAttach(w http.ResponseWriter, _ *http.Request) {
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer conn.Close()
	_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: discobox-session\r\n\r\n")
	if err := rw.Flush(); err != nil {
		return
	}
	attach := &attachWriter{w: rw, done: make(chan struct{})}
	r.addAttacher(attach)
	defer r.removeAttacher(attach)
	atomic.AddInt64(&r.activeAttaches, 1)
	defer atomic.AddInt64(&r.activeAttaches, -1)
	go r.readAttachFrames(attach, rw)
	select {
	case <-attach.done:
	case <-r.done:
	}
}

func (r *supervisorRuntime) readAttachFrames(attach *attachWriter, rw frameReader) {
	for {
		frame, err := sessions.ReadFrame(rw)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				_ = attach.writeFrame(sessions.FrameError, []byte(err.Error()))
				attach.close()
			}
			return
		}
		switch frame.Type {
		case sessions.FrameInput:
			if _, err := r.tty.Write(frame.Payload); err != nil {
				_ = attach.writeFrame(sessions.FrameError, []byte(err.Error()))
				attach.close()
				return
			}
		case sessions.FrameResize:
			resize, err := sessions.DecodeResize(frame.Payload)
			if err != nil {
				_ = attach.writeFrame(sessions.FrameError, []byte(err.Error()))
				attach.close()
				return
			}
			_ = pty.Setsize(r.tty, &pty.Winsize{Rows: resize.Rows, Cols: resize.Cols})
		case sessions.FrameSignal:
			if err := signalProcess(r.cmd.Process, string(frame.Payload)); err != nil {
				_ = attach.writeFrame(sessions.FrameError, []byte(err.Error()))
				attach.close()
				return
			}
		default:
			_ = attach.writeFrame(sessions.FrameError, []byte("unknown frame type"))
			attach.close()
			return
		}
	}
}

type frameReader interface {
	io.Reader
}

func (r *supervisorRuntime) addAttacher(attach *attachWriter) {
	r.mu.Lock()
	r.attachers[attach] = struct{}{}
	r.mu.Unlock()
}

func (r *supervisorRuntime) removeAttacher(attach *attachWriter) {
	r.mu.Lock()
	delete(r.attachers, attach)
	r.mu.Unlock()
}

func (r *supervisorRuntime) broadcast(payload []byte) {
	r.mu.Lock()
	attachers := make([]*attachWriter, 0, len(r.attachers))
	for attach := range r.attachers {
		attachers = append(attachers, attach)
	}
	r.mu.Unlock()
	for _, attach := range attachers {
		if err := attach.writeFrame(sessions.FrameOutput, payload); err != nil {
			r.removeAttacher(attach)
		}
	}
}

func (r *supervisorRuntime) writeRuntime(exit store.RuntimeExit) error {
	if r.cfg.RuntimePath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(r.cfg.RuntimePath), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(exit, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(r.cfg.RuntimePath, data, 0o600)
}
