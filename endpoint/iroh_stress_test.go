//go:build iroh

package endpoint

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Stress and recovery tests for the transport. The functional tests drive one
// request at a time against a server that stays up, which is the case that
// works by construction; these cover the two that do not — many streams on
// one connection at once, and a peer that goes away underneath a live client.

// irohEchoServer binds an endpoint on key, serves an echo handler over it, and
// returns the addresses that reach it plus a stop func.
//
// The identity is passed in rather than generated so that a caller can restart
// "the same server" on a new socket, which is what a peer restarting looks
// like from the other side: same address, different path to it.
func irohEchoServer(t *testing.T, key ed25519.PrivateKey) (addrs []string, admitted *atomic.Int64, stop func()) {
	t.Helper()

	admitted = &atomic.Int64{}
	server, err := NewIrohEndpoint(IrohConfig{
		SecretKey: key,
		Authorize: func(IrohID) bool {
			// Authorize runs once per connection, which makes it the count of
			// connections this server accepted.
			admitted.Add(1)
			return true
		},
		DisableRelay: true,
	})
	if err != nil {
		t.Fatalf("NewIrohEndpoint() error = %v", err)
	}

	listener, _, cleanup, err := server.Listen()
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(body)
	})
	mux.HandleFunc("/attach", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")
		for {
			typ, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			if err := conn.Write(r.Context(), typ, data); err != nil {
				return
			}
		}
	})

	httpServer := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = httpServer.Serve(listener) }()

	addrs, err = server.DirectAddrs()
	if err != nil {
		t.Fatalf("DirectAddrs() error = %v", err)
	}
	return addrs, admitted, func() {
		_ = httpServer.Close()
		cleanup()
	}
}

// post sends body to /echo and requires it back.
func post(ctx context.Context, client *http.Client, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, LogicalHTTPBaseURL+"/echo", bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %s", resp.Status)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, body) {
		return fmt.Errorf("echo mismatch: got %d bytes, sent %d", len(got), len(body))
	}
	return nil
}

// A client holds one QUIC connection per peer and opens a stream per request.
// The peer restarting kills that connection, and every request after it has to
// find its way onto a new one rather than failing forever — a control plane
// that needs the client restarted after every server restart is not one anyone
// would leave running.
func TestIrohRoundTripperRecoversWhenTheServerRestarts(t *testing.T) {
	serverKey := newSecretKey(t)
	addrs, _, stop := irohEchoServer(t, serverKey)

	// The addresses change when the server rebinds, and Locate is consulted on
	// every dial, so the client learns the new ones the way a real deployment
	// would learn them from discovery.
	var (
		mu      sync.Mutex
		current = addrs
	)
	client := newIrohEndpointForTest(t, IrohConfig{
		SecretKey:    newSecretKey(t),
		DisableRelay: true,
		Locate: func(IrohID) []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), current...)
		},
	})

	serverPub, ok := serverKey.Public().(ed25519.PublicKey)
	if !ok {
		t.Fatal("server key is not ed25519")
	}
	serverID, err := IrohIDFromPublicKey(serverPub)
	if err != nil {
		t.Fatalf("IrohIDFromPublicKey() error = %v", err)
	}
	transport, err := client.RoundTripper(serverID, nil)
	if err != nil {
		t.Fatalf("RoundTripper() error = %v", err)
	}
	httpClient := &http.Client{Transport: transport}

	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()

	if err := post(ctx, httpClient, []byte("before the restart")); err != nil {
		t.Fatalf("request before the restart: %v", err)
	}

	// The server goes away and comes back on a new socket with the same
	// identity, which is what a restart is.
	stop()
	restarted, _, stopAgain := irohEchoServer(t, serverKey)
	t.Cleanup(stopAgain)
	mu.Lock()
	current = restarted
	mu.Unlock()

	// The client is holding a connection to a server that no longer exists.
	// Requests from here have to recover on their own.
	var lastErr error
	deadline := time.Now().Add(60 * time.Second)
	for attempt := 1; time.Now().Before(deadline); attempt++ {
		if lastErr = post(ctx, httpClient, []byte("after the restart")); lastErr == nil {
			t.Logf("recovered on request %d after the restart", attempt)
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("client never recovered from the server restart: %v", lastErr)
	}

	// And it keeps working, rather than recovering once and then flapping.
	for i := 0; i < 5; i++ {
		if err := post(ctx, httpClient, []byte(fmt.Sprintf("settled %d", i))); err != nil {
			t.Fatalf("request %d after recovery: %v", i, err)
		}
	}
}

// Many requests at once over one connection, mixed with the long-lived
// websockets that carry attach and the project stream. Each request gets its
// own stream, so this is the case where a shared connection, a per-request
// stream and a hijacked one all have to coexist.
func TestIrohConcurrentRequestsAndWebsockets(t *testing.T) {
	serverKey := newSecretKey(t)
	addrs, _, stop := irohEchoServer(t, serverKey)
	t.Cleanup(stop)

	client := newIrohEndpointForTest(t, IrohConfig{
		SecretKey:    newSecretKey(t),
		DisableRelay: true,
		Locate:       func(IrohID) []string { return addrs },
	})
	serverPub, _ := serverKey.Public().(ed25519.PublicKey)
	serverID, err := IrohIDFromPublicKey(serverPub)
	if err != nil {
		t.Fatalf("IrohIDFromPublicKey() error = %v", err)
	}
	transport, err := client.RoundTripper(serverID, nil)
	if err != nil {
		t.Fatalf("RoundTripper() error = %v", err)
	}
	httpClient := &http.Client{Transport: transport}

	ctx, cancel := context.WithTimeout(t.Context(), 180*time.Second)
	defer cancel()

	const (
		workers          = 12
		requestsPerWorks = 20
		sockets          = 6
		messagesPerSock  = 25
	)
	errs := make(chan error, workers*requestsPerWorks+sockets)
	var wg sync.WaitGroup

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < requestsPerWorks; i++ {
				// Sizes that span a single packet up to several flow control
				// windows, so framing and chunking are both exercised.
				body := bytes.Repeat([]byte(fmt.Sprintf("w%02d-r%02d.", w, i)), 1+(w*i*97)%4096)
				if err := post(ctx, httpClient, body); err != nil {
					errs <- fmt.Errorf("worker %d request %d: %w", w, i, err)
					return
				}
			}
		}(w)
	}

	for s := 0; s < sockets; s++ {
		wg.Add(1)
		go func(s int) {
			defer wg.Done()
			ws, resp, err := websocket.Dial(ctx, "ws://discobox.local/attach", &websocket.DialOptions{HTTPClient: httpClient})
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			if err != nil {
				errs <- fmt.Errorf("socket %d dial: %w", s, err)
				return
			}
			defer func() { _ = ws.Close(websocket.StatusNormalClosure, "done") }()
			for i := 0; i < messagesPerSock; i++ {
				want := []byte(fmt.Sprintf("socket %d message %d", s, i))
				if err := ws.Write(ctx, websocket.MessageBinary, want); err != nil {
					errs <- fmt.Errorf("socket %d write %d: %w", s, i, err)
					return
				}
				_, got, err := ws.Read(ctx)
				if err != nil {
					errs <- fmt.Errorf("socket %d read %d: %w", s, i, err)
					return
				}
				if !bytes.Equal(got, want) {
					errs <- fmt.Errorf("socket %d message %d echoed %q", s, i, got)
					return
				}
			}
		}(s)
	}

	wg.Wait()
	close(errs)
	failed := 0
	for err := range errs {
		failed++
		if failed <= 5 {
			t.Error(err)
		}
	}
	if failed > 5 {
		t.Errorf("... and %d more failures", failed-5)
	}
}

// A hijacked connection is what attach and the tunnels are: the handler takes
// the stream and speaks its own protocol on it, in both directions, until one
// side is done. Half-close is how "done sending" is said, and it has to reach
// the other end as a clean EOF rather than as a broken stream.
func TestIrohHijackedStreamHalfCloses(t *testing.T) {
	server, client := irohPair(t, func(IrohID) bool { return true })

	listener, _, cleanup, err := server.Listen()
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	t.Cleanup(cleanup)

	handled := make(chan error, 1)
	httpServer := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				handled <- fmt.Errorf("response writer is not a hijacker")
				return
			}
			conn, buf, err := hijacker.Hijack()
			if err != nil {
				handled <- err
				return
			}
			defer func() { _ = conn.Close() }()
			// Read until the client half-closes, then answer and half-close
			// in turn.
			got, err := io.ReadAll(buf)
			if err != nil {
				handled <- err
				return
			}
			if _, err := conn.Write(append([]byte("read:"), got...)); err != nil {
				handled <- err
				return
			}
			if cw, ok := conn.(interface{ CloseWrite() error }); ok {
				handled <- cw.CloseWrite()
				return
			}
			handled <- fmt.Errorf("hijacked connection cannot half-close")
		}),
	}
	go func() { _ = httpServer.Serve(listener) }()
	t.Cleanup(func() { _ = httpServer.Close() })

	serverID, err := server.ID()
	if err != nil {
		t.Fatalf("ID() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	// Dial the transport directly: this is below HTTP's response handling,
	// which is the point of a hijack.
	transport, err := client.RoundTripper(serverID, nil)
	if err != nil {
		t.Fatalf("RoundTripper() error = %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, LogicalHTTPBaseURL+"/hijack", nil)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := dialThrough(ctx, transport)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if err := req.Write(conn); err != nil {
		t.Fatalf("write request: %v", err)
	}
	if _, err := conn.Write([]byte("payload-after-headers")); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	closer, ok := conn.(interface{ CloseWrite() error })
	if !ok {
		t.Fatal("iroh connection cannot half-close, so attach cannot work over it")
	}
	if err := closer.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite() error = %v", err)
	}

	reply, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if !bytes.Contains(reply, []byte("read:payload-after-headers")) {
		t.Fatalf("reply = %q, want it to carry the payload the handler read", reply)
	}
	if err := <-handled; err != nil {
		t.Fatalf("handler: %v", err)
	}
}

// dialThrough reaches the transport's dialer, which is what an http.Client
// would use, without going through a request.
func dialThrough(ctx context.Context, rt http.RoundTripper) (net.Conn, error) {
	local, ok := rt.(localRoundTripper)
	if !ok {
		return nil, fmt.Errorf("round tripper is %T, want localRoundTripper", rt)
	}
	transport, ok := local.base.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("base is %T, want *http.Transport", local.base)
	}
	return transport.DialContext(ctx, "tcp", "discobox.local:80")
}

// The same restart, with requests in flight. Every one of them finds the
// connection dead at the same moment, and they must converge on one
// replacement rather than each opening their own: a peer restarting under
// load would otherwise cost a full handshake per request in flight, which is
// exactly when a server can least afford them.
func TestIrohRedialConvergesOnOneConnection(t *testing.T) {
	serverKey := newSecretKey(t)
	addrs, _, stop := irohEchoServer(t, serverKey)

	var (
		mu      sync.Mutex
		current = addrs
	)
	client := newIrohEndpointForTest(t, IrohConfig{
		SecretKey:    newSecretKey(t),
		DisableRelay: true,
		Locate: func(IrohID) []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), current...)
		},
	})

	serverPub, _ := serverKey.Public().(ed25519.PublicKey)
	serverID, err := IrohIDFromPublicKey(serverPub)
	if err != nil {
		t.Fatalf("IrohIDFromPublicKey() error = %v", err)
	}
	transport, err := client.RoundTripper(serverID, nil)
	if err != nil {
		t.Fatalf("RoundTripper() error = %v", err)
	}
	httpClient := &http.Client{Transport: transport}

	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()

	if err := post(ctx, httpClient, []byte("before the restart")); err != nil {
		t.Fatalf("request before the restart: %v", err)
	}

	stop()
	restarted, admitted, stopAgain := irohEchoServer(t, serverKey)
	t.Cleanup(stopAgain)
	mu.Lock()
	current = restarted
	mu.Unlock()

	const requests = 12
	var wg sync.WaitGroup
	errs := make(chan error, requests)
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Each retries on its own, the way a caller would: what matters
			// is that they share the connection they end up on.
			var lastErr error
			for attempt := 0; attempt < 5; attempt++ {
				if lastErr = post(ctx, httpClient, []byte(fmt.Sprintf("request %d", i))); lastErr == nil {
					return
				}
				time.Sleep(200 * time.Millisecond)
			}
			errs <- fmt.Errorf("request %d: %w", i, lastErr)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	if got := admitted.Load(); got != 1 {
		t.Fatalf("the restarted server admitted %d connections for %d concurrent requests, want 1", got, requests)
	}
}
