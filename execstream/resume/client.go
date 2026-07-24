package resume

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"time"

	"github.com/obot-platform/discobox/execstream"
	"github.com/obot-platform/discobox/execstream/frame"
)

type ConnectionState string

const (
	ConnectionReconnecting ConnectionState = "reconnecting"
	ConnectionReconnected  ConnectionState = "reconnected"
)

type Event struct {
	State ConnectionState
	Err   error
}

type Options struct {
	// Dial opens a replacement physical connection. Nil makes the connection
	// non-reconnecting while retaining positioned-action acknowledgement.
	Dial func(context.Context) (execstream.Conn, error)
	// Done reports whether the remote process has reached terminal state. It
	// prevents an ended process from turning into an infinite reconnect loop.
	Done func(context.Context) (bool, error)
	// Event receives transport lifecycle changes. They are deliberately separate
	// from terminal output.
	Event func(Event)
	// Timing enables transport-heartbeat and positioned-action RTT events. It is
	// disabled when Timing.Observe is nil.
	Timing TimingOptions
	// MaxPendingBytes bounds unacknowledged action payload retained client-side.
	// Zero uses DefaultMaxPendingBytes.
	MaxPendingBytes int
}

// DefaultMaxPendingBytes bounds accepted-but-unacknowledged input while still
// allowing several full 32 KiB terminal reads to be in flight.
const DefaultMaxPendingBytes = 256 * 1024

type pendingAction struct {
	position   uint64
	typ        byte
	wire       []byte
	size       int
	acceptedAt time.Time
}

// Conn is one logical resumable execstream connection. It decorates replaceable
// physical connections while preserving the ordinary execstream.Conn contract.
type Conn struct {
	ctx    context.Context
	cancel context.CancelFunc
	dial   func(context.Context) (execstream.Conn, error)
	done   func(context.Context) (bool, error)
	event  func(Event)
	timing timingConfig
	token  []byte

	mu              sync.Mutex
	conn            execstream.Conn
	connecting      execstream.Conn
	closed          bool
	terminalErr     error
	lastAck         uint64
	nextPosition    uint64
	pending         []pendingAction
	pendingBytes    int
	incoming        []frame.Frame
	maxPendingBytes int
	ready           bool
	resize          []byte
	changed         chan struct{}
	writeMu         sync.Mutex
	reconnectMu     sync.Mutex
	backoff         func(int) time.Duration
}

func New(ctx context.Context, initial execstream.Conn, opts Options) (*Conn, error) {
	if initial == nil {
		return nil, errors.New("initial exec stream connection is required")
	}
	timing, err := resolveTimingOptions(opts.Timing)
	if err != nil {
		return nil, err
	}
	token := make([]byte, tokenSize)
	if _, err := rand.Read(token); err != nil {
		return nil, fmt.Errorf("generate exec stream session token: %w", err)
	}
	maxPendingBytes := opts.MaxPendingBytes
	if maxPendingBytes == 0 {
		maxPendingBytes = DefaultMaxPendingBytes
	}
	if maxPendingBytes < actionHeaderLen {
		return nil, fmt.Errorf("max pending bytes must be at least %d", actionHeaderLen)
	}

	ctx, cancel := context.WithCancel(ctx)
	c := &Conn{
		ctx:             ctx,
		cancel:          cancel,
		dial:            opts.Dial,
		done:            opts.Done,
		event:           opts.Event,
		timing:          timing,
		token:           token,
		maxPendingBytes: maxPendingBytes,
		changed:         make(chan struct{}),
		backoff:         reconnectBackoff,
	}
	c.connecting = initial
	go func() {
		<-c.ctx.Done()
		_ = c.closeTransports()
	}()
	c.writeMu.Lock()
	err = c.establishLocked(initial)
	c.writeMu.Unlock()
	if err != nil {
		contextErr := c.ctx.Err()
		cancel()
		_ = c.closeTransports()
		if contextErr != nil {
			return nil, contextErr
		}
		return nil, err
	}
	if c.timing.observe != nil {
		go c.observeHeartbeats()
	}
	return c, nil
}

func reconnectBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := 100 * time.Millisecond
	for i := 1; i < attempt && delay < 5*time.Second; i++ {
		delay *= 2
	}
	if delay > 5*time.Second {
		return 5 * time.Second
	}
	return delay
}

func (c *Conn) ReadFrame() (frame.Frame, error) {
	var cause error
	for {
		if next, ok := c.popIncoming(); ok {
			return next, nil
		}
		conn, err := c.connection(cause)
		if err != nil {
			return frame.Frame{}, err
		}
		// Establishment can consume process output that raced ahead of a replay
		// acknowledgement. Prefer that retained output before reading the newly
		// established physical connection again.
		if next, ok := c.popIncoming(); ok {
			return next, nil
		}
		next, err := conn.ReadFrame()
		if err != nil {
			cause = err
			c.invalidate(conn)
			continue
		}
		switch next.Type {
		case frame.Ack:
			position, err := decodePosition(next.Payload)
			if err != nil {
				return frame.Frame{}, c.fail(err)
			}
			if err := c.acknowledge(position); err != nil {
				return frame.Frame{}, c.fail(err)
			}
			continue
		case frame.Session, frame.SessionOK, frame.Action:
			return frame.Frame{}, c.fail(fmt.Errorf("%w: unexpected host frame type %d", ErrProtocol, next.Type))
		default:
			return next, nil
		}
	}
}

func (c *Conn) WriteFrame(typ byte, payload []byte) error {
	switch typ {
	case frame.Input, frame.Signal, frame.CloseInput:
		return c.writeAction(typ, payload)
	case frame.Resize, frame.Ready:
		return c.writeState(typ, payload)
	default:
		return fmt.Errorf("%w: unsupported client frame type %d", ErrProtocol, typ)
	}
}

func (c *Conn) writeAction(typ byte, payload []byte) error {
	size := actionHeaderLen + len(payload)
	if size > c.maxPendingBytes {
		return fmt.Errorf("exec stream action is %d bytes, larger than pending buffer limit %d", size, c.maxPendingBytes)
	}

	for {
		if err := c.waitForSpace(size); err != nil {
			return err
		}

		c.writeMu.Lock()
		c.mu.Lock()
		if err := c.currentErrorLocked(); err != nil {
			c.mu.Unlock()
			c.writeMu.Unlock()
			return err
		}
		if c.pendingBytes+size > c.maxPendingBytes {
			c.mu.Unlock()
			c.writeMu.Unlock()
			continue
		}
		if c.nextPosition == math.MaxUint64 {
			c.mu.Unlock()
			c.writeMu.Unlock()
			return fmt.Errorf("%w: action position exhausted", ErrProtocol)
		}
		position := c.nextPosition + 1
		wire, err := encodeAction(position, typ, payload)
		if err != nil {
			c.mu.Unlock()
			c.writeMu.Unlock()
			return err
		}
		acceptedAt := c.timingNow()
		c.nextPosition = position
		c.pending = append(c.pending, pendingAction{
			position:   position,
			typ:        typ,
			wire:       wire,
			size:       size,
			acceptedAt: acceptedAt,
		})
		c.pendingBytes += size
		conn := c.conn
		c.mu.Unlock()

		if conn == nil {
			c.writeMu.Unlock()
			go func() { _ = c.reconnect(nil) }()
			return nil
		}
		err = conn.WriteFrame(frame.Action, wire)
		if err != nil {
			c.invalidate(conn)
			go func() { _ = c.reconnect(err) }()
		}
		c.writeMu.Unlock()
		// A failed physical write remains pending and will be retransmitted. From
		// the logical stream's perspective the action was accepted successfully.
		return nil
	}
}

func (c *Conn) writeState(typ byte, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	c.mu.Lock()
	if err := c.currentErrorLocked(); err != nil {
		c.mu.Unlock()
		return err
	}
	switch typ {
	case frame.Resize:
		c.resize = append(c.resize[:0], payload...)
	case frame.Ready:
		c.ready = true
	}
	conn := c.conn
	c.mu.Unlock()

	if conn == nil {
		go func() { _ = c.reconnect(nil) }()
		return nil
	}
	if err := conn.WriteFrame(typ, payload); err != nil {
		c.invalidate(conn)
		go func() { _ = c.reconnect(err) }()
	}
	// Resize and Ready are retained state and will be restored after reconnect.
	return nil
}

func (c *Conn) Close() error {
	c.cancel()
	return c.closeTransports()
}

func (c *Conn) closeTransports() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	conn := c.conn
	connecting := c.connecting
	c.conn = nil
	c.connecting = nil
	c.notifyLocked()
	c.mu.Unlock()
	var err error
	if conn != nil {
		err = conn.Close()
	}
	if connecting != nil {
		if closeErr := connecting.Close(); err == nil {
			err = closeErr
		}
	}
	return err
}

func (c *Conn) establishLocked(conn execstream.Conn) error {
	c.mu.Lock()
	firstAvailable := c.lastAck + 1
	c.mu.Unlock()
	request, err := encodeSession(c.token, firstAvailable)
	if err != nil {
		return err
	}
	if err := conn.WriteFrame(frame.Session, request); err != nil {
		return err
	}
	response, err := conn.ReadFrame()
	if err != nil {
		return err
	}
	if response.Type == frame.Error {
		message := string(response.Payload)
		if message == "" {
			message = "host rejected session"
		}
		return fmt.Errorf("%w: %s", ErrRejected, message)
	}
	if response.Type != frame.SessionOK {
		return fmt.Errorf("%w: session response type %d, want %d", ErrProtocol, response.Type, frame.SessionOK)
	}
	position, err := decodePosition(response.Payload)
	if err != nil {
		return err
	}
	c.mu.Lock()
	lastAck := c.lastAck
	c.mu.Unlock()
	if position < lastAck {
		return fmt.Errorf("%w: host position %d precedes discarded client position %d", ErrRejected, position, lastAck)
	}
	if err := c.acknowledge(position); err != nil {
		return err
	}

	c.mu.Lock()
	pending := append([]pendingAction(nil), c.pending...)
	resize := append([]byte(nil), c.resize...)
	ready := c.ready
	c.mu.Unlock()
	for _, action := range pending {
		if err := conn.WriteFrame(frame.Action, action.wire); err != nil {
			return err
		}
		if err := c.awaitAck(conn, action.position); err != nil {
			return err
		}
	}
	if len(resize) > 0 {
		if err := conn.WriteFrame(frame.Resize, resize); err != nil {
			return err
		}
	}
	if ready {
		if err := conn.WriteFrame(frame.Ready, nil); err != nil {
			return err
		}
	}

	c.mu.Lock()
	if err := c.currentErrorLocked(); err != nil {
		c.mu.Unlock()
		return err
	}
	c.conn = conn
	if c.connecting == conn {
		c.connecting = nil
	}
	c.notifyLocked()
	c.mu.Unlock()
	return nil
}

func (c *Conn) awaitAck(conn execstream.Conn, want uint64) error {
	for {
		next, err := conn.ReadFrame()
		if err != nil {
			return err
		}
		switch next.Type {
		case frame.Error:
			return fmt.Errorf("%w: %s", ErrRejected, string(next.Payload))
		case frame.Session, frame.SessionOK, frame.Action:
			return fmt.Errorf("%w: frame type %d while awaiting action acknowledgement", ErrProtocol, next.Type)
		case frame.Ack:
			// Handled below.
		default:
			c.mu.Lock()
			c.incoming = append(c.incoming, next)
			c.mu.Unlock()
			continue
		}
		position, err := decodePosition(next.Payload)
		if err != nil {
			return err
		}
		if err := c.acknowledge(position); err != nil {
			return err
		}
		if position >= want {
			return nil
		}
	}
}

func (c *Conn) popIncoming() (frame.Frame, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.incoming) == 0 {
		return frame.Frame{}, false
	}
	next := c.incoming[0]
	copy(c.incoming, c.incoming[1:])
	c.incoming = c.incoming[:len(c.incoming)-1]
	return next, true
}

func (c *Conn) acknowledge(position uint64) error {
	c.mu.Lock()
	if position < c.lastAck {
		c.mu.Unlock()
		return nil
	}
	if position > c.nextPosition {
		c.mu.Unlock()
		return fmt.Errorf("%w: host acknowledged position %d beyond sent position %d", ErrRejected, position, c.nextPosition)
	}
	c.lastAck = position
	remove := 0
	var acknowledged []pendingAction
	for remove < len(c.pending) && c.pending[remove].position <= position {
		if c.timing.observe != nil {
			acknowledged = append(acknowledged, c.pending[remove])
		}
		c.pendingBytes -= c.pending[remove].size
		remove++
	}
	if remove > 0 {
		copy(c.pending, c.pending[remove:])
		c.pending = c.pending[:len(c.pending)-remove]
		c.notifyLocked()
	}
	pendingBytes := c.pendingBytes
	c.mu.Unlock()
	acknowledgedAt := c.timingNow()
	for _, action := range acknowledged {
		c.emitTiming(TimingEvent{
			At:           acknowledgedAt,
			Source:       TimingActionAcknowledgement,
			RoundTrip:    acknowledgedAt.Sub(action.acceptedAt),
			Position:     action.position,
			ActionType:   action.typ,
			PayloadBytes: action.size - actionHeaderLen,
			PendingBytes: pendingBytes,
		})
	}
	return nil
}

func (c *Conn) timingNow() time.Time {
	if c.timing.observe == nil {
		return time.Time{}
	}
	return time.Now()
}

func (c *Conn) emitTiming(event TimingEvent) {
	if c.timing.observe == nil {
		return
	}
	event.Slow = event.Err != nil || event.RoundTrip >= c.timing.slowAfter
	c.timing.observe(event)
}

func (c *Conn) observeHeartbeats() {
	ticker := time.NewTicker(c.timing.heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
		}

		c.mu.Lock()
		conn := c.conn
		c.mu.Unlock()
		prober, ok := conn.(execstream.Prober)
		if !ok {
			continue
		}

		started := time.Now()
		probeCtx, cancel := context.WithTimeout(c.ctx, c.timing.heartbeatTimeout)
		err := prober.Probe(probeCtx)
		cancel()
		finished := time.Now()
		if c.ctx.Err() != nil {
			return
		}

		// A late result from the physical connection that was replaced while its
		// probe was in flight says nothing about the current attach.
		c.mu.Lock()
		current := c.conn == conn
		c.mu.Unlock()
		if !current {
			continue
		}
		c.emitTiming(TimingEvent{
			At:        finished,
			Source:    TimingHeartbeat,
			RoundTrip: finished.Sub(started),
			Err:       err,
		})
	}
}

func (c *Conn) waitForSpace(size int) error {
	for {
		c.mu.Lock()
		if err := c.currentErrorLocked(); err != nil {
			c.mu.Unlock()
			return err
		}
		if c.pendingBytes+size <= c.maxPendingBytes {
			c.mu.Unlock()
			return nil
		}
		changed := c.changed
		c.mu.Unlock()
		select {
		case <-c.ctx.Done():
			return netClosedError(c.ctx)
		case <-changed:
		}
	}
}

func (c *Conn) connection(cause error) (execstream.Conn, error) {
	c.mu.Lock()
	conn, terminalErr, closed := c.conn, c.terminalErr, c.closed
	c.mu.Unlock()
	if conn != nil {
		return conn, nil
	}
	if terminalErr != nil {
		return nil, terminalErr
	}
	if closed {
		return nil, netClosedError(c.ctx)
	}
	if err := c.reconnect(cause); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		return c.conn, nil
	}
	if c.terminalErr != nil {
		return nil, c.terminalErr
	}
	return nil, netClosedError(c.ctx)
}

func (c *Conn) reconnect(cause error) error {
	c.reconnectMu.Lock()
	defer c.reconnectMu.Unlock()

	c.mu.Lock()
	if c.conn != nil {
		c.mu.Unlock()
		return nil
	}
	if c.terminalErr != nil {
		err := c.terminalErr
		c.mu.Unlock()
		return err
	}
	if c.closed {
		c.mu.Unlock()
		return netClosedError(c.ctx)
	}
	c.mu.Unlock()
	if c.dial == nil {
		if cause == nil {
			cause = io.EOF
		}
		return c.setTerminalError(cause)
	}

	c.emit(Event{State: ConnectionReconnecting, Err: cause})
	for attempt := 1; ; attempt++ {
		if done, err := c.attachDone(); done {
			return c.setTerminalError(err)
		}
		if err := waitBackoff(c.ctx, c.backoff(attempt)); err != nil {
			return err
		}
		conn, err := c.dial(c.ctx)
		if err != nil {
			continue
		}
		c.mu.Lock()
		if err := c.currentErrorLocked(); err != nil {
			c.mu.Unlock()
			_ = conn.Close()
			return err
		}
		c.connecting = conn
		c.mu.Unlock()
		c.writeMu.Lock()
		err = c.establishLocked(conn)
		c.writeMu.Unlock()
		if err != nil {
			c.clearConnecting(conn)
			_ = conn.Close()
			if errors.Is(err, ErrRejected) || errors.Is(err, ErrProtocol) {
				return c.setTerminalError(err)
			}
			continue
		}
		c.emit(Event{State: ConnectionReconnected})
		return nil
	}
}

func (c *Conn) invalidate(conn execstream.Conn) {
	c.mu.Lock()
	if c.conn != conn {
		c.mu.Unlock()
		return
	}
	c.conn = nil
	c.notifyLocked()
	c.mu.Unlock()
	_ = conn.Close()
}

func (c *Conn) clearConnecting(conn execstream.Conn) {
	c.mu.Lock()
	if c.connecting == conn {
		c.connecting = nil
	}
	c.mu.Unlock()
}

func (c *Conn) fail(err error) error {
	c.mu.Lock()
	conn := c.conn
	c.conn = nil
	c.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	return c.setTerminalError(err)
}

func (c *Conn) attachDone() (bool, error) {
	if c.done == nil {
		return false, nil
	}
	return c.done(c.ctx)
}

func (c *Conn) setTerminalError(err error) error {
	if err == nil {
		err = io.EOF
	}
	c.mu.Lock()
	if c.terminalErr == nil {
		c.terminalErr = err
	}
	err = c.terminalErr
	c.notifyLocked()
	c.mu.Unlock()
	return err
}

func (c *Conn) currentErrorLocked() error {
	if c.terminalErr != nil {
		return c.terminalErr
	}
	if c.closed {
		return netClosedError(c.ctx)
	}
	return nil
}

func (c *Conn) notifyLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

func (c *Conn) emit(event Event) {
	if c.event != nil {
		c.event(event)
	}
}

func waitBackoff(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return netClosedError(ctx)
	case <-timer.C:
		return nil
	}
}

func netClosedError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New("exec stream connection closed")
}
