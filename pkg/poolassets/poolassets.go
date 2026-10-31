// Package poolassets reads the server's asset routes as a pool (ADR
// 26-10-16-423 §13): a manifest or blob the pool's scope pins, under
// /api/pools/{poolId}/assets, with an assertion the pool signs with its
// identity key. The pool agent reads through it to fill its cache (ADR
// 26-10-24-953 §3). A pool's own bundles are not read through it: its maker
// places them in its box (ADR 26-10-30-875 §5).
package poolassets

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/discobox-ai/discobox/pkg/poolauth"
)

// Prefix is where a pool's asset routes are, {poolId} its own.
const Prefix = "/api/pools/{poolId}/assets"

// Client reads one pool's asset routes.
type Client struct {
	// BaseURL is the control plane's HTTP base URL, as wire.HTTPClient
	// answers it.
	BaseURL string
	// HTTP is the client the control plane is reached with: never one that
	// honors HTTP_PROXY, since a pool reaches its control plane directly.
	HTTP *http.Client
	// ProjectID and PoolID are the pool's; Key is its identity key.
	ProjectID string
	PoolID    string
	Key       ed25519.PrivateKey
}

// Fetch reads path beneath the pool's asset prefix,
// /{name}/manifests/{digest} or /blobs/{digest}, or /{name}. A status other
// than 200 is an error; the body is the caller's to close, and to check
// against its digest: a response is never trusted for its content.
func (c *Client) Fetch(ctx context.Context, path string) (io.ReadCloser, error) {
	baseURL := strings.TrimRight(c.BaseURL, "/")
	if baseURL == "" {
		return nil, fmt.Errorf("control plane URL is required")
	}
	poolID, projectID := strings.TrimSpace(c.PoolID), strings.TrimSpace(c.ProjectID)
	if poolID == "" || projectID == "" {
		return nil, fmt.Errorf("project and pool IDs are required")
	}
	token, err := poolauth.CreateToken(c.Key, poolauth.Claims{ProjectID: projectID, PoolID: poolID})
	if err != nil {
		return nil, fmt.Errorf("create pool assertion: %w", err)
	}
	prefix := strings.ReplaceAll(Prefix, "{poolId}", url.PathEscape(poolID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+prefix+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if c.HTTP == nil {
		return nil, fmt.Errorf("no client to reach the control plane with")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("asset read %s failed: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return resp.Body, nil
}
