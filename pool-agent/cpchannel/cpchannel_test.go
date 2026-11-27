package cpchannel

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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

func TestARequestThatIsNotAWebSocketIsRefused(t *testing.T) {
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

// A pool's front that is HTTPS and offers HTTP/2, as boxd's edge does, still
// opens a channel: its WebSocket is asked for over HTTP/1.1, which alone has an Upgrade.
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

// websocketEdge is a TLS front offering HTTP/2 that reverse-proxies to
// backend and forwards WebSockets alone, as a hosted edge does: an Upgrade to
// anything else is refused before it reaches the backend.
func websocketEdge(t *testing.T, backend string) *httptest.Server {
	t.Helper()
	target, err := url.Parse(backend)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	edge := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if upgrade := r.Header.Get("Upgrade"); upgrade != "" && !strings.EqualFold(upgrade, "websocket") {
			http.Error(w, "only WebSocket upgrades are forwarded", http.StatusBadRequest)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	edge.EnableHTTP2 = true
	edge.StartTLS()
	t.Cleanup(edge.Close)
	return edge
}

// Through an edge that forwards WebSockets and refuses any other upgrade, the
// channel opens, and carries traffic both ways over its TLS and cpmux: the
// pool's connection reaches the server, and a reply bigger than any one TLS
// record or WebSocket message comes back whole.
func TestTheChannelOpensThroughAnEdgeThatForwardsOnlyWebSockets(t *testing.T) {
	serverKey, poolKey := newKey(t), newKey(t)
	channel, agentAPI, socket := agent(t, poolKey, public(serverKey))
	edge := websocketEdge(t, agentAPI.URL)
	transport, ok := edge.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("test server transport is %T", edge.Client().Transport)
	}
	transport.ForceAttemptHTTP2 = true

	// The fixture is the edge it claims to be: an arbitrary upgrade is refused.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, edge.URL+Path("proj", "pool"), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "discobox-control-plane/1")
	resp, err := (&http.Client{Transport: http1(transport)}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("the edge answered an arbitrary upgrade %s, want 400", resp.Status)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, err := Dial(ctx, transport, edge.URL, "proj", "pool", "token", serverKey, public(poolKey))
	if err != nil {
		t.Fatalf("Dial through the edge: %v", err)
	}
	session, err := cpmux.Client(conn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	if err := channel.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	reply := bytes.Repeat([]byte("0123456789abcdef"), 1<<16) // 1 MiB
	targets := serveControlPlane(t, session, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(reply)
	}))
	got, err := get(ctx, socketClient(socket), "/ping")
	if err != nil {
		t.Fatalf("GET through the channel: %v", err)
	}
	if target := <-targets; target != cpmux.TargetControlPlane {
		t.Fatalf("server saw a stream to %q", target)
	}
	if !bytes.Equal([]byte(got), reply) {
		t.Fatal("the reply came back altered")
	}
}

// A channel that drops is replaced by the next one the server opens, and a
// pool's connection made after the drop rides the new one.
func TestANewChannelReplacesADroppedOne(t *testing.T) {
	serverKey, poolKey := newKey(t), newKey(t)
	channel, agentAPI, socket := agent(t, poolKey, public(serverKey))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	first, err := Dial(ctx, http.DefaultTransport, agentAPI.URL, "proj", "pool", "token", serverKey, public(poolKey))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if _, err := cpmux.Client(first); err != nil {
		t.Fatal(err)
	}
	if err := channel.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	dropped := channel.installed()
	_ = first.Close()

	second, err := Dial(ctx, http.DefaultTransport, agentAPI.URL, "proj", "pool", "token", serverKey, public(poolKey))
	if err != nil {
		t.Fatalf("redial: %v", err)
	}
	session, err := cpmux.Client(second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	// The client's TLS finishes before the pool's, so the pool may not have
	// installed the new session yet: wait until it has replaced the old one.
	for channel.installed() == dropped {
		if ctx.Err() != nil {
			t.Fatal("the pool never installed the new channel")
		}
		time.Sleep(10 * time.Millisecond)
	}

	serveControlPlane(t, session, pong())
	if got, err := get(ctx, socketClient(socket), "/ping"); err != nil || got != "pong /ping" {
		t.Fatalf("GET on the new channel = %q, %v", got, err)
	}
}

// installed is the session the channel holds now.
func (c *Channel) installed() *cpmux.Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.session
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

// sessionListener is the server's end of a channel as the control plane
// serves it: each stream the pool opens, a connection; targets has each
// stream's target.
type sessionListener struct {
	session *cpmux.Session
	targets chan<- string
}

func (l sessionListener) Accept() (net.Conn, error) {
	conn, target, err := l.session.Accept()
	if err != nil {
		return nil, err
	}
	select {
	case l.targets <- target:
	default:
	}
	return conn, nil
}

func (l sessionListener) Close() error   { return l.session.Close() }
func (l sessionListener) Addr() net.Addr { return &net.UnixAddr{Name: "channel", Net: "unix"} }

// serveControlPlane serves handler on the server's end of session, as the
// control plane's HTTP API is served on the streams a pool opens, and
// returns the streams' targets.
func serveControlPlane(t *testing.T, session *cpmux.Session, handler http.Handler) <-chan string {
	t.Helper()
	targets := make(chan string, 16)
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(sessionListener{session: session, targets: targets}) }()
	t.Cleanup(func() { _ = server.Close() })
	return targets
}

// pong answers "pong " and the path.
func pong() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "pong "+r.URL.Path)
	})
}

// socketClient is an HTTP client of the pool's control-plane socket, as the
// pool's own processes are.
func socketClient(socket string) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "unix", socket)
	}}}
}

// get is the body of a GET of path through client, which must answer 200.
func get(ctx context.Context, client *http.Client, path string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://control-plane"+path, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return string(body), fmt.Errorf("status %s", resp.Status)
	}
	return string(body), nil
}

// openChannel opens a channel to the pool behind agentAPI, as the server
// does, and waits for the pool to install it.
func openChannel(ctx context.Context, t *testing.T, channel *Channel, agentAPI string, serverKey, poolKey ed25519.PrivateKey) *cpmux.Session {
	t.Helper()
	conn, err := Dial(ctx, http.DefaultTransport, agentAPI, "proj", "pool", "token", serverKey, public(poolKey))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	session, err := cpmux.Client(conn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	if err := channel.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	return session
}

// What the pool's processes ask of the socket reaches the server as a
// control-plane stream, and the server's answer comes back.
func TestAPoolsRequestReachesTheServerOverTheChannel(t *testing.T) {
	serverKey, poolKey := newKey(t), newKey(t)
	channel, agentAPI, socket := agent(t, poolKey, public(serverKey))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session := openChannel(ctx, t, channel, agentAPI.URL, serverKey, poolKey)
	targets := serveControlPlane(t, session, pong())
	got, err := get(ctx, socketClient(socket), "/ping")
	if err != nil || got != "pong /ping" {
		t.Fatalf("GET = %q, %v", got, err)
	}
	if target := <-targets; target != cpmux.TargetControlPlane {
		t.Fatalf("server saw a stream to %q", target)
	}
}

// A request made while there is no channel waits for the server to open one.
func TestALocalRequestWaitsForAChannel(t *testing.T) {
	serverKey, poolKey := newKey(t), newKey(t)
	channel, agentAPI, socket := agent(t, poolKey, public(serverKey))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type result struct {
		body string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		body, err := get(ctx, socketClient(socket), "/ping")
		done <- result{body, err}
	}()
	time.Sleep(100 * time.Millisecond)
	session := openChannel(ctx, t, channel, agentAPI.URL, serverKey, poolKey)
	serveControlPlane(t, session, pong())
	if got := <-done; got.err != nil || got.body != "pong /ping" {
		t.Fatalf("GET = %q, %v", got.body, got.err)
	}
}

// A draining agent's channel stays while an answer is still being streamed
// to the pool, gaps longer than any idle guess included, and closes once it
// has been answered, so the server dials again and reaches the agent that
// took over.
func TestADrainCarriesAStreamedAnswerToItsEnd(t *testing.T) {
	serverKey, poolKey := newKey(t), newKey(t)
	channel, agentAPI, socket := agent(t, poolKey, public(serverKey))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	session := openChannel(ctx, t, channel, agentAPI.URL, serverKey, poolKey)
	serveControlPlane(t, session, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flusher, _ := w.(http.Flusher)
		for i := range 3 {
			if i > 0 {
				time.Sleep(700 * time.Millisecond)
			}
			_, _ = fmt.Fprintf(w, "tick %d\n", i)
			flusher.Flush()
		}
	}))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://control-plane/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := socketClient(socket).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	reader := bufio.NewReader(resp.Body)
	if line, err := reader.ReadString('\n'); err != nil || line != "tick 0\n" {
		t.Fatalf("first line = %q, %v", line, err)
	}

	drained := make(chan struct{})
	go func() {
		channel.Drain(ctx)
		close(drained)
	}()
	rest, err := io.ReadAll(reader)
	if err != nil || string(rest) != "tick 1\ntick 2\n" {
		t.Fatalf("the streamed answer was cut by the drain: %q, %v", rest, err)
	}
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("the drain did not end once the answer did")
	}
	if !channel.installed().Closed() {
		t.Fatal("the drained channel is still open: the server would not dial again")
	}
}

// An idle connection kept alive between a pool's requests holds no drain:
// it closes at once, so the server dials again without waiting.
func TestAnIdleConnectionDoesNotHoldADrain(t *testing.T) {
	serverKey, poolKey := newKey(t), newKey(t)
	channel, agentAPI, socket := agent(t, poolKey, public(serverKey))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session := openChannel(ctx, t, channel, agentAPI.URL, serverKey, poolKey)
	serveControlPlane(t, session, pong())
	client := socketClient(socket)
	if got, err := get(ctx, client, "/ping"); err != nil || got != "pong /ping" {
		t.Fatalf("GET = %q, %v", got, err)
	}
	started := time.Now()
	channel.Drain(ctx)
	if took := time.Since(started); took > time.Second {
		t.Fatalf("the drain took %s with only an idle connection on it", took)
	}
	if !channel.installed().Closed() {
		t.Fatal("the drained channel is still open")
	}
}

// A request still waiting on its answer when the drain's bound comes is cut:
// the channel closes at the bound.
func TestADrainEndsAtItsBound(t *testing.T) {
	serverKey, poolKey := newKey(t), newKey(t)
	channel, agentAPI, socket := agent(t, poolKey, public(serverKey))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session := openChannel(ctx, t, channel, agentAPI.URL, serverKey, poolKey)
	release := make(chan struct{})
	defer close(release)
	arrived := make(chan struct{}, 1)
	serveControlPlane(t, session, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		arrived <- struct{}{}
		<-release
	}))
	go func() { _, _ = get(ctx, socketClient(socket), "/long-poll") }()
	<-arrived
	bound, stop := context.WithTimeout(ctx, 300*time.Millisecond)
	defer stop()
	started := time.Now()
	channel.Drain(bound)
	if took := time.Since(started); took < 250*time.Millisecond || took > 3*time.Second {
		t.Fatalf("the drain took %s, want its bound", took)
	}
	if !channel.installed().Closed() {
		t.Fatal("the channel is still open after the drain's bound")
	}
}
