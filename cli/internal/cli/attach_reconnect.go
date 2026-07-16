package cli

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

type attachConnectionState string

const (
	attachConnectionReconnecting attachConnectionState = "reconnecting"
	attachConnectionReconnected  attachConnectionState = "reconnected"
)

type attachConnectionEvent struct {
	State attachConnectionState
	Err   error
}

type reconnectingAttachFrames struct {
	ctx    context.Context
	cancel context.CancelFunc
	dial   func(context.Context) (io.ReadWriteCloser, error)
	done   func(context.Context) (bool, error)
	event  func(attachConnectionEvent)

	mu          sync.Mutex
	conn        io.ReadWriteCloser
	closed      bool
	terminalErr error
	ready       bool
	resize      []byte

	writeMu     sync.Mutex
	reconnectMu sync.Mutex
	backoff     func(int) time.Duration
}

func newReconnectingAttachFrames(
	ctx context.Context,
	conn io.ReadWriteCloser,
	dial func(context.Context) (io.ReadWriteCloser, error),
	done func(context.Context) (bool, error),
	event func(attachConnectionEvent),
) *reconnectingAttachFrames {
	ctx, cancel := context.WithCancel(ctx)
	return &reconnectingAttachFrames{
		ctx:     ctx,
		cancel:  cancel,
		conn:    conn,
		dial:    dial,
		done:    done,
		event:   event,
		backoff: attachReconnectBackoff,
	}
}

func attachReconnectBackoff(attempt int) time.Duration {
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

func (c *reconnectingAttachFrames) ReadFrame() (terminalFrame, error) {
	var cause error
	for {
		conn, err := c.connection(cause)
		if err != nil {
			return terminalFrame{}, err
		}
		frame, err := readTerminalFrame(conn)
		if err == nil {
			return frame, nil
		}
		cause = err
		c.invalidate(conn)
	}
}

func (c *reconnectingAttachFrames) WriteFrame(typ byte, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	c.mu.Lock()
	switch typ {
	case attachFrameReady:
		c.ready = true
	case attachFrameResize:
		c.resize = append(c.resize[:0], payload...)
	}
	conn := c.conn
	closed := c.closed
	c.mu.Unlock()

	if closed {
		return netClosedError(c.ctx)
	}
	if conn == nil {
		// Input, resize, and signal frames are intentionally dropped while the
		// transport is reconnecting. In particular, keyboard input is never queued
		// and replayed into the terminal after a reconnect.
		return nil
	}
	if err := writeTerminalFrame(conn, typ, payload); err != nil {
		c.invalidate(conn)
		go func() { _ = c.reconnect(err) }()
		return nil
	}
	return nil
}

func (c *reconnectingAttachFrames) Close() error {
	c.cancel()
	c.mu.Lock()
	c.closed = true
	conn := c.conn
	c.conn = nil
	c.mu.Unlock()
	if conn != nil {
		return conn.Close()
	}
	return nil
}

func (c *reconnectingAttachFrames) connection(cause error) (io.ReadWriteCloser, error) {
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

func (c *reconnectingAttachFrames) reconnect(cause error) error {
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

	c.emit(attachConnectionEvent{State: attachConnectionReconnecting, Err: cause})
	for attempt := 1; ; attempt++ {
		if done, err := c.attachDone(); done {
			return c.setTerminalError(err)
		}
		if err := waitAttachBackoff(c.ctx, c.backoff(attempt)); err != nil {
			return err
		}
		conn, err := c.dial(c.ctx)
		if err != nil {
			continue
		}
		if err := c.restore(conn); err != nil {
			_ = conn.Close()
			continue
		}

		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			_ = conn.Close()
			return netClosedError(c.ctx)
		}
		c.conn = conn
		c.mu.Unlock()
		c.emit(attachConnectionEvent{State: attachConnectionReconnected})
		return nil
	}
}

func (c *reconnectingAttachFrames) restore(conn io.ReadWriteCloser) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	resize := append([]byte(nil), c.resize...)
	ready := c.ready
	c.mu.Unlock()
	if len(resize) > 0 {
		if err := writeTerminalFrame(conn, attachFrameResize, resize); err != nil {
			return err
		}
	}
	if ready {
		return writeTerminalFrame(conn, attachFrameReady, nil)
	}
	return nil
}

func (c *reconnectingAttachFrames) invalidate(conn io.ReadWriteCloser) {
	c.mu.Lock()
	if c.conn != conn {
		c.mu.Unlock()
		return
	}
	c.conn = nil
	c.mu.Unlock()
	_ = conn.Close()
}

func (c *reconnectingAttachFrames) attachDone() (bool, error) {
	if c.done == nil {
		return false, nil
	}
	return c.done(c.ctx)
}

func (c *reconnectingAttachFrames) setTerminalError(err error) error {
	if err == nil {
		err = io.EOF
	}
	c.mu.Lock()
	c.terminalErr = err
	c.mu.Unlock()
	return err
}

func (c *reconnectingAttachFrames) emit(event attachConnectionEvent) {
	if c.event != nil {
		c.event(event)
	}
}

func waitAttachBackoff(ctx context.Context, delay time.Duration) error {
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
	return errors.New("attach connection closed")
}
