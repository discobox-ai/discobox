package dnsforward

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/proxy"
)

// udpAnswer is the fake upstream's UDP answer to query: the start of a DNS
// header with the TC bit clear, then the query echoed back.
func udpAnswer(query string) string { return "\x00\x00\x00answer:" + query }

// startUpstream answers each message over UDP with udpAnswer and over TCP with
// "tcp:" + the message, on one port. A query starting "big" is answered over
// UDP with only a header with TC set, the way a resolver answers what does not
// fit a datagram.
func startUpstream(t *testing.T) string {
	t.Helper()
	var config net.ListenConfig
	udp, err := config.ListenPacket(t.Context(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := udp.LocalAddr().(*net.UDPAddr).Port
	tcp, err := config.Listen(t.Context(), "tcp", netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(port)).String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = udp.Close(); _ = tcp.Close() })
	go func() {
		buf := make([]byte, maxMessage)
		for {
			n, from, err := udp.ReadFrom(buf)
			if err != nil {
				return
			}
			answer := []byte(udpAnswer(string(buf[:n])))
			if bytes.HasPrefix(buf[:n], []byte("big")) {
				answer = []byte{0, 0, 0x02} // just a header with TC set
			}
			_, _ = udp.WriteTo(answer, from)
		}
	}()
	go func() {
		for {
			conn, err := tcp.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				for {
					message, err := readMessage(conn)
					if err != nil {
						return
					}
					if err := writeMessage(conn, append([]byte("tcp:"), message...)); err != nil {
						return
					}
				}
			}()
		}
	}()
	return udp.LocalAddr().String()
}

type certs struct {
	server  *tls.Config
	clients map[string]*tls.Config
}

// newCerts issues the pool's server certificate and a client certificate per
// sandbox from one CA, the way the pool proxy's bundle does.
func newCerts(t *testing.T, sandboxes ...string) certs {
	t.Helper()
	prepared, err := proxy.PrepareCertificates(proxy.PrepareOptions{
		Dir:         t.TempDir(),
		ServerHosts: []string{"127.0.0.1"},
		ClientIDs:   sandboxes,
	})
	if err != nil {
		t.Fatal(err)
	}
	bundle := prepared.Bundle
	caPEM, err := os.ReadFile(bundle.MTLSCAPath)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(caPEM)
	out := certs{
		server: &tls.Config{
			Certificates: []tls.Certificate{bundle.ServerCert},
			ClientCAs:    bundle.ClientCAPool,
			ClientAuth:   tls.RequireAndVerifyClientCert,
			MinVersion:   tls.VersionTLS12,
		},
		clients: map[string]*tls.Config{},
	}
	for id, material := range prepared.Clients {
		pair, err := tls.LoadX509KeyPair(material.ClientCertPath, material.ClientKeyPath)
		if err != nil {
			t.Fatal(err)
		}
		out.clients[id] = &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{pair}, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}
	}
	return out
}

func startServer(t *testing.T, serverTLS *tls.Config, upstream string) string {
	t.Helper()
	var config net.ListenConfig
	tcp, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { New(nil, upstream).Serve(ctx, tls.NewListener(tcp, serverTLS)); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return tcp.Addr().String()
}

func dial(t *testing.T, addr string, clientTLS *tls.Config) *tls.Conn {
	t.Helper()
	dialer := tls.Dialer{Config: clientTLS}
	conn, err := dialer.DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	return conn.(*tls.Conn)
}

func ask(t *testing.T, conn net.Conn, query string) string {
	t.Helper()
	if err := writeMessage(conn, []byte(query)); err != nil {
		t.Fatal(err)
	}
	answer, err := readMessage(conn)
	if err != nil {
		t.Fatal(err)
	}
	return string(answer)
}

func TestServerAnswersSeveralQueriesOnOneConnection(t *testing.T) {
	c := newCerts(t, "sbx_a")
	conn := dial(t, startServer(t, c.server, startUpstream(t)), c.clients["sbx_a"])
	for _, query := range []string{"first", "second"} {
		if got, want := ask(t, conn, query), udpAnswer(query); got != want {
			t.Fatalf("answer = %q, want %q", got, want)
		}
	}
}

func TestServerRetriesATruncatedAnswerOverTCP(t *testing.T) {
	c := newCerts(t, "sbx_a")
	conn := dial(t, startServer(t, c.server, startUpstream(t)), c.clients["sbx_a"])
	if got, want := ask(t, conn, "big query"), "tcp:big query"; got != want {
		t.Fatalf("answer = %q, want %q", got, want)
	}
}

func TestServerRefusesAClientWithoutACertificate(t *testing.T) {
	c := newCerts(t, "sbx_a")
	addr := startServer(t, c.server, startUpstream(t))
	anonymous := c.clients["sbx_a"].Clone()
	anonymous.Certificates = nil
	dialer := tls.Dialer{Config: anonymous}
	conn, err := dialer.DialContext(t.Context(), "tcp", addr)
	if err != nil {
		return // refused during the handshake
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	// TLS 1.3 finishes the client's side of the handshake before the server
	// has checked its certificate; the refusal arrives on first use.
	_ = writeMessage(conn, []byte("q"))
	if _, err := readMessage(conn); err == nil {
		t.Fatal("answered a client that presented no certificate")
	}
}

func TestServerCapsConnectionsPerSandbox(t *testing.T) {
	c := newCerts(t, "sbx_a", "sbx_b")
	addr := startServer(t, c.server, startUpstream(t))
	// Each held connection is answered once, so it has been admitted before
	// the next is opened.
	for range maxConnsPerClient {
		ask(t, dial(t, addr, c.clients["sbx_a"]), "q")
	}
	over := dial(t, addr, c.clients["sbx_a"])
	_ = writeMessage(over, []byte("q"))
	if _, err := readMessage(over); !errors.Is(err, io.EOF) {
		t.Fatalf("connection over the sandbox's cap: read error = %v, want EOF", err)
	}
	// Another sandbox is not starved by the first one's connections.
	if got := ask(t, dial(t, addr, c.clients["sbx_b"]), "q"); got != udpAnswer("q") {
		t.Fatalf("other sandbox's answer = %q", got)
	}
}

// The source cap is taken at accept, so connections that never complete a
// handshake — no certificate at all — are bounded too.
func TestServerCapsUnauthenticatedConnectionsPerSource(t *testing.T) {
	c := newCerts(t, "sbx_a")
	addr := startServer(t, c.server, startUpstream(t))
	var dialer net.Dialer
	for range maxConnsPerSource {
		conn, err := dialer.DialContext(t.Context(), "tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
	}
	// The raw connections are accepted in order behind this one's dial; an
	// over-cap one is closed at once, so its first read ends rather than
	// waiting out the handshake deadline.
	over, err := dialer.DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = over.Close() }()
	_ = over.SetDeadline(time.Now().Add(queryTimeout / 2))
	if _, err := over.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("connection over the source cap: read error = %v, want EOF", err)
	}
}

func TestServeReturnsOnCancel(t *testing.T) {
	c := newCerts(t)
	var config net.ListenConfig
	tcp, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { New(nil, "127.0.0.1:1").Serve(ctx, tls.NewListener(tcp, c.server)); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
}

func TestSystemUpstream(t *testing.T) {
	for _, tc := range []struct {
		name, content, want string
		wantErr             bool
	}{
		{name: "docker embedded", content: "# Generated by Docker Engine.\nnameserver 127.0.0.11\nsearch .\noptions ndots:0\n", want: "127.0.0.11:53"},
		{name: "first of several", content: "nameserver 10.0.0.2\nnameserver 10.0.0.3\n", want: "10.0.0.2:53"},
		{name: "ipv6", content: "nameserver fd00::1\n", want: "[fd00::1]:53"},
		{name: "none", content: "search .\n", wantErr: true},
		{name: "malformed", content: "nameserver not-an-ip\n", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "resolv.conf")
			if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := SystemUpstream(path)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("SystemUpstream = %q, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("SystemUpstream = %q, want %q", got, tc.want)
			}
		})
	}
}
