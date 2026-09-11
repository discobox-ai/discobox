package server

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/execstream/frame"
)

// tcp:connect is not udp:connect. The two tunnels share an authorization
// boundary, but a scope names what it grants (ADR 0109).
func TestAttachUDPTunnelRequiresUDPConnectScope(t *testing.T) {
	publicKey, signToken := sandboxAgentTestSigner(t)
	router, err := NewRouter(testConfig(publicKey))
	if err != nil {
		t.Fatalf("new router: %v", err)
	}

	resp := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/projects/project-1/sandboxes/sandbox-1/udp/attach?host=127.0.0.1&port=53", nil)
	req.Header.Set("Authorization", "Bearer "+signToken("project-1", "sandbox-1", "worker-1", ScopeTCPConnect))
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusForbidden {
		t.Fatalf("GET udp/attach with only tcp:connect status = %d, body = %s", resp.Code, resp.Body.String())
	}
}

func TestAttachUDPTunnelRejectsMissingQueryParams(t *testing.T) {
	publicKey, signToken := sandboxAgentTestSigner(t)
	router, err := NewRouter(testConfig(publicKey))
	if err != nil {
		t.Fatalf("new router: %v", err)
	}

	for _, query := range []string{"", "?host=127.0.0.1", "?port=53", "?host=127.0.0.1&port=0", "?host=127.0.0.1&port=70000"} {
		resp := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/projects/project-1/sandboxes/sandbox-1/udp/attach"+query, nil)
		req.Header.Set("Authorization", "Bearer "+signToken("project-1", "sandbox-1", "worker-1", ScopeUDPConnect))
		router.ServeHTTP(resp, req)
		if resp.Code != http.StatusBadRequest {
			t.Fatalf("GET udp/attach%s status = %d, body = %s", query, resp.Code, resp.Body.String())
		}
	}
}

// udpEcho answers every datagram with itself, from the address it was sent to.
func udpEcho(t *testing.T, address string) net.PacketConn {
	t.Helper()
	conn, err := new(net.ListenConfig).ListenPacket(context.Background(), "udp", address)
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buf := make([]byte, udpDatagramLimit)
		for {
			n, peer, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = conn.WriteTo(buf[:n], peer)
		}
	}()
	return conn
}

func dialUDP(t *testing.T, address string) net.Conn {
	t.Helper()
	var dialer net.Dialer
	conn, err := dialer.DialContext(context.Background(), "udp", address)
	if err != nil {
		t.Fatalf("dial udp: %v", err)
	}
	return conn
}

// Each Input frame is one datagram and each datagram back is one Stdout frame,
// so the boundaries a UDP protocol depends on survive a stream in between.
func TestPumpUDPToFramesCarriesOneDatagramPerFrame(t *testing.T) {
	server := udpEcho(t, "127.0.0.1:0")
	udpConn := dialUDP(t, server.LocalAddr().String())

	wsSide, appSide := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		pumpUDPToFrames(udpConn, wsSide)
	}()

	for _, datagram := range []string{"first", "second"} {
		if err := frame.Write(appSide, frame.Input, []byte(datagram)); err != nil {
			t.Fatalf("write input frame: %v", err)
		}
	}
	for _, want := range []string{"first", "second"} {
		if got := readFrameOfType(t, appSide, frame.Stdout); string(got) != want {
			t.Fatalf("Stdout = %q, want %q: one frame per datagram", got, want)
		}
	}

	// A datagram the size of the largest a UDP payload can be arrives whole.
	large := make([]byte, 65507)
	for i := range large {
		large[i] = byte(i)
	}
	if err := frame.Write(appSide, frame.Input, large); err != nil {
		t.Fatalf("write large input frame: %v", err)
	}
	if got := readFrameOfType(t, appSide, frame.Stdout); len(got) != len(large) {
		t.Fatalf("large datagram came back as %d bytes, want %d", len(got), len(large))
	}

	_ = appSide.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pumpUDPToFrames did not return after the client side closed")
	}
}

// A datagram sent while nothing is listening is refused, and that is not the
// end of the tunnel: a UDP server that is restarting answers the next one.
func TestPumpUDPToFramesOutlivesARefusedDatagram(t *testing.T) {
	placeholder := udpEcho(t, "127.0.0.1:0")
	address := placeholder.LocalAddr().String()
	_ = placeholder.Close()

	udpConn := dialUDP(t, address)
	wsSide, appSide := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		pumpUDPToFrames(udpConn, wsSide)
	}()
	defer func() {
		_ = appSide.Close()
		<-done
	}()

	// Refused: nothing is bound. The ICMP error lands on the socket, which is
	// what would end a pump that treated it as fatal.
	if err := frame.Write(appSide, frame.Input, []byte("anyone?")); err != nil {
		t.Fatalf("write input frame: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	// The server comes up on the same port, and the same tunnel reaches it. A
	// datagram can race the bind, so it is sent until one is answered.
	udpEcho(t, address)
	answered := make(chan []byte, 1)
	go func() { answered <- readFrameOfType(t, appSide, frame.Stdout) }()
	deadline := time.After(5 * time.Second)
	for {
		if err := frame.Write(appSide, frame.Input, []byte("back")); err != nil {
			t.Fatalf("write input frame: %v", err)
		}
		select {
		case got := <-answered:
			if string(got) != "back" {
				t.Fatalf("Stdout = %q, want %q", got, "back")
			}
			return
		case <-deadline:
			t.Fatal("the tunnel never reached the server after a refused datagram")
		case <-time.After(100 * time.Millisecond):
		}
	}
}
