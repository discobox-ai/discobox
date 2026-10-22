package bundlestore

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// AssetsURL is where a box fetches its bundles: the pool's proxy answers it,
// read-only, with only what the sandbox's own pins reach (ADR 26-10-16-423
// §13).
const AssetsURL = "https://assets.discobox.internal"

// stall is how long a fetch may wait for its response, or for its next byte,
// before it is given up, as a provisioning download is.
var stall = time.Minute

// HTTPFetcher fetches from the asset host through the sandbox's proxy, the
// environment's (HTTPS_PROXY), trusting the system's roots and the one
// SSL_CERT_FILE names: the pool proxy's CA, which the runtime config
// delivers after boot. That file is read on every fetch rather than once,
// so a fetch is never judged by a pool of roots taken before it arrived.
type HTTPFetcher struct {
	// BaseURL is AssetsURL but in tests.
	BaseURL string
	// Transport is used as it is when set, in tests.
	Transport http.RoundTripper
}

// Fetch reads path of the asset host. A status other than 200 is an error.
func (f HTTPFetcher) Fetch(ctx context.Context, path string) (io.ReadCloser, error) {
	transport := f.Transport
	if transport == nil {
		roots, err := roots()
		if err != nil {
			return nil, err
		}
		transport = &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			TLSClientConfig:       &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: stall,
		}
	}
	base := f.BaseURL
	if base == "" {
		base = AssetsURL
	}
	url := strings.TrimSuffix(base, "/") + path
	ctx, cancel := context.WithCancelCause(ctx)
	stalled := time.AfterFunc(stall, func() {
		cancel(fmt.Errorf("GET %s: nothing received for %s", url, stall))
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err == nil {
		var resp *http.Response
		if resp, err = (&http.Client{Transport: transport}).Do(req); err == nil {
			if resp.StatusCode == http.StatusOK {
				return &stallReader{body: resp.Body, ctx: ctx, stalled: stalled, cancel: cancel}, nil
			}
			resp.Body.Close()
			err = fmt.Errorf("GET %s: %s", url, resp.Status)
		}
	}
	stalled.Stop()
	cancel(nil)
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		return nil, cause
	}
	return nil, err
}

// roots is the system's roots and SSL_CERT_FILE's certificates.
func roots() (*x509.CertPool, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	path := os.Getenv("SSL_CERT_FILE")
	if path == "" {
		return pool, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read the proxy's CA %s: %w", path, err)
	}
	if !pool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("the proxy's CA %s holds no certificate", path)
	}
	return pool, nil
}

// stallReader is a response body whose request is canceled once no byte has
// arrived for stall.
type stallReader struct {
	body    io.ReadCloser
	ctx     context.Context
	stalled *time.Timer
	cancel  context.CancelCauseFunc
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.body.Read(p)
	if n > 0 {
		s.stalled.Reset(stall)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		if cause := context.Cause(s.ctx); cause != nil {
			return n, cause
		}
	}
	return n, err
}

func (s *stallReader) Close() error {
	s.stalled.Stop()
	err := s.body.Close()
	s.cancel(nil)
	return err
}
