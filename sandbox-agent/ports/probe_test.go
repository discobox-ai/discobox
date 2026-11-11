package ports

import (
	"bufio"
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func targetOf(t *testing.T, address string) netip.AddrPort {
	t.Helper()
	target, err := netip.ParseAddrPort(address)
	if err != nil {
		t.Fatalf("parse %q: %v", address, err)
	}
	return target
}

func serverTarget(t *testing.T, server *httptest.Server) netip.AddrPort {
	t.Helper()
	return targetOf(t, server.Listener.Addr().String())
}

func TestProbeIdentifiesPlainHTTP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	if got := Probe(context.Background(), serverTarget(t, server)).Protocol; got != ProtocolHTTP {
		t.Fatalf("Probe = %q, want http", got)
	}
}

func TestProbeIdentifiesHTTPSBehindASelfSignedCertificate(t *testing.T) {
	// httptest's TLS server uses a certificate nothing in the world trusts,
	// which is exactly the shape of a dev server inside a sandbox: rejecting it
	// would report "not https" for the case this is for.
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if got := Probe(context.Background(), serverTarget(t, server)).Protocol; got != ProtocolHTTPS {
		t.Fatalf("Probe = %q, want https", got)
	}
}

func TestProbeReportsTCPForAPortThatSpeaksSomethingElse(t *testing.T) {
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() {
		_ = listener.Close()
		<-done
	})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.SetDeadline(time.Now().Add(probeIOTimeout))
			// A banner protocol: it announces itself and never speaks HTTP.
			_, writeErr := conn.Write([]byte("SSH-2.0-OpenSSH_9.6\r\n"))
			// Consume the complete probe request before closing. Closing with
			// unread input can reset TCP, racing the probe's write/read on
			// Windows and hiding a banner the fixture successfully wrote.
			_, readErr := http.ReadRequest(bufio.NewReader(conn))
			_ = conn.Close()
			if writeErr != nil || readErr != nil {
				t.Errorf("banner exchange: write: %v; read: %v", writeErr, readErr)
				return
			}
		}
	}()

	// Exercise both sides of the socket scheduling race on every CI platform.
	for attempt := range 100 {
		if got := Probe(t.Context(), targetOf(t, listener.Addr().String())); got != (ProbeResult{Protocol: ProtocolTCP}) {
			t.Fatalf("attempt %d: Probe = %+v, want tcp with bytes", attempt, got)
		}
	}
}

func TestProbeReportsTCPForAPortThatAcceptsAndSaysNothing(t *testing.T) {
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			// Accept and hang up: could be a TLS server that refused the
			// plaintext request, so the probe tries a handshake and only then
			// gives up on HTTP.
			_ = conn.Close()
		}
	}()

	if got := Probe(context.Background(), targetOf(t, listener.Addr().String())); got != (ProbeResult{Protocol: ProtocolTCP, Silent: true}) {
		t.Fatalf("Probe = %+v, want silent tcp", got)
	}
}

func TestProbeReportsUnknownWhenNothingIsListening(t *testing.T) {
	// Bind and release: the port is almost certainly free, and "unreachable"
	// must be unknown-and-retry rather than a claim about what is behind it.
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	target := targetOf(t, listener.Addr().String())
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	if got := Probe(context.Background(), target).Protocol; got != ProtocolUnknown {
		t.Fatalf("Probe = %q, want unknown", got)
	}
}

func TestProbeIdentifiesHTTPWhenTheServerHasNoRootRoute(t *testing.T) {
	// A dev server with no route at / is the everyday case, and a 404 is proof
	// the request was parsed as HTTP -- it must not cost a second connection.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, &http.Request{})
	}))
	defer server.Close()

	if got := Probe(context.Background(), serverTarget(t, server)).Protocol; got != ProtocolHTTP {
		t.Fatalf("Probe = %q, want http", got)
	}
}

func TestProbeIdentifiesHTTPWhenTheServerRefusesHEAD(t *testing.T) {
	// The probe asks HEAD so a dev server does not render a page to produce a
	// body it discards. A server that only handles GET says so in HTTP, which
	// is all the classification needs.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if got := Probe(context.Background(), serverTarget(t, server)).Protocol; got != ProtocolHTTP {
		t.Fatalf("Probe = %q, want http", got)
	}
}

func TestProbeAsksHEADSoNoBodyIsProduced(t *testing.T) {
	methods := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods <- r.Method
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if got := Probe(context.Background(), serverTarget(t, server)).Protocol; got != ProtocolHTTP {
		t.Fatalf("Probe = %q, want http", got)
	}
	close(methods)
	for method := range methods {
		if method != http.MethodHead {
			t.Fatalf("server saw a %s, want only HEAD", method)
		}
	}
}

func TestMayBeTLSDistinguishesAnHTTPSHintFromAPlaintextAnswer(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		reply     []byte
		plaintext Protocol
		want      bool
	}{
		{name: "alert record", reply: []byte{0x15, 0x03, 0x03}, plaintext: ProtocolTCP, want: true},
		{name: "handshake record", reply: []byte{0x16, 0x03, 0x01}, plaintext: ProtocolTCP, want: true},
		{name: "no reply at all", reply: nil, plaintext: ProtocolTCP, want: true},
		{name: "plaintext banner", reply: []byte("SSH-2.0-OpenSSH_9.6\r\n"), plaintext: ProtocolTCP, want: false},
		{name: "redis error", reply: []byte("-ERR unknown command\r\n"), plaintext: ProtocolTCP, want: false},
		{
			name:      "https server answering plaintext",
			reply:     []byte("HTTP/1.0 400 Bad Request\r\n"),
			plaintext: ProtocolHTTP,
			want:      true,
		},
		{name: "ordinary http reply", reply: []byte("HTTP/1.1 200 OK\r\n"), plaintext: ProtocolHTTP, want: false},
		{name: "http reply with no route", reply: []byte("HTTP/1.1 404 Not Found\r\n"), plaintext: ProtocolHTTP, want: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := mayBeTLS(testCase.reply, testCase.plaintext); got != testCase.want {
				t.Fatalf("mayBeTLS(%q, %q) = %v, want %v", testCase.reply, testCase.plaintext, got, testCase.want)
			}
		})
	}
}

// Bytes rejected during TLS still prove the service was answering.
func TestProbeTLSAlertIsNotSilent(t *testing.T) {
	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 512)
			n, _ := c.Read(buf)
			if n > 0 && buf[0] == 0x16 {
				_, _ = c.Write([]byte{0x15, 0x03, 0x03, 0x00, 0x02, 0x02, 0x28})
			}
			_ = c.Close()
		}
	}()
	if got := Probe(t.Context(), targetOf(t, l.Addr().String())); got != (ProbeResult{Protocol: ProtocolTCP}) {
		t.Fatalf("Probe = %+v, want non-silent TCP", got)
	}
}

func TestProbeSuccessfulTLSHandshakeWithoutApplicationReplyIsSilent(t *testing.T) {
	certServer := httptest.NewTLSServer(http.NotFoundHandler())
	cert := certServer.TLS.Certificates[0]
	certServer.Close()
	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := l.Accept()
		if err != nil {
			return
		}
		// The plaintext attempt receives nothing.
		_, _ = bufio.NewReader(c).ReadString('\n')
		_ = c.Close()
		c, err = l.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
		if tc.HandshakeContext(t.Context()) != nil {
			return
		}
		// Read a complete HEAD, then close without an application response.
		_, _ = http.ReadRequest(bufio.NewReader(tc))
	}()
	if got := Probe(t.Context(), targetOf(t, l.Addr().String())); got != (ProbeResult{Protocol: ProtocolTCP, Silent: true}) {
		t.Fatalf("Probe = %+v, want silent TCP after successful TLS handshake", got)
	}
	<-done
}
