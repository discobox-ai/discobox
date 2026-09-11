package portforward

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// udpEchoServer stands in for a UDP server inside the sandbox. It answers each
// datagram with the datagram, prefixed by the address it came from, so a test
// can tell which tunnel carried it.
func udpEchoServer(t *testing.T) net.Addr {
	t.Helper()
	conn, err := new(net.ListenConfig).ListenPacket(t.Context(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buf := make([]byte, datagramLimit)
		for {
			n, peer, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = conn.WriteTo(append([]byte(peer.String()+"|"), buf[:n]...), peer)
		}
	}()
	return conn.LocalAddr()
}

// udpDialerTo dials a connected UDP socket at addr, which is exactly the
// datagram-per-Read, datagram-per-Write conn a UDP target's Dialer owes the
// forwarder. It counts the dials, one per flow.
func udpDialerTo(addr net.Addr, dials *atomic.Int32) Dialer {
	return funcDialer(func(ctx context.Context, target Target) (net.Conn, error) {
		if target.Network != UDP {
			return nil, errors.New("dialed a UDP target as " + string(target.Network))
		}
		dials.Add(1)
		var dialer net.Dialer
		return dialer.DialContext(ctx, "udp", addr.String())
	})
}

func dialUDPTest(t *testing.T, port int) net.Conn {
	t.Helper()
	var dialer net.Dialer
	conn, err := dialer.DialContext(t.Context(), "udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("dial udp: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// exchange sends one datagram and returns the one that comes back.
func exchange(t *testing.T, conn net.Conn, datagram string) string {
	t.Helper()
	if _, err := conn.Write([]byte(datagram)); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, datagramLimit)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read reply to %q: %v", datagram, err)
	}
	return string(buf[:n])
}

// freeUDPPort is a port nothing has bound for UDP, found by binding one and
// letting it go.
func freeUDPPort(t *testing.T) int {
	t.Helper()
	conn, err := new(net.ListenConfig).ListenPacket(t.Context(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	_ = conn.Close()
	return port
}

func TestForwarderForwardsDatagramsWithTheirBoundaries(t *testing.T) {
	var dials atomic.Int32
	sandbox := udpEchoServer(t)
	events := newCollector()
	forwarder := New(t.Context(), Options{Dialer: udpDialerTo(sandbox, &dials), Observe: events.observe})
	defer forwarder.Close()

	remotePort := freeUDPPort(t)
	forwarder.Set([]Target{{Network: UDP, Port: remotePort, Protocol: "udp"}})
	bound := events.awaitUDP(t, Bound, remotePort)

	client := dialUDPTest(t, bound.Local)
	for _, datagram := range []string{"one", "two", "three"} {
		reply := exchange(t, client, datagram)
		if _, body, _ := strings.Cut(reply, "|"); body != datagram {
			t.Fatalf("reply = %q, want the echo of %q alone", reply, datagram)
		}
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("dialed %d tunnels for one peer, want 1", got)
	}
	if accepted := events.awaitUDP(t, Accepted, remotePort); accepted.Peer != client.LocalAddr().String() {
		t.Fatalf("flow peer = %q, want %q", accepted.Peer, client.LocalAddr())
	}
}

// Each local peer is its own flow, with its own tunnel and so its own source
// port inside the remote — which is how the server there tells clients apart,
// and how each reply finds its way back to the peer that caused it.
func TestForwarderGivesEachPeerItsOwnFlow(t *testing.T) {
	var dials atomic.Int32
	sandbox := udpEchoServer(t)
	events := newCollector()
	forwarder := New(t.Context(), Options{Dialer: udpDialerTo(sandbox, &dials), Observe: events.observe})
	defer forwarder.Close()

	remotePort := freeUDPPort(t)
	forwarder.Set([]Target{{Network: UDP, Port: remotePort}})
	bound := events.awaitUDP(t, Bound, remotePort)

	first := dialUDPTest(t, bound.Local)
	second := dialUDPTest(t, bound.Local)
	firstSource, _, _ := strings.Cut(exchange(t, first, "a"), "|")
	secondSource, _, _ := strings.Cut(exchange(t, second, "b"), "|")
	if firstSource == secondSource {
		t.Fatalf("both peers reached the server from %s; each flow needs its own source", firstSource)
	}
	if got := dials.Load(); got != 2 {
		t.Fatalf("dialed %d tunnels for two peers, want 2", got)
	}
}

// A flow with nothing to say is closed, cleanly, and the next datagram from
// the same peer opens a new one.
func TestForwarderClosesAnIdleFlowAndRedialsOnTheNextDatagram(t *testing.T) {
	var dials atomic.Int32
	sandbox := udpEchoServer(t)
	events := newCollector()
	forwarder := New(t.Context(), Options{Dialer: udpDialerTo(sandbox, &dials), Observe: events.observe})
	forwarder.flowIdle = 50 * time.Millisecond
	defer forwarder.Close()

	remotePort := freeUDPPort(t)
	forwarder.Set([]Target{{Network: UDP, Port: remotePort}})
	bound := events.awaitUDP(t, Bound, remotePort)

	client := dialUDPTest(t, bound.Local)
	exchange(t, client, "hello")
	if closed := events.awaitUDP(t, Closed, remotePort); closed.Err != nil {
		t.Fatalf("an idle flow ended with %v, want a clean close", closed.Err)
	}

	exchange(t, client, "again")
	if got := dials.Load(); got != 2 {
		t.Fatalf("dialed %d tunnels, want a second one after the first went idle", got)
	}
}

// A tunnel that cannot be opened is reported, the binding stays, and the peer
// is tried again on a later datagram rather than on every one.
func TestForwarderRedialsAFailedFlowAfterAPause(t *testing.T) {
	var dials atomic.Int32
	events := newCollector()
	forwarder := New(t.Context(), Options{
		Dialer: funcDialer(func(context.Context, Target) (net.Conn, error) {
			dials.Add(1)
			return nil, errors.New("sandbox is not reachable")
		}),
		Observe: events.observe,
	})
	forwarder.flowRedial = 100 * time.Millisecond
	defer forwarder.Close()

	remotePort := freeUDPPort(t)
	forwarder.Set([]Target{{Network: UDP, Port: remotePort}})
	bound := events.awaitUDP(t, Bound, remotePort)
	client := dialUDPTest(t, bound.Local)

	// A burst inside the pause is one dial, not one per datagram.
	for range 5 {
		if _, err := client.Write([]byte("x")); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if failed := events.awaitUDP(t, DialFailed, remotePort); failed.Err == nil {
		t.Fatal("expected the dial failure to carry its error")
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("dialed %d times for a burst inside the pause, want 1", got)
	}

	deadline := time.After(5 * time.Second)
	for dials.Load() < 2 {
		if _, err := client.Write([]byte("x")); err != nil {
			t.Fatalf("write: %v", err)
		}
		select {
		case <-deadline:
			t.Fatal("the peer was never tried again after the pause")
		case <-time.After(20 * time.Millisecond):
		}
	}
	if bindings := forwarder.Bindings(); len(bindings) != 1 || !bindings[0].Active {
		t.Fatalf("bindings = %#v, want the binding kept", bindings)
	}
}

// The TCP and UDP ports of one number are two bindings in two port spaces, so
// both get the number itself, and each comes and goes on its own.
func TestForwarderBindsTheTCPAndUDPPortsOfANumberIndependently(t *testing.T) {
	var dials atomic.Int32
	events := newCollector()
	forwarder := New(t.Context(), Options{Dialer: udpDialerTo(udpEchoServer(t), &dials), Observe: events.observe})
	defer forwarder.Close()

	port := freeUDPPort(t)
	if listener, err := listenTest(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(port))); err != nil {
		t.Skipf("port %d is free for UDP but not TCP: %v", port, err)
	} else {
		_ = listener.Close()
	}
	forwarder.Set([]Target{{Port: port, Protocol: "tcp"}, {Network: UDP, Port: port, Protocol: "udp"}})

	bindings := forwarder.Bindings()
	if len(bindings) != 2 {
		t.Fatalf("bindings = %#v, want one per transport", bindings)
	}
	if bindings[0].Target.Network != TCP || bindings[1].Target.Network != UDP {
		t.Fatalf("bindings = %#v, want TCP then UDP", bindings)
	}
	for _, binding := range bindings {
		if binding.Local != port {
			t.Errorf("%s binding is on %d, want %d: the port spaces do not compete", binding.Target.Network, binding.Local, port)
		}
	}

	forwarder.Set([]Target{{Port: port, Protocol: "tcp"}})
	if gone := events.awaitUDP(t, Gone, port); gone.Target.Network != UDP {
		t.Fatalf("gone = %+v, want the UDP port", gone)
	}
	if n := events.count(Gone, port); n != 1 {
		t.Fatalf("%d ports of %d went away, want only the UDP one", n, port)
	}
	bindings = forwarder.Bindings()
	if !bindings[0].Active || bindings[1].Active {
		t.Fatalf("bindings = %#v, want TCP active and UDP held", bindings)
	}
}

// A UDP client of "localhost" may send to ::1, and has no way to learn it
// should have tried 127.0.0.1, so a loopback binding answers on both.
func TestForwarderAnswersAUDPClientOnEitherLoopback(t *testing.T) {
	if conn, err := new(net.ListenConfig).ListenPacket(t.Context(), "udp", "[::1]:0"); err != nil {
		t.Skipf("no IPv6 loopback here: %v", err)
	} else {
		_ = conn.Close()
	}
	var dials atomic.Int32
	events := newCollector()
	forwarder := New(t.Context(), Options{Dialer: udpDialerTo(udpEchoServer(t), &dials), Observe: events.observe})
	defer forwarder.Close()

	remotePort := freeUDPPort(t)
	forwarder.Set([]Target{{Network: UDP, Port: remotePort}})
	bound := events.awaitUDP(t, Bound, remotePort)

	for _, host := range []string{"127.0.0.1", "::1"} {
		var dialer net.Dialer
		client, err := dialer.DialContext(t.Context(), "udp", net.JoinHostPort(host, strconv.Itoa(bound.Local)))
		if err != nil {
			t.Fatalf("dial %s: %v", host, err)
		}
		if _, body, _ := strings.Cut(exchange(t, client, "hi "+host), "|"); body != "hi "+host {
			t.Fatalf("reply over %s = %q", host, body)
		}
		_ = client.Close()
	}
}

// A flow is not retired as idle while a datagram is queued for it — that
// datagram arrived as the timer fired, and ending the flow would drop it. Once
// retired, the peer's next datagram starts a new flow rather than joining the
// old one on its way out.
func TestAFlowWithADatagramQueuedIsNotRetiredAsIdle(t *testing.T) {
	table := &flowTable{flows: map[string]*flow{}}
	peer := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5000}

	first := table.deliver(nil, peer, []byte("one"))
	if first == nil {
		t.Fatal("the first datagram from a peer did not start a flow")
	}
	if table.retire(first, true) {
		t.Fatal("an idle retire succeeded with a datagram still queued")
	}
	<-first.queue
	if !table.retire(first, true) {
		t.Fatal("an idle retire failed with nothing queued")
	}
	second := table.deliver(nil, peer, []byte("two"))
	if second == nil || second == first {
		t.Fatal("a datagram after the flow retired did not start a new one")
	}
	// A retire that is not about idleness always succeeds, and only takes the
	// flow it names out of the table.
	if !table.retire(first, false) || table.flows[peer.String()] != second {
		t.Fatal("retiring the old flow again took out the new one")
	}
}
