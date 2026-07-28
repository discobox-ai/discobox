//go:build windows

package wslcsession

import (
	"io"
	"net"
	"sync"
	"time"
)

const sdSend = 1

// guestConn implements net.Conn over the raw Winsock socket handles
// GetStdHandle returns for a guest process's stdin/stdout. These are real
// hvsocket-relayed connections under the hood (wslcsession.exe relays them
// to the guest over its private vsock control channel) - from here they're
// just SOCKET handles, but .NET's Socket/NetworkStream (and, per the same
// reasoning, Go's net.FileConn) refuse to wrap a handle they didn't create
// via Connect()/Accept() themselves, since their "connected" tracking is a
// cached flag, not a live query of OS state. Raw recv()/send() sidesteps
// that entirely.
type guestConn struct {
	session        *Session
	process        wslcProcess
	stdin          socketHandle // write here -> guest process's stdin
	stdout         socketHandle // read here <- guest process's stdout
	closeOnce      sync.Once
	writeCloseOnce sync.Once
}

func newGuestConn(session *Session, process wslcProcess, stdin, stdout socketHandle) *guestConn {
	return &guestConn{session: session, process: process, stdin: stdin, stdout: stdout}
}

func (c *guestConn) Read(b []byte) (int, error) {
	n, err := recv(c.stdout, b)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, io.EOF
	}
	return n, nil
}

func (c *guestConn) Write(b []byte) (int, error) {
	total := 0
	for total < len(b) {
		n, err := send(c.stdin, b[total:])
		if err != nil {
			return total, err
		}
		if n <= 0 {
			return total, io.ErrClosedPipe
		}
		total += n
	}
	return total, nil
}

// CloseWrite shuts down the write half of the connection - it signals EOF to
// the guest-side process (which propagates it as a real half-close on the
// underlying unix/TCP target - see bridgesrc/bridge.c) while leaving Read
// available to consume the rest of the response. This is the same
// convention *net.TCPConn.CloseWrite() establishes, so code that
// type-asserts for a `CloseWrite() error` method (a common Go idiom for
// half-close-aware protocols) picks this up automatically.
func (c *guestConn) CloseWrite() error {
	c.writeCloseOnce.Do(func() {
		shutdownSocket(c.stdin, sdSend)
	})
	return nil
}

func (c *guestConn) Close() error {
	c.closeOnce.Do(func() {
		// recv/send/shutdown/closesocket are plain Winsock calls, not COM -
		// unlike releasing the IWSLCProcess reference, they have no
		// OS-thread affinity and are safe to make from any goroutine.
		_ = c.CloseWrite()
		closesocket(c.stdin)
		closesocket(c.stdout)
		c.session.releaseProcess(c.process)
	})
	return nil
}

func (c *guestConn) LocalAddr() net.Addr  { return guestAddr{} }
func (c *guestConn) RemoteAddr() net.Addr { return guestAddr{} }

// Deadlines are not implemented: this is a synchronous blocking relay over a
// vsock-backed handle, not a plain Windows socket amenable to
// SO_RCVTIMEO/SO_SNDTIMEO the way a normal net.Conn would be. These are
// no-ops rather than errors so code that opportunistically sets a deadline
// (e.g. some HTTP client plumbing) doesn't fail outright.
func (c *guestConn) SetDeadline(_ time.Time) error      { return nil }
func (c *guestConn) SetReadDeadline(_ time.Time) error  { return nil }
func (c *guestConn) SetWriteDeadline(_ time.Time) error { return nil }

type guestAddr struct{}

func (guestAddr) Network() string { return "vsock" }
func (guestAddr) String() string  { return "wslc-guest" }
