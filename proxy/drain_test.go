package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// drainProxies are two proxies of one pool serving one listening socket, as
// a handover's old and new do: each with its own descriptor of the socket,
// so one closing its own leaves the socket to the other.
type drainProxies struct {
	old, next    *Server
	oldListener  net.Listener
	nextListener net.Listener
	addr         string
	material     ClientMaterial
	oldServed    chan error
	nextServed   chan error
	nextStarted  bool
}

func newDrainProxies(ctx context.Context, t *testing.T, origin *url.URL) *drainProxies {
	t.Helper()
	dir := t.TempDir()
	prepared, err := PrepareCertificates(PrepareOptions{
		Dir:         filepath.Join(dir, "certs"),
		ServerHosts: []string{"127.0.0.1", "localhost"},
		ClientIDs:   []string{"sandbox-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	newServer := func(name string) *Server {
		server, err := NewServer(ctx, Config{
			ListenAddress:             "127.0.0.1:0",
			AllowLoopbackDestinations: true,
			CertDir:                   prepared.Bundle.Dir,
			DatabaseDSN:               filepath.Join(dir, name+".db"),
		}, prepared.Bundle, nil)
		if err != nil {
			t.Fatal(err)
		}
		var dialer net.Dialer
		server.http.proxy.Tr = &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, network, origin.Host)
			},
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test origin is self-signed
		}
		return server
	}
	var listenConfig net.ListenConfig
	socket, err := listenConfig.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	file, err := socket.(*net.TCPListener).File()
	if err != nil {
		t.Fatal(err)
	}
	// systemd's own descriptor, which outlives both proxies'.
	t.Cleanup(func() { _ = file.Close() })
	dup := func() net.Listener {
		listener, err := net.FileListener(file)
		if err != nil {
			t.Fatal(err)
		}
		return listener
	}
	p := &drainProxies{
		old:          newServer("old"),
		next:         newServer("next"),
		oldListener:  socket,
		nextListener: &countingListener{Listener: dup()},
		addr:         socket.Addr().String(),
		material:     prepared.Clients["sandbox-1"],
		oldServed:    make(chan error, 1),
		nextServed:   make(chan error, 1),
	}
	go func() { p.oldServed <- p.old.Serve(p.oldListener) }()
	t.Cleanup(func() {
		_ = p.old.Close()
		_ = p.next.Close()
		_ = p.nextListener.Close()
	})
	return p
}

func (p *drainProxies) startNext() {
	p.nextStarted = true
	go func() { p.nextServed <- p.next.Serve(p.nextListener) }()
}

// A draining proxy stops accepting, so a new connection reaches the proxy
// that took over; it closes a plain connection kept alive between requests,
// so its client's next request does too; and it carries a CONNECT tunnel it
// holds until the tunnel ends, which is when its drain does.
func TestADrainingProxyCarriesItsTunnelsAndGivesWayForTheRest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("goproxy's MITM leg fails before the handler runs on Windows")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var origin tunnelOrigin
	server := newOrigin(origin.handler)
	defer server.Close()
	originURL, _ := url.Parse(server.URL)
	p := newDrainProxies(ctx, t, originURL)

	client := mtlsHTTPClient(t, p.addr, p.material)
	get := func(path string) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://plain.example.test:8080"+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(body) != "ok "+path {
			t.Fatalf("GET %s = %d %q", path, resp.StatusCode, body)
		}
	}
	get("/before")
	addr, err := net.ResolveTCPAddr("tcp", p.addr)
	if err != nil {
		t.Fatal(err)
	}
	tunnel := openTunnel(ctx, t, addr, p.material, "plain.example.test:8080")
	x := tunnelExchange{conn: tunnel, reader: bufio.NewReader(tunnel)}
	if status, _ := x.do(t, http.MethodGet, "GET /tunnel-before HTTP/1.1\r\nHost: plain.example.test:8080\r\n\r\n"); status != http.StatusOK {
		t.Fatalf("tunneled GET before the drain = %d", status)
	}

	drained := make(chan error, 1)
	go func() { drained <- p.old.Drain(ctx) }()
	select {
	case err := <-p.oldServed:
		if err != nil {
			t.Fatalf("the draining proxy's Serve = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the draining proxy's Serve did not return once it stopped accepting")
	}
	p.startNext()

	// The kept-alive connection was closed between its requests: the next
	// is on a new connection, which the proxy that took over accepts.
	get("/after")
	if accepted := p.nextListener.(*countingListener).accepted.Load(); accepted != 1 {
		t.Fatalf("the proxy that took over accepted %d connections, want the client's next one", accepted)
	}
	if status, body := x.do(t, http.MethodGet, "GET /tunnel-after HTTP/1.1\r\nHost: plain.example.test:8080\r\n\r\n"); status != http.StatusOK || body != "ok /tunnel-after" {
		t.Fatalf("tunneled GET during the drain = %d %q; want the tunnel still carried", status, body)
	}
	select {
	case err := <-drained:
		t.Fatalf("the drain ended with a tunnel still open: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	_ = tunnel.Close()
	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("Drain = %v, want nil once the tunnel ended", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the drain did not end with the last tunnel")
	}
	if err := p.old.Close(); err != nil {
		t.Fatal(err)
	}
	// The socket is still open, and served.
	get("/after-close")
}

// A drain that runs past its bound returns its context's error, and the
// proxy closes what is left.
func TestADrainEndsAtItsBound(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("goproxy's MITM leg fails before the handler runs on Windows")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var origin tunnelOrigin
	server := newOrigin(origin.handler)
	defer server.Close()
	originURL, _ := url.Parse(server.URL)
	p := newDrainProxies(ctx, t, originURL)
	addr, err := net.ResolveTCPAddr("tcp", p.addr)
	if err != nil {
		t.Fatal(err)
	}
	tunnel := openTunnel(ctx, t, addr, p.material, "plain.example.test:8080")
	bound, stop := context.WithTimeout(ctx, 200*time.Millisecond)
	defer stop()
	if err := p.old.Drain(bound); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Drain past its bound = %v, want the deadline", err)
	}
	if err := p.old.Close(); err != nil {
		t.Fatal(err)
	}
	_ = tunnel.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := tunnel.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("a tunnel left at the bound read %v, want it cut", err)
	}
}

// countingListener counts the connections it accepts.
type countingListener struct {
	net.Listener
	accepted atomic.Int64
}

func (l *countingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.accepted.Add(1)
	}
	return conn, err
}
