package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/proxy/internal/secrets"
)

// truncatingGateResolver lets every call in and answers it with a body the
// upstream cuts off after a prefix, the way a revoked files read through the
// control plane ends.
type truncatingGateResolver struct {
	gateResolver
	knownLength bool
}

const truncatedGatePrefix = "first part of the file "

func (r *truncatingGateResolver) Gate(_ context.Context, req secrets.GateRequest) (secrets.GateAdmission, error) {
	resp := &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": []string{"application/octet-stream"}},
		Body:          io.NopCloser(io.MultiReader(strings.NewReader(truncatedGatePrefix), errReader{io.ErrUnexpectedEOF})),
		ContentLength: -1,
		Request:       req.Request,
		Proto:         "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
	}
	if r.knownLength {
		resp.ContentLength = 1 << 20
		resp.Header.Set("Content-Length", "1048576")
	}
	return secrets.GateAdmission{Response: resp}, nil
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// An upstream body that fails after a prefix reaches the sandbox as an
// incomplete transfer on every path to the gate — plain HTTP through the
// proxy, HTTPS over HTTP/1.1, HTTPS over HTTP/2 — never as a clean end of a
// shorter body.
func TestHTTPProxyGateUpstreamBodyFailureIsAnIncompleteTransfer(t *testing.T) {
	for _, length := range []struct {
		name  string
		known bool
	}{{"chunked", false}, {"known-length", true}} {
		for _, path := range []struct {
			name   string
			scheme string
			h2     bool
		}{{"http", "http", false}, {"https-h1", "https", false}, {"https-h2", "https", true}} {
			t.Run(length.name+"/"+path.name, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
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
					ListenAddress:             "127.0.0.1:0",
					AllowLoopbackDestinations: true,
					CertDir:                   prepared.Bundle.Dir,
					DatabaseDSN:               filepath.Join(dir, "audit.db"),
					Allowlist:                 AllowlistConfig{Clients: map[string][]string{"sandbox-1": {"only.allowed.example.com"}}},
					Secrets:                   SecretsConfig{GateHost: testGateHost},
				}, prepared.Bundle, &truncatingGateResolver{knownLength: length.known})
				if err != nil {
					t.Fatalf("NewServer() error = %v", err)
				}
				go func() { _ = server.ListenAndServe() }()
				t.Cleanup(func() { _ = server.Close() })
				client := gateClient(t, waitForAddr(t, server).String(), prepared.Clients["sandbox-1"])
				if path.h2 {
					transport := client.Transport.(*http.Transport)
					transport.ForceAttemptHTTP2 = true
					transport.TLSClientConfig.NextProtos = []string{"h2", "http/1.1"}
				}
				reqCtx, reqCancel := context.WithTimeout(ctx, 10*time.Second)
				defer reqCancel()
				req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, path.scheme+"://"+testGateHost+"/projects/default/sandboxes/sb/files/content?path=/f", nil)
				if err != nil {
					t.Fatal(err)
				}
				resp, err := client.Do(req)
				if err != nil {
					t.Fatalf("client.Do() error = %v", err)
				}
				defer resp.Body.Close()
				if path.h2 && resp.ProtoMajor != 2 {
					t.Fatalf("response proto = %s, want HTTP/2 from the MITM", resp.Proto)
				}
				body, err := io.ReadAll(resp.Body)
				if err == nil {
					t.Fatalf("read %q (%d bytes) ending cleanly, want an incomplete transfer error", body, len(body))
				}
				if errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("read hung until the deadline after %d bytes, want the transfer to fail at once", len(body))
				}
				if string(body) != truncatedGatePrefix {
					t.Fatalf("read %q before the failure, want every byte the upstream sent, %q", body, truncatedGatePrefix)
				}
			})
		}
	}
}
