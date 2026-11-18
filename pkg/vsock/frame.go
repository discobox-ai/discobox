package vsock

import (
	"errors"
	"io"
	"net"
	"strings"
	"sync"
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
// a further stream from the peer is reset rather than buffered. No keepalive:
// a connection is as alive as the stream it carries, as an unframed one is.
func frameConfig() *yamux.Config {
	config := yamux.DefaultConfig()
	config.AcceptBacklog = 1
	config.EnableKeepAlive = false
	config.MaxStreamWindowSize = FrameWindow
	config.LogOutput = io.Discard
	return config
}

// FrameDialed frames a connection this side opened: it starts the session,
// whose receive loop reads conn from now on, and opens the one stream. Closing
// the returned connection closes conn.
func FrameDialed(conn net.Conn) (net.Conn, error) {
	session, err := yamux.Client(conn, frameConfig())
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	stream, err := session.OpenStream()
	if err != nil {
		_ = session.Close()
		return nil, err
	}
	return oneStream(session, stream)
}

// oneStream closes the session to any stream but stream, and returns the two
// as a connection.
func oneStream(session *yamux.Session, stream *yamux.Stream) (net.Conn, error) {
	conn := &framedConn{stream: stream, session: session}
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
		session, err := yamux.Server(conn, frameConfig())
		if err != nil {
			_ = conn.Close()
			l.fail(err)
			continue
		}
		go l.deliver(session)
	}
}

// deliver waits for the session's stream and hands it to Accept. The session
// is reading its connection all the while.
func (l *frameListener) deliver(session *yamux.Session) {
	timer := time.AfterFunc(frameHandshakeTimeout, func() { _ = session.Close() })
	stream, err := session.AcceptStream()
	timer.Stop()
	if err != nil {
		_ = session.Close()
		l.fail(errors.Join(errors.New("vsock: peer did not open a framed stream"), err))
		return
	}
	conn, err := oneStream(session, stream)
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

// framedConn is one stream and the session that carries only it. It is a
// net.Conn and nothing more: it has no CloseWrite, because closing either side
// ends the session, and a caller that found one would believe the peer saw a
// half-close.
type framedConn struct {
	stream  *yamux.Stream
	session *yamux.Session
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

func (c *framedConn) Write(b []byte) (int, error) { return c.stream.Write(b) }

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
