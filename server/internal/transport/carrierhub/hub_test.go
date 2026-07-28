package carrierhub

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestHubServesHTTPOverReversedConnections is the core proof of the reverse
// control-plane tunnel: an ordinary http.Server serves an ordinary http.Client
// over a connection the *server* side opened. This is what lets the in-guest
// pool agent reach the control plane without the control plane listening on a
// port.
func TestHubServesHTTPOverReversedConnections(t *testing.T) {
	hub := New()
	t.Cleanup(func() { _ = hub.Close() })

	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/pool/register" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			body, _ := io.ReadAll(r.Body)
			_, _ = w.Write([]byte("registered:" + string(body)))
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = server.Serve(hub) }()
	t.Cleanup(func() { _ = server.Close() })

	// net.Pipe stands in for one wslc bridge connection: the driver end is what
	// the host opened into the guest, the agent end is what the guest accepted.
	driverSide, agentSide := net.Pipe()
	cancel := make(chan struct{})
	go func() {
		if err := hub.Push(driverSide, cancel); err != nil {
			t.Errorf("Push: %v", err)
		}
	}()

	// The agent speaks HTTP as a client over the connection it did not initiate.
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(context.Context, string, string) (net.Conn, error) { return agentSide, nil },
		},
		Timeout: 10 * time.Second,
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"http://control-plane/api/pool/register", strings.NewReader("pool-1"))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "text/plain")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request over reversed connection: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if got, want := string(body), "registered:pool-1"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestHubPushIsCancellable(t *testing.T) {
	hub := New()
	t.Cleanup(func() { _ = hub.Close() })

	conn, other := net.Pipe()
	t.Cleanup(func() { _ = conn.Close(); _ = other.Close() })

	cancel := make(chan struct{})
	pushed := make(chan error, 1)
	go func() { pushed <- hub.Push(conn, cancel) }()

	// Nothing is accepting, so Push must still be blocked.
	select {
	case err := <-pushed:
		t.Fatalf("Push returned early with %v; it should block until accepted", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(cancel)
	select {
	case err := <-pushed:
		if !errors.Is(err, ErrPushCanceled) {
			t.Fatalf("Push error = %v, want ErrPushCanceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Push did not return after cancellation")
	}
}

func TestHubPushAfterCloseFails(t *testing.T) {
	hub := New()
	if err := hub.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	conn, other := net.Pipe()
	t.Cleanup(func() { _ = conn.Close(); _ = other.Close() })

	if err := hub.Push(conn, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("Push after Close = %v, want ErrClosed", err)
	}
}

// A closed Hub must end Serve rather than spin: net/http stops on a
// non-temporary listener error.
func TestHubAcceptUnblocksOnClose(t *testing.T) {
	hub := New()
	accepted := make(chan error, 1)
	go func() {
		_, err := hub.Accept()
		accepted <- err
	}()
	if err := hub.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-accepted:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Accept error = %v, want net.ErrClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Accept did not unblock after Close")
	}
}

func TestHubCloseIsIdempotent(t *testing.T) {
	hub := New()
	if err := hub.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := hub.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
