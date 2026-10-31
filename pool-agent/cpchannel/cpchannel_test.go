package cpchannel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pool-agent/cpmux"
)

func newKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func public(key ed25519.PrivateKey) ed25519.PublicKey {
	pub, _ := key.Public().(ed25519.PublicKey)
	return pub
}

// agent is a pool agent's API serving only the channel route, and the socket
// its processes reach the control plane at.
func agent(t *testing.T, poolKey ed25519.PrivateKey, controlPlane ed25519.PublicKey) (*Channel, *httptest.Server, string) {
	t.Helper()
	channel, err := New(slog.New(slog.DiscardHandler), poolKey, controlPlane)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle(Path("proj", "pool"), channel)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	socket := filepath.Join(shortTempDir(t), "cp.sock")
	var listen net.ListenConfig
	listener, err := listen.Listen(t.Context(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() { _ = channel.Serve(context.Background(), listener) }()
	return channel, server, socket
}

// What the pool's processes write to the socket arrives at the server as a
// control-plane stream, and the server's answer comes back.
func TestAPoolsConnectionReachesTheServerOverTheChannel(t *testing.T) {
	serverKey, poolKey := newKey(t), newKey(t)
	channel, agentAPI, socket := agent(t, poolKey, public(serverKey))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := Dial(ctx, http.DefaultTransport, agentAPI.URL, "proj", "pool", "token", serverKey, public(poolKey))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	session, err := cpmux.Client(conn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	if err := channel.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	accepted := make(chan string, 1)
	go func() {
		stream, target, err := session.Accept()
		if err != nil {
			accepted <- "accept: " + err.Error()
			return
		}
		defer func() { _ = stream.Close() }()
		buf := make([]byte, 4)
		if _, err := io.ReadFull(stream, buf); err != nil {
			accepted <- "read: " + err.Error()
			return
		}
		_, _ = stream.Write([]byte("pong"))
		accepted <- target + " " + string(buf)
	}()

	var dialer net.Dialer
	local, err := dialer.DialContext(t.Context(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = local.Close() }()
	if _, err := local.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 4)
	if _, err := io.ReadFull(local, reply); err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if got := <-accepted; got != cpmux.TargetControlPlane+" ping" {
		t.Fatalf("server saw %q", got)
	}
	if string(reply) != "pong" {
		t.Fatalf("reply = %q", reply)
	}
}

// Each end accepts exactly one key: a server that is not the pool's control
// plane, or a pool that is not the one the server dialed for, gets no channel.
func TestTheChannelIsPinnedAtBothEnds(t *testing.T) {
	serverKey, poolKey, stranger := newKey(t), newKey(t), newKey(t)
	_, agentAPI, _ := agent(t, poolKey, public(serverKey))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if conn, err := Dial(ctx, http.DefaultTransport, agentAPI.URL, "proj", "pool", "token", stranger, public(poolKey)); err == nil {
		// TLS 1.3 completes the client's half before the server has checked
		// the client certificate; the refusal arrives on first read.
		if _, err := conn.Read(make([]byte, 1)); err == nil {
			t.Fatal("a server with another key opened a channel")
		}
		_ = conn.Close()
	}
	if _, err := Dial(ctx, http.DefaultTransport, agentAPI.URL, "proj", "pool", "token", serverKey, public(stranger)); err == nil {
		t.Fatal("the server accepted a pool with another key")
	}
}

func TestARequestThatIsNotAnUpgradeIsRefused(t *testing.T) {
	serverKey, poolKey := newKey(t), newKey(t)
	_, agentAPI, _ := agent(t, poolKey, public(serverKey))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, agentAPI.URL+Path("proj", "pool"), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("status = %d, want 426", resp.StatusCode)
	}
}

// A connection made while no channel is open waits for one, rather than
// failing at once while the server redials.
func TestALocalConnectionWaitsForAChannel(t *testing.T) {
	serverKey, poolKey := newKey(t), newKey(t)
	_, agentAPI, socket := agent(t, poolKey, public(serverKey))
	var dialer net.Dialer
	local, err := dialer.DialContext(t.Context(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = local.Close() }()
	if _, err := local.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := Dial(ctx, http.DefaultTransport, agentAPI.URL, "proj", "pool", "token", serverKey, public(poolKey))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	session, err := cpmux.Client(conn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	stream, _, err := session.Accept()
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	defer func() { _ = stream.Close() }()
	buf := make([]byte, 4)
	if _, err := io.ReadFull(stream, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("read %q, %v", buf, err)
	}
}

// A pool's front that is HTTPS and offers HTTP/2, as boxd's edge does, still
// opens a channel: the upgrade is asked for over HTTP/1.1, which alone has one.
func TestTheChannelOpensThroughAnHTTP2Front(t *testing.T) {
	serverKey, poolKey := newKey(t), newKey(t)
	channel, err := New(slog.New(slog.DiscardHandler), poolKey, public(serverKey))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle(Path("proj", "pool"), channel)
	front := httptest.NewUnstartedServer(mux)
	front.EnableHTTP2 = true
	front.StartTLS()
	t.Cleanup(front.Close)

	transport, ok := front.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("test server transport is %T", front.Client().Transport)
	}
	transport.ForceAttemptHTTP2 = true
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, err := Dial(ctx, transport, front.URL, "proj", "pool", "token", serverKey, public(poolKey))
	if err != nil {
		t.Fatalf("Dial through an HTTP/2 front: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := cpmux.Client(conn); err != nil {
		t.Fatal(err)
	}
	if err := channel.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

// shortTempDir is a temporary directory short enough to hold a Unix socket:
// t.TempDir's path, under a CI runner's temporary root, can pass the ~100
// bytes a socket path is limited to, and bind fails with "invalid argument".
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
