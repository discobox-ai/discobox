package wstunnel

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// testServer serves tunnels over TLS and counts the tunnels it accepted.
type testServer struct {
	*httptest.Server
	tunnels atomic.Int32
}

func newTestServer(t *testing.T, serve func(net.Conn)) *testServer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ts := &testServer{}
	handler := Handler(ctx, serve)
	ts.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only upgrades count: whatever else probes a new listener is not a
		// tunnel.
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			ts.tunnels.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func (ts *testServer) client() *Client {
	return NewClient(strings.Replace(ts.URL, "https://", "wss://", 1), ts.Client())
}

// echo serves a stream by sending back what it reads until the client ends it.
func echo(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_, _ = io.Copy(conn, conn)
}

func roundTrip(t *testing.T, s *Stream, msg string) {
	t.Helper()
	if _, err := s.Write([]byte(msg)); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(s, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != msg {
		t.Fatalf("read %q, want %q", got, msg)
	}
}

// waitIdle waits until the client holds n idle tunnels.
func waitIdle(t *testing.T, c *Client, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		got := len(c.idle)
		c.mu.Unlock()
		if got == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("client holds %d idle tunnels, want %d", got, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStreamsReuseOneTunnelInTurn(t *testing.T) {
	ts := newTestServer(t, echo)
	c := ts.client()
	for i := range 3 {
		s, err := c.Dial(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got, want := s.Reused(), i > 0; got != want {
			t.Fatalf("stream %d Reused() = %v, want %v", i, got, want)
		}
		roundTrip(t, s, "hello")
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		waitIdle(t, c, 1)
	}
	if got := ts.tunnels.Load(); got != 1 {
		t.Fatalf("server accepted %d tunnels, want 1", got)
	}
}

func TestConcurrentStreamsTakeTheirOwnTunnels(t *testing.T) {
	ts := newTestServer(t, echo)
	c := ts.client()
	a, err := c.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, a, "a")
	roundTrip(t, b, "b")
	if got := ts.tunnels.Load(); got != 2 {
		t.Fatalf("server accepted %d tunnels, want 2", got)
	}
	_ = a.Close()
	_ = b.Close()
	waitIdle(t, c, 2)
}

func TestServerEndingFirstReadsAsEOFAndTheTunnelIsReused(t *testing.T) {
	ts := newTestServer(t, func(conn net.Conn) {
		_, _ = conn.Write([]byte("bye"))
		_ = conn.Close()
	})
	c := ts.client()
	for range 2 {
		s, err := c.Dial(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		// The client speaks first; the server answers and ends the stream.
		if _, err := s.Write([]byte("hi")); err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(s)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(got) != "bye" {
			t.Fatalf("read %q, want %q", got, "bye")
		}
		_ = s.Close()
		waitIdle(t, c, 1)
	}
	if got := ts.tunnels.Load(); got != 1 {
		t.Fatalf("server accepted %d tunnels, want 1", got)
	}
}

func TestBothSidesEndingAtOnceLeavesTheTunnelReusable(t *testing.T) {
	read := make(chan struct{})
	ts := newTestServer(t, func(conn net.Conn) {
		buf := make([]byte, 1)
		_, _ = conn.Read(buf)
		close(read)
		_ = conn.Close()
	})
	c := ts.client()
	s, err := c.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	<-read
	_ = s.Close()
	waitIdle(t, c, 1)
	next, err := c.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !next.Reused() {
		t.Fatal("the next stream did not reuse the tunnel")
	}
	_ = next.Close()
}

func TestAStreamClosedBeforeSendingIsAnswered(t *testing.T) {
	ts := newTestServer(t, echo)
	c := ts.client()
	s, err := c.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	waitIdle(t, c, 1)
}

func TestDataTheServerSendsAfterTheClientEndedIsDiscarded(t *testing.T) {
	ended := make(chan struct{})
	ts := newTestServer(t, func(conn net.Conn) {
		defer func() { _ = conn.Close() }()
		buf := make([]byte, 1)
		_, _ = conn.Read(buf)
		// Keep writing until the client's end arrives.
		for {
			if _, err := conn.Write(bytes.Repeat([]byte("z"), 1024)); err != nil {
				break
			}
			if _, err := conn.Read(buf); errors.Is(err, io.EOF) {
				close(ended)
				return
			}
		}
	})
	c := ts.client()
	s, err := c.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	<-ended
	waitIdle(t, c, 1)
	next, err := c.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !next.Reused() {
		t.Fatal("the next stream did not reuse the tunnel")
	}
	_ = next.Close()
}

// edge relays TCP to a server and can cut every connection it carries the way
// an edge cuts an idle one: the server side goes at once, and the client finds
// out only when it next sends.
type edge struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	cut    bool
	conns  []net.Conn
}

func newEdge(t *testing.T, target string) *edge {
	t.Helper()
	var listenConfig net.ListenConfig
	ln, err := listenConfig.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e := &edge{ln: ln, target: target}
	t.Cleanup(func() { _ = ln.Close() })
	go e.serve()
	return e
}

func (e *edge) serve() {
	for {
		client, err := e.ln.Accept()
		if err != nil {
			return
		}
		var dialer net.Dialer
		server, err := dialer.DialContext(context.Background(), "tcp", e.target)
		if err != nil {
			_ = client.Close()
			continue
		}
		e.mu.Lock()
		e.conns = append(e.conns, server)
		e.mu.Unlock()
		go func() {
			_, _ = io.Copy(client, server)
			e.mu.Lock()
			cut := e.cut
			e.mu.Unlock()
			if !cut {
				_ = client.Close()
			}
		}()
		go func() {
			_, _ = io.Copy(server, client)
			_ = client.Close()
		}()
	}
}

// cutAll drops the server side of every connection so far.
func (e *edge) cutAll() {
	e.mu.Lock()
	e.cut = true
	conns := e.conns
	e.conns = nil
	e.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
	e.mu.Lock()
	e.cut = false
	e.mu.Unlock()
}

func TestATunnelTheEdgeCutFailsOnUseAndDialNewRecovers(t *testing.T) {
	ts := newTestServer(t, echo)
	e := newEdge(t, ts.Listener.Addr().String())
	c := NewClient("wss://"+e.ln.Addr().String(), ts.Client())
	s, err := c.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, s, "one")
	_ = s.Close()
	waitIdle(t, c, 1)
	e.cutAll()

	stale, err := c.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !stale.Reused() {
		t.Fatal("the stream on the cut tunnel is not marked reused")
	}
	_, _ = stale.Write([]byte("two"))
	_ = stale.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := stale.Read(make([]byte, 3)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("read on a cut tunnel = %v, want the tunnel's failure", err)
	}
	_ = stale.Close()

	fresh, err := c.DialNew(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Reused() {
		t.Fatal("DialNew reused a tunnel")
	}
	roundTrip(t, fresh, "three")
	_ = fresh.Close()
}

func TestReadDeadlineTimesOutWithoutEndingTheStream(t *testing.T) {
	ts := newTestServer(t, echo)
	c := ts.client()
	s, err := c.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_ = s.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	_, err = s.Read(make([]byte, 1))
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("read past its deadline = %v, want a timeout", err)
	}
	_ = s.SetReadDeadline(time.Time{})
	roundTrip(t, s, "still here")
	_ = s.Close()
	waitIdle(t, c, 1)
}

func TestALargeWriteIsCarriedWhole(t *testing.T) {
	ts := newTestServer(t, echo)
	c := ts.client()
	s, err := c.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("0123456789abcdef"), 3*maxChunk/16+5)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := s.Write(payload); err != nil {
			t.Errorf("write: %v", err)
		}
	}()
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(s, got); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if !bytes.Equal(got, payload) {
		t.Fatal("payload changed in the tunnel")
	}
	_ = s.Close()
}

func TestIdleTunnelsAreRetiredAfterTheirTimeout(t *testing.T) {
	defer func(d time.Duration) { idleTimeout = d }(idleTimeout)
	idleTimeout = 50 * time.Millisecond
	ts := newTestServer(t, echo)
	c := ts.client()
	s, err := c.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	waitIdle(t, c, 1)
	waitIdle(t, c, 0)
	next, err := c.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if next.Reused() {
		t.Fatal("a stream reused a tunnel past its idle timeout")
	}
	_ = next.Close()
}

func TestATunnelPastItsAgeIsRetiredWhenItsStreamEnds(t *testing.T) {
	defer func(d time.Duration) { maxAge = d }(maxAge)
	maxAge = 0
	ts := newTestServer(t, echo)
	c := ts.client()
	s, err := c.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, s, "old")
	_ = s.Close()
	select {
	case <-s.t.dead:
	case <-time.After(5 * time.Second):
		t.Fatal("the aged tunnel was not retired")
	}
	waitIdle(t, c, 0)
}

func TestTheServerRefusesAClientWithoutTheSubprotocol(t *testing.T) {
	ts := newTestServer(t, echo)
	ws, resp, err := websocket.Dial(context.Background(), strings.Replace(ts.URL, "https://", "wss://", 1), &websocket.DialOptions{HTTPClient: ts.Client()})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = ws.Read(context.Background())
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("read = %v, want a policy-violation close", err)
	}
}

func TestDataOnAnIdleClientTunnelClosesIt(t *testing.T) {
	var server atomic.Pointer[Stream]
	ts := newTestServer(t, func(conn net.Conn) {
		server.Store(conn.(*Stream))
		echo(conn)
	})
	c := ts.client()
	s, err := c.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, s, "x")
	_ = s.Close()
	waitIdle(t, c, 1)
	if err := server.Load().t.ws.Write(context.Background(), websocket.MessageBinary, []byte("stray")); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, c, 0)
	if !errors.Is(s.t.err, errProtocol) {
		t.Fatalf("tunnel stopped for %v, want a protocol violation", s.t.err)
	}
}

// A server that closes a tunnel the moment it is upgraded — a pool shutting
// down — gives DialNew a stream that fails on use, never a nil one.
func TestDialNewOnATunnelClosedAtOnceReturnsAStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // every tunnel the handler accepts stops at once
	ts := httptest.NewTLSServer(Handler(ctx, echo))
	t.Cleanup(ts.Close)
	c := NewClient(strings.Replace(ts.URL, "https://", "wss://", 1), ts.Client())
	for range 20 {
		s, err := c.DialNew(context.Background())
		if err != nil {
			continue
		}
		if s == nil {
			t.Fatal("DialNew returned neither a stream nor an error")
		}
		_ = s.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _ = s.Write([]byte("x"))
		if _, err := s.Read(make([]byte, 1)); err == nil {
			t.Fatal("read on a tunnel the server closed succeeded")
		}
		_ = s.Close()
	}
}

// The server's idle timeout runs from the end of the last stream, whichever
// side ended it, so a client that vanishes without closing its connection does
// not hold the tunnel open.
func TestTheServerStopsATunnelIdleSinceItsLastStream(t *testing.T) {
	defer func(d time.Duration) { serverIdleTimeout = d }(serverIdleTimeout)
	serverIdleTimeout = 100 * time.Millisecond
	var server atomic.Pointer[Stream]
	ts := newTestServer(t, func(conn net.Conn) {
		server.Store(conn.(*Stream))
		echo(conn)
	})
	c := ts.client()
	s, err := c.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, s, "x")
	_ = s.Close() // the client ends first; the server answers
	waitIdle(t, c, 1)
	select {
	case <-server.Load().t.dead:
	case <-time.After(5 * time.Second):
		t.Fatal("the server kept a tunnel idle past its timeout")
	}
	if !errors.Is(server.Load().t.err, errIdle) {
		t.Fatalf("server tunnel stopped for %v, want idle", server.Load().t.err)
	}
}

// A peer that never answers a stream's end holds the tunnel for endWait, not
// for ever.
func TestAnUnansweredEndGivesUpTheTunnel(t *testing.T) {
	defer func(d time.Duration) { endWait = d }(endWait)
	endWait = 100 * time.Millisecond
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	ts := newTestServer(t, func(conn net.Conn) {
		<-hold // never closes, so never answers the client's end
		_ = conn.Close()
	})
	c := ts.client()
	s, err := c.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	select {
	case <-s.t.dead:
	case <-time.After(5 * time.Second):
		t.Fatal("the tunnel waited on an unanswered end past endWait")
	}
	if !errors.Is(s.t.err, errUnanswered) {
		t.Fatalf("tunnel stopped for %v, want an unanswered end", s.t.err)
	}
}

// A write deadline set while a write is blocked on a peer that is not reading
// ends that write, as net.Conn's does.
func TestAWriteDeadlineReachesAWriteUnderWay(t *testing.T) {
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	ts := newTestServer(t, func(conn net.Conn) {
		<-hold // never reads
		_ = conn.Close()
	})
	c := ts.client()
	s, err := c.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		payload := bytes.Repeat([]byte("x"), maxChunk)
		for {
			if _, err := s.Write(payload); err != nil {
				result <- err
				return
			}
		}
	}()
	// Let the writes back up behind the peer, then cut the one under way.
	time.Sleep(500 * time.Millisecond)
	_ = s.SetWriteDeadline(time.Now())
	select {
	case err := <-result:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("write = %v, want the deadline", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a write deadline set during a blocked write did not end it")
	}
}

// A tunnel that never begins a stream is held only for firstStreamTimeout, so
// holding one open costs a request.
func TestTheServerStopsATunnelThatNeverBeginsAStream(t *testing.T) {
	defer func(d time.Duration) { firstStreamTimeout = d }(firstStreamTimeout)
	firstStreamTimeout = 100 * time.Millisecond
	ts := newTestServer(t, echo)
	ws, resp, err := websocket.Dial(context.Background(), strings.Replace(ts.URL, "https://", "wss://", 1),
		&websocket.DialOptions{HTTPClient: ts.Client(), Subprotocols: []string{Subprotocol}})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.CloseNow() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := ws.Read(ctx); err == nil || ctx.Err() != nil {
		t.Fatalf("read on a tunnel with no stream = %v, want the server to close it", err)
	}
}
