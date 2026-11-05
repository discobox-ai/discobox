package bundlestore

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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

func TestHTTPFetcherClassifiesFailures(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 408, 429, 500, 502, 503, 504} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
			defer server.Close()
			_, err := (HTTPFetcher{BaseURL: server.URL, Transport: http.DefaultTransport}).Fetch(context.Background(), "/blob")
			want := status == 408 || status == 429 || status >= 500
			if err == nil || Retryable(err) != want {
				t.Fatalf("status %d: err=%v retryable=%v want=%v", status, err, Retryable(err), want)
			}
		})
	}
	t.Run("body interrupted", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "100")
			_, _ = io.WriteString(w, "partial")
		}))
		defer server.Close()
		body, err := (HTTPFetcher{BaseURL: server.URL, Transport: http.DefaultTransport}).Fetch(context.Background(), "/blob")
		if err != nil {
			t.Fatal(err)
		}
		defer body.Close()
		_, err = io.ReadAll(body)
		if !errors.Is(err, io.ErrUnexpectedEOF) || !Retryable(err) {
			t.Fatalf("body error = %v, retryable=%v", err, Retryable(err))
		}
	})
	t.Run("connection refused", func(t *testing.T) {
		server := httptest.NewServer(http.NotFoundHandler())
		server.Close()
		_, err := (HTTPFetcher{BaseURL: server.URL, Transport: http.DefaultTransport}).Fetch(context.Background(), "/blob")
		if !Retryable(err) {
			t.Fatalf("connection error = %v", err)
		}
	})
	t.Run("TLS verification", func(t *testing.T) {
		server := httptest.NewTLSServer(http.NotFoundHandler())
		defer server.Close()
		_, err := (HTTPFetcher{BaseURL: server.URL, Transport: http.DefaultTransport}).Fetch(context.Background(), "/blob")
		if err == nil || Retryable(err) {
			t.Fatalf("TLS error = %v, retryable=%v", err, Retryable(err))
		}
	})
}

func TestHTTPFetcherRetriesProxyConnectOutage(t *testing.T) {
	for _, status := range []int{403, 502, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
			defer proxy.Close()
			// This subtest runs in a fresh process because ProxyFromEnvironment caches
			// the environment on first use.
			//nolint:gosec // G204: re-executes this test binary with a fixed test name.
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestProxyConnectChild$")
			cmd.Env = append(os.Environ(), "DISCOBOX_TEST_PROXY_CONNECT=1", "HTTPS_PROXY="+proxy.URL, "https_proxy="+proxy.URL, "NO_PROXY=", "no_proxy=", fmt.Sprintf("DISCOBOX_TEST_PROXY_STATUS=%d", status))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%v: %s", err, out)
			}
		})
	}
}

func TestProxyConnectChild(t *testing.T) {
	if os.Getenv("DISCOBOX_TEST_PROXY_CONNECT") != "1" {
		t.Skip("subprocess only")
	}
	t.Setenv("SSL_CERT_FILE", "")
	_, err := (HTTPFetcher{}).Fetch(context.Background(), "/blob")
	want := os.Getenv("DISCOBOX_TEST_PROXY_STATUS") != "403"
	if err == nil || Retryable(err) != want {
		t.Fatalf("CONNECT: err=%v retryable=%v want=%v", err, Retryable(err), want)
	}
}
