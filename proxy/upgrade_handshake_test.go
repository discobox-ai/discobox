package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestHTTPProxyMITMUpgradeHandshakeIsNotFragmented pins how a 101 handshake
// response reaches an intercepted client: as few reads, not one per header
// fragment.
//
// A WebSocket client reads the handshake response itself, before any framing
// exists to reassemble it, so the read boundaries the proxy creates are the
// ones it sees. Writing the response header field by field onto a TLS
// connection puts each fragment in its own TLS record — key, ": ", value,
// CRLF, four records per header — and a strict client counts those. tungstenite
// (Rust; what Codex CLI uses for wss://chatgpt.com/backend-api/codex/responses)
// treats more than 64 reads averaging under 128 bytes as a slow-loris attempt
// and fails the handshake with "Attack attempt detected", which Codex reports
// as "Falling back from WebSockets to HTTPS transport". Roughly 17 response
// headers is enough to cross that line, which any CDN-fronted endpoint sends.
//
// The proxy must not be the thing that fragments it. This asserts the property
// (few reads for the whole header) rather than the mechanism, so it keeps
// holding if the write path moves.
func TestHTTPProxyMITMUpgradeHandshakeIsNotFragmented(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A CDN-fronted handshake response: the required headers plus enough edge
	// headers to cross tungstenite's threshold when each one is fragmented.
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijack unsupported", http.StatusInternalServerError)
			return
		}
		conn, rw, err := hijacker.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		var head strings.Builder
		head.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
		head.WriteString("Connection: Upgrade\r\n")
		head.WriteString("Upgrade: websocket\r\n")
		head.WriteString("Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n")
		for i := range 16 {
			fmt.Fprintf(&head, "X-Edge-Header-%02d: v%02d\r\n", i, i)
		}
		head.WriteString("\r\n")
		_, _ = rw.WriteString(head.String())
		_ = rw.Flush()
		// Hold the upgraded connection open until the client is done reading.
		buf := make([]byte, 4)
		_, _ = rw.Read(buf)
	}))
	origin.EnableHTTP2 = false
	origin.StartTLS()
	defer origin.Close()
	originURL, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	prepared, err := PrepareCertificates(PrepareOptions{
		Dir:         filepath.Join(dir, "certs"),
		ProxyURL:    "https://127.0.0.1:0",
		ServerHosts: []string{"127.0.0.1", "localhost"},
		ClientIDs:   []string{"sandbox-1"},
	})
	if err != nil {
		t.Fatalf("PrepareCertificates() error = %v", err)
	}
	server, err := NewServer(ctx, Config{
		ListenAddress: "127.0.0.1:0",
		CertDir:       prepared.Bundle.Dir,
		DatabaseDSN:   filepath.Join(dir, "audit.db"),
		Recording:     RecordingConfig{Enabled: true, QueueSize: 16, StreamDir: filepath.Join(dir, "streams")},
	}, prepared.Bundle, nil)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	// The self-signed test origin would be rejected by the proxy's verifying
	// upstream transport.
	server.http.proxy.Tr = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // test origin is self-signed
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	t.Cleanup(closeProxyServer(t, server, errCh))
	addr := waitForAddr(t, server)

	// CONNECT, then speak TLS to the proxy's MITM certificate, which is the
	// path a sandbox's `wss://` connection takes.
	conn := dialProxyMTLS(ctx, t, addr.String(), prepared.Clients["sandbox-1"])
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", originURL.Host, originURL.Host); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	connectReader := bufio.NewReader(conn)
	connectResp, err := http.ReadResponse(connectReader, nil)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	defer connectResp.Body.Close()
	if connectResp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", connectResp.StatusCode)
	}
	if buffered := connectReader.Buffered(); buffered > 0 {
		t.Fatalf("proxy sent %d bytes after the CONNECT response; the TLS handshake below would lose them", buffered)
	}

	mitmCA, err := os.ReadFile(prepared.Clients["sandbox-1"].MITMCAPath)
	if err != nil {
		t.Fatalf("read MITM CA: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(mitmCA) {
		t.Fatal("parse MITM CA")
	}
	inner := tls.Client(conn, &tls.Config{
		RootCAs:    pool,
		ServerName: originURL.Hostname(),
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
	})
	if err := inner.HandshakeContext(ctx); err != nil {
		t.Fatalf("inner TLS handshake: %v", err)
	}
	if _, err := fmt.Fprintf(inner,
		"GET / HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n",
		originURL.Host); err != nil {
		t.Fatalf("write upgrade request: %v", err)
	}

	// Read the way a WebSocket client does — raw, until the header terminator —
	// counting what each read returns.
	if err := inner.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var (
		header []byte
		reads  []int
		total  int
	)
	buf := make([]byte, 4096)
	for !strings.Contains(string(header), "\r\n\r\n") {
		n, err := inner.Read(buf)
		if n > 0 {
			reads = append(reads, n)
			total += n
			header = append(header, buf[:n]...)
		}
		if err != nil {
			t.Fatalf("read handshake response after %d reads (%q): %v", len(reads), header, err)
		}
		if len(reads) > 512 {
			break // tungstenite's hard packet ceiling; enough to fail on below
		}
	}
	head := string(header)
	if idx := strings.Index(head, "\r\n\r\n"); idx >= 0 {
		head = head[:idx+4]
	}

	// tungstenite's own rule, applied to what this proxy produced.
	if len(reads) > 64 && len(reads)*128 > total {
		t.Errorf("handshake arrived in %d reads for %d bytes, which a strict WebSocket client rejects as an attack (read sizes: %v)\nresponse:\n%s",
			len(reads), total, reads, head)
	}
	// A 1xx response carries no body, so it must carry no framing header
	// either; a client that believes one would look for a body that never comes.
	if strings.Contains(strings.ToLower(head), "transfer-encoding") {
		t.Errorf("101 response carries a Transfer-Encoding header:\n%s", head)
	}
	if !strings.Contains(head, "Sec-Websocket-Accept") && !strings.Contains(head, "Sec-WebSocket-Accept") {
		t.Errorf("101 response lost the handshake accept header:\n%s", head)
	}
}
