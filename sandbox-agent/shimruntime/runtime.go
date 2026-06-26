package shimruntime

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/creack/pty"
	"github.com/obot-platform/discobox/sandbox-agent/terminal/frame"
)

type Runtime struct {
	protocol    string
	done        <-chan struct{}
	onFrame     func(*Attacher, frame.Frame)
	mu          sync.Mutex
	attachers   map[*Attacher]struct{}
	resize      *frame.ResizePayload
	resizeReady chan struct{}
	resizeOnce  sync.Once
}

type Attacher struct {
	mu        sync.Mutex
	w         *bufio.ReadWriter
	done      chan struct{}
	closeOnce sync.Once
}

func New(protocol string, done <-chan struct{}, onFrame func(*Attacher, frame.Frame)) *Runtime {
	return &Runtime{
		protocol:    protocol,
		done:        done,
		onFrame:     onFrame,
		attachers:   map[*Attacher]struct{}{},
		resizeReady: make(chan struct{}),
	}
}

func ListenUnix(ctx context.Context, socketPath string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		return nil, err
	}
	if err := prepareSocketPath(socketPath); err != nil {
		return nil, err
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "unix", socketPath)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

func (r *Runtime) HandleAttach(w http.ResponseWriter) {
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer conn.Close()
	_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + r.protocol + "\r\n\r\n")
	if err := rw.Flush(); err != nil {
		return
	}
	attach := &Attacher{w: rw, done: make(chan struct{})}
	r.addAttacher(attach)
	defer r.removeAttacher(attach)
	go r.readFrames(attach, rw)
	select {
	case <-attach.done:
	case <-r.done:
	}
}

func (r *Runtime) readFrames(attach *Attacher, rw *bufio.ReadWriter) {
	for {
		next, err := frame.Read(rw)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) && !errors.Is(err, net.ErrClosed) {
				_ = attach.WriteFrame(frame.Error, []byte(err.Error()))
			}
			attach.Close()
			return
		}
		if r.onFrame != nil {
			r.onFrame(attach, next)
		}
	}
}

func (r *Runtime) InitialWinsize(rows, cols uint16) *pty.Winsize {
	size := &pty.Winsize{Rows: rows, Cols: cols}
	r.mu.Lock()
	if r.resize != nil {
		size.Rows = r.resize.Rows
		size.Cols = r.resize.Cols
	}
	r.mu.Unlock()
	if size.Rows == 0 {
		size.Rows = 24
	}
	if size.Cols == 0 {
		size.Cols = 80
	}
	return size
}

func (r *Runtime) ApplyResize(tty *os.File, resize frame.ResizePayload) {
	r.mu.Lock()
	r.resize = &resize
	r.mu.Unlock()
	r.resizeOnce.Do(func() {
		close(r.resizeReady)
	})
	if tty != nil {
		_ = pty.Setsize(tty, &pty.Winsize{Rows: resize.Rows, Cols: resize.Cols})
	}
}

func (r *Runtime) WaitForResize(ctx context.Context) {
	r.mu.Lock()
	ready := r.resize != nil
	r.mu.Unlock()
	if ready {
		return
	}
	select {
	case <-r.resizeReady:
	case <-ctx.Done():
	}
}

func (r *Runtime) Broadcast(payload []byte) {
	for _, attach := range r.Attachers() {
		if err := attach.WriteFrame(frame.Output, payload); err != nil {
			r.removeAttacher(attach)
		}
	}
}

func (r *Runtime) Attachers() []*Attacher {
	r.mu.Lock()
	defer r.mu.Unlock()
	attachers := make([]*Attacher, 0, len(r.attachers))
	for attach := range r.attachers {
		attachers = append(attachers, attach)
	}
	return attachers
}

func (r *Runtime) HasAttachers() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.attachers) > 0
}

func (r *Runtime) addAttacher(attach *Attacher) {
	r.mu.Lock()
	r.attachers[attach] = struct{}{}
	r.mu.Unlock()
}

func (r *Runtime) removeAttacher(attach *Attacher) {
	r.mu.Lock()
	delete(r.attachers, attach)
	r.mu.Unlock()
}

func (a *Attacher) WriteFrame(typ byte, payload []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := frame.Write(a.w, typ, payload); err != nil {
		a.Close()
		return err
	}
	if err := a.w.Flush(); err != nil {
		a.Close()
		return err
	}
	return nil
}

func (a *Attacher) Close() {
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
