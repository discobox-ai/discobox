package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/discobox-ai/discobox/cli/internal/portforward"
	"github.com/discobox-ai/discobox/execstream/frame"
)

// tcpTunnelTestServer stands in for the control-plane route, speaking the
// frames the sandbox-agent speaks: Input in, Stdout back, CloseInput for a
// half-close.
func tcpTunnelTestServer(t *testing.T, closedWrite chan<- struct{}) *httptest.Server {
	t.Helper()
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/tcp/attach") {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("host") == "" || r.URL.Query().Get("port") == "" {
			http.Error(w, `{"error":"host and a valid port query parameter are required"}`, http.StatusBadRequest)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")
		conn.SetReadLimit(-1)
		stream := websocket.NetConn(r.Context(), conn, websocket.MessageBinary)
		for {
			read, err := frame.Read(stream)
			if err != nil {
				return
			}
			switch read.Type {
			case frame.Input:
				if err := frame.Write(stream, frame.Stdout, read.Payload); err != nil {
					return
				}
			case frame.CloseInput:
				once.Do(func() { close(closedWrite) })
			}
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestSandboxTCPDialerCarriesBytesAndAHalfClose(t *testing.T) {
	closedWrite := make(chan struct{})
	server := tcpTunnelTestServer(t, closedWrite)
	app := &App{serverURL: server.URL, autoStart: autoStartServerFalse}

	dialer, err := app.sandboxTCPDialer("proj-1", "sbx-1")
	if err != nil {
		t.Fatalf("sandbox tcp dialer: %v", err)
	}
	conn, err := dialer.DialPort(t.Context(), portforward.Target{Host: "127.0.0.1", Port: 8080})
	if err != nil {
		t.Fatalf("dial port: %v", err)
	}
	defer conn.Close()

	if _, err := io.WriteString(conn, "ping"); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "ping" {
		t.Fatalf("read %q, want ping", got)
	}

	// A frame does not have to fit the caller's buffer, so read the next one
	// back in pieces.
	if _, err := io.WriteString(conn, "hello"); err != nil {
		t.Fatalf("write: %v", err)
	}
	part := make([]byte, 2)
	if _, err := io.ReadFull(conn, part); err != nil {
		t.Fatalf("partial read: %v", err)
	}
	rest := make([]byte, 3)
	if _, err := io.ReadFull(conn, rest); err != nil {
		t.Fatalf("rest read: %v", err)
	}
	if string(part)+string(rest) != "hello" {
		t.Fatalf("read %q%q, want hello", part, rest)
	}

	if err := conn.(*tcpTunnelConn).CloseWrite(); err != nil {
		t.Fatalf("close write: %v", err)
	}
	select {
	case <-closedWrite:
	case <-time.After(5 * time.Second):
		t.Fatal("the half-close never reached the tunnel")
	}
}

// A refused tunnel has to say why: the handshake response body is the only
// place the reason exists, and it is gone once the dial error is returned.
func TestSandboxTCPDialerReportsTheHandshakeError(t *testing.T) {
	closedWrite := make(chan struct{})
	server := tcpTunnelTestServer(t, closedWrite)
	app := &App{serverURL: server.URL, autoStart: autoStartServerFalse}

	dialer, err := app.sandboxTCPDialer("proj-1", "sbx-1")
	if err != nil {
		t.Fatalf("sandbox tcp dialer: %v", err)
	}
	_, err = dialer.DialPort(t.Context(), portforward.Target{Host: "", Port: 8080})
	if err == nil {
		t.Fatal("expected the dial to fail")
	}
	if !strings.Contains(err.Error(), "host and a valid port") {
		t.Fatalf("error = %v, want the server's reason", err)
	}
}

// The mirror of the half-close above: the far end finishes sending and this
// conn reports EOF, the way any net.Conn does, while remaining writable. A
// tunnel that closed outright instead would look to a caller like a connection
// dropped mid-request (ADR 0024 §4).
func TestSandboxTCPDialerReportsTheFarEndsHalfClose(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")
		stream := websocket.NetConn(r.Context(), conn, websocket.MessageBinary)
		if err := frame.Write(stream, frame.Stdout, []byte("answered")); err != nil {
			return
		}
		if err := frame.Write(stream, frame.CloseOutput, nil); err != nil {
			return
		}
		// Still reading: a half-closed far end can receive.
		for {
			if _, err := frame.Read(stream); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)

	app := &App{serverURL: server.URL, autoStart: autoStartServerFalse}
	dialer, err := app.sandboxTCPDialer("proj-1", "sbx-1")
	if err != nil {
		t.Fatalf("sandbox tcp dialer: %v", err)
	}
	conn, err := dialer.DialPort(t.Context(), portforward.Target{Host: "127.0.0.1", Port: 8080})
	if err != nil {
		t.Fatalf("dial port: %v", err)
	}
	defer conn.Close()

	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read to EOF: %v", err)
	}
	if string(got) != "answered" {
		t.Fatalf("read %q, want %q", got, "answered")
	}
	// EOF on the read half says nothing about the write half.
	if _, err := io.WriteString(conn, "still writing"); err != nil {
		t.Fatalf("write after the far end's half-close: %v", err)
	}
}
