//go:build darwin

package vsock

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// A macOS 13 or newer guest under Virtualization.framework has AF_VSOCK, and
// x/sys/unix knows its address type; what is missing is a net.Listener. The
// sockets are made non-blocking and handed to the runtime poller through
// os.NewFile, so Accept, Read, Write, and deadlines behave as they do for any
// other network connection.

func listen(cid, port uint32) (net.Listener, error) {
	fd, err := socket()
	if err != nil {
		return nil, err
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: cid, Port: port}); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("vsock: bind port %d: %w", port, err)
	}
	if err := unix.Listen(fd, unix.SOMAXCONN); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("vsock: listen on port %d: %w", port, err)
	}
	file := os.NewFile(uintptr(fd), fmt.Sprintf("vsock-listener:%d", port))
	raw, err := file.SyscallConn()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("vsock: listener: %w", err)
	}
	return &listener{file: file, raw: raw, addr: Addr{CID: cid, Port: port}}, nil
}

// errDialNotImplemented is a value rather than an error built in dial so that
// dial's error stays opaque to staticcheck: a function that can only return a
// concrete non-nil error makes every caller's check provably true (SA4023).
var errDialNotImplemented = errors.New("vsock: dialing is not implemented on darwin")

// dial is not needed by anything a macOS guest runs yet: the lifecycle service
// only listens. It fails plainly rather than pretending, so the first caller
// that does need it is told where the gap is.
func dial(uint32, uint32) (net.Conn, error) {
	return nil, errDialNotImplemented
}

func socket() (int, error) {
	// Held so a concurrent fork cannot inherit the descriptor between socket
	// and close-on-exec, which darwin cannot set atomically.
	syscall.ForkLock.RLock()
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err == nil {
		unix.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return -1, fmt.Errorf("vsock: socket: %w", err)
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("vsock: set non-blocking: %w", err)
	}
	return fd, nil
}

type listener struct {
	file *os.File
	raw  syscall.RawConn
	addr Addr
}

func (l *listener) Accept() (net.Conn, error) {
	var (
		nfd int
		sa  unix.Sockaddr
		err error
	)
	// The poller waits for the listening socket to become readable whenever
	// the function reports it has nothing yet.
	if rerr := l.raw.Read(func(fd uintptr) bool {
		syscall.ForkLock.RLock()
		nfd, sa, err = unix.Accept(int(fd))
		if err == nil {
			unix.CloseOnExec(nfd)
		}
		syscall.ForkLock.RUnlock()
		return !errors.Is(err, unix.EAGAIN)
	}); rerr != nil {
		return nil, rerr
	}
	if err != nil {
		return nil, fmt.Errorf("vsock: accept: %w", err)
	}
	if err := unix.SetNonblock(nfd, true); err != nil {
		_ = unix.Close(nfd)
		return nil, fmt.Errorf("vsock: set non-blocking: %w", err)
	}
	remote := Addr{}
	if vm, ok := sa.(*unix.SockaddrVM); ok {
		remote = Addr{CID: vm.CID, Port: vm.Port}
	}
	return &conn{file: os.NewFile(uintptr(nfd), "vsock-conn"), local: l.addr, remote: remote}, nil
}

func (l *listener) Close() error   { return l.file.Close() }
func (l *listener) Addr() net.Addr { return l.addr }

// conn deliberately has no CloseWrite: shutting down only the write side is
// not something this transport has been shown to do on a macOS guest, and a
// caller that finds the method believes the peer saw EOF.
type conn struct {
	file          *os.File
	local, remote Addr
}

func (c *conn) Read(b []byte) (int, error)         { return c.file.Read(b) }
func (c *conn) Write(b []byte) (int, error)        { return c.file.Write(b) }
func (c *conn) Close() error                       { return c.file.Close() }
func (c *conn) LocalAddr() net.Addr                { return c.local }
func (c *conn) RemoteAddr() net.Addr               { return c.remote }
func (c *conn) SetDeadline(t time.Time) error      { return c.file.SetDeadline(t) }
func (c *conn) SetReadDeadline(t time.Time) error  { return c.file.SetReadDeadline(t) }
func (c *conn) SetWriteDeadline(t time.Time) error { return c.file.SetWriteDeadline(t) }

// Addr is a VSOCK endpoint.
type Addr struct {
	CID, Port uint32
}

// Network reports the address family.
func (Addr) Network() string { return "vsock" }

func (a Addr) String() string { return fmt.Sprintf("vsock://%d:%d", a.CID, a.Port) }
