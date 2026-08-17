package ports

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
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

	if got := Probe(context.Background(), serverTarget(t, server)); got != ProtocolHTTP {
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

	if got := Probe(context.Background(), serverTarget(t, server)); got != ProtocolHTTPS {
		t.Fatalf("Probe = %q, want https", got)
	}
}

func TestProbeReportsTCPForAPortThatSpeaksSomethingElse(t *testing.T) {
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
			// A banner protocol: it announces itself and never speaks HTTP.
			_, _ = conn.Write([]byte("SSH-2.0-OpenSSH_9.6\r\n"))
			_ = conn.Close()
		}
	}()

	if got := Probe(context.Background(), targetOf(t, listener.Addr().String())); got != ProtocolTCP {
		t.Fatalf("Probe = %q, want tcp", got)
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

	if got := Probe(context.Background(), targetOf(t, listener.Addr().String())); got != ProtocolTCP {
		t.Fatalf("Probe = %q, want tcp", got)
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

	if got := Probe(context.Background(), target); got != ProtocolUnknown {
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

	if got := Probe(context.Background(), serverTarget(t, server)); got != ProtocolHTTP {
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

	if got := Probe(context.Background(), serverTarget(t, server)); got != ProtocolHTTP {
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

	if got := Probe(context.Background(), serverTarget(t, server)); got != ProtocolHTTP {
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
