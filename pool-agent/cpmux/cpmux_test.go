package cpmux

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// pair builds a host/guest session over an in-memory duplex stream, standing in
// for the relayed stdio of a guest helper process.
func pair(t *testing.T) (host, guest *Session) {
	t.Helper()
	hostConn, guestConn := net.Pipe()
	var wg sync.WaitGroup
	var hostErr, guestErr error
	wg.Add(2)
	go func() { defer wg.Done(); host, hostErr = Client(hostConn) }()
	go func() { defer wg.Done(); guest, guestErr = Server(guestConn) }()
	wg.Wait()
	if hostErr != nil || guestErr != nil {
		t.Fatalf("start sessions: host=%v guest=%v", hostErr, guestErr)
	}
	t.Cleanup(func() { _ = host.Close(); _ = guest.Close() })
	return host, guest
}

// The property the whole design rests on: both ends can open streams over one
// connection, so the guest reaches the control plane even though nothing in the
// guest can dial the host.
func TestBothSidesCanOpenStreams(t *testing.T) {
	host, guest := pair(t)

	// Guest -> host, the direction the underlying transport cannot do.
	go func() {
		conn, err := guest.Dial(context.Background(), TargetControlPlane)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.WriteString(conn, "register")
	}()
	conn, target, err := host.Accept()
	if err != nil {
		t.Fatalf("host accept: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if target != TargetControlPlane {
		t.Fatalf("target = %q, want %q", target, TargetControlPlane)
	}
	got := make([]byte, len("register"))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read guest payload: %v", err)
	}
	if string(got) != "register" {
		t.Fatalf("payload = %q, want register", got)
	}

	// Host -> guest, naming a guest-side address.
	go func() {
		c, err := host.Dial(context.Background(), "tcp:127.0.0.1:3002")
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_, _ = io.WriteString(c, "sandbox-op")
	}()
	gconn, gtarget, err := guest.Accept()
	if err != nil {
		t.Fatalf("guest accept: %v", err)
	}
	defer func() { _ = gconn.Close() }()
	if gtarget != "tcp:127.0.0.1:3002" {
		t.Fatalf("guest target = %q", gtarget)
	}
	got = make([]byte, len("sandbox-op"))
	if _, err := io.ReadFull(gconn, got); err != nil {
		t.Fatalf("read host payload: %v", err)
	}
	if string(got) != "sandbox-op" {
		t.Fatalf("payload = %q, want sandbox-op", got)
	}
}

// Real HTTP must survive the mux in the guest-initiated direction, since that is
// how the agent registers. Serve stands in for the host routing control-plane
// streams into its own handler.
func TestControlPlaneHTTPOverGuestInitiatedStream(t *testing.T) {
	host, guest := pair(t)

	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			_, _ = w.Write([]byte("ok:" + r.URL.Path + ":" + string(body)))
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	t.Cleanup(func() { _ = server.Close() })

	// Host: every stream the guest opens is served by the control-plane handler.
	go func() {
		for {
			conn, target, err := host.Accept()
			if err != nil {
				return
			}
			if target != TargetControlPlane {
				_ = conn.Close()
				continue
			}
			go server.Serve(newSingleConnListener(conn)) //nolint:errcheck // listener ends with the test
		}
	}()

	// Guest: an ordinary HTTP client whose transport dials over the mux.
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return guest.Dial(ctx, TargetControlPlane)
			},
			DisableKeepAlives: true,
		},
		Timeout: 10 * time.Second,
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"http://control-plane/api/pool/register", strings.NewReader("pool-1"))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("register over mux: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if got, want := string(body), "ok:/api/pool/register:pool-1"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

// Half-close must survive: a caller writes, signals end-of-request, and reads
// the reply to EOF. Losing this is what disqualified Wisp.
func TestStreamHalfCloseDeliversPendingReply(t *testing.T) {
	host, guest := pair(t)

	go func() {
		conn, _, err := guest.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		// Read to EOF: proves the peer's CloseWrite arrived as a real EOF.
		got, _ := io.ReadAll(conn)
		_, _ = io.WriteString(conn, "reply-to:"+string(got))
		closeWrite(conn)
	}()

	conn, err := host.Dial(context.Background(), "tcp:127.0.0.1:1")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := io.WriteString(conn, "request"); err != nil {
		t.Fatalf("write: %v", err)
	}
	cw, ok := conn.(interface{ CloseWrite() error })
	if !ok {
		t.Fatal("stream does not implement CloseWrite; half-close is unavailable")
	}
	if err := cw.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}

	done := make(chan []byte, 1)
	go func() { body, _ := io.ReadAll(conn); done <- body }()
	select {
	case body := <-done:
		if got, want := string(body), "reply-to:request"; got != want {
			t.Fatalf("reply = %q, want %q", got, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("read after CloseWrite hung: half-close did not propagate")
	}
}

// Serve is the guest helper's loop: it dials whatever the host names and splices.
func TestServeDialsTargetAndSplices(t *testing.T) {
	host, guest := pair(t)

	var dialedTarget string
	go func() {
		_ = Serve(context.Background(), guest, func(_ context.Context, target string) (net.Conn, error) {
			dialedTarget = target
			local, remote := net.Pipe()
			go func() {
				defer func() { _ = remote.Close() }()
				buf := make([]byte, 5)
				if _, err := io.ReadFull(remote, buf); err != nil {
					return
				}
				_, _ = remote.Write([]byte("echo:" + string(buf)))
			}()
			return local, nil
		})
	}()

	conn, err := host.Dial(context.Background(), "unix:/var/run/docker.sock")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := io.WriteString(conn, "hello"); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len("echo:hello"))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "echo:hello" {
		t.Fatalf("payload = %q", got)
	}
	if dialedTarget != "unix:/var/run/docker.sock" {
		t.Fatalf("dialed target = %q", dialedTarget)
	}
}

// The header must not be allowed to consume the payload, and a hostile peer must
// not be able to make the reader allocate without bound.
func TestTargetHeaderRoundTripsWithoutEatingPayload(t *testing.T) {
	var buf strings.Builder
	if err := writeTarget(&buf, "tcp:127.0.0.1:3002"); err != nil {
		t.Fatalf("writeTarget: %v", err)
	}
	buf.WriteString("PAYLOAD-FIRST-BYTES")

	reader := strings.NewReader(buf.String())
	target, err := readTarget(reader)
	if err != nil {
		t.Fatalf("readTarget: %v", err)
	}
	if target != "tcp:127.0.0.1:3002" {
		t.Fatalf("target = %q", target)
	}
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if string(rest) != "PAYLOAD-FIRST-BYTES" {
		t.Fatalf("payload after header = %q, want it intact", rest)
	}
}

func TestReadTargetRejectsOversizedAndEmpty(t *testing.T) {
	if _, err := readTarget(strings.NewReader(strings.Repeat("x", maxTargetLength+10))); err == nil {
		t.Fatal("readTarget accepted an oversized header")
	}
	if _, err := readTarget(strings.NewReader("\n")); err == nil {
		t.Fatal("readTarget accepted an empty target")
	}
	if err := writeTarget(io.Discard, "bad\ntarget"); err == nil {
		t.Fatal("writeTarget accepted a newline in the target")
	}
}

// singleConnListener serves one already-open connection, which is how the host
// hands a mux stream to net/http.
type singleConnListener struct {
	conn      net.Conn
	done      chan struct{}
	once      sync.Once
	closeOnce sync.Once
}

func newSingleConnListener(conn net.Conn) *singleConnListener {
	return &singleConnListener{conn: conn, done: make(chan struct{})}
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	var conn net.Conn
	l.once.Do(func() { conn = l.conn })
	if conn != nil {
		return conn, nil
	}
	<-l.done
	return nil, net.ErrClosed
}

func (l *singleConnListener) Close() error {
	l.closeOnce.Do(func() { close(l.done) })
	return nil
}

func (l *singleConnListener) Addr() net.Addr { return l.conn.LocalAddr() }
