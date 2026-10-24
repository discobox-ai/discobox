// Package wstunnel carries byte streams over WebSocket, so a sandbox can reach
// its pool through an HTTPS edge that terminates TLS and forwards only HTTP
// (ADR 26-10-23-719).
//
// A tunnel is one WebSocket on one TCP connection, and it carries one stream at
// a time: never two at once, so streams never share a congestion window. A
// stream's bytes travel in binary messages, and a stream ends with text control
// messages rather than with the WebSocket's close, so the tunnel outlives it and
// carries the next:
//
//   - The side that closes its stream sends "end". The peer reads it as EOF.
//   - The peer, once it has closed its own end, answers "end-ack". Both sides
//     then know the stream is drained and the tunnel is idle. Two sides that
//     close at once each send "end", and each takes the other's as the answer.
//   - Anything else — a read or write error, a deadline cut short mid-write, a
//     WebSocket close, or a message the protocol does not allow — closes the
//     whole tunnel, and it is never reused.
//
// A Client keeps the tunnels whose streams ended cleanly and starts the next
// stream on one of them. A Handler serves tunnels and hands each stream they
// carry to a function that owns it. The package knows nothing of TLS: what
// rides a stream — for a sandbox, its mTLS to the pool — is the caller's.
package wstunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// Subprotocol names this protocol in the WebSocket handshake. Both sides
// require it, so a tunnel never runs against something that speaks another.
const Subprotocol = "discobox-tunnel.v1"

const (
	msgEnd    = "end"
	msgEndAck = "end-ack"

	// maxChunk is the most one binary message carries; a larger write is
	// split. maxMessage is the read limit, with room above it.
	maxChunk   = 32 << 10
	maxMessage = 64 << 10
)

// The tunnel limits (ADR 26-10-23-719 §3). They are kept under what the edges
// a pool sits behind allow: an idle connection is commonly cut at 60 seconds,
// and the strictest cap on one request is an hour. They are variables so tests
// can shorten them.
var (
	// idleTimeout is how long a client keeps a tunnel with no stream on it.
	idleTimeout = 30 * time.Second
	// maxAge is how long a client keeps reusing a tunnel. One older than this
	// is retired when its stream ends, rather than going idle, so the edge only
	// ever cuts a tunnel this side had finished with.
	maxAge = 50 * time.Minute
	// maxIdle is how many idle tunnels a client keeps.
	maxIdle = 16
	// pingInterval is how often a client pings a tunnel carrying a stream, so a
	// quiet stream is not cut as idle.
	pingInterval = 20 * time.Second
	// serverIdleTimeout is how long a server keeps a tunnel with no stream on
	// it: longer than the client's, so a client always retires its own.
	serverIdleTimeout = 90 * time.Second
	// firstStreamTimeout is how long a server keeps a new tunnel before its
	// first stream begins. A client opens a tunnel to send on it at once, so
	// only something holding tunnels open for nothing waits longer.
	firstStreamTimeout = 10 * time.Second
	// closeWait bounds what Close may spend on the peer: waiting for a write
	// already under way, and writing the stream's end. Past it the tunnel is
	// given up, which ends the stream anyway.
	closeWait = 5 * time.Second
	// endWait is how long a side that sent "end" waits for the peer's answer
	// before it gives up on the tunnel, so a peer that never answers holds it
	// for a bounded time.
	endWait = 30 * time.Second
)

// limits are the tunnel limits as they were when a Client or Handler was made,
// which every tunnel it makes keeps.
type limits struct {
	idleTimeout, maxAge, pingInterval, serverIdleTimeout, firstStreamTimeout, closeWait, endWait time.Duration
	maxIdle                                                                                      int
}

func currentLimits() limits {
	return limits{
		idleTimeout:        idleTimeout,
		maxAge:             maxAge,
		pingInterval:       pingInterval,
		serverIdleTimeout:  serverIdleTimeout,
		firstStreamTimeout: firstStreamTimeout,
		closeWait:          closeWait,
		endWait:            endWait,
		maxIdle:            maxIdle,
	}
}

// errProtocol reports a message the protocol does not allow; the tunnel that
// received it is closed.
var errProtocol = errors.New("wstunnel: protocol violation")

// errRetired is the reason a tunnel this side finished with stopped.
var errRetired = errors.New("wstunnel: tunnel retired")

// errIdle is the reason a server stopped a tunnel that carried no stream for
// serverIdleTimeout, and errUnanswered the reason a side stopped one whose
// peer did not answer its end within endWait.
var (
	errIdle       = errors.New("wstunnel: tunnel idle")
	errUnanswered = errors.New("wstunnel: end not answered")
)

type tunnel struct {
	limits limits
	ws     *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc
	server bool
	born   time.Time
	local  net.Addr
	remote net.Addr

	// wmu is held for every message a stream writes, so "end" can never pass
	// data the stream wrote before it, nor the next stream's data pass "end".
	wmu chan struct{}

	// onStream hands a server a stream the client began. onIdle hands a
	// client back a tunnel whose stream ended cleanly. onDead tells a client a
	// tunnel stopped.
	onStream func(*Stream)
	onIdle   func(*tunnel)
	onDead   func(*tunnel)

	mu   sync.Mutex
	cur  *Stream
	err  error
	dead chan struct{}
	// idleGen counts the server's idle periods, so an idle timer armed for
	// one that a stream has since ended does nothing.
	idleGen uint64

	// idleTimer is the client's, guarded by the client's lock.
	idleTimer *time.Timer
}

func newTunnel(ctx context.Context, lim limits, ws *websocket.Conn, server bool, local, remote net.Addr) *tunnel {
	ws.SetReadLimit(maxMessage)
	ctx, cancel := context.WithCancel(ctx)
	return &tunnel{
		limits: lim,
		ws:     ws,
		ctx:    ctx,
		cancel: cancel,
		server: server,
		born:   time.Now(),
		local:  local,
		remote: remote,
		wmu:    make(chan struct{}, 1),
		dead:   make(chan struct{}),
	}
}

// run reads the tunnel's messages until it stops.
func (t *tunnel) run() {
	for {
		typ, data, err := t.ws.Read(t.ctx)
		if err != nil {
			t.fail(err)
			return
		}
		if err := t.receive(typ, data); err != nil {
			t.fail(err)
			return
		}
	}
}

func (t *tunnel) idle() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cur == nil
}

func (t *tunnel) receive(typ websocket.MessageType, data []byte) error {
	t.mu.Lock()
	s := t.cur
	if typ == websocket.MessageBinary {
		fresh := false
		if s == nil {
			if !t.server {
				t.mu.Unlock()
				return fmt.Errorf("%w: data on an idle tunnel", errProtocol)
			}
			// The client begins a stream by sending on it.
			s = t.newStream(false)
			fresh = true
		}
		if s.remoteEnded {
			t.mu.Unlock()
			return fmt.Errorf("%w: data after end", errProtocol)
		}
		discard := s.localClosed
		t.mu.Unlock()
		if fresh {
			t.onStream(s)
		}
		if !discard {
			s.deliver(data)
		}
		return nil
	}
	switch string(data) {
	case msgEnd:
		if s == nil {
			t.mu.Unlock()
			if !t.server {
				return fmt.Errorf("%w: end on an idle tunnel", errProtocol)
			}
			// A stream that ended before it sent anything: there is nothing
			// to serve, only the end to answer.
			return t.writeControl(msgEndAck)
		}
		if s.remoteEnded {
			t.mu.Unlock()
			return fmt.Errorf("%w: a second end", errProtocol)
		}
		s.remoteEnded = true
		// This side's own end already went out: the peer's is its answer.
		done := s.localClosed
		if done {
			t.cur = nil
		}
		t.mu.Unlock()
		close(s.in)
		if done {
			t.completed(s)
		}
		return nil
	case msgEndAck:
		if s == nil || !s.localClosed || s.remoteEnded {
			t.mu.Unlock()
			return fmt.Errorf("%w: end-ack without an end", errProtocol)
		}
		s.remoteEnded = true
		t.cur = nil
		t.mu.Unlock()
		close(s.in)
		t.completed(s)
		return nil
	default:
		t.mu.Unlock()
		return fmt.Errorf("%w: unknown control message %q", errProtocol, data)
	}
}

// writeControl sends a control message under the write lock.
func (t *tunnel) writeControl(msg string) error {
	select {
	case t.wmu <- struct{}{}:
	case <-t.dead:
		return t.failure()
	}
	defer t.release()
	return t.sendControl(msg)
}

// sendControl writes a control message, giving up on the tunnel when the peer
// does not take it within closeWait. The caller holds the write lock.
func (t *tunnel) sendControl(msg string) error {
	ctx, cancel := context.WithTimeout(t.ctx, t.limits.closeWait)
	defer cancel()
	return t.ws.Write(ctx, websocket.MessageText, []byte(msg))
}

func (t *tunnel) release() { <-t.wmu }

// completed is called once s has ended on both sides, and the tunnel is idle.
func (t *tunnel) completed(s *Stream) {
	close(s.done)
	if t.server {
		t.armIdle(t.limits.serverIdleTimeout)
	}
	if t.onIdle != nil {
		t.onIdle(t)
	}
}

// armIdle starts a server tunnel's idle period: it is stopped if no stream
// begins on it within wait. A timer rather than a deadline on the read, since
// the read that will see the next stream may already be waiting.
func (t *tunnel) armIdle(wait time.Duration) {
	t.mu.Lock()
	t.idleGen++
	gen := t.idleGen
	t.mu.Unlock()
	time.AfterFunc(wait, func() {
		t.mu.Lock()
		idle := t.cur == nil && t.idleGen == gen
		t.mu.Unlock()
		if idle {
			t.fail(errIdle)
		}
	})
}

// start begins a stream on an idle client tunnel, or reports that it cannot.
func (t *tunnel) start(reused bool) *Stream {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.err != nil || t.cur != nil {
		return nil
	}
	return t.newStream(reused)
}

// newStream makes s the tunnel's stream. The caller holds t.mu.
func (t *tunnel) newStream(reused bool) *Stream {
	s := &Stream{
		t:             t,
		reused:        reused,
		in:            make(chan []byte, 16),
		closed:        make(chan struct{}),
		done:          make(chan struct{}),
		readDeadline:  makeDeadline(),
		writeDeadline: makeDeadline(),
	}
	t.cur = s
	t.idleGen++
	return s
}

// fail stops the tunnel at once, for err.
func (t *tunnel) fail(err error) {
	if !t.stop(err) {
		return
	}
	t.cancel()
	_ = t.ws.CloseNow()
}

// retire stops a tunnel this side has finished with, closing the WebSocket
// cleanly so the peer does not take it for a failure.
func (t *tunnel) retire() {
	if !t.stop(errRetired) {
		return
	}
	go func() {
		_ = t.ws.Close(websocket.StatusNormalClosure, "")
		t.cancel()
	}()
}

// stop records why the tunnel stopped, the first time it does.
func (t *tunnel) stop(err error) bool {
	t.mu.Lock()
	if t.err != nil {
		t.mu.Unlock()
		return false
	}
	t.err = err
	t.mu.Unlock()
	close(t.dead)
	if t.onDead != nil {
		t.onDead(t)
	}
	return true
}

func (t *tunnel) failure() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return fmt.Errorf("wstunnel: tunnel closed: %w", t.err)
}

// keepalive pings the tunnel while it carries a stream, until it stops.
func (t *tunnel) keepalive() {
	ticker := time.NewTicker(t.limits.pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-t.dead:
			return
		case <-ticker.C:
		}
		if t.idle() {
			continue
		}
		// A ping that gets no pong is not a failure: a reader that is not
		// reading holds the pong behind its data. The ping has already gone
		// out, which is all the edge needs to see.
		ctx, cancel := context.WithTimeout(t.ctx, t.limits.pingInterval)
		_ = t.ws.Ping(ctx)
		cancel()
	}
}

// Stream is one byte stream on a tunnel. It is a net.Conn whose Close ends the
// stream, leaving the tunnel to carry the next one.
type Stream struct {
	t      *tunnel
	reused bool

	in     chan []byte
	buf    []byte
	closed chan struct{}
	// done is closed once the stream has ended on both sides.
	done chan struct{}
	once sync.Once

	// Guarded by t.mu.
	localClosed bool
	remoteEnded bool

	readDeadline  deadline
	writeDeadline deadline
}

var _ net.Conn = (*Stream)(nil)

// Reused reports whether the stream runs on a tunnel an earlier stream used.
// The edge may have cut such a tunnel while it sat idle without either side
// noticing, which shows only once something is read from the stream.
func (s *Stream) Reused() bool { return s.reused }

func (s *Stream) deliver(data []byte) {
	select {
	case s.in <- data:
	case <-s.closed:
	case <-s.t.ctx.Done():
	}
}

func (s *Stream) Read(p []byte) (int, error) {
	if len(s.buf) == 0 {
		select {
		case <-s.closed:
			return 0, net.ErrClosed
		default:
		}
		// What already arrived is read before a failure is reported.
		select {
		case data, ok := <-s.in:
			if !ok {
				return 0, io.EOF
			}
			s.buf = data
		default:
			select {
			case data, ok := <-s.in:
				if !ok {
					return 0, io.EOF
				}
				s.buf = data
			case <-s.closed:
				return 0, net.ErrClosed
			case <-s.t.dead:
				return 0, s.t.failure()
			case <-s.readDeadline.wait():
				return 0, os.ErrDeadlineExceeded
			}
		}
	}
	n := copy(p, s.buf)
	s.buf = s.buf[n:]
	return n, nil
}

func (s *Stream) Write(p []byte) (int, error) {
	select {
	case s.t.wmu <- struct{}{}:
	case <-s.closed:
		return 0, net.ErrClosed
	case <-s.t.dead:
		return 0, s.t.failure()
	case <-s.writeDeadline.wait():
		return 0, os.ErrDeadlineExceeded
	}
	defer s.t.release()
	written := 0
	for len(p) > 0 {
		select {
		case <-s.closed:
			return written, net.ErrClosed
		default:
		}
		chunk := p[:min(len(p), maxChunk)]
		if err := s.writeChunk(chunk); err != nil {
			return written, err
		}
		written += len(chunk)
		p = p[len(chunk):]
	}
	return written, nil
}

// writeChunk writes one binary message. The write deadline applies to it while
// it is under way, as net.Conn's does to pending I/O, including a deadline set
// after it began. A write cut short leaves a partial message on the wire, so
// the tunnel cannot carry anything after it and is given up.
func (s *Stream) writeChunk(chunk []byte) error {
	ctx, cancel := context.WithCancel(s.t.ctx)
	var expired atomic.Bool
	stop := make(chan struct{})
	go func() {
		select {
		case <-s.writeDeadline.wait():
			expired.Store(true)
			cancel()
		case <-stop:
		}
	}()
	err := s.t.ws.Write(ctx, websocket.MessageBinary, chunk)
	close(stop)
	cancel()
	if err == nil {
		return nil
	}
	s.t.fail(err)
	if expired.Load() {
		return os.ErrDeadlineExceeded
	}
	return err
}

// Close ends the stream: it sends "end", or "end-ack" when the peer ended
// first. It waits on the peer for at most closeWait, and never for its answer:
// the tunnel finishes the exchange, and gives up on itself if the answer does
// not come within endWait.
func (s *Stream) Close() error {
	s.once.Do(func() {
		close(s.closed)
		t := s.t
		timer := time.NewTimer(t.limits.closeWait)
		defer timer.Stop()
		select {
		case t.wmu <- struct{}{}:
		case <-t.dead:
			return
		case <-timer.C:
			// A write the peer is not reading holds the lock. Ending the
			// stream would mean waiting on the peer; the tunnel is given up
			// instead, which ends the write too.
			t.fail(errors.New("wstunnel: stream closed while a write was blocked"))
			return
		}
		defer t.release()
		t.mu.Lock()
		s.localClosed = true
		ack := s.remoteEnded
		if ack {
			t.cur = nil
		}
		t.mu.Unlock()
		msg := msgEnd
		if ack {
			msg = msgEndAck
		}
		if err := t.sendControl(msg); err != nil {
			t.fail(err)
			return
		}
		if ack {
			t.completed(s)
			return
		}
		time.AfterFunc(t.limits.endWait, func() {
			select {
			case <-s.done:
			default:
				t.fail(errUnanswered)
			}
		})
	})
	return nil
}

func (s *Stream) LocalAddr() net.Addr  { return s.t.local }
func (s *Stream) RemoteAddr() net.Addr { return s.t.remote }

func (s *Stream) SetDeadline(t time.Time) error {
	_ = s.SetReadDeadline(t)
	return s.SetWriteDeadline(t)
}

func (s *Stream) SetReadDeadline(t time.Time) error {
	s.readDeadline.set(t)
	return nil
}

func (s *Stream) SetWriteDeadline(t time.Time) error {
	s.writeDeadline.set(t)
	return nil
}

// addr is a tunnel end's address: the URL a client dialed, or the address an
// upgrade arrived from.
type addr string

func (a addr) Network() string { return "wss" }
func (a addr) String() string  { return string(a) }

// deadline is a resettable deadline, as net.Pipe keeps one: wait's channel is
// closed once the deadline passes.
type deadline struct {
	mu     sync.Mutex
	timer  *time.Timer
	cancel chan struct{}
}

func makeDeadline() deadline {
	return deadline{cancel: make(chan struct{})}
}

func (d *deadline) set(t time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.timer != nil && !d.timer.Stop() {
		<-d.cancel // the timer fired; wait for it to close the channel
	}
	d.timer = nil
	closed := isClosed(d.cancel)
	if t.IsZero() {
		if closed {
			d.cancel = make(chan struct{})
		}
		return
	}
	if wait := time.Until(t); wait > 0 {
		if closed {
			d.cancel = make(chan struct{})
		}
		cancel := d.cancel
		d.timer = time.AfterFunc(wait, func() { close(cancel) })
		return
	}
	if !closed {
		close(d.cancel)
	}
}

func (d *deadline) wait() chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cancel
}

func isClosed(c chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}
