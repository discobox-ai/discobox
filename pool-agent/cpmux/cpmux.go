// Package cpmux multiplexes the control-plane channel over a single duplex
// byte stream.
//
// Some pool transports can only be dialed inward: a wslc guest is reached by
// spawning a process and relaying its stdio, and nothing in the guest can open
// a connection back to the host. That breaks the one hop that must run the
// other way — the pool agent registering with, and reporting to, the control
// plane.
//
// One multiplexed session over one such stream fixes both directions at once,
// because a stream multiplexer is symmetric: either end may open a stream. The
// host opens streams to reach the agent's API; the guest opens streams to reach
// the control plane. Neither side needs a listener the other can dial.
//
// Only control-plane traffic belongs here. Docker traffic keeps its own
// per-connection transport: it carries image loads and build contexts, and
// funneling that bulk through the same session would let it head-of-line block
// the agent's heartbeats.
//
// Half-close is preserved end to end. yamux's Stream.Close sends FIN and keeps
// reads working ("LocalClose only prohibits further local writes"), so it is
// exposed here as CloseWrite — the convention HTTP and the Docker API depend on.
package cpmux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/hashicorp/yamux"
)

// TargetControlPlane is the target a guest names when it opens a stream toward
// the control plane. Host-opened streams instead name a guest-side address,
// using the same "tcp:host:port" and "unix:/path" vocabulary as the bridge.
const TargetControlPlane = "control-plane"

// maxTargetLength bounds the per-stream header so a malformed peer cannot make
// the reader allocate without limit.
const maxTargetLength = 512

// Session is a multiplexed control-plane channel. Both ends may open streams.
type Session struct {
	inner *yamux.Session
}

// Client starts the session end that initiates the underlying connection. Use
// it on the host, which spawns the guest helper and owns its stdio.
func Client(conn net.Conn) (*Session, error) {
	inner, err := yamux.Client(conn, sessionConfig())
	if err != nil {
		return nil, fmt.Errorf("start control-plane mux client: %w", err)
	}
	return &Session{inner: inner}, nil
}

// Server starts the other end. Use it in the guest helper.
func Server(conn net.Conn) (*Session, error) {
	inner, err := yamux.Server(conn, sessionConfig())
	if err != nil {
		return nil, fmt.Errorf("start control-plane mux server: %w", err)
	}
	return &Session{inner: inner}, nil
}

func sessionConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	// The underlying transport is a relayed pipe with no independent liveness
	// signal, so the session's own keepalive is what detects a dead guest.
	cfg.EnableKeepAlive = true
	cfg.KeepAliveInterval = 30 * time.Second
	// yamux logs to stderr by default, which would interleave with the guest
	// helper's own output.
	cfg.LogOutput = io.Discard
	return cfg
}

// Dial opens a stream and asks the far side to connect it to target.
func (s *Session) Dial(ctx context.Context, target string) (net.Conn, error) {
	if strings.TrimSpace(target) == "" {
		return nil, errors.New("cpmux: target is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := s.inner.OpenStream()
	if err != nil {
		return nil, fmt.Errorf("open control-plane stream: %w", err)
	}
	conn := &streamConn{Stream: raw}
	if err := writeTarget(conn, target); err != nil {
		_ = raw.Close()
		return nil, err
	}
	return conn, nil
}

// Accept returns the next stream the far side opened, together with the target
// it named.
func (s *Session) Accept() (net.Conn, string, error) {
	raw, err := s.inner.AcceptStream()
	if err != nil {
		return nil, "", err
	}
	conn := &streamConn{Stream: raw}
	target, err := readTarget(conn)
	if err != nil {
		_ = raw.Close()
		return nil, "", err
	}
	return conn, target, nil
}

// Close tears down the session and every stream on it.
func (s *Session) Close() error { return s.inner.Close() }

// Closed reports whether the session has gone away, so a supervisor can restart
// the guest helper.
func (s *Session) Closed() bool { return s.inner.IsClosed() }

// Dialer connects to a target named by the peer.
type Dialer func(ctx context.Context, target string) (net.Conn, error)

// Serve accepts streams and splices each one to whatever the peer asked for.
// It is the guest helper's main loop, and also the host's loop for the streams a
// guest opens.
func Serve(ctx context.Context, session *Session, dial Dialer) error {
	for {
		conn, target, err := session.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		go func() {
			defer func() { _ = conn.Close() }()
			peer, err := dial(ctx, target)
			if err != nil {
				return
			}
			defer func() { _ = peer.Close() }()
			Splice(conn, peer)
		}()
	}
}

// Splice copies between two connections until both directions finish,
// propagating each side's end-of-stream as a half-close rather than tearing the
// whole connection down. A one-shot request that writes, half-closes, and reads
// to EOF depends on this.
func Splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(b, a)
		closeWrite(b)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(a, b)
		closeWrite(a)
		done <- struct{}{}
	}()
	<-done
	<-done
}

func closeWrite(conn net.Conn) {
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = conn.Close()
}

// writeTarget sends the stream header: the target followed by a newline.
func writeTarget(w io.Writer, target string) error {
	if len(target) > maxTargetLength {
		return fmt.Errorf("cpmux: target %q exceeds %d bytes", target[:32], maxTargetLength)
	}
	if strings.ContainsRune(target, '\n') {
		return errors.New("cpmux: target must not contain a newline")
	}
	if _, err := io.WriteString(w, target+"\n"); err != nil {
		return fmt.Errorf("write stream target: %w", err)
	}
	return nil
}

// readTarget reads the stream header written by writeTarget.
//
// It reads one byte at a time on purpose. A buffered reader would read past the
// newline and swallow the first bytes of the payload, which the caller then
// reads directly from the stream — a corruption that only shows up under load,
// when the peer's header and first write land in the same segment.
func readTarget(r io.Reader) (string, error) {
	var builder strings.Builder
	buf := make([]byte, 1)
	for builder.Len() <= maxTargetLength {
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", fmt.Errorf("read stream target: %w", err)
		}
		if buf[0] == '\n' {
			target := builder.String()
			if strings.TrimSpace(target) == "" {
				return "", errors.New("cpmux: peer sent an empty stream target")
			}
			return target, nil
		}
		builder.WriteByte(buf[0])
	}
	return "", fmt.Errorf("cpmux: stream target exceeds %d bytes", maxTargetLength)
}

// streamConn adapts a yamux stream to the half-close convention. yamux's Close
// already means "no more writes from this side, reads continue", which is what
// CloseWrite means everywhere else; exposing it under that name lets Splice and
// net/http treat these streams like any other connection.
type streamConn struct {
	*yamux.Stream
}

func (c *streamConn) CloseWrite() error {
	// The explicit selector is deliberate: c.Close() would resolve to the same
	// promoted method but read like recursion into CloseWrite.
	return c.Stream.Close() //nolint:staticcheck // QF1008: see above.
}
