//go:build unix

package vsock

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

func TestFramedByKernel(t *testing.T) {
	for cmdline, want := range map[string]bool{
		"console=hvc0 root=/dev/vda ro " + FramingKernelParameter + " panic=5": true,
		FramingKernelParameter:                    true,
		"console=hvc0 root=/dev/vda ro":           false,
		"discobox.vsock=yamuxx":                   false,
		"xdiscobox.vsock=yamux":                   false,
		"console=hvc0\n" + FramingKernelParameter: true,
	} {
		if got := FramedByKernel(cmdline); got != want {
			t.Errorf("FramedByKernel(%q) = %v, want %v", cmdline, got, want)
		}
	}
}

// framedPair frames both ends of a fresh in-memory connection: the dialing
// side as FrameDialed does, the accepting side through FrameListener.
func framedPair(t *testing.T) (dialed, accepted net.Conn) {
	t.Helper()
	guest, host := net.Pipe()
	listener := newChanListener()
	framed := FrameListener(listener, nil)
	t.Cleanup(func() { _ = framed.Close() })
	listener.push(host)
	dialed, err := FrameDialed(guest)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err = framed.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dialed.Close(); _ = accepted.Close() })
	return dialed, accepted
}

func TestFramedConnCloseDeliversEverythingWrittenThenEOF(t *testing.T) {
	dialed, accepted := framedPair(t)
	payload := bytes.Repeat([]byte("discobox"), 3*FrameWindow/8)
	go func() {
		_, _ = dialed.Write(payload)
		_ = dialed.Close()
	}()
	got, err := io.ReadAll(accepted)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("read %d bytes, want %d", len(got), len(payload))
	}
}

// A read can find the connection's write side already gone: the peer wrote
// its last bytes and hung up, and the window update the read sends cannot be
// written. The bytes read must still be returned without that error.
func TestFramedConnReturnsWhatItReadWhenTheWindowUpdateCannotBeSent(t *testing.T) {
	guest, host := net.Pipe()
	hostSide := &failingWrites{Conn: host}
	listener := newChanListener()
	framed := FrameListener(listener, nil)
	t.Cleanup(func() { _ = framed.Close() })
	listener.push(hostSide)
	dialed, err := FrameDialed(guest)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dialed.Close() })
	accepted, err := framed.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = accepted.Close() })

	payload := bytes.Repeat([]byte("v"), FrameWindow)
	if _, err := dialed.Write(payload); err != nil {
		t.Fatal(err)
	}
	hostSide.fail.Store(true)
	got := make([]byte, len(payload))
	n, err := accepted.Read(got)
	if n == 0 || err != nil {
		t.Fatalf("Read = %d, %v; want the bytes it read and no error", n, err)
	}
}

// failingWrites fails every Write once fail is set, as a socket whose peer
// has gone does.
type failingWrites struct {
	net.Conn
	fail atomic.Bool
}

func (c *failingWrites) Write(b []byte) (int, error) {
	if c.fail.Load() {
		return 0, syscall.EPIPE
	}
	return c.Conn.Write(b)
}

func TestFramedConnHasNoCloseWrite(t *testing.T) {
	dialed, accepted := framedPair(t)
	for _, conn := range []net.Conn{dialed, accepted} {
		if _, ok := conn.(interface{ CloseWrite() error }); ok {
			t.Fatalf("%T has CloseWrite; a framed connection cannot half-close", conn)
		}
	}
}

func TestFrameListenerReportsAnUnframedPeer(t *testing.T) {
	listener := newChanListener()
	reported := make(chan error, 1)
	framed := FrameListener(listener, func(err error) { reported <- err })
	defer framed.Close()

	peer, host := net.Pipe()
	defer peer.Close()
	listener.push(host)
	go func() { _, _ = io.WriteString(peer, "GET / HTTP/1.1\r\nHost: x\r\n\r\n") }()

	select {
	case err := <-reported:
		if !strings.Contains(err.Error(), "framed stream") {
			t.Fatalf("report = %v, want it to name the missing framing", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an unframed peer was not reported")
	}

	accepted := make(chan net.Conn, 1)
	go func() {
		if conn, err := framed.Accept(); err == nil {
			accepted <- conn
		}
	}()
	select {
	case <-accepted:
		t.Fatal("Accept returned an unframed connection")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestFrameListenerAcceptEndsWithInner(t *testing.T) {
	listener := newChanListener()
	framed := FrameListener(listener, nil)
	_ = listener.Close()
	if _, err := framed.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Accept after inner closed = %v, want net.ErrClosed", err)
	}
	_ = framed.Close()
}

func TestFramedSessionCarriesOneStream(t *testing.T) {
	dialed, accepted := framedPair(t)
	// Each side has sent its go-away by the time it has its connection, so a
	// second stream from the other side either cannot open (the go-away has
	// arrived) or is reset on arrival (it has not yet).
	for name, conn := range map[string]net.Conn{"dialing": dialed, "accepting": accepted} {
		second, err := conn.(*framedConn).session.OpenStream()
		if err != nil {
			continue
		}
		_ = second.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := second.Read(make([]byte, 1)); !errors.Is(err, yamux.ErrConnectionReset) {
			t.Fatalf("a second stream from the %s side read %v, want it reset", name, err)
		}
	}
	// The one stream is untouched.
	go func() { _, _ = dialed.Write([]byte("still here")) }()
	got := make([]byte, len("still here"))
	_ = accepted.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(accepted, got); err != nil || string(got) != "still here" {
		t.Fatalf("read %q, %v", got, err)
	}
}

// temporaryError is what Accept returns when the process is out of
// descriptors: a net.Error that is temporary.
type temporaryError struct{}

func (temporaryError) Error() string   { return "too many open files" }
func (temporaryError) Timeout() bool   { return false }
func (temporaryError) Temporary() bool { return true }

func TestFrameListenerPassesATemporaryErrorThroughAndKeepsAccepting(t *testing.T) {
	listener := newChanListener()
	framed := FrameListener(listener, nil)
	defer framed.Close()

	listener.fail(temporaryError{})
	if _, err := framed.Accept(); !errors.Is(err, temporaryError{}) {
		t.Fatalf("Accept = %v, want the inner listener's temporary error", err)
	}

	guest, host := net.Pipe()
	listener.push(host)
	dialed, err := FrameDialed(guest)
	if err != nil {
		t.Fatal(err)
	}
	defer dialed.Close()
	accepted, err := framed.Accept()
	if err != nil {
		t.Fatalf("Accept after a temporary error: %v", err)
	}
	_ = accepted.Close()
}

func TestFrameListenerKeepsDrainingWhileAnErrorIsUnread(t *testing.T) {
	listener := newChanListener()
	framed := FrameListener(listener, nil)
	defer framed.Close()

	// Errors nobody reads, then a connection: the loop must not wait on the
	// caller to take the errors before it takes the connection from inner.
	for range 3 {
		listener.fail(temporaryError{})
	}
	guest, host := net.Pipe()
	listener.push(host)
	dialed := make(chan error, 1)
	go func() {
		conn, err := FrameDialed(guest)
		if err == nil {
			t.Cleanup(func() { _ = conn.Close() })
		}
		dialed <- err
	}()
	deadline := time.After(5 * time.Second)
	for len(listener.conns) > 0 || len(listener.errs) > 0 {
		select {
		case <-deadline:
			t.Fatal("the accept loop stopped taking from inner while an error was unread")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if err := <-dialed; err != nil {
		t.Fatalf("FrameDialed: %v", err)
	}
	// One error is left for the caller, and the connection: Accept returns
	// both, in whichever order they are ready.
	var gotErr, gotConn bool
	for range 2 {
		conn, err := framed.Accept()
		switch {
		case err == nil:
			gotConn = true
			_ = conn.Close()
		case errors.Is(err, temporaryError{}):
			gotErr = true
		default:
			t.Fatalf("Accept: %v", err)
		}
	}
	if !gotErr || !gotConn {
		t.Fatalf("Accept returned error %v, connection %v; want one of each", gotErr, gotConn)
	}
}

func TestFrameListenerClosedDuringBackoffReturnsErrClosed(t *testing.T) {
	listener := newChanListener()
	framed := FrameListener(listener, nil)
	listener.fail(temporaryError{})
	// Let the loop take the error and enter its backoff, then close it there.
	for len(listener.errs) > 0 {
		time.Sleep(time.Millisecond)
	}
	_ = framed.Close()
	for i := range 100 {
		conn, err := framed.Accept()
		if conn != nil || !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Accept %d after Close = %v, %v; want net.ErrClosed", i, conn, err)
		}
	}
}

// The model below stands in for Virtualization.framework (ADR 26-11-17-182
// §6): every connection's bytes, in both directions, pass through one
// goroutine that writes each chunk to its destination with a blocking write,
// as the framework's one serial queue delivers guest data with a blocking
// writev. The host end of each connection is a Unix socket with a small send
// buffer, so a host that stops reading blocks the queue, and with it every
// connection, exactly as on a Mac.

const hairpinBytes = 32 << 20

func TestModelUnframedHairpinStallsTheQueue(t *testing.T) {
	vm := newModelVM(t)
	child := vm.connect()  // host dials guest: the child's output (3002)
	parent := vm.connect() // guest dials host: the parent's stream (3001)

	go func() { _, _ = child.guest.Write(make([]byte, hairpinBytes)) }()
	go func() { _, _ = io.Copy(parent.host, child.host) }()
	counted := &counter{}
	go func() { _, _ = io.Copy(counted, parent.guest) }()

	if !counted.stalls(time.Second, 10*time.Second) {
		t.Fatalf("the unframed hairpin delivered %d bytes without stalling; the model no longer reproduces the deadlock", counted.load())
	}
	if counted.load() >= hairpinBytes {
		t.Fatal("the unframed hairpin delivered everything; the model no longer reproduces the deadlock")
	}
}

func TestModelFramedHairpinDeliversEveryByte(t *testing.T) {
	vm := newModelVM(t)
	childGuest, childHost := vm.framed(t, guestListens)
	parentGuest, parentHost := vm.framed(t, hostListens)

	go func() {
		_, _ = childGuest.Write(make([]byte, hairpinBytes))
		_ = childGuest.Close()
	}()
	go func() {
		_, _ = io.Copy(parentHost, childHost)
		_ = parentHost.Close()
	}()
	counted := &counter{}
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(counted, parentGuest)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("parent read: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("framed hairpin stalled at %d of %d bytes", counted.load(), hairpinBytes)
	}
	if counted.load() != hairpinBytes {
		t.Fatalf("parent counted %d bytes, want %d", counted.load(), hairpinBytes)
	}
	vm.assertQueueNeverBlocked(t)
}

func TestModelFramedStalledReaderStallsOnlyItsStream(t *testing.T) {
	vm := newModelVM(t)
	stalledGuest, stalledHost := vm.framed(t, guestListens)
	echoGuest, echoHost := vm.framed(t, guestListens)

	// The guest streams into a host consumer that does not read.
	written := &counter{}
	go func() { _, _ = io.Copy(stalledGuest, io.TeeReader(io.LimitReader(zeros{}, 4<<20), written)) }()

	// Another connection to the same guest keeps answering.
	go func() { _, _ = io.Copy(echoGuest, echoGuest) }()
	request := bytes.Repeat([]byte("ping"), 4096)
	reply := make([]byte, len(request))
	for i := range 200 {
		if _, err := echoHost.Write(request); err != nil {
			t.Fatalf("echo %d write: %v", i, err)
		}
		_ = echoHost.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(echoHost, reply); err != nil {
			t.Fatalf("echo %d beside a stalled reader: %v", i, err)
		}
	}

	// The stalled stream's sender is held to one window, plus what one Copy
	// buffer had read from the source when Write blocked.
	if got := written.load(); got > FrameWindow+32<<10 {
		t.Fatalf("guest wrote %d bytes into a stream nobody reads; the window is %d", got, FrameWindow)
	}
	vm.assertQueueNeverBlocked(t)

	// Reading again releases it.
	_ = stalledHost.SetReadDeadline(time.Now().Add(30 * time.Second))
	if n, err := io.CopyN(io.Discard, stalledHost, 4<<20); err != nil {
		t.Fatalf("resumed reader got %d bytes: %v", n, err)
	}
}

const (
	guestListens = iota
	hostListens
)

// modelVM is the model's framework: one queue serving every connection.
type modelVM struct {
	t      *testing.T
	queue  chan chunk
	mu     sync.Mutex
	maxBlk time.Duration
	closed chan struct{}
}

// chunk is one write the queue makes, or, with end set, the close of dst
// once everything queued before it is written.
type chunk struct {
	dst  net.Conn
	data []byte
	end  bool
}

// modelConn is one VSOCK connection: the host program's end and the guest
// program's end.
type modelConn struct {
	host, guest net.Conn
}

func newModelVM(t *testing.T) *modelVM {
	vm := &modelVM{t: t, queue: make(chan chunk), closed: make(chan struct{})}
	go vm.serve()
	t.Cleanup(func() { close(vm.closed) })
	return vm
}

func (vm *modelVM) serve() {
	for {
		select {
		case c := <-vm.queue:
			if c.end {
				_ = c.dst.Close()
				continue
			}
			start := time.Now()
			_, _ = c.dst.Write(c.data)
			if took := time.Since(start); took > 0 {
				vm.mu.Lock()
				vm.maxBlk = max(vm.maxBlk, took)
				vm.mu.Unlock()
			}
		case <-vm.closed:
			return
		}
	}
}

func (vm *modelVM) assertQueueNeverBlocked(t *testing.T) {
	t.Helper()
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if vm.maxBlk > time.Second {
		t.Fatalf("the queue was blocked writing one chunk for %v", vm.maxBlk)
	}
}

// connect makes one connection. Each program's end is one of a Unix socket
// pair whose other end the queue reads and writes, with 8 KiB buffers, as the
// framework's end of a host connection has.
func (vm *modelVM) connect() modelConn {
	hostProgram, hostQueue := socketPair(vm.t)
	guestProgram, guestQueue := socketPair(vm.t)
	vm.pump(guestQueue, hostQueue)
	vm.pump(hostQueue, guestQueue)
	return modelConn{host: hostProgram, guest: guestProgram}
}

// pump reads src for as long as it has data, as the framework credits the
// guest, and hands every chunk to the one queue.
func (vm *modelVM) pump(src, dst net.Conn) {
	go func() {
		for {
			buf := make([]byte, 32<<10)
			n, err := src.Read(buf)
			if n > 0 {
				select {
				case vm.queue <- chunk{dst: dst, data: buf[:n]}:
				case <-vm.closed:
					return
				}
			}
			if err != nil {
				select {
				case vm.queue <- chunk{dst: dst, end: true}:
				case <-vm.closed:
				}
				return
			}
		}
	}()
}

// framed makes one framed connection, with the side that listens taking it
// through FrameListener.
func (vm *modelVM) framed(t *testing.T, listens int) (guest, host net.Conn) {
	t.Helper()
	conn := vm.connect()
	dialing, accepting := conn.guest, conn.host
	if listens == guestListens {
		dialing, accepting = conn.host, conn.guest
	}
	listener := newChanListener()
	framedListener := FrameListener(listener, nil)
	t.Cleanup(func() { _ = framedListener.Close() })
	listener.push(accepting)
	dialed, err := FrameDialed(dialing)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := framedListener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dialed.Close(); _ = accepted.Close() })
	if listens == guestListens {
		return accepted, dialed
	}
	return dialed, accepted
}

// chanListener is a net.Listener fed by hand.
type chanListener struct {
	conns  chan net.Conn
	errs   chan error
	once   sync.Once
	closed chan struct{}
}

func newChanListener() *chanListener {
	return &chanListener{conns: make(chan net.Conn, 4), errs: make(chan error, 4), closed: make(chan struct{})}
}

func (l *chanListener) push(conn net.Conn) { l.conns <- conn }
func (l *chanListener) fail(err error)     { l.errs <- err }

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case err := <-l.errs:
		return nil, err
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *chanListener) Addr() net.Addr { return &net.UnixAddr{Name: "model", Net: "unix"} }

// counter counts what is written to it.
type counter struct {
	mu sync.Mutex
	n  int64
}

func (c *counter) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.n += int64(len(p))
	c.mu.Unlock()
	return len(p), nil
}

func (c *counter) load() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// stalls reports whether the count stops moving for still, within limit.
func (c *counter) stalls(still, limit time.Duration) bool {
	deadline := time.Now().Add(limit)
	last, since := c.load(), time.Now()
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		if now := c.load(); now != last {
			last, since = now, time.Now()
			continue
		}
		if time.Since(since) >= still {
			return true
		}
	}
	return false
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// socketPair returns both ends of a Unix stream socket pair with 8 KiB
// buffers.
func socketPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	conns := make([]net.Conn, 2)
	for i, fd := range fds {
		for _, opt := range []int{syscall.SO_SNDBUF, syscall.SO_RCVBUF} {
			if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, opt, 8<<10); err != nil {
				t.Fatal(err)
			}
		}
		file := os.NewFile(uintptr(fd), "model")
		conn, err := net.FileConn(file)
		_ = file.Close()
		if err != nil {
			t.Fatal(err)
		}
		conns[i] = conn
		t.Cleanup(func() { _ = conn.Close() })
	}
	return conns[0], conns[1]
}
