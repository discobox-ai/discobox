// Command discobox-cp-relay is the guest half of the control-plane channel.
//
// It runs in a pool guest's root namespace with its stdin and stdout wired to
// the host, and multiplexes that single duplex stream (see pool-agent/cpmux) so
// both sides can open connections:
//
//   - streams the host opens name a guest address ("tcp:host:port",
//     "unix:/path"); the relay dials it and splices;
//   - connections accepted on the local socket become streams toward the
//     control plane, which is the direction the guest transport cannot dial.
//
// The socket is shared with the pool-agent container, so the agent reaches the
// control plane with an ordinary unix:// URL and needs no transport-specific
// code of its own.
//
// --dial is the same program in one-shot form: dial one guest address and
// splice this process's own stdio to it, no multiplexing. That is what the
// host uses for Docker traffic, which carries image loads and build contexts
// and would head-of-line block the agent behind the single muxed stream. One
// binary covers both because the guest dials the same way either way; the
// alternative was a second program in the guest, and the only one available
// there was a C file compiled on first use by the guest's own dockerd.
//
// Usage:
//
//	discobox-cp-relay --socket /run/discobox/cp.sock
//	discobox-cp-relay --dial unix:/var/run/docker.sock
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/discobox-ai/discobox/pool-agent/cpmux"
)

func main() {
	socket := flag.String("socket", "/run/discobox/cp.sock", "unix socket the pool agent dials for the control plane")
	dial := flag.String("dial", "", "splice this process's stdio to one guest address (\"unix:/path\", \"tcp:host:port\") and exit, instead of serving the control plane")
	flag.Parse()

	// The two modes are exclusive: --dial is one connection's worth of work,
	// not a relay that also serves a socket.
	var err error
	if *dial != "" {
		err = dialOnce(*dial)
	} else {
		err = run(*socket)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "discobox-cp-relay:", err)
		os.Exit(1)
	}
}

// dialOnce is the bridge mode: one guest connection, spliced to this process's
// stdio, which the host holds as a net.Conn of its own.
//
// Splice is what carries the half-closes, and both of them matter here. The
// host closing its write half has to reach the target as a real shutdown, or a
// server waiting for the end of a request never answers; the target closing has
// to reach the host as EOF, or a caller that wrote a request and is reading the
// response to completion waits forever. The stdio side of that is stdioConn's
// CloseWrite, which closes stdout alone and leaves stdin readable.
func dialOnce(target string) error {
	conn, err := dialGuest(context.Background(), target)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	stdio := newStdioConn()
	defer func() { _ = stdio.Close() }()

	cpmux.Splice(stdio, conn)
	return nil
}

func run(socket string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	session, err := cpmux.Server(newStdioConn())
	if err != nil {
		return err
	}
	defer func() { _ = session.Close() }()

	listener, err := listenSocket(socket)
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()

	go func() {
		<-ctx.Done()
		_ = listener.Close()
		_ = session.Close()
	}()

	// Local connections become control-plane streams.
	go acceptLocal(ctx, listener, session)

	// Host-opened streams are dialed inside the guest.
	return cpmux.Serve(ctx, session, dialGuest)
}

func listenSocket(socket string) (net.Listener, error) {
	socket = strings.TrimSpace(socket)
	if !filepath.IsAbs(socket) {
		return nil, fmt.Errorf("socket path %q must be absolute", socket)
	}
	if err := os.MkdirAll(filepath.Dir(socket), 0o755); err != nil {
		return nil, fmt.Errorf("create socket directory: %w", err)
	}
	// A socket file left by a previous relay would block the bind even though
	// nothing holds it.
	if info, err := os.Lstat(socket); err == nil && info.Mode()&os.ModeSocket != 0 {
		if err := os.Remove(socket); err != nil {
			return nil, fmt.Errorf("remove stale socket: %w", err)
		}
	}
	var config net.ListenConfig
	listener, err := config.Listen(context.Background(), "unix", socket)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", socket, err)
	}
	// The pool-agent container runs as a different user than this relay.
	if err := os.Chmod(socket, 0o666); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("chmod socket: %w", err)
	}
	return listener, nil
}

func acceptLocal(ctx context.Context, listener net.Listener, session *cpmux.Session) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { _ = conn.Close() }()
			stream, err := session.Dial(ctx, cpmux.TargetControlPlane)
			if err != nil {
				return
			}
			defer func() { _ = stream.Close() }()
			cpmux.Splice(conn, stream)
		}()
	}
}

// dialGuest connects to an address inside the guest, in the unix:/tcp: target
// vocabulary shared by --dial and the streams the host opens on the mux.
func dialGuest(ctx context.Context, target string) (net.Conn, error) {
	var dialer net.Dialer
	switch {
	case strings.HasPrefix(target, "unix:"):
		return dialer.DialContext(ctx, "unix", strings.TrimPrefix(target, "unix:"))
	case strings.HasPrefix(target, "tcp:"):
		return dialer.DialContext(ctx, "tcp", strings.TrimPrefix(target, "tcp:"))
	default:
		return nil, fmt.Errorf("unsupported target %q", target)
	}
}

// stdioConn presents this process's stdin and stdout as one net.Conn. They are
// separate handles relayed independently by the host, so the pair is the
// transport rather than any single file.
type stdioConn struct {
	in  *os.File
	out *os.File

	writeCloseOnce sync.Once
}

func newStdioConn() net.Conn { return &stdioConn{in: os.Stdin, out: os.Stdout} }

func (c *stdioConn) Read(b []byte) (int, error)  { return c.in.Read(b) }
func (c *stdioConn) Write(b []byte) (int, error) { return c.out.Write(b) }

// CloseWrite closes stdout alone, which is how this end signals EOF to the
// host while still reading whatever the host has left to send. The host's own
// read of the relayed handle is very likely blocked on exactly this, so it has
// to be a real close rather than a flag: see the same reasoning, and the same
// bug it once caused, in cpmux.Splice's half-close handling.
func (c *stdioConn) CloseWrite() error {
	var err error
	c.writeCloseOnce.Do(func() { err = c.out.Close() })
	return err
}

func (c *stdioConn) Close() error {
	err := c.in.Close()
	if outErr := c.CloseWrite(); err == nil {
		err = outErr
	}
	return err
}

func (c *stdioConn) LocalAddr() net.Addr  { return stdioAddr{} }
func (c *stdioConn) RemoteAddr() net.Addr { return stdioAddr{} }

// Deadlines apply to the underlying handles when they support them; a relayed
// pipe generally does not, and yamux's keepalive is what detects a dead peer.
func (c *stdioConn) SetDeadline(t time.Time) error {
	err := c.in.SetReadDeadline(t)
	if writeErr := c.out.SetWriteDeadline(t); err == nil {
		err = writeErr
	}
	return err
}
func (c *stdioConn) SetReadDeadline(t time.Time) error  { return c.in.SetReadDeadline(t) }
func (c *stdioConn) SetWriteDeadline(t time.Time) error { return c.out.SetWriteDeadline(t) }

type stdioAddr struct{}

func (stdioAddr) Network() string { return "stdio" }
func (stdioAddr) String() string  { return "cp-relay-stdio" }
