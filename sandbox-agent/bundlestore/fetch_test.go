package bundlestore

import (
	"context"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHTTPFetcherTrustsTheCAThatSSLCertFileNamesWhenItFetches(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/blobs/sha256:x" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, "blob")
	}))
	defer server.Close()
	ca := filepath.Join(t.TempDir(), "mitm-ca.crt")
	t.Setenv("SSL_CERT_FILE", ca)
	fetcher := HTTPFetcher{BaseURL: server.URL}
	// The CA arrives after the box boots, with its runtime config.
	if _, err := fetcher.Fetch(context.Background(), "/blobs/sha256:x"); err == nil || !strings.Contains(err.Error(), "the proxy's CA") {
		t.Fatalf("Fetch before the CA = %v, want it to name the missing CA", err)
	}
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(ca, cert, 0o644); err != nil {
		t.Fatal(err)
	}
	body, err := fetcher.Fetch(context.Background(), "/blobs/sha256:x")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(body)
	body.Close()
	if string(data) != "blob" {
		t.Fatalf("body = %q", data)
	}
	if _, err := fetcher.Fetch(context.Background(), "/blobs/sha256:y"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("Fetch of what the host does not answer = %v, want a 404", err)
	}
}
