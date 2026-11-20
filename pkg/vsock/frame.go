package vsock

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"
)

// A framed connection carries one yamux stream over one VSOCK connection
// (ADR 26-11-17-182). Virtualization.framework serves every VSOCK connection
// of a VM from one serial queue, and delivers guest data into the host with a
// blocking write: a host that stops reading any connection stops the queue,
// and with it every connection of the VM. The stream's receive window moves
// that wait into the guest. A sender may have at most FrameWindow unread bytes
// outstanding, so a stalled reader blocks the guest's Write on that stream
// alone, while the session underneath keeps reading the socket.
//
// The framing applies only where the endpoint asks for it. A connection that
// is not framed is used exactly as before.

// FrameWindow is a framed stream's receive window: yamux's initial window,
// which is also its minimum, and never grown past. It bounds what the reading
// side holds for a stream whose consumer has stopped.
const FrameWindow = 256 << 10

// FramingKernelParameter is what a host puts on a guest's kernel command line
// to make the guest's VSOCK listeners framed. A guest service whose listen
// address is fixed in the image reads its framing from here, since the image
// cannot know which host booted it.
const FramingKernelParameter = "discobox.vsock=yamux"

// frameHandshakeTimeout bounds how long an accepted connection may take to
// open its stream before it is closed. A framed peer opens it with its first
// bytes.
const frameHandshakeTimeout = 30 * time.Second

// FramedByKernel reports whether a kernel command line carries
// FramingKernelParameter.
func FramedByKernel(cmdline string) bool {
	for _, field := range strings.Fields(cmdline) {
		if field == FramingKernelParameter {
			return true
		}
	}
	return false
}

// frameConfig is the session configuration both ends use. A session carries
// one stream: each end sends a go-away once its stream exists (oneStream), so
// a further stream from the peer is reset rather than buffered. No keepalive,
// and no close timeout: a connection is as alive as the stream it carries, as
// an unframed one is. yamux's close timeout would reset a stream half-closed
// by CloseWrite whose peer is still sending after it (a container's output
// after its client closed stdin); the session ends when its connection is
// closed instead.
func frameConfig() *yamux.Config {
	config := yamux.DefaultConfig()
	config.AcceptBacklog = 1
	config.EnableKeepAlive = false
	config.StreamCloseTimeout = 0
	config.MaxStreamWindowSize = FrameWindow
	config.LogOutput = io.Discard
	return config
}

// FrameDialed frames a connection this side opened: it starts the session,
// whose receive loop reads conn from now on, and opens the one stream. Closing
// the returned connection closes conn.
//
// A peer that does not speak the framing answers in plain bytes, which the
// session ends on without a word, so the caller would read only EOF. report,
// when not nil, is told once if the peer's first byte is not a frame's: that
// is how such a peer shows up, as it does through FrameListener.
func FrameDialed(conn net.Conn, report func(error)) (net.Conn, error) {
	if report != nil {
		conn = &firstFrameCheck{Conn: conn, report: report}
	}
	transport := &frameTransport{Conn: conn}
	session, err := yamux.Client(transport, frameConfig())
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	stream, err := session.OpenStream()
	if err != nil {
		_ = session.Close()
		return nil, err
	}
	return oneStream(session, stream, transport)
}

// yamuxVersion is the first byte of every yamux frame: the protocol version a
// framed peer speaks.
const yamuxVersion = 0

// errUnframedPeer is what FrameDialed reports of a peer whose first byte is
// not a frame's.
var errUnframedPeer = errors.New("vsock: peer answered without framing")

// firstFrameCheck is the raw connection under a framed one FrameDialed opened.
// It calls report once if the peer's first byte is not a yamux frame's. It
// carries net.Conn's methods and nothing more.
type firstFrameCheck struct {
	net.Conn
	checked atomic.Bool
	report  func(error)
}

func (c *firstFrameCheck) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 && c.checked.CompareAndSwap(false, true) && b[0] != yamuxVersion {
		c.report(errUnframedPeer)
	}
	return n, err
}

// oneStream closes the session to any stream but stream, and returns the two
// as a connection.
func oneStream(session *yamux.Session, stream *yamux.Stream, transport *frameTransport) (net.Conn, error) {
	conn := &framedConn{stream: stream, session: session, transport: transport}
	if err := session.GoAway(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// FrameListener frames every connection inner accepts. Its own accept loop
// starts each connection's session as soon as inner hands the connection over,
// so the connection is read from that moment whether or not anyone has called
// Accept yet, and a connection waiting for Accept never holds up the next one.
//
// A connection whose peer does not open a framed stream is closed and never
// returned by Accept; report, when not nil, is told why. That is how a peer
// that does not speak the framing shows up.
//
// The accept loop never waits on its caller, not even to report an error. A
// connection the framework has handed over is a socket nobody reads until
// this loop starts its session, and on vz a guest write that overflows an
// unread socket's buffer (8 KiB) blocks the framework's queue. An error inner's Accept returns
// is offered to the next Accept as it is, so a caller that retries a
// temporary error retries it here too; while one is still unread, later ones
// are dropped, and the loop backs off before accepting again.
//
// The loop stops when inner's Accept returns net.ErrClosed. An inner whose
// Accept never returns once closed (vz's binding) leaves the loop parked in
// it; either way, Accept on this listener returns net.ErrClosed once Close has
// been called.
func FrameListener(inner net.Listener, report func(error)) net.Listener {
	l := &frameListener{
		inner:  inner,
		report: report,
		conns:  make(chan net.Conn),
		errs:   make(chan error, 1),
		closed: make(chan struct{}),
		done:   make(chan struct{}),
		// The loop also ends on Close, during a backoff; it overwrites this
		// only with inner's own closed error.
		doneErr: net.ErrClosed,
	}
	go l.acceptLoop()
	return l
}

type frameListener struct {
	inner  net.Listener
	report func(error)
	conns  chan net.Conn
	// errs holds at most one error inner's Accept returned, until an Accept
	// takes it.
	errs chan error

	closeOnce sync.Once
	closed    chan struct{}

	// done is closed when inner is closed, with doneErr the error it said so
	// with.
	done    chan struct{}
	doneErr error
}

// Backoff after an error from inner's Accept, as net/http's Serve does.
const (
	minAcceptBackoff = 5 * time.Millisecond
	maxAcceptBackoff = time.Second
)

func (l *frameListener) acceptLoop() {
	defer close(l.done)
	var backoff time.Duration
	for {
		conn, err := l.inner.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				l.doneErr = err
				return
			}
			select {
			case l.errs <- err:
			default:
			}
			backoff = min(max(2*backoff, minAcceptBackoff), maxAcceptBackoff)
			select {
			case <-time.After(backoff):
			case <-l.closed:
				return
			}
			continue
		}
		backoff = 0
		transport := &frameTransport{Conn: conn}
		session, err := yamux.Server(transport, frameConfig())
		if err != nil {
			_ = conn.Close()
			l.fail(err)
			continue
		}
		go l.deliver(session, transport)
	}
}

// deliver waits for the session's stream and hands it to Accept. The session
// is reading its connection all the while.
func (l *frameListener) deliver(session *yamux.Session, transport *frameTransport) {
	timer := time.AfterFunc(frameHandshakeTimeout, func() { _ = session.Close() })
	stream, err := session.AcceptStream()
	timer.Stop()
	if err != nil {
		_ = session.Close()
		l.fail(errors.Join(errors.New("vsock: peer did not open a framed stream"), err))
		return
	}
	conn, err := oneStream(session, stream, transport)
	if err != nil {
		l.fail(err)
		return
	}
	select {
	case l.conns <- conn:
	case <-l.closed:
		_ = conn.Close()
	}
}

func (l *frameListener) fail(err error) {
	if l.report != nil {
		l.report(err)
	}
}

func (l *frameListener) Accept() (net.Conn, error) {
	select {
	case <-l.closed:
		return nil, net.ErrClosed
	default:
	}
	select {
	case conn := <-l.conns:
		return conn, nil
	case err := <-l.errs:
		return nil, err
	case <-l.closed:
		return nil, net.ErrClosed
	case <-l.done:
		return nil, l.doneErr
	}
}

func (l *frameListener) Close() error {
	var err error
	l.closeOnce.Do(func() {
		close(l.closed)
		err = l.inner.Close()
	})
	return err
}

func (l *frameListener) Addr() net.Addr { return l.inner.Addr() }

// framedConn is one stream and the session that carries only it. CloseWrite
// half-closes it as TCP's does (ADR 26-11-18-067): the stream's end goes to the
// peer, which reads EOF after everything written before it, and this side
// keeps reading until the peer ends its own. Close ends both directions and
// the session.
type framedConn struct {
	stream    *yamux.Stream
	session   *yamux.Session
	transport *frameTransport

	writeMu       sync.Mutex
	writeTimedOut bool
}

// Read returns what the stream holds. After taking bytes, yamux sends the
// window update that lets the peer write more, and returns that send's error
// with the bytes: a peer that wrote its last bytes and hung up would turn a
// complete read into a failed one. The bytes are returned alone, and the next
// Read reports the stream as it then is.
func (c *framedConn) Read(b []byte) (int, error) {
	n, err := c.stream.Read(b)
	if n > 0 {
		return n, nil
	}
	return n, err
}

// Write accounts for a send whose result raced with yamux's session shutdown.
func (c *framedConn) Write(b []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	before := c.transport.written.Load()
	n, err := c.stream.Write(b)
	if errors.Is(err, yamux.ErrConnectionWriteTimeout) {
		// A timed-out frame can still be queued in yamux, so later raw
		// writes can include bytes from this call.
		c.writeTimedOut = true
	}
	if err == nil || c.writeTimedOut {
		return n, err
	}

	if errors.Is(err, yamux.ErrSessionShutdown) {
		// yamux publishes shutdown before closing the raw connection. Close
		// joins that shutdown, including the send loop and its final write.
		_ = c.session.Close()
		if c.transport.written.Load()-before == uint64(len(b)) {
			return len(b), nil
		}
	}
	return max(n, int(c.transport.written.Load()-before)), err
}

// frameTransport counts payload bytes accepted by the underlying connection.
// yamux v0.1.2's single send loop writes a 12-byte header and, for nonempty
// data frames, a separate body. Only our one stream sends data.
type frameTransport struct {
	net.Conn
	inBody  bool
	written atomic.Uint64
}

func (c *frameTransport) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if c.inBody {
		c.written.Add(uint64(n))
		c.inBody = false
	} else if err == nil {
		const (
			headerSize = 12
			typeData   = 0
		)
		c.inBody = len(b) == headerSize && b[1] == typeData && binary.BigEndian.Uint32(b[8:]) != 0
	}
	return n, err
}

// CloseWrite sends the stream's end, which the peer reads as EOF after
// everything written before it. Reads go on until the peer ends its side; a
// write after it fails.
func (c *framedConn) CloseWrite() error {
	err := c.stream.Close()
	if errors.Is(err, yamux.ErrSessionShutdown) {
		err = nil
	}
	return err
}

// Close sends the stream's end, which the peer reads as EOF after everything
// written before it, then ends the session and the connection under it.
func (c *framedConn) Close() error {
	err := c.stream.Close()
	if errors.Is(err, yamux.ErrSessionShutdown) {
		err = nil
	}
	return errors.Join(err, c.session.Close())
}

func (c *framedConn) LocalAddr() net.Addr                { return c.stream.LocalAddr() }
func (c *framedConn) RemoteAddr() net.Addr               { return c.stream.RemoteAddr() }
func (c *framedConn) SetDeadline(t time.Time) error      { return c.stream.SetDeadline(t) }
func (c *framedConn) SetReadDeadline(t time.Time) error  { return c.stream.SetReadDeadline(t) }
func (c *framedConn) SetWriteDeadline(t time.Time) error { return c.stream.SetWriteDeadline(t) }
