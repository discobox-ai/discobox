package bridge_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/wstunnel"
	"github.com/discobox-ai/discobox/proxy/bridge"
)

// credentialedEdge is a provider's edge that admits only a holder of its port
// credential, as Archil's does (ADR 26-11-16-619 §2): it offers HTTP/2, checks
// the credential on HTTP/1.1, and answers 401 to a request without it before
// anything reaches the pool. What it admits is spliced onto the pool's
// listener, as the pool's tunnel listener splices it.
type credentialedEdge struct {
	server *httptest.Server

	mu       sync.Mutex
	token    string
	hosts    []string
	refused  int
	admitted int
}

const edgeHeader = "X-Edge-Token"

func newCredentialedEdge(t *testing.T, pool, token string) *credentialedEdge {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	e := &credentialedEdge{token: token}
	tunnel := wstunnel.Handler(ctx, func(stream net.Conn) {
		defer func() { _ = stream.Close() }()
		var dialer net.Dialer
		target, err := dialer.DialContext(ctx, "tcp", pool)
		if err != nil {
			return
		}
		defer func() { _ = target.Close() }()
		go func() { _, _ = io.Copy(target, stream) }()
		_, _ = io.Copy(stream, target)
	})
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		admit := r.ProtoMajor == 1 && r.Header.Get(edgeHeader) == e.token
		e.hosts = append(e.hosts, r.Host)
		if admit {
			e.admitted++
		} else {
			e.refused++
		}
		e.mu.Unlock()
		if !admit {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		tunnel.ServeHTTP(w, r)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	e.server = server
	return e
}

func (e *credentialedEdge) rotate(token string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.token = token
}

func (e *credentialedEdge) counts() (admitted, refused int, hosts []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.admitted, e.refused, append([]string(nil), e.hosts...)
}

func writeEdge(t *testing.T, path string, edge bridge.Edge) {
	t.Helper()
	data, err := json.Marshal(edge)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A sandbox's tunnel through an edge that answers 401 without its credential
// carries the credential the edge file names, over HTTP/1.1, and dials the
// address the file names in place of the edge's name, resolving nothing: the
// edge sees its own name as the Host and the TLS server name, so it routes as
// it would by name. A credential renewed in the file is sent on the next
// tunnel, with no new dialer.
func TestDialerTunnelsThroughACredentialedEdgeAtItsGivenAddress(t *testing.T) {
	prepared := poolMaterial(t)
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	servePool(t, listener, prepared.Bundle)
	edge := newCredentialedEdge(t, listener.Addr().String(), "first")
	_, port, err := net.SplitHostPort(edge.server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	// example.com is a name httptest's certificate covers, and it does not
	// lead to this edge: only the address the file names does.
	url := "wss://example.com:" + port + "/tunnel/proxy"
	edgePath := filepath.Join(t.TempDir(), bridge.EdgeFile)

	// Without the credential the edge refuses the tunnel.
	writeEdge(t, edgePath, bridge.Edge{Address: "127.0.0.1"})
	config := dialConfig(prepared.Clients["sandbox-1"], url, poolServerName)
	config.EdgePath = edgePath
	dialer, err := bridge.NewDialer(config)
	if err != nil {
		t.Fatalf("NewDialer: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if conn, err := dialer.Dial(ctx); err == nil {
		_ = conn.Close()
		t.Fatal("Dial without the edge's credential went through, want the edge's 401")
	} else if !strings.Contains(err.Error(), "401") {
		t.Fatalf("Dial without the edge's credential = %v, want the edge's 401", err)
	}

	writeEdge(t, edgePath, bridge.Edge{Address: "127.0.0.1", Header: edgeHeader, Token: "first"})
	conn, err := dialer.Dial(ctx)
	if err != nil {
		t.Fatalf("Dial with the edge's credential: %v", err)
	}
	if _, identity, _ := readIdentity(t, conn); identity != "sandbox-1" {
		t.Fatalf("pool saw client %q, want sandbox-1", identity)
	}

	// Renewed: the open tunnel is busy, so the next stream opens a new one,
	// which carries the new credential.
	edge.rotate("second")
	writeEdge(t, edgePath, bridge.Edge{Address: "127.0.0.1", Header: edgeHeader, Token: "second"})
	again, err := dialer.Dial(ctx)
	if err != nil {
		t.Fatalf("Dial with the renewed credential: %v", err)
	}
	if _, identity, _ := readIdentity(t, again); identity != "sandbox-1" {
		t.Fatalf("pool saw client %q, want sandbox-1", identity)
	}
	_ = again.Close()
	_ = conn.Close()

	admitted, refused, hosts := edge.counts()
	if admitted != 2 || refused != 1 {
		t.Fatalf("edge admitted %d and refused %d tunnels, want 2 and 1", admitted, refused)
	}
	for _, host := range hosts {
		if host != "example.com:"+port {
			t.Fatalf("edge saw Host %q, want its own name, never the address", host)
		}
	}
}

// An edge file answers a tunnel alone: an https pool URL is dialed by its own
// rules (Address).
func TestNewDialerRefusesAnEdgeFileForAnHTTPSURL(t *testing.T) {
	config := dialConfig(poolMaterial(t).Clients["sandbox-1"], "https://pool.example.com:443", "")
	config.EdgePath = filepath.Join(t.TempDir(), bridge.EdgeFile)
	if _, err := bridge.NewDialer(config); err == nil || !strings.Contains(err.Error(), "edge file") {
		t.Fatalf("NewDialer with an edge file for an https URL = %v, want it refused", err)
	}
}
