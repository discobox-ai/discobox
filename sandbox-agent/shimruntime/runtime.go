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
	"time"

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
	// screen, when set (TTY execs), mirrors the live output in an in-memory
	// terminal emulator so a late attacher can be repainted with the current
	// screen state instead of only output produced after it connected. It is fed
	// and snapshotted under mu, in lockstep with the attacher set, so a snapshot
	// captured at an attacher's registration reflects exactly the output
	// broadcast before it (see Broadcast and addReplayAttacher).
	screen *screenBuffer
}

// EnableScreen turns on in-memory screen tracking for repaint-on-attach. It is
// called once, before output starts, for TTY execs; plain (pipe) execs have no
// screen and attach without a repaint.
func (r *Runtime) EnableScreen(rows, cols uint16, scrollbackLines int) {
	r.mu.Lock()
	r.screen = newScreenBuffer(rows, cols, scrollbackLines)
	r.mu.Unlock()
}

type Attacher struct {
	mu        sync.Mutex
	w         *bufio.ReadWriter
	done      chan struct{}
	closeOnce sync.Once
	// ready is closed when the client sends a frame.Ready, signaling that the
	// attach tunnel is fully established and the client is reading output.
	ready     chan struct{}
	readyOnce sync.Once
	// While buffering, live frames are queued instead of written so a replay can
	// first drain history to the wire. flushBuffer replays the queue and clears
	// the flag once history has been sent.
	buffering bool
	buffered  []bufferedFrame
}

type bufferedFrame struct {
	typ     byte
	payload []byte
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

// HandleAttach upgrades w to a framed stream and joins it to the broadcast set.
// When the client requests a repaint and the exec has a screen, the attacher
// starts buffering live output, the captured screen snapshot is streamed as the
// repaint, then the buffered live frames are flushed and normal live streaming
// resumes.
func (r *Runtime) HandleAttach(w http.ResponseWriter, repaintRequested bool) {
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
	attach := &Attacher{w: rw, done: make(chan struct{}), ready: make(chan struct{})}
	var snapshot []byte
	repaint := repaintRequested && r.hasScreen()
	if repaint {
		snapshot = r.addReplayAttacher(attach)
	} else {
		r.addAttacher(attach)
	}
	defer r.removeAttacher(attach)
	go r.readFrames(attach, rw)
	if repaint {
		// Hold the repaint until the client signals it is attached and reading
		// (frame.Ready), so nothing is written during the upgrade handshake window
		// where an intermediate proxy hop may drop buffered bytes. Fall back after
		// a timeout so a client that never signals still gets a (best-effort)
		// repaint instead of hanging.
		r.waitForReady(attach)
		err := attach.writeSnapshot(snapshot)
		if err == nil {
			err = attach.flushBuffer()
		}
		if err != nil {
			attach.Close()
		}
	}
	select {
	case <-attach.done:
	case <-r.done:
	}
}

func (r *Runtime) hasScreen() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.screen != nil
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
		if next.Type == frame.Ready {
			attach.markReady()
			continue
		}
		if r.onFrame != nil {
			r.onFrame(attach, next)
		}
	}
}

// replayReadyTimeout bounds how long the shim waits for a client's frame.Ready
// before streaming replay history anyway. Clients on this codebase always send
// it; the timeout only covers older or non-conforming clients.
const replayReadyTimeout = 5 * time.Second

func (r *Runtime) waitForReady(attach *Attacher) {
	timer := time.NewTimer(replayReadyTimeout)
	defer timer.Stop()
	select {
	case <-attach.ready:
	case <-attach.done:
	case <-r.done:
	case <-timer.C:
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
	if r.screen != nil {
		r.screen.resize(resize.Rows, resize.Cols)
	}
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
	// Feed the screen and snapshot the attacher set under one lock so a
	// concurrently registering repaint attacher falls cleanly on one side of this
	// chunk: either the screen absorbs it before the attacher's snapshot is taken
	// (delivered via the snapshot, not in the attacher set here) or the attacher
	// is registered first (in the set below, delivered as a buffered live frame).
	// The network writes stay outside the lock so a slow client cannot stall the
	// PTY reader.
	r.mu.Lock()
	if r.screen != nil {
		r.screen.write(payload)
	}
	attachers := r.snapshotAttachersLocked()
	r.mu.Unlock()
	for _, attach := range attachers {
		if err := attach.WriteFrame(frame.Output, payload); err != nil {
			r.removeAttacher(attach)
		}
	}
}

func (r *Runtime) Attachers() []*Attacher {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshotAttachersLocked()
}

func (r *Runtime) snapshotAttachersLocked() []*Attacher {
	attachers := make([]*Attacher, 0, len(r.attachers))
	for attach := range r.attachers {
		attachers = append(attachers, attach)
	}
	return attachers
}

// addReplayAttacher registers a buffering attacher and returns the screen
// snapshot captured at that instant. Capturing the snapshot and registering the
// attacher under one lock keeps the repaint and the buffered live output exactly
// contiguous: every output chunk is either already in the snapshot or will be
// buffered from registration onward. See Broadcast.
func (r *Runtime) addReplayAttacher(attach *Attacher) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	attach.buffering = true
	r.attachers[attach] = struct{}{}
	if r.screen == nil {
		return nil
	}
	return r.screen.snapshot()
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
	if a.buffering {
		a.buffered = append(a.buffered, bufferedFrame{typ: typ, payload: append([]byte(nil), payload...)})
		return nil
	}
	return a.writeLocked(typ, payload)
}

func (a *Attacher) writeLocked(typ byte, payload []byte) error {
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

// writeSnapshot writes the repaint snapshot straight to the wire as one output
// frame, bypassing the live buffer. The attacher is still buffering while this
// runs, so live frames queue behind the snapshot and the wire is written only
// here. An empty snapshot writes nothing.
func (a *Attacher) writeSnapshot(payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.writeLocked(frame.Output, payload)
}

// flushBuffer writes the buffered live frames in order and switches the attacher
// to normal live streaming. The buffer holds exactly the output produced at and
// after the attacher registered, so it continues seamlessly from the snapshot.
func (a *Attacher) flushBuffer() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, f := range a.buffered {
		if err := a.writeLocked(f.typ, f.payload); err != nil {
			return err
		}
	}
	a.buffered = nil
	a.buffering = false
	return nil
}

func (a *Attacher) Close() {
	a.closeOnce.Do(func() {
		close(a.done)
	})
}

func (a *Attacher) markReady() {
	a.readyOnce.Do(func() {
		close(a.ready)
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
