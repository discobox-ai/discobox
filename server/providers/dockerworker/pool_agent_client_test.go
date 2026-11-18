package dockerworker

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/discobox-ai/x/shorttmp"
)

// A vz guest that panics reboots inside the same VM. A request written to the
// boot that died may never be answered or reset (on a Mac, requests sent just
// after a panic waited past a 60s timeout, #1335). The rebooting guest
// refusing a new dial is the first sign the host gets, so that refusal drops
// every connection, and a request still waiting on the old boot fails rather
// than waiting for its caller to give up.
func TestPoolAgentClientDropsConnectionsWhenTheGuestRefusesADial(t *testing.T) {
	socket := filepath.Join(shorttmp.Dir(t), "agent.sock")
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	// The dead boot: it takes the request and never answers.
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()

	var dials atomic.Int32
	client := NewPoolAgentClient(func(ctx context.Context) (net.Conn, error) {
		if dials.Add(1) == 1 {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}
		return nil, &net.OpError{Op: "dial", Net: "vsock", Err: syscall.ECONNRESET}
	})

	stuck := make(chan error, 1)
	go func() {
		request, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://pool.local/execs", nil)
		response, err := client.Do(request)
		if err == nil {
			_ = response.Body.Close()
		}
		stuck <- err
	}()
	var held net.Conn
	select {
	case held = <-accepted:
		defer held.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("the first request never reached the guest")
	}
	// The request was written, so its connection is the client's: the guest
	// accepting it says only that the dial connected, not that it returned.
	if _, err := http.ReadRequest(bufio.NewReader(held)); err != nil {
		t.Fatal(err)
	}

	// The rebooting guest refuses the next request's dial.
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://pool.local/services", nil)
	if response, err := client.Do(request); err == nil {
		_ = response.Body.Close()
		t.Fatal("a request whose dial the guest refused succeeded")
	}

	select {
	case err := <-stuck:
		if err == nil {
			t.Fatal("the request on the dead boot's connection succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a refused dial left the request on the dead boot's connection waiting")
	}
}

// A dial that connected to the dead boot but had not returned when another
// dial was refused is not yet among the connections the refusal drops. It is
// dropped as it returns, so its request fails too (#1355).
func TestPoolAgentClientDropsADialInProgressWhenTheGuestRefusesADial(t *testing.T) {
	socket := filepath.Join(shorttmp.Dir(t), "agent.sock")
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	// The dead boot: it takes the connection and never answers.
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()

	refused := make(chan struct{})
	var dials atomic.Int32
	client := NewPoolAgentClient(func(ctx context.Context) (net.Conn, error) {
		if dials.Add(1) == 1 {
			conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
			// Return only once the other request's dial has been refused.
			<-refused
			return conn, err
		}
		return nil, &net.OpError{Op: "dial", Net: "vsock", Err: syscall.ECONNRESET}
	})

	stuck := make(chan error, 1)
	go func() {
		request, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://pool.local/execs", nil)
		response, err := client.Do(request)
		if err == nil {
			_ = response.Body.Close()
		}
		stuck <- err
	}()
	select {
	case held := <-accepted:
		defer held.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("the first dial never reached the guest")
	}

	request, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://pool.local/services", nil)
	if response, err := client.Do(request); err == nil {
		_ = response.Body.Close()
		t.Fatal("a request whose dial the guest refused succeeded")
	}
	close(refused)

	select {
	case err := <-stuck:
		if err == nil {
			t.Fatal("the request on the dead boot's connection succeeded")
		}
		// It fails as a dial, which callers wait out as an unanswered agent
		// rather than settling the request as failed.
		var opErr *net.OpError
		if !errors.As(err, &opErr) || opErr.Op != "dial" {
			t.Fatalf("the dropped dial failed with %v, not a dial *net.OpError", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a refused dial left the request dialed across it waiting")
	}
}

// Closing the client, as stopping its VM does, ends a request in flight as
// well as the idle connections.
func TestPoolAgentClientCloseEndsRequestsInFlight(t *testing.T) {
	socket := filepath.Join(shorttmp.Dir(t), "agent.sock")
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	started := make(chan struct{})
	server := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/slow" {
				close(started)
				<-r.Context().Done()
				return
			}
			_, _ = io.WriteString(w, "ok")
		}),
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	client := NewPoolAgentClient(func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	})
	inFlight := make(chan error, 1)
	go func() {
		request, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://pool.local/slow", nil)
		response, err := client.Do(request)
		if err == nil {
			_ = response.Body.Close()
		}
		inFlight <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the request never reached the agent")
	}

	client.Close()
	select {
	case err := <-inFlight:
		if err == nil {
			t.Fatal("a request in flight when the client closed succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("closing the client left a request in flight waiting")
	}

	// The client stays usable: the next request dials again.
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://pool.local/ok", nil)
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("a request after Close: %v", err)
	}
	_ = response.Body.Close()
}
