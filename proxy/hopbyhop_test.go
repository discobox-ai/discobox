package proxy

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"testing"
)

// TestHTTPProxyDropsHopByHopHeadersBeforeAnHTTP2Upstream sends HTTP/1.1
// requests carrying hop-by-hop headers through the MITM to an origin that
// speaks only HTTP/2. Go's HTTP/2 server refuses a request with a
// connection-specific header — with a 400 where extrepo's upstream sends
// GOAWAY, which the proxy answers 502 — so a header that leaked would not come
// back as a 200.
func TestHTTPProxyDropsHopByHopHeadersBeforeAnHTTP2Upstream(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("goproxy's MITM leg fails before the handler runs on Windows")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type seen struct{ proto, te, keepAlive, named string }
	saw := make(chan seen, 1)
	origin := httptest.NewUnstartedServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		saw <- seen{proto: r.Proto, te: r.Header.Get("Te"), keepAlive: r.Header.Get("Keep-Alive"), named: r.Header.Get("X-Hop")}
		_, _ = io.WriteString(w, "ok")
	}))
	origin.EnableHTTP2 = true
	origin.StartTLS()
	defer origin.Close()

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
		Recording:     RecordingConfig{Enabled: true, QueueSize: 16},
	}, prepared.Bundle, nil)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	server.http.proxy.Tr = &http.Transport{
		ForceAttemptHTTP2: true,
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test origin is self-signed
	}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	t.Cleanup(func() { _ = server.Close(); <-errCh })
	client := mitmClient(t, waitForAddr(t, server).String(), prepared.Clients["sandbox-1"])

	for _, tc := range []struct {
		name   string
		header http.Header
		wantTE string
	}{
		// What libwww-perl, and so extrepo, sends on every request.
		{name: "libwww-perl", header: http.Header{"Te": {"deflate,gzip;q=0.3"}, "Connection": {"TE"}}},
		{name: "TE gzip", header: http.Header{"Te": {"gzip"}}},
		{name: "named by Connection", header: http.Header{"Connection": {"X-Hop"}, "X-Hop": {"1"}, "Keep-Alive": {"timeout=5"}}},
		{name: "trailers", header: http.Header{"Te": {"trailers"}}, wantTE: "trailers"},
		{name: "trailers among others", header: http.Header{"Te": {"gzip, trailers"}}, wantTE: "trailers"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL+"/", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header = tc.header
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("Do() error = %v", err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, body %q; want 200", resp.StatusCode, body)
			}
			got := <-saw
			if got.proto != "HTTP/2.0" {
				t.Fatalf("origin saw %s, want HTTP/2.0", got.proto)
			}
			if got.te != tc.wantTE || got.keepAlive != "" || got.named != "" {
				t.Fatalf("origin saw TE %q, Keep-Alive %q, X-Hop %q; want TE %q and neither of the others", got.te, got.keepAlive, got.named, tc.wantTE)
			}
		})
	}
}

func TestDropHopByHopHeaders(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   http.Header
		want http.Header
	}{
		{
			name: "every hop-by-hop header and what Connection names",
			in: http.Header{
				"Connection":          {"TE, close", "X-Hop"},
				"Te":                  {"deflate,gzip;q=0.3"},
				"X-Hop":               {"1"},
				"Keep-Alive":          {"timeout=5"},
				"Proxy-Connection":    {"keep-alive"},
				"Proxy-Authorization": {"Basic eDp5"},
				"Transfer-Encoding":   {"chunked"},
				"Trailer":             {"X-Checksum"},
				"Upgrade":             {"h2c"},
				"Accept":              {"*/*"},
			},
			want: http.Header{"Accept": {"*/*"}},
		},
		{
			name: "trailers kept alone",
			in:   http.Header{"Te": {"trailers, deflate"}},
			want: http.Header{"Te": {"trailers"}},
		},
		{
			name: "an upgrade other than WebSocket loses both",
			in: http.Header{
				"Connection":     {"Upgrade, HTTP2-Settings"},
				"Upgrade":        {"h2c"},
				"Http2-Settings": {"AAMAAABkAAQAoAAAAAIAAAAA"},
				"Accept":         {"*/*"},
			},
			want: http.Header{"Accept": {"*/*"}},
		},
		{
			name: "a WebSocket handshake keeps its Connection and Upgrade",
			in: http.Header{
				"Connection":        {"keep-alive, Upgrade"},
				"Upgrade":           {"websocket"},
				"Sec-Websocket-Key": {"dGhlIHNhbXBsZSBub25jZQ=="},
			},
			want: http.Header{
				"Connection":        {"Upgrade"},
				"Upgrade":           {"websocket"},
				"Sec-Websocket-Key": {"dGhlIHNhbXBsZSBub25jZQ=="},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dropHopByHopHeaders(tc.in)
			if len(tc.in) != len(tc.want) {
				t.Fatalf("headers = %v, want %v", tc.in, tc.want)
			}
			for name, values := range tc.want {
				if got := tc.in.Values(name); len(got) != 1 || got[0] != values[0] {
					t.Fatalf("headers = %v, want %v", tc.in, tc.want)
				}
			}
		})
	}
}
