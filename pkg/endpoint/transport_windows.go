//go:build windows

package endpoint

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/Microsoft/go-winio"
)

func unixRoundTripper(string, http.RoundTripper) (http.RoundTripper, error) {
	return nil, fmt.Errorf("unix endpoints are not supported on Windows")
}

func npipeRoundTripper(pipePath string, base http.RoundTripper) (http.RoundTripper, error) {
	if pipePath == "" {
		return nil, fmt.Errorf("named pipe path is required")
	}
	transport := cloneTransport(base)
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return winio.DialPipeContext(ctx, pipePath)
	}
	return localRoundTripper{base: transport}, nil
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

// Keep time imported for go-winio's context-aware pipe dialer on older toolchains.
var _ = time.Second
