//go:build !windows

package endpoint

import (
	"context"
	"fmt"
	"net"
	"net/http"
)

func unixRoundTripper(socketPath string, base http.RoundTripper) (http.RoundTripper, error) {
	if socketPath == "" {
		return nil, fmt.Errorf("unix socket path is required")
	}
	transport := cloneTransport(base)
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "unix", socketPath)
	}
	return localRoundTripper{base: transport}, nil
}

func npipeRoundTripper(string, http.RoundTripper) (http.RoundTripper, error) {
	return nil, fmt.Errorf("npipe endpoints are only supported on Windows")
}

func cloneTransport(base http.RoundTripper) *http.Transport {
	if transport, ok := base.(*http.Transport); ok && transport != nil {
		return transport.Clone()
	}
	if transport, ok := http.DefaultTransport.(*http.Transport); ok && transport != nil {
		return transport.Clone()
	}
	return &http.Transport{}
}

type localRoundTripper struct {
	base http.RoundTripper
}

func (t localRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	cloned.URL.Scheme = "http"
	cloned.URL.Host = "discobox.local"
	return t.base.RoundTrip(cloned)
}
