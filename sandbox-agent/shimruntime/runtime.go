// Package shimruntime is the sandbox-agent's platform half of an exec attach
// stream. The fan-out — attachers, ordering, buffering, the retained exit frame
// — lives in the root module's execstream/host; what stays here is everything
// that touches this machine: the PTY, the terminal emulator behind
// repaint-on-attach, the Unix socket, and the HTTP upgrade.
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
	"github.com/obot-platform/discobox/execstream/host"
)

// Runtime owns the terminal side of one exec and delegates the stream to
// host.Stream. It is also the stream's host.Replayer: repaint-on-attach is
// backed by the screen emulator and the PTY, both of which are this machine's
// business, not the protocol's.
type Runtime struct {
	protocol string
	stream   *host.Stream

	mu sync.Mutex
	// screen, when set (TTY execs), mirrors the live output in an in-memory
	// terminal emulator so a late attacher can be repainted with the current
	// screen state instead of only output produced after it connected.
	screen *screenBuffer
	// tty is the PTY master for TTY execs, set alongside the screen in
	// EnableScreen and nil for pipe execs. It receives resizes, the emulator's
	// terminal-query answers while no client is attached, and the post-replay
	// redraw jiggle. It outlives the screen: an emulator panic drops screen but
	// keeps tty, so attach still forces a program redraw.
	tty *os.File
}

func New(protocol string, done <-chan struct{}, onFrame func(frame.Frame) error) *Runtime {
	r := &Runtime{protocol: protocol}
	r.stream = host.New(host.Options{Done: done, OnFrame: onFrame})
	return r
}

// EnableScreen turns on in-memory screen tracking for repaint-on-attach. It is
// called once, after the PTY exists but before output is broadcast, for TTY
// execs; plain (pipe) execs have no screen and attach without a repaint.
//
// Installing the replayer here rather than at New is what keeps a pipe exec
// from waiting on a repaint handshake it will never satisfy.
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
	r.stream.SetReplayer(r)
	go r.pumpScreenResponses(screen, tty)
}

// Title is the window title the program last set (OSC 0/2), held by the
// screen emulator as it goes past. Empty for a program that never set one,
// and always empty for pipe execs, which have no screen to hold it.
func (r *Runtime) Title() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.screen == nil {
		return ""
	}
	return r.screen.title
}

// Observe feeds the screen emulator, implementing host.Replayer. It runs under
// the stream lock, so it must not block — see pumpScreenResponses.
func (r *Runtime) Observe(payload []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.screen == nil {
		return
	}
	r.runScreenLocked(func(screen *screenBuffer) { screen.write(payload) })
}

// Snapshot serializes the current screen for a repaint, implementing
// host.Replayer. It returns nil once the emulator has been dropped after a
// panic; the post-replay redraw still recovers the picture.
func (r *Runtime) Snapshot() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.screen == nil {
		return nil
	}
	var snapshot []byte
	r.runScreenLocked(func(screen *screenBuffer) { snapshot = screen.snapshot() })
	return snapshot
}

// AfterReplay forces the running program to repaint itself by shrinking the PTY
// by one row and restoring it, which delivers SIGWINCH and makes a TUI redraw at
// the (restored) real size. It implements host.Replayer.
//
// The replayed snapshot paints instantly but is only as faithful as the
// emulator; the program's own redraw is the ground truth the client screen
// converges to, and it is also what recovers a repaint whose snapshot was lost
// to an emulator panic.
func (r *Runtime) AfterReplay() {
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
// which Observe calls holding both the stream lock and r.mu — so an undrained
// pipe blocks output and deadlocks the whole runtime. This pump must therefore
// never stall: the reader goroutine below only reads the pipe and drops chunks
// when the delivery loop falls behind, so it can never be the blocked end.
//
// Delivery emulates a real terminal for a program running headless: while no
// client is attached the response is written to the PTY, so a TUI that blocks
// on a startup query (Claude Code waits on DA1) still comes up. While a client
// is attached its real terminal sees the query in the raw output stream and
// answers it, so the emulator's answer is dropped to avoid double responses.
//
// The pump lives for the process lifetime: vt.Emulator.Close races with the
// writes Observe issues, so the pipe is intentionally never closed.
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
		if input == nil || r.stream.HasAttachers() {
			continue
		}
		_, _ = input.Write(chunk)
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

// HandleAttach upgrades w to a framed stream and hands it to the stream.
//
// The 101 is written from the Ready callback, which host.Stream invokes only
// after the attacher has joined the broadcast set: a client that sees the
// upgrade may start the process immediately, and output broadcast before
// registration would be lost.
func (r *Runtime) HandleAttach(w http.ResponseWriter, repaintRequested bool) {
	netConn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer netConn.Close()
	// The http.Server's per-request read/write deadlines survive the hijack and
	// would kill this long-lived attach stream mid-session; the attach owns the
	// conn from here on, so clear them.
	_ = netConn.SetDeadline(time.Time{})

	conn := &upgradeConn{rw: rw, conn: netConn}
	_ = r.stream.Attach(context.Background(), conn, host.AttachOptions{
		Replay: repaintRequested,
		Ready: func() error {
			_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + r.protocol + "\r\n\r\n")
			return rw.Flush()
		},
	})
}

// upgradeConn is an execstream.Conn over a hijacked HTTP connection.
type upgradeConn struct {
	rw   *bufio.ReadWriter
	conn net.Conn
	mu   sync.Mutex
}

func (u *upgradeConn) ReadFrame() (frame.Frame, error) { return frame.Read(u.rw) }

func (u *upgradeConn) WriteFrame(typ byte, payload []byte) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if err := frame.Write(u.rw, typ, payload); err != nil {
		return err
	}
	return u.rw.Flush()
}

func (u *upgradeConn) Close() error { return u.conn.Close() }

// Broadcast delivers one chunk of process output to every attacher.
func (r *Runtime) Broadcast(typ byte, payload []byte) { r.stream.Broadcast(typ, payload) }

// MarkExited retains the encoded exit frame for clients that attach later.
func (r *Runtime) MarkExited(payload []byte) { r.stream.MarkExited(payload) }

// Attachers returns the current attacher set.
func (r *Runtime) Attachers() []*host.Attacher { return r.stream.Attachers() }

// HasAttachers reports whether any client is attached.
func (r *Runtime) HasAttachers() bool { return r.stream.HasAttachers() }

// WaitForResize blocks until a client has sent a size or ctx is done.
func (r *Runtime) WaitForResize(ctx context.Context) { r.stream.WaitForResize(ctx) }

// InitialWinsize is the size to start the PTY at: the size a client already
// asked for, else the configured one, else a sane default.
func (r *Runtime) InitialWinsize(rows, cols uint16) *pty.Winsize {
	size := &pty.Winsize{Rows: rows, Cols: cols}
	if pending, ok := r.stream.PendingResize(); ok {
		size.Rows = pending.Rows
		size.Cols = pending.Cols
	}
	if size.Rows == 0 {
		size.Rows = 24
	}
	if size.Cols == 0 {
		size.Cols = 80
	}
	return size
}

// ApplyResize records the requested size and applies it to the screen and PTY.
func (r *Runtime) ApplyResize(resize frame.ResizePayload) {
	r.stream.ApplyResize(resize)
	r.mu.Lock()
	if r.screen != nil {
		r.runScreenLocked(func(screen *screenBuffer) { screen.resize(resize.Rows, resize.Cols) })
	}
	tty := r.tty
	r.mu.Unlock()
	if tty != nil {
		_ = pty.Setsize(tty, &pty.Winsize{Rows: resize.Rows, Cols: resize.Cols})
	}
}

// hasScreen reports whether repaint-on-attach is still available.
func (r *Runtime) hasScreen() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.screen != nil
}

// hasTTY reports whether this exec has a PTY.
func (r *Runtime) hasTTY() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tty != nil
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
