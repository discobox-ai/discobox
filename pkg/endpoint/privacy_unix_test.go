//go:build !windows

package endpoint

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultSocketPrivacy(t *testing.T) {
	for _, mode := range []string{"private", "public", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			base := t.TempDir()
			if err := os.Chmod(base, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("XDG_RUNTIME_DIR", base)
			child := filepath.Join(base, "discobox")
			switch mode {
			case "public":
				if err := os.Mkdir(child, 0o755); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(t.TempDir(), child); err != nil {
					t.Fatal(err)
				}
			}
			listener, _, cleanup, err := Listen(DefaultEndpoint())
			if mode != "private" {
				if err == nil {
					cleanup()
					t.Fatal("unsafe socket directory accepted")
				}
				parsed, _ := Parse(DefaultEndpoint())
				_, client, err := HTTPClient(parsed, nil)
				if err != nil {
					t.Fatal(err)
				}
				req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://discobox.local/", nil)
				if resp, err := client.Do(req); err == nil {
					resp.Body.Close()
					t.Fatal("unsafe directory dialed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })}
			defer server.Close()
			go func() { _ = server.Serve(listener) }()
			parsed, _ := Parse(DefaultEndpoint())
			baseURL, client, err := HTTPClient(parsed, nil)
			if err != nil {
				t.Fatal(err)
			}
			req, _ := http.NewRequestWithContext(context.Background(), "GET", baseURL, nil)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
		})
	}
}
