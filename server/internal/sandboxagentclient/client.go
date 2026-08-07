// Package sandboxagentclient builds requests against a pool agent's
// sandbox-directed routes (the exec/terminal/git/tcp proxies and,
// eventually, sshd's in-process callers) from a leased HTTP client. It has
// no chi or HTTP-router dependency so any server-side caller, hand-wired
// proxy or otherwise, can share it instead of re-deriving the target URL and
// auth header logic per call site.
package sandboxagentclient

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/obot-platform/discobox/server/internal/transport"
)

// DefaultBaseURL is used when a lease carries no logical base URL.
const DefaultBaseURL = "https://pool"

// TargetURL builds the pool agent's logical URL for a sandbox-directed route:
// /api/project/{projectID}/pool/{poolID}/sandboxes/{sandboxID}{suffix} on
// baseURL. suffix must be empty or start with "/".
func TargetURL(baseURL, projectID, poolID, sandboxID, suffix string) (*url.URL, error) {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = DefaultBaseURL
	}
	target, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("parse sandbox agent client target: %w", err)
	}
	if target.Scheme == "" || target.Host == "" {
		return nil, fmt.Errorf("sandbox agent client target %q must include scheme and host", baseURL)
	}
	if suffix != "" && !strings.HasPrefix(suffix, "/") {
		return nil, fmt.Errorf("sandbox agent client suffix %q must start with /", suffix)
	}
	// projectID/poolID/sandboxID are generated identifiers (see the id
	// package) that never contain reserved path characters in practice;
	// PathEscape here is defense in depth, not a guarantee against
	// double-escaping if a caller ever passed one through with such
	// characters. suffix is caller-built and inserted as-is.
	target.Path = fmt.Sprintf(
		"/api/project/%s/pool/%s/sandboxes/%s%s",
		url.PathEscape(projectID),
		url.PathEscape(poolID),
		url.PathEscape(sandboxID),
		suffix,
	)
	target.RawPath = ""
	target.RawQuery = ""
	return target, nil
}

// AuthTransport attaches a leased client's bearer token and forwarded
// sandbox-agent token to every outbound request.
type AuthTransport struct {
	Base  http.RoundTripper
	Lease *transport.HTTPClientLease
}

func (t AuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	authToken, err := t.Lease.AuthorizationToken(req.Context())
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(authToken) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(authToken))
	} else {
		req.Header.Del("Authorization")
	}
	req.Header.Del("X-Discobox-Sandbox-Agent-Authorization")
	forwardAuthToken, err := t.Lease.ForwardAuthorizationToken(req.Context())
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(forwardAuthToken) != "" {
		req.Header.Set("X-Discobox-Sandbox-Agent-Authorization", "Bearer "+strings.TrimSpace(forwardAuthToken))
	}
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// HTTPClient returns an *http.Client that authenticates every request with
// lease's bearer and forwarded tokens, using lease's own transport (or
// http.DefaultTransport) underneath.
func HTTPClient(lease *transport.HTTPClientLease) *http.Client {
	base := http.DefaultTransport
	if lease.Client != nil && lease.Client.Transport != nil {
		base = lease.Client.Transport
	}
	return &http.Client{Transport: AuthTransport{Base: base, Lease: lease}}
}
