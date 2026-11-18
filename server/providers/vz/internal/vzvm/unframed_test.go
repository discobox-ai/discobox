package vzvm

import (
	"io"
	"net"
	"sync/atomic"
	"testing"

	guestvsock "github.com/discobox-ai/discobox/pkg/vsock"
)

// A guest whose services do not frame answers a framed dial in plain bytes.
// The consumer only reads EOF, so the check is what names the guest.
func TestUnframedCheckReportsAGuestThatAnswersInPlainBytes(t *testing.T) {
	host, guest := net.Pipe()
	defer guest.Close()
	var reports atomic.Int32
	go func() {
		// The framed dial's first frame arrives; the unframed guest answers
		// as an HTTP server would, and hangs up.
		_, _ = io.ReadAtLeast(guest, make([]byte, 12), 12)
		_, _ = io.WriteString(guest, "HTTP/1.1 400 Bad Request\r\n\r\n")
		_ = guest.Close()
	}()
	conn, err := guestvsock.FrameDialed(&unframedCheck{Conn: host, report: func() { reports.Add(1) }})
	if err == nil {
		_, _ = io.ReadAll(conn)
		_ = conn.Close()
	}
	if got := reports.Load(); got != 1 {
		t.Fatalf("reports = %d, want 1", got)
	}
}

// A framed guest is never reported, however much it sends.
func TestUnframedCheckIsQuietForAFramedGuest(t *testing.T) {
	host, guest := net.Pipe()
	listener := &oneConnListener{conn: guest, done: make(chan struct{})}
	framed := guestvsock.FrameListener(listener, nil)
	defer framed.Close()
	var reports atomic.Int32
	dialed := make(chan net.Conn, 1)
	go func() {
		conn, err := guestvsock.FrameDialed(&unframedCheck{Conn: host, report: func() { reports.Add(1) }})
		if err != nil {
			t.Error(err)
			close(dialed)
			return
		}
		dialed <- conn
	}()
	accepted, err := framed.Accept()
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = accepted.Write([]byte("framed reply")); _ = accepted.Close() }()
	conn := <-dialed
	if conn == nil {
		return
	}
	got, _ := io.ReadAll(conn)
	_ = conn.Close()
	if string(got) != "framed reply" || reports.Load() != 0 {
		t.Fatalf("read %q with %d reports; want the reply and none", got, reports.Load())
	}
}

// oneConnListener hands out one connection, then blocks until closed.
type oneConnListener struct {
	conn   net.Conn
	given  atomic.Bool
	done   chan struct{}
	closed atomic.Bool
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	if l.given.CompareAndSwap(false, true) {
		return l.conn, nil
	}
	<-l.done
	return nil, net.ErrClosed
}

func (l *oneConnListener) Close() error {
	if l.closed.CompareAndSwap(false, true) {
		close(l.done)
	}
	return nil
}

func (l *oneConnListener) Addr() net.Addr { return l.conn.LocalAddr() }
