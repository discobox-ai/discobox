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
	"runtime/debug"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/obot-platform/discobox/execstream/frame"
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
	// tty is the PTY master for TTY execs, set alongside the screen in
	// EnableScreen and nil for pipe execs. It receives resizes, the emulator's
	// terminal-query answers while no client is attached, and the post-replay
	// redraw jiggle. It outlives the screen: an emulator panic drops screen but
	// keeps tty, so attach still forces a program redraw.
	tty *os.File
	// exitPayload holds the encoded exit frame once the process has exited, so a
	// client that attaches after the exit still receives it (with the screen
	// replay) rather than a bare disconnect. nil while the process runs.
	exitPayload []byte
}

// EnableScreen turns on in-memory screen tracking for repaint-on-attach. It is
// called once, after the PTY exists but before output is broadcast, for TTY
// execs; plain (pipe) execs have no screen and attach without a repaint.
//
// tty is the PTY master: it answers the program's terminal queries while no
// client is attached (see pumpScreenResponses) and carries resizes and the
// post-replay redraw jiggle.
func (r *Runtime) EnableScreen(rows, cols uint16, scrollbackLines int, tty *os.File) {
	r.mu.Lock()
	screen := newScreenBuffer(rows, cols, scrollbackLines)
	r.screen = screen
	r.tty = tty
	r.mu.Unlock()
	go r.pumpScreenResponses(screen, tty)
}

// runScreenLocked runs fn against the screen buffer, dropping the screen if fn
// panics. The emulator is third-party code fed untrusted program output, so a
// bug in it must degrade repaint-on-attach to plain live streaming — the
// post-replay redraw jiggle still recovers the picture — rather than kill the
// exec. Callers hold r.mu and have checked r.screen is non-nil.
func (r *Runtime) runScreenLocked(fn func(*screenBuffer)) {
	defer func() {
		if v := recover(); v != nil {
			r.screen = nil
			fmt.Fprintf(os.Stderr, "screen emulator panic; disabling repaint-on-attach: %v\n%s", v, debug.Stack())
		}
	}()
	fn(r.screen)
}

// pumpScreenResponses drains the terminal-query responses (DA1, DSR, DECRQM,
// ...) that the screen emulator generates while absorbing output. The emulator
// writes responses to an unbuffered internal pipe during screenBuffer.write —
// which Broadcast calls holding r.mu — so an undrained pipe blocks Broadcast
// and deadlocks the whole runtime. This pump must therefore never stall: the
// reader goroutine below only reads the pipe and drops chunks when the
// delivery loop falls behind, so it can never be the blocked end.
//
// Delivery emulates a real terminal for a program running headless: while no
// client is attached the response is written to the PTY, so a TUI that blocks
// on a startup query (Claude Code waits on DA1) still comes up. While a client
// is attached its real terminal sees the query in the raw output stream and
// answers it, so the emulator's answer is dropped to avoid double responses.
//
// The pump lives for the process lifetime: vt.Emulator.Close races with the
// writes Broadcast issues, so the pipe is intentionally never closed.
func (r *Runtime) pumpScreenResponses(screen *screenBuffer, input io.Writer) {
	responses := make(chan []byte, 16)
	go func() {
		defer close(responses)
		defer func() {
			if v := recover(); v != nil {
				fmt.Fprintf(os.Stderr, "screen response pump panic; terminal queries will go unanswered: %v\n", v)
			}
		}()
		buf := make([]byte, 4096)
		for {
			n, err := screen.emu.Read(buf)
			if n > 0 {
				chunk := append([]byte(nil), buf[:n]...)
				select {
				case responses <- chunk:
				default:
					// Delivery is stalled (e.g. a full PTY input buffer); drop
					// the response rather than block the emulator pipe.
				}
			}
			if err != nil {
				return
			}
		}
	}()
	for chunk := range responses {
		if input == nil || r.HasAttachers() {
			continue
		}
		_, _ = input.Write(chunk)
	}
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
	// The http.Server's per-request read/write deadlines survive the hijack and
	// would kill this long-lived attach stream mid-session; the attach owns the
	// conn from here on, so clear them.
	_ = conn.SetDeadline(time.Time{})
	// Register before announcing the upgrade. A client that sees 101 may start the
	// process immediately, and anything the process wrote before this goroutine
	// joined the broadcast set would be lost — for a fast command that is its
	// entire output. The attacher starts buffering, so registering this early
	// cannot race the handshake bytes onto the wire: live frames queue until
	// flushBuffer below, after 101 is flushed.
	attach := &Attacher{w: rw, done: make(chan struct{}), ready: make(chan struct{}), buffering: true}
	var snapshot []byte
	repaint := repaintRequested && r.hasScreen()
	if repaint {
		snapshot = r.addReplayAttacher(attach)
	} else {
		r.addAttacher(attach)
	}
	defer r.removeAttacher(attach)

	_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + r.protocol + "\r\n\r\n")
	if err := rw.Flush(); err != nil {
		return
	}
	go r.readFrames(attach, rw)
	replayTTY := repaintRequested && r.hasTTY()
	if replayTTY {
		// Hold the repaint until the client signals it is attached and reading
		// (frame.Ready), so nothing is written during the upgrade handshake window
		// where an intermediate proxy hop may drop buffered bytes. Fall back after
		// a timeout so a client that never signals still gets a (best-effort)
		// repaint instead of hanging.
		r.waitForReady(attach)
		if repaint {
			if err := attach.writeSnapshot(snapshot); err != nil {
				attach.Close()
			}
		}
	}
	// Live streaming begins here for every attach: everything broadcast since
	// registration goes out in order, behind the snapshot when there was one.
	if err := attach.flushBuffer(); err != nil {
		attach.Close()
	}
	if replayTTY {
		// The program's own repaint is authoritative: jiggle the PTY size after
		// the replay so the program redraws itself and any snapshot imperfection
		// — or a missing snapshot after the screen was dropped — converges to
		// the program's real screen right after attach.
		r.redrawAfterReplay()
	}
	// If the process already exited (a late attacher, e.g. one that connected just
	// after a fast exit), deliver the retained exit frame after the replay so the
	// client sees the final screen and the exit code, then close.
	if payload, ok := r.exitFrame(); ok {
		_ = attach.WriteFrame(frame.Exit, payload)
		return
	}
	select {
	case <-attach.done:
	case <-r.done:
	}
}

// MarkExited records the process's encoded exit frame so late attachers receive
// it after the screen replay. Call it once the process has exited and its output
// has fully drained.
func (r *Runtime) MarkExited(payload []byte) {
	r.mu.Lock()
	r.exitPayload = payload
	r.mu.Unlock()
}

// exitFrame returns the retained exit payload, if the process has exited.
func (r *Runtime) exitFrame() ([]byte, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.exitPayload, r.exitPayload != nil
}

func (r *Runtime) hasScreen() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.screen != nil
}

func (r *Runtime) hasTTY() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tty != nil
}

// redrawAfterReplay forces the running program to repaint itself by shrinking
// the PTY by one row and restoring it, which delivers SIGWINCH and makes a TUI
// redraw at the (restored) real size. The replayed snapshot paints instantly
// but is only as faithful as the emulator; the program's redraw is the ground
// truth the client screen converges to.
func (r *Runtime) redrawAfterReplay() {
	r.mu.Lock()
	tty := r.tty
	r.mu.Unlock()
	if tty == nil {
		return
	}
	size, err := pty.GetsizeFull(tty)
	if err != nil || size.Rows == 0 || size.Cols == 0 {
		return
	}
	jiggle := *size
	if jiggle.Rows > 1 {
		jiggle.Rows--
	} else {
		jiggle.Rows++
	}
	_ = pty.Setsize(tty, &jiggle)
	_ = pty.Setsize(tty, size)
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

func (r *Runtime) ApplyResize(resize frame.ResizePayload) {
	r.mu.Lock()
	r.resize = &resize
	if r.screen != nil {
		r.runScreenLocked(func(screen *screenBuffer) { screen.resize(resize.Rows, resize.Cols) })
	}
	tty := r.tty
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

// Broadcast delivers one chunk of the process's output to every attacher as a
// frame of typ, which is frame.Stdout or frame.Stderr. Only the screen-bearing
// TTY path produces frame.Stdout through a PTY; pipe execs label each chunk with
// the stream it came from.
func (r *Runtime) Broadcast(typ byte, payload []byte) {
	// Feed the screen and snapshot the attacher set under one lock so a
	// concurrently registering repaint attacher falls cleanly on one side of this
	// chunk: either the screen absorbs it before the attacher's snapshot is taken
	// (delivered via the snapshot, not in the attacher set here) or the attacher
	// is registered first (in the set below, delivered as a buffered live frame).
	// The network writes stay outside the lock so a slow client cannot stall the
	// PTY reader.
	r.mu.Lock()
	// Only the merged PTY stream is screen state. A pipe exec has no screen, so
	// this never fires for stderr; the check keeps that true by construction.
	if r.screen != nil && typ == frame.Stdout {
		r.runScreenLocked(func(screen *screenBuffer) { screen.write(payload) })
	}
	attachers := r.snapshotAttachersLocked()
	r.mu.Unlock()
	for _, attach := range attachers {
		if err := attach.WriteFrame(typ, payload); err != nil {
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
// The attacher is already buffering when it arrives (HandleAttach constructs it
// that way), so live output queues from registration onward.
func (r *Runtime) addReplayAttacher(attach *Attacher) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attachers[attach] = struct{}{}
	if r.screen == nil {
		return nil
	}
	var snapshot []byte
	r.runScreenLocked(func(screen *screenBuffer) { snapshot = screen.snapshot() })
	return snapshot
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
	return a.writeLocked(frame.Stdout, payload)
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
