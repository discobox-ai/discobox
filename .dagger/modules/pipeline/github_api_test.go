package main

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// Exercise the real gh API client against an isolated TLS endpoint, with a
// dummy token and a private test CA. No production host receives a request.
func TestGitHubTagAPIClient(t *testing.T) {
	var revision atomic.Value
	revision.Store("1111111111111111111111111111111111111111")
	var requests atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/repos/test/product/commits/v1.2.3" {
			http.Error(w, "unexpected API route", 404)
			return
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sha":"` + revision.Load().(string) + `"}`))
	}))
	defer server.Close()
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, cert, 0600); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{revision.Load().(string), "2222222222222222222222222222222222222222"} {
		revision.Store(want)
		cmd := exec.Command("gh", "api", "repos/test/product/commits/v1.2.3", "--jq", ".sha")
		cmd.Env = append(os.Environ(), "GH_HOST="+strings.TrimPrefix(server.URL, "https://"), "GH_TOKEN=", "GH_ENTERPRISE_TOKEN=local-test-only", "SSL_CERT_FILE="+ca, "NO_PROXY=127.0.0.1", "no_proxy=127.0.0.1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gh API: %v\n%s", err, out)
		}
		if strings.TrimSpace(string(out)) != want {
			t.Fatalf("stale revision: %s", out)
		}
	}
	if requests.Load() != 2 {
		t.Fatalf("expected two fresh API reads, got %d", requests.Load())
	}
}
