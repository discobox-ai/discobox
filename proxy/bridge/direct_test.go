package bridge_test

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/proxy/bridge"
)

// localForwarder serves a forwarder whose host is directly connected to
// subnets, in front of a pool that answers with the client's identity, and
// returns a connection to it.
func localForwarder(t *testing.T, subnets ...string) net.Conn {
	t.Helper()
	prepared := poolMaterial(t)
	material := prepared.Clients["sandbox-1"]
	forwarder, err := bridge.New(context.Background(), bridge.Config{
		WorkerProxyURL: unixPool(t, prepared.Bundle),
		ServerName:     poolServerName,
		MTLSCAPath:     material.MTLSCAPath,
		ClientCertPath: material.ClientCertPath,
		ClientKeyPath:  material.ClientKeyPath,
		LocalSubnets:   func() []string { return subnets },
	})
	if err != nil {
		t.Fatalf("bridge.New: %v", err)
	}
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = forwarder.Serve(listener) }()
	t.Cleanup(func() { _ = forwarder.Close() })

	var dialer net.Dialer
	conn, err := dialer.DialContext(context.Background(), "tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial forwarder: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	return conn
}

// echoServer serves loopback, echoing each connection line by line. It keeps
// accepting, so the agent's probe of a new port does not take the only one.
func echoServer(t *testing.T) string {
	t.Helper()
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				reader := bufio.NewReader(conn)
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					if _, err := io.WriteString(conn, line); err != nil {
						return
					}
				}
			}()
		}
	}()
	return listener.Addr().String()
}

// The pool proxy cannot reach a sandbox's own networks, so a CONNECT to an
// address in one is tunneled directly — whether or not the client could match
// the subnet in NO_PROXY.
func TestForwarderTunnelsConnectToALocalSubnetDirectly(t *testing.T) {
	target := echoServer(t)
	conn := localForwarder(t, "10.0.0.0/8", "127.0.0.0/8")

	if _, err := io.WriteString(conn, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n\r\n"); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
	}
	if _, err := io.WriteString(conn, "ping\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if echo, err := reader.ReadString('\n'); err != nil || echo != "ping\n" {
		t.Fatalf("echo = %q, %v; want ping", echo, err)
	}
}

// originHead serves loopback, sends the head of the first request it reads
// on seen, and answers with a body of its own. A HEAD request is skipped: a
// discobox's agent probes every new port with one, and no test here sends it.
func originHead(t *testing.T, seen chan<- string) string {
	t.Helper()
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				reader := bufio.NewReader(conn)
				var head strings.Builder
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					head.WriteString(line)
					if line == "\r\n" {
						break
					}
				}
				if strings.HasPrefix(head.String(), "HEAD ") {
					return
				}
				seen <- head.String()
				_, _ = io.WriteString(conn, "HTTP/1.0 200 OK\r\nConnection: close\r\n\r\ndirect")
			}()
		}
	}()
	return listener.Addr().String()
}

// An absolute-form request (how most clients send plain HTTP to a proxy) for a
// local address reaches the origin as the client wrote it, except for what a
// proxy must change: origin form, no proxy or hop-by-hop headers, and
// "Connection: close", so the client cannot reuse the connection for a host
// this one was not decided for. Nothing is added or upgraded on the way.
func TestForwarderSendsAbsoluteFormRequestToALocalSubnetDirectly(t *testing.T) {
	seen := make(chan string, 1)
	host := originHead(t, seen)
	conn := localForwarder(t, "127.0.0.0/8")

	request := "GET http://" + host + "/a|b\"{c}%2F?q=1 HTTP/1.0\r\nHost: " + host +
		"\r\nConnection: keep-alive, X-Hop\r\nProxy-Connection: keep-alive" +
		"\r\nProxy-Authorization: Basic eDp5\r\nKeep-Alive: timeout=5\r\nX-Hop: 1\r\nX-Kept: 2\r\n\r\n"
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatalf("write request: %v", err)
	}
	body, err := io.ReadAll(conn)
	if err != nil || !strings.HasSuffix(string(body), "\r\n\r\ndirect") {
		t.Fatalf("response = %q, %v; want the origin's", body, err)
	}
	want := "GET /a|b\"{c}%2F?q=1 HTTP/1.0\r\nHost: " + host + "\r\nX-Kept: 2\r\nConnection: close\r\n\r\n"
	if got := <-seen; got != want {
		t.Fatalf("origin saw\n%q\nwant\n%q", got, want)
	}
}

// A target with no path takes "/", and a query straight after the authority
// keeps every byte, slashes included.
func TestForwarderGivesAnEmptyPathItsSlash(t *testing.T) {
	for target, want := range map[string]string{
		"":             "/",
		"?next=/a/b":   "/?next=/a/b",
		"/p?next=/a/b": "/p?next=/a/b",
	} {
		t.Run(want, func(t *testing.T) {
			seen := make(chan string, 1)
			host := originHead(t, seen)
			conn := localForwarder(t, "127.0.0.0/8")
			if _, err := io.WriteString(conn, "GET http://"+host+target+" HTTP/1.1\r\nHost: "+host+"\r\n\r\n"); err != nil {
				t.Fatalf("write request: %v", err)
			}
			if got, line := <-seen, "GET "+want+" HTTP/1.1\r\n"; !strings.HasPrefix(got, line) {
				t.Fatalf("origin saw %q, want it to start %q", got, line)
			}
		})
	}
}

// An upgrade request keeps the headers that ask for it, so a WebSocket to a
// local address still upgrades.
func TestForwarderKeepsAnUpgradeRequestsConnectionHeader(t *testing.T) {
	seen := make(chan string, 1)
	host := originHead(t, seen)
	conn := localForwarder(t, "127.0.0.0/8")

	request := "GET http://" + host + "/ws HTTP/1.1\r\nHost: " + host +
		"\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n"
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatalf("write request: %v", err)
	}
	want := "GET /ws HTTP/1.1\r\nHost: " + host + "\r\nUpgrade: websocket\r\nConnection: upgrade\r\n\r\n"
	if got := <-seen; got != want {
		t.Fatalf("origin saw\n%q\nwant\n%q", got, want)
	}
}

// A local address nothing answers on gets a 502 from the forwarder, not a
// bare reset.
func TestForwarderAnswersBadGatewayForAnUnreachableLocalAddress(t *testing.T) {
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	closed := listener.Addr().String()
	_ = listener.Close()
	conn := localForwarder(t, "127.0.0.0/8")

	if _, err := io.WriteString(conn, "CONNECT "+closed+" HTTP/1.1\r\nHost: "+closed+"\r\n\r\n"); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

// Everything that is not an HTTP proxy request for a local address still goes
// to the pool proxy: another address, a name (even a local one), and a
// protocol that is not HTTP at all.
func TestForwarderSendsEverythingElseToThePool(t *testing.T) {
	for name, start := range map[string]string{
		"remote address": "CONNECT 192.0.2.1:443 HTTP/1.1\r\n\r\n",
		"name":           "CONNECT localhost:443 HTTP/1.1\r\n\r\n",
		"https absolute": "GET https://127.0.0.1/ HTTP/1.1\r\n\r\n",
		"socks5":         "\x05\x01\x00",
	} {
		t.Run(name, func(t *testing.T) {
			conn := localForwarder(t, "127.0.0.0/8")
			if _, err := io.WriteString(conn, start); err != nil {
				t.Fatalf("write: %v", err)
			}
			if _, identity, _ := readIdentity(t, conn); identity != "sandbox-1" {
				t.Fatalf("pool saw client %q, want sandbox-1", identity)
			}
		})
	}
}
