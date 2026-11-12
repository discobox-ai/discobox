package createprotocol

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestDiscoveryAndCreateStayOnOneServingInstance(t *testing.T) {
	var creates atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { creates.Add(1); _, _ = io.Copy(w, r.Body) })
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { Serve(w, r, handler) }))
	defer old.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	client, release, status, err := Dial(ctx, old.Client(), old.URL, "p", "pool", "token")
	if err != nil {
		t.Fatalf("discovery %d: %v", status, err)
	}
	defer release()
	// Closing the public listener after discovery cannot redirect dispatch to
	// any replacement listening at the same address.
	old.Listener.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://pool/create", strings.NewReader(`{"field":true}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"field":true}` || creates.Load() != 1 {
		t.Fatalf("body=%s creates=%d", data, creates.Load())
	}
}

func TestReconnectRequiresFreshDiscovery(t *testing.T) {
	var creates atomic.Int32
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		Serve(w, r, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			creates.Add(1)
			w.Header().Set("Connection", "close")
			w.WriteHeader(204)
		}))
	}))
	defer agent.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	client, release, _, err := Dial(ctx, agent.Client(), agent.URL, "p", "pool", "token")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://pool/create", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	req, err = http.NewRequestWithContext(ctx, http.MethodPost, "http://pool/create", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	again, err := client.Do(req)
	if again != nil {
		_ = again.Body.Close()
	}
	if err == nil || creates.Load() != 1 {
		t.Fatalf("reconnect=%v creates=%d", err, creates.Load())
	}
}

func TestClosingResponseDrainsBody(t *testing.T) {
	// Span many bridge copies so closing HTTP must drain both the response
	// headers and the final body bytes before the WebSocket is torn down.
	body := strings.Repeat("create result\n", 1<<16)
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		Serve(w, r, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Connection", "close")
			_, _ = io.WriteString(w, body)
		}))
	}))
	defer agent.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	client, release, _, err := Dial(ctx, agent.Client(), agent.URL, "p", "pool", "token")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://pool/create", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read closing response (%d/%d bytes): %v", len(got), len(body), err)
	}
	if resp.StatusCode != http.StatusOK || string(got) != body {
		t.Fatalf("closing response: status=%s bytes=%d, want %d", resp.Status, len(got), len(body))
	}
}

func TestServeReturnsOnClientDisconnect(t *testing.T) {
	returned := make(chan struct{})
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.UserAgent() == "discobox-sandbox-agent (port probe)" {
			return
		}
		defer close(returned)
		Serve(w, r, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Error("unexpected create")
		}))
	}))
	defer agent.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	_, release, _, err := Dial(ctx, agent.Client(), agent.URL, "p", "pool", "token")
	if err != nil {
		t.Fatal(err)
	}
	release()
	select {
	case <-returned:
	case <-ctx.Done():
		t.Fatal("Serve did not return after the client disconnected")
	}
}

func TestDiscoveryRefusesInvalidRevisions(t *testing.T) {
	for _, tc := range []struct {
		name, answer string
		incompatible bool
	}{
		{"future", fmt.Sprintf(`{"createRevision":%d}`, Revision+1), true},
		// An agent from before the last contract change, whose strict decoder
		// would refuse what this server sends.
		{"past", fmt.Sprintf(`{"createRevision":%d}`, Revision-1), true},
		{"missing", `{}`, false}, {"malformed", `no`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{Subprotocol}})
				if err != nil {
					return
				}
				defer func() { _ = ws.CloseNow() }()
				_ = ws.Write(r.Context(), websocket.MessageText, []byte(tc.answer))
				_, _, _ = ws.Read(r.Context())
			}))
			defer agent.Close()
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			_, _, _, err := Dial(ctx, agent.Client(), agent.URL, "p", "pool", "token")
			var incompatible *Incompatible
			if err == nil || errors.As(err, &incompatible) != tc.incompatible {
				t.Fatalf("discovery=%v", err)
			}
		})
	}
}

func TestDiscoveryAcrossPoolLeaseTransports(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		Serve(w, r, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	})
	backend := httptest.NewServer(handler)
	defer backend.Close()
	upstream, _ := url.Parse(backend.URL)
	edge := httptest.NewUnstartedServer(httputil.NewSingleHostReverseProxy(upstream))
	edge.EnableHTTP2 = true
	edge.StartTLS()
	defer edge.Close()
	socket := filepath.Join(t.TempDir(), "agent.sock")
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	defer server.Close()
	go func() { _ = server.Serve(listener) }()
	unixTransport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer unixTransport.CloseIdleConnections()
	for _, tc := range []struct {
		name, url string
		client    *http.Client
	}{
		{"tcp", backend.URL, backend.Client()}, {"tls edge offering h2", edge.URL, edge.Client()}, {"unix", "http://pool", &http.Client{Transport: unixTransport}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			client, release, _, err := Dial(ctx, tc.client, tc.url, "p", "pool", "token")
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://pool/create", strings.NewReader(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != 204 {
				t.Fatal(resp.Status)
			}
		})
	}
}
