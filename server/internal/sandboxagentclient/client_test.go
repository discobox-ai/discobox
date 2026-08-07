package sandboxagentclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/obot-platform/discobox/server/internal/transport"
)

func TestTargetURL(t *testing.T) {
	tests := []struct {
		name      string
		baseURL   string
		projectID string
		poolID    string
		sandboxID string
		suffix    string
		want      string
		wantErr   bool
	}{
		{
			name:      "default base URL",
			projectID: "proj_1",
			poolID:    "pool_1",
			sandboxID: "sbx_1",
			suffix:    "/execs",
			want:      "https://pool/api/project/proj_1/pool/pool_1/sandboxes/sbx_1/execs",
		},
		{
			name:      "explicit base URL, empty suffix",
			baseURL:   "https://pool.internal:8443/",
			projectID: "proj_1",
			poolID:    "pool_1",
			sandboxID: "sbx_1",
			want:      "https://pool.internal:8443/api/project/proj_1/pool/pool_1/sandboxes/sbx_1",
		},
		{
			// projectID/poolID/sandboxID are generated identifiers that never
			// contain reserved characters in practice (see the id package);
			// this documents the actual (pre-existing) behavior rather than
			// an idealized escaping scheme, since url.URL.Path re-escapes
			// whatever was written into it.
			name:      "IDs with reserved characters double-escape (inherited from the pre-extraction proxies)",
			baseURL:   "https://pool",
			projectID: "proj 1",
			poolID:    "pool_1",
			sandboxID: "sbx_1",
			suffix:    "/tcp/attach",
			want:      "https://pool/api/project/proj%25201/pool/pool_1/sandboxes/sbx_1/tcp/attach",
		},
		{
			name:      "malformed base URL",
			baseURL:   "://bad",
			projectID: "p",
			poolID:    "p",
			sandboxID: "s",
			wantErr:   true,
		},
		{
			name:      "base URL missing host",
			baseURL:   "not-a-url",
			projectID: "p",
			poolID:    "p",
			sandboxID: "s",
			wantErr:   true,
		},
		{
			name:      "suffix without leading slash",
			projectID: "p",
			poolID:    "p",
			sandboxID: "s",
			suffix:    "execs",
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := TargetURL(tt.baseURL, tt.projectID, tt.poolID, tt.sandboxID, tt.suffix)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got none (url=%v)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.String() != tt.want {
				t.Fatalf("got %q, want %q", got.String(), tt.want)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAuthTransportRoundTrip(t *testing.T) {
	t.Run("sets bearer and forwarded headers", func(t *testing.T) {
		var gotAuth, gotForward string
		base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
			gotAuth = req.Header.Get("Authorization")
			gotForward = req.Header.Get("X-Discobox-Sandbox-Agent-Authorization")
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
		})
		lease := &transport.HTTPClientLease{
			AuthToken: "server-token",
			ForwardAuthTokenProvider: func(context.Context) (string, error) {
				return "sandbox-token", nil
			},
		}
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "https://pool/x", nil)
		req.Header.Set("X-Discobox-Sandbox-Agent-Authorization", "Bearer stale")
		tr := AuthTransport{Base: base, Lease: lease}
		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		resp.Body.Close()
		if gotAuth != "Bearer server-token" {
			t.Fatalf("Authorization = %q", gotAuth)
		}
		if gotForward != "Bearer sandbox-token" {
			t.Fatalf("forwarded header = %q", gotForward)
		}
	})

	t.Run("clears stale forward header when nothing to forward", func(t *testing.T) {
		var sawForward bool
		base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
			_, sawForward = req.Header["X-Discobox-Sandbox-Agent-Authorization"]
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
		})
		lease := &transport.HTTPClientLease{}
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "https://pool/x", nil)
		req.Header.Set("X-Discobox-Sandbox-Agent-Authorization", "Bearer stale")
		tr := AuthTransport{Base: base, Lease: lease}
		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		resp.Body.Close()
		if sawForward {
			t.Fatalf("expected forwarded header to be cleared")
		}
	})

	t.Run("clears authorization when no token", func(t *testing.T) {
		var sawAuth bool
		base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
			_, sawAuth = req.Header["Authorization"]
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
		})
		lease := &transport.HTTPClientLease{}
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "https://pool/x", nil)
		req.Header.Set("Authorization", "Bearer stale")
		tr := AuthTransport{Base: base, Lease: lease}
		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		resp.Body.Close()
		if sawAuth {
			t.Fatalf("expected Authorization header to be cleared")
		}
	})

	t.Run("propagates auth token provider error", func(t *testing.T) {
		wantErr := errors.New("boom")
		base := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			t.Fatal("base transport should not be called on provider error")
			return nil, nil
		})
		lease := &transport.HTTPClientLease{
			AuthTokenProvider: func(context.Context) (string, error) { return "", wantErr },
		}
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "https://pool/x", nil)
		tr := AuthTransport{Base: base, Lease: lease}
		resp, err := tr.RoundTrip(req)
		if resp != nil {
			resp.Body.Close()
		}
		if !errors.Is(err, wantErr) {
			t.Fatalf("got err %v, want %v", err, wantErr)
		}
	})

	t.Run("defaults to http.DefaultTransport when Base is nil", func(t *testing.T) {
		lease := &transport.HTTPClientLease{}
		tr := AuthTransport{Lease: lease}
		if tr.Base != nil {
			t.Fatalf("expected nil Base in this test setup")
		}
		// RoundTrip must not panic dereferencing a nil Base; use an httptest server
		// so the default transport has somewhere real to dial.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
		defer srv.Close()
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
		req.RequestURI = ""
		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("status = %d", resp.StatusCode)
		}
	})
}

func TestHTTPClient(t *testing.T) {
	lease := &transport.HTTPClientLease{AuthToken: "tok"}
	client := HTTPClient(lease)
	tr, ok := client.Transport.(AuthTransport)
	if !ok {
		t.Fatalf("Transport = %T, want AuthTransport", client.Transport)
	}
	if tr.Lease != lease {
		t.Fatalf("lease not wired through")
	}
	if tr.Base != http.DefaultTransport {
		t.Fatalf("expected default transport when lease.Client is nil")
	}

	custom := roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, nil })
	lease2 := &transport.HTTPClientLease{Client: &http.Client{Transport: custom}}
	client2 := HTTPClient(lease2)
	tr2 := client2.Transport.(AuthTransport)
	if tr2.Base == nil {
		t.Fatalf("expected lease client's transport to be used as base")
	}
}
