package bridge_test

import (
	"context"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/wstunnel"
	"github.com/discobox-ai/discobox/proxy/bridge"
)

// TestMain trusts httptest's certificate as a system root, as a sandbox trusts
// the public certificate of the edge its pool sits behind. httptest serves the
// same certificate from every TLS server, and the system roots are read once,
// on first use, so this has to happen before any test runs.
func TestMain(m *testing.M) {
	edge := httptest.NewTLSServer(http.NotFoundHandler())
	block := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: edge.Certificate().Raw})
	edge.Close()
	dir, err := os.MkdirTemp("", "edge-roots")
	if err != nil {
		panic(err)
	}
	roots := filepath.Join(dir, "roots.pem")
	if err := os.WriteFile(roots, block, 0o600); err != nil {
		panic(err)
	}
	_ = os.Setenv("SSL_CERT_FILE", roots)
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// tunnelEdge stands in for a pool behind an edge that terminates TLS: an HTTPS
// server whose tunnels are spliced onto the pool service's own listener, as the
// pool's tunnel listener splices them. It counts the tunnels it accepted, and
// relays TCP so a test can cut every connection the way an edge cuts an idle
// one.
type tunnelEdge struct {
	url     string
	tunnels atomic.Int32

	mu    sync.Mutex
	conns []net.Conn
}

func newTunnelEdge(t *testing.T, pool string) *tunnelEdge {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	e := &tunnelEdge{}
	handler := wstunnel.Handler(ctx, func(stream net.Conn) {
		defer func() { _ = stream.Close() }()
		var dialer net.Dialer
		target, err := dialer.DialContext(ctx, "tcp", pool)
		if err != nil {
			return
		}
		defer func() { _ = target.Close() }()
		done := make(chan struct{})
		go func() {
			_, _ = io.Copy(target, stream)
			_ = target.Close()
			close(done)
		}()
		_, _ = io.Copy(stream, target)
		_ = stream.Close()
		<-done
	})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only upgrades count: whatever else probes a new listener is not a
		// tunnel.
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			e.tunnels.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)

	var listenConfig net.ListenConfig
	relay, err := listenConfig.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = relay.Close() })
	go func() {
		for {
			client, err := relay.Accept()
			if err != nil {
				return
			}
			var dialer net.Dialer
			upstream, err := dialer.DialContext(ctx, "tcp", server.Listener.Addr().String())
			if err != nil {
				_ = client.Close()
				continue
			}
			e.mu.Lock()
			e.conns = append(e.conns, upstream)
			e.mu.Unlock()
			// Only the client's next write finds out the upstream is gone.
			go func() { _, _ = io.Copy(client, upstream) }()
			go func() {
				_, _ = io.Copy(upstream, client)
				_ = client.Close()
			}()
		}
	}()
	// 127.0.0.1 is a name httptest's certificate covers.
	e.url = "wss://" + relay.Addr().String() + "/tunnel/proxy"
	return e
}

// cut drops the edge's side of every connection so far.
func (e *tunnelEdge) cut() {
	e.mu.Lock()
	conns := e.conns
	e.conns = nil
	e.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

// A sandbox behind an edge that terminates TLS still authenticates with its
// client certificate: the mTLS runs inside the tunnel, end to end, and the pool
// sees the same identity it would over TCP (ADR 26-10-23-719 §1).
func TestDialerCarriesMTLSThroughATunnelAndReusesIt(t *testing.T) {
	prepared := poolMaterial(t)
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	servePool(t, listener, prepared.Bundle)
	edge := newTunnelEdge(t, listener.Addr().String())

	dialer, err := bridge.NewDialer(dialConfig(prepared.Clients["sandbox-1"], edge.url, poolServerName))
	if err != nil {
		t.Fatalf("NewDialer: %v", err)
	}
	if got := dialer.ServerURL(); got != "https://"+poolServerName {
		t.Fatalf("ServerURL = %q, want the pool's name", got)
	}
	if got := dialer.TransportHost(); got != "127.0.0.1" {
		t.Fatalf("TransportHost = %q, want the edge's host", got)
	}
	for i := range 2 {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		conn, err := dialer.Dial(ctx)
		cancel()
		if err != nil {
			t.Fatalf("Dial %d: %v", i, err)
		}
		reader, identity, _ := readIdentity(t, conn)
		if identity != "sandbox-1" {
			t.Fatalf("pool saw client %q, want sandbox-1", identity)
		}
		if _, err := io.WriteString(conn, "ping\n"); err != nil {
			t.Fatalf("write: %v", err)
		}
		if echo, err := reader.ReadString('\n'); err != nil || echo != "ping\n" {
			t.Fatalf("echo = %q, %v", echo, err)
		}
		_ = conn.Close()
		// The next dial finds the tunnel idle only once the end is answered.
		time.Sleep(50 * time.Millisecond)
	}
	if got := edge.tunnels.Load(); got != 1 {
		t.Fatalf("edge accepted %d tunnels for two connections in turn, want 1", got)
	}
}

// A tunnel the edge cut while it sat idle fails in the handshake, before any of
// the caller's bytes are sent, and the dial is retried on a new tunnel.
func TestDialerRetriesOnceOnANewTunnelWhenAReusedOneWasCut(t *testing.T) {
	prepared := poolMaterial(t)
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	servePool(t, listener, prepared.Bundle)
	edge := newTunnelEdge(t, listener.Addr().String())
	dialer, err := bridge.NewDialer(dialConfig(prepared.Clients["sandbox-1"], edge.url, poolServerName))
	if err != nil {
		t.Fatalf("NewDialer: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := dialer.Dial(ctx)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	readIdentity(t, conn)
	_ = conn.Close()
	time.Sleep(50 * time.Millisecond)
	edge.cut()

	conn, err = dialer.Dial(ctx)
	if err != nil {
		t.Fatalf("Dial after the edge cut the idle tunnel: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, identity, _ := readIdentity(t, conn); identity != "sandbox-1" {
		t.Fatalf("pool saw client %q, want sandbox-1", identity)
	}
	if got := edge.tunnels.Load(); got != 2 {
		t.Fatalf("edge accepted %d tunnels, want 2: the cut one and its replacement", got)
	}
}

func TestNewDialerWSSNeedsThePoolsServerName(t *testing.T) {
	material := poolMaterial(t).Clients["sandbox-1"]
	_, err := bridge.NewDialer(dialConfig(material, "wss://edge.example.com/tunnel/proxy", ""))
	if err == nil || !strings.Contains(err.Error(), "server name") {
		t.Fatalf("NewDialer without a server name = %v, want it refused", err)
	}
}
