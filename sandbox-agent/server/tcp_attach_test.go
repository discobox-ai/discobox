package server

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/execstream/frame"
)

func TestAttachTCPTunnelRequiresTCPConnectScope(t *testing.T) {
	publicKey, signToken := sandboxAgentTestSigner(t)
	router, err := NewRouter(testConfig(publicKey))
	if err != nil {
		t.Fatalf("new router: %v", err)
	}

	resp := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/projects/project-1/sandboxes/sandbox-1/tcp/attach?host=127.0.0.1&port=1", nil)
	req.Header.Set("Authorization", "Bearer "+signToken("project-1", "sandbox-1", "worker-1", ScopeExecRead, ScopeExecWrite))
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusForbidden {
		t.Fatalf("GET tcp/attach without tcp:connect status = %d, body = %s", resp.Code, resp.Body.String())
	}
}

func TestAttachTCPTunnelRejectsMissingQueryParams(t *testing.T) {
	publicKey, signToken := sandboxAgentTestSigner(t)
	router, err := NewRouter(testConfig(publicKey))
	if err != nil {
		t.Fatalf("new router: %v", err)
	}

	resp := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/projects/project-1/sandboxes/sandbox-1/tcp/attach", nil)
	req.Header.Set("Authorization", "Bearer "+signToken("project-1", "sandbox-1", "worker-1", ScopeTCPConnect))
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("GET tcp/attach with no host/port status = %d, body = %s", resp.Code, resp.Body.String())
	}
}

func TestAttachTCPTunnelRejectsUnreachableTarget(t *testing.T) {
	publicKey, signToken := sandboxAgentTestSigner(t)
	router, err := NewRouter(testConfig(publicKey))
	if err != nil {
		t.Fatalf("new router: %v", err)
	}

	// Port 0 on loopback is never listening; the dial must fail fast, before
	// any websocket upgrade is attempted.
	ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().(*net.TCPAddr)
	ln.Close()

	resp := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/api/projects/project-1/sandboxes/sandbox-1/tcp/attach?host=127.0.0.1&port="+strconv.Itoa(addr.Port), nil)
	req.Header.Set("Authorization", "Bearer "+signToken("project-1", "sandbox-1", "worker-1", ScopeTCPConnect))
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusBadGateway {
		t.Fatalf("GET tcp/attach against a closed port status = %d, body = %s", resp.Code, resp.Body.String())
	}
}

// TestPumpTCPToFramesRoundTrip exercises the frame<->TCP bridging directly:
// Input frames become writes to the TCP conn, TCP reads become Stdout
// frames, and CloseInput half-closes the TCP conn (ADR 0024 §4) rather than
// fully closing it.
func TestPumpTCPToFramesRoundTrip(t *testing.T) {
	ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	echoDone := make(chan struct{})
	go func() {
		defer close(echoDone)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Echo everything until the client half-closes, then reply once more
		// to prove the connection stayed open for reading after CloseInput.
		buf := make([]byte, 4096)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				_, _ = conn.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		_, _ = conn.Write([]byte("after-close"))
	}()

	var dialer net.Dialer
	tcpConn, err := dialer.DialContext(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	wsSide, appSide := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		pumpTCPToFrames(tcpConn, wsSide)
	}()

	// Input -> TCP -> Stdout frame back.
	if err := frame.Write(appSide, frame.Input, []byte("hello")); err != nil {
		t.Fatalf("write input frame: %v", err)
	}
	f, err := readFrameWithTimeout(t, appSide)
	if err != nil {
		t.Fatalf("read stdout frame: %v", err)
	}
	if f.Type != frame.Stdout || string(f.Payload) != "hello" {
		t.Fatalf("frame = %+v, want Stdout %q", f, "hello")
	}

	// CloseInput half-closes; the echo goroutine's final write must still
	// arrive.
	if err := frame.Write(appSide, frame.CloseInput, nil); err != nil {
		t.Fatalf("write close-input frame: %v", err)
	}
	f, err = readFrameWithTimeout(t, appSide)
	if err != nil {
		t.Fatalf("read post-close frame: %v", err)
	}
	if f.Type != frame.Stdout || string(f.Payload) != "after-close" {
		t.Fatalf("frame after close-input = %+v, want Stdout %q", f, "after-close")
	}

	_ = appSide.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pumpTCPToFrames did not return after the app side closed")
	}
	<-echoDone
}

func readFrameWithTimeout(t *testing.T, conn net.Conn) (frame.Frame, error) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	f, err := frame.Read(conn)
	_ = conn.SetReadDeadline(time.Time{})
	return f, err
}

// A target that closes its side of a forwarded connection has not closed the
// tunnel: the client may still be sending, and used to be cut off when this
// closed the websocket outright (ADR 0024 §4).
func TestPumpTCPToFramesHalfClosesOnTargetEOF(t *testing.T) {
	target, agentSide := net.Pipe()
	clientSide, wsSide := net.Pipe()

	go pumpTCPToFrames(agentSide, wsSide)

	// The target answers and hangs up its own side.
	go func() {
		_, _ = target.Write([]byte("answered"))
		_ = target.Close()
	}()

	// The client sees the data, then the half-close, and the tunnel lives on.
	if got := readFrameOfType(t, clientSide, frame.Stdout); string(got) != "answered" {
		t.Fatalf("Stdout = %q, want %q", got, "answered")
	}
	if got := readFrameOfType(t, clientSide, frame.CloseOutput); len(got) != 0 {
		t.Fatalf("CloseOutput carried %q, want nothing", got)
	}
}

// readFrameOfType reads until a frame of the wanted type arrives, failing
// rather than blocking if it never does.
func readFrameOfType(t *testing.T, conn net.Conn, want byte) []byte {
	t.Helper()
	type result struct {
		payload []byte
		err     error
	}
	done := make(chan result, 1)
	go func() {
		for {
			read, err := frame.Read(conn)
			if err != nil {
				done <- result{nil, err}
				return
			}
			if read.Type == want {
				done <- result{read.Payload, nil}
				return
			}
		}
	}()
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("waiting for frame type %d: %v", want, res.err)
		}
		return res.payload
	case <-time.After(10 * time.Second):
		t.Fatalf("frame type %d never arrived", want)
		return nil
	}
}
