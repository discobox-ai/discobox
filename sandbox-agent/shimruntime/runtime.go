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
	// outputOffset counts every output byte ever broadcast. A replay attacher
	// captures it atomically with its own registration to define the exact
	// cutover between history (streamed from disk) and live output (buffered
	// from registration onward), so no byte is lost or duplicated across the two.
	outputOffset int64
}

// ReplayFunc streams recorded output up to cutover bytes to a replay attacher
// before its buffered live output is flushed. It receives writeOutput to emit
// each historical chunk as an output frame directly to the client. Returning an
// error aborts the attach.
type ReplayFunc func(ctx context.Context, cutover int64, writeOutput func([]byte) error) error

type Attacher struct {
	mu        sync.Mutex
	w         *bufio.ReadWriter
	done      chan struct{}
	closeOnce sync.Once
	// ready is closed when the client sends a frame.Ready, signalling that the
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
// When replay is non-nil the attacher starts buffering live output, replay
// streams the recorded history up to the captured cutover, then the buffered
// live frames are flushed and normal live streaming resumes.
func (r *Runtime) HandleAttach(w http.ResponseWriter, replay ReplayFunc) {
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
	var cutover int64
	if replay != nil {
		cutover = r.addReplayAttacher(attach)
	} else {
		r.addAttacher(attach)
	}
	defer r.removeAttacher(attach)
	go r.readFrames(attach, rw)
	if replay != nil {
		// Hold the history stream until the client signals it is attached and
		// reading (frame.Ready), so nothing is written during the upgrade
		// handshake window where an intermediate proxy hop may drop buffered
		// bytes. Fall back after a timeout so a client that never signals still
		// gets a (best-effort) replay instead of hanging.
		r.waitForReady(attach)
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			select {
			case <-attach.done:
			case <-r.done:
			}
			cancel()
		}()
		err := replay(ctx, cutover, attach.writeReplayOutput)
		cancel()
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
	// Advance the output offset and snapshot the attacher set under one lock so a
	// concurrently registering replay attacher captures a cutover that cleanly
	// splits this chunk to exactly one side: either it is counted below the
	// cutover (read from disk, not in the snapshot) or at/above it (in the
	// snapshot and buffered). The network writes stay outside the lock so a slow
	// client cannot stall the PTY reader.
	r.mu.Lock()
	r.outputOffset += int64(len(payload))
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

// addReplayAttacher registers a buffering attacher and returns the output offset
// captured at that instant. See Broadcast for why this must be atomic with the
// offset counter.
func (r *Runtime) addReplayAttacher(attach *Attacher) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	attach.buffering = true
	r.attachers[attach] = struct{}{}
	return r.outputOffset
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

// writeReplayOutput writes a historical output frame straight to the wire,
// bypassing the live buffer. The attacher is still buffering while this runs, so
// live frames queue behind the history and the wire is written only here.
func (a *Attacher) writeReplayOutput(payload []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.writeLocked(frame.Output, payload)
}

// flushBuffer writes the buffered live frames in order and switches the attacher
// to normal live streaming. The buffer holds exactly the output produced at and
// after the replay cutover, so it continues seamlessly from the history.
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
