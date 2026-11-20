package sandboxruntime

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"
)

// A request through a Dialer's transport is not sent Connection: close, so a
// sandbox's server that refuses it before reading its body still drains that
// body and the refusal arrives as itself rather than as a reset (#106); and
// the connection is closed once the response is read, so none is kept idle.
func TestDialerTransportKeepsAliveAndHoldsNoIdleConnection(t *testing.T) {
	type seen struct {
		addr  string
		close bool
	}
	requests := make(chan seen, 2)
	// closed is told each connection the server saw close, by its client's
	// address: the sandbox agent's port probe opens one of its own when this
	// test runs inside a discobox (see the root REVIEW.md).
	var mu sync.Mutex
	closed := map[string]chan struct{}{}
	closedChan := func(addr string) chan struct{} {
		mu.Lock()
		defer mu.Unlock()
		if closed[addr] == nil {
			closed[addr] = make(chan struct{})
		}
		return closed[addr]
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("User-Agent") == "discobox-sandbox-agent (port probe)" {
			return
		}
		requests <- seen{addr: req.RemoteAddr, close: req.Close}
		if req.ContentLength > 0 {
			http.Error(w, "no", http.StatusUnauthorized) // the body is never read
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	server.Config.ConnState = func(conn net.Conn, state http.ConnState) {
		if state != http.StateClosed {
			return
		}
		ch := closedChan(conn.RemoteAddr().String())
		mu.Lock()
		defer mu.Unlock()
		select {
		case <-ch: // httptest reports a close it makes as well
		default:
			close(ch)
		}
	}
	server.Start()
	t.Cleanup(server.Close)
	transport := (&Dialer{DialContext: func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", server.Listener.Addr().String())
	}}).Transport()

	for _, tc := range []struct {
		name   string
		body   io.Reader
		status int
	}{
		{name: "a request answered in full", status: http.StatusNoContent},
		{name: "a refusal over an unread body", body: strings.NewReader(strings.Repeat("x", 64<<10)), status: http.StatusUnauthorized},
	} {
		method := http.MethodGet
		if tc.body != nil {
			method = http.MethodPut
		}
		req, err := http.NewRequestWithContext(context.Background(), method, HTTPURL(SandboxAgentPort, "/runtime-config").String(), tc.body)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := (&http.Client{Transport: transport}).Do(req)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != tc.status {
			t.Fatalf("%s: status %d, want %d", tc.name, resp.StatusCode, tc.status)
		}
		got := <-requests
		if got.close {
			t.Fatalf("%s: the request was sent Connection: close", tc.name)
		}
		select {
		case <-closedChan(got.addr):
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the connection was kept after its response was read", tc.name)
		}
	}
}

// An edge routes on Host and admits only HTTP/1.1 requests with its own
// credential. The sandbox agent independently checks the pool's bearer token.
func TestCredentialedSandboxAgentThroughEdge(t *testing.T) {
	const header = "X-Test-Port-Token"
	const token = "edge-token"
	const publicHost = "example.com"
	const agentToken = "Bearer agent-token"
	guest := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != agentToken {
			http.Error(w, "agent authorization", http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	edge := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(header) != token {
			http.Error(w, "edge authorization", http.StatusUnauthorized)
			return
		}
		if r.Proto != "HTTP/1.1" || r.Host != publicHost || r.URL.Path != "/healthz" {
			http.Error(w, "edge route", http.StatusBadRequest)
			return
		}
		r.Header.Del(header)
		guest.ServeHTTP(w, r)
	}))
	edge.EnableHTTP2 = true
	edge.StartTLS()
	t.Cleanup(edge.Close)

	addr := edge.Listener.Addr().String()
	endpoint := machine.Endpoint{
		URL: "https://" + publicHost,
		Credential: &machine.Credential{
			Header: header, Token: token, Expires: time.Now().Add(time.Hour),
		},
	}
	dial, err := urlDialer(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	// Trust the test CA and resolve the edge hostname to the local listener.
	edgeTLS := &tls.Dialer{Config: &tls.Config{
		RootCAs:    edge.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs,
		ServerName: publicHost,
		MinVersion: tls.VersionTLS12,
	}}
	dial.DialContext = func(ctx context.Context) (net.Conn, error) {
		return edgeTLS.DialContext(ctx, "tcp", addr)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, HTTPURL(SandboxAgentPort, "/healthz").String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// An incoming value under the driver's header must be overwritten.
	req.Header.Set(header, "caller-supplied")
	req.Header.Set("Authorization", agentToken)
	for _, tc := range []struct {
		dial *Dialer
		want int
	}{
		{dial: &Dialer{DialContext: dial.DialContext}, want: http.StatusUnauthorized},
		{dial: dial, want: http.StatusNoContent},
	} {
		resp, err := (&http.Client{Transport: tc.dial.Transport()}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
		}
	}
	if req.Host != HTTPURL(SandboxAgentPort, "/healthz").Host || req.Header.Get(header) != "caller-supplied" {
		t.Fatal("edge transport mutated the original request")
	}
}
