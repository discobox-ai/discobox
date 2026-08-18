package portforward

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"
)

type funcDialer func(ctx context.Context, target Target) (net.Conn, error)

func (f funcDialer) DialPort(ctx context.Context, target Target) (net.Conn, error) {
	return f(ctx, target)
}

// echoServer stands in for whatever is listening inside the sandbox.
func echoServer(t *testing.T) net.Addr {
	t.Helper()
	listener, err := listenTest(t, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return listener.Addr()
}

func dialerTo(addr net.Addr) Dialer {
	return funcDialer(func(ctx context.Context, _ Target) (net.Conn, error) {
		var config net.Dialer
		return config.DialContext(ctx, "tcp", addr.String())
	})
}

// collector serializes the events a test asserts on, the way a frontend
// observer has to.
type collector struct {
	mu     sync.Mutex
	events []Event
	wake   chan struct{}
}

func newCollector() *collector {
	return &collector{wake: make(chan struct{}, 64)}
}

func (c *collector) observe(event Event) {
	c.mu.Lock()
	c.events = append(c.events, event)
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// await waits for an event of kind on the remote port and returns it.
func (c *collector) await(t *testing.T, kind Kind, port int) Event {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		c.mu.Lock()
		for _, event := range c.events {
			if event.Kind == kind && event.Target.Port == port {
				c.mu.Unlock()
				return event
			}
		}
		c.mu.Unlock()
		select {
		case <-c.wake:
		case <-deadline:
			c.mu.Lock()
			defer c.mu.Unlock()
			t.Fatalf("timed out waiting for %s on %d; saw %v", kind, port, c.events)
		}
	}
}

func TestForwarderForwardsBytesToTheDialedPort(t *testing.T) {
	sandbox := echoServer(t)
	events := newCollector()
	forwarder := New(t.Context(), Options{Dialer: dialerTo(sandbox), Observe: events.observe})
	defer forwarder.Close()

	remotePort := 4711
	forwarder.Set([]Target{{Port: remotePort, Protocol: "http"}})
	bound := events.await(t, Bound, remotePort)

	conn, err := dialTest(t, bound.Local)
	if err != nil {
		t.Fatalf("dial forwarded port: %v", err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "hello"); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, 5)
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("echo = %q, want hello", got)
	}

	accepted := events.await(t, Accepted, remotePort)
	if accepted.Local != bound.Local {
		t.Fatalf("accepted local port = %d, want %d", accepted.Local, bound.Local)
	}
	if accepted.Peer == "" {
		t.Fatal("expected the accepted event to name the client")
	}
}

func TestForwarderBindsTheNearestFreePortAbove(t *testing.T) {
	sandbox := echoServer(t)
	// Whatever port this listener got is the one the sandbox will claim to be
	// serving, so the forwarder has to find a different local port for it.
	taken, err := listenTest(t, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer taken.Close()
	takenPort := taken.Addr().(*net.TCPAddr).Port

	events := newCollector()
	forwarder := New(t.Context(), Options{Dialer: dialerTo(sandbox), Observe: events.observe})
	defer forwarder.Close()

	forwarder.Set([]Target{{Port: takenPort}})
	bound := events.await(t, Bound, takenPort)
	if bound.Local == takenPort {
		t.Fatalf("bound the port that was already taken (%d)", takenPort)
	}
	if bound.Local <= takenPort || bound.Local > takenPort+DefaultSearch {
		t.Fatalf("local port = %d, want the nearest free port above %d", bound.Local, takenPort)
	}
}

func TestForwarderKeepsTheLocalPortWhenTheSandboxPortComesAndGoes(t *testing.T) {
	sandbox := echoServer(t)
	events := newCollector()
	forwarder := New(t.Context(), Options{Dialer: dialerTo(sandbox), Observe: events.observe})
	defer forwarder.Close()

	remotePort := 4712
	forwarder.Set([]Target{{Port: remotePort}})
	bound := events.await(t, Bound, remotePort)

	forwarder.Set(nil)
	gone := events.await(t, Gone, remotePort)
	if gone.Local != bound.Local {
		t.Fatalf("gone local port = %d, want %d held open", gone.Local, bound.Local)
	}
	bindings := forwarder.Bindings()
	if len(bindings) != 1 || bindings[0].Active {
		t.Fatalf("bindings = %#v, want the port still held and inactive", bindings)
	}

	forwarder.Set([]Target{{Port: remotePort}})
	back := events.await(t, Back, remotePort)
	if back.Local != bound.Local {
		t.Fatalf("back local port = %d, want the original %d", back.Local, bound.Local)
	}
	bindings = forwarder.Bindings()
	if len(bindings) != 1 || !bindings[0].Active {
		t.Fatalf("bindings = %#v, want one active binding", bindings)
	}
}

func TestForwarderRebindingTheSamePortIsANoOp(t *testing.T) {
	sandbox := echoServer(t)
	events := newCollector()
	forwarder := New(t.Context(), Options{Dialer: dialerTo(sandbox), Observe: events.observe})
	defer forwarder.Close()

	target := Target{Port: 4713}
	forwarder.Set([]Target{target})
	events.await(t, Bound, target.Port)
	for range 3 {
		forwarder.Set([]Target{target})
	}

	events.mu.Lock()
	defer events.mu.Unlock()
	var bounds int
	for _, event := range events.events {
		if event.Kind == Bound {
			bounds++
		}
	}
	if bounds != 1 {
		t.Fatalf("bound %d times, want 1: %v", bounds, events.events)
	}
}

// halfCloseConn reports whether the forwarder passed a half-close through
// instead of only closing the connection outright.
type halfCloseConn struct {
	net.Conn
	closedWrite chan struct{}
	once        sync.Once
}

func (c *halfCloseConn) CloseWrite() error {
	c.once.Do(func() { close(c.closedWrite) })
	if closer, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return closer.CloseWrite()
	}
	return nil
}

func TestForwarderPassesAHalfCloseToTheRemote(t *testing.T) {
	sandbox := echoServer(t)
	closedWrite := make(chan struct{})
	dialer := funcDialer(func(ctx context.Context, _ Target) (net.Conn, error) {
		var config net.Dialer
		conn, err := config.DialContext(ctx, "tcp", sandbox.String())
		if err != nil {
			return nil, err
		}
		return &halfCloseConn{Conn: conn, closedWrite: closedWrite}, nil
	})

	events := newCollector()
	forwarder := New(t.Context(), Options{Dialer: dialer, Observe: events.observe})
	defer forwarder.Close()

	remotePort := 4714
	forwarder.Set([]Target{{Port: remotePort}})
	bound := events.await(t, Bound, remotePort)

	conn, err := dialTest(t, bound.Local)
	if err != nil {
		t.Fatalf("dial forwarded port: %v", err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "bye"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("close write: %v", err)
	}
	// The echo comes back after the half-close, which is the point: the
	// connection is not over, only this side's half of it.
	got := make([]byte, 3)
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read after half-close: %v", err)
	}
	if string(got) != "bye" {
		t.Fatalf("echo = %q, want bye", got)
	}
	select {
	case <-closedWrite:
	case <-time.After(5 * time.Second):
		t.Fatal("the half-close never reached the remote conn")
	}
}

func TestForwarderReportsADialFailureWithoutLosingTheBinding(t *testing.T) {
	events := newCollector()
	forwarder := New(t.Context(), Options{
		Dialer: funcDialer(func(context.Context, Target) (net.Conn, error) {
			return nil, errors.New("connection refused")
		}),
		Observe: events.observe,
	})
	defer forwarder.Close()

	remotePort := 4715
	forwarder.Set([]Target{{Port: remotePort}})
	bound := events.await(t, Bound, remotePort)

	conn, err := dialTest(t, bound.Local)
	if err != nil {
		t.Fatalf("dial forwarded port: %v", err)
	}
	defer conn.Close()
	if _, err := io.ReadAll(conn); err != nil {
		t.Fatalf("read: %v", err)
	}

	failed := events.await(t, DialFailed, remotePort)
	if failed.Err == nil {
		t.Fatal("expected the dial failure to carry its error")
	}
	if bindings := forwarder.Bindings(); len(bindings) != 1 || !bindings[0].Active {
		t.Fatalf("bindings = %#v, want the binding kept", bindings)
	}
}

func TestForwarderCloseReleasesTheLocalPorts(t *testing.T) {
	sandbox := echoServer(t)
	events := newCollector()
	forwarder := New(t.Context(), Options{Dialer: dialerTo(sandbox), Observe: events.observe})

	remotePort := 4716
	forwarder.Set([]Target{{Port: remotePort}})
	bound := events.await(t, Bound, remotePort)
	if err := forwarder.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// The port is free again, which is only checkable by taking it.
	listener, err := listenTest(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(bound.Local)))
	if err != nil {
		t.Fatalf("local port %d was not released: %v", bound.Local, err)
	}
	_ = listener.Close()

	// A closed forwarder takes no more work.
	forwarder.Set([]Target{{Port: 4717}})
	if bindings := forwarder.Bindings(); len(bindings) != 1 {
		t.Fatalf("bindings after close = %#v, want the closed set unchanged", bindings)
	}
}

func TestForwarderSkipsPortsOutsideTheValidRange(t *testing.T) {
	forwarder := New(t.Context(), Options{Dialer: funcDialer(func(context.Context, Target) (net.Conn, error) {
		return nil, errors.New("not dialed")
	})})
	defer forwarder.Close()

	forwarder.Set([]Target{{Port: 0}, {Port: -1}, {Port: 70000}})
	if bindings := forwarder.Bindings(); len(bindings) != 0 {
		t.Fatalf("bindings = %#v, want none", bindings)
	}
}

func TestEventStringNamesTheMoveAndTheReason(t *testing.T) {
	for _, testCase := range []struct {
		event Event
		want  string
	}{
		{Event{Kind: Bound, Target: Target{Port: 8080, Protocol: "http"}, Local: 8080}, "listening on 8080 -> sandbox 8080 (http)"},
		{Event{Kind: Bound, Target: Target{Port: 8080}, Local: 8081}, "listening on 8081 -> sandbox 8080 (8080 was taken)"},
		{Event{Kind: Gone, Target: Target{Port: 8080}, Local: 8081}, "sandbox 8080 stopped listening; 8081 is held open"},
		{Event{Kind: Accepted, Target: Target{Port: 8080}, Local: 8081, Peer: "127.0.0.1:5000"}, "8081 -> sandbox 8080: connection from 127.0.0.1:5000"},
		{Event{Kind: DialFailed, Target: Target{Port: 8080}, Local: 8081, Err: fmt.Errorf("refused")}, "8081 -> sandbox 8080: refused"},
	} {
		if got := testCase.event.String(); got != testCase.want {
			t.Errorf("String() = %q, want %q", got, testCase.want)
		}
	}
}

// listenTest and dialTest are the context-carrying forms of net.Listen and
// net.Dial, which the linter requires and every test here wants bound to the
// test's own context anyway.
func listenTest(t *testing.T, address string) (net.Listener, error) {
	t.Helper()
	var config net.ListenConfig
	return config.Listen(t.Context(), "tcp", address)
}

func dialTest(t *testing.T, port int) (net.Conn, error) {
	t.Helper()
	var dialer net.Dialer
	return dialer.DialContext(t.Context(), "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
}
