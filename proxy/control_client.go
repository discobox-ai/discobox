package proxy

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/proxy/internal/audit"
)

// AuditHTTPExchange is one audited HTTP exchange, as the control API returns it.
type AuditHTTPExchange = audit.HTTPExchange

// AuditQuery narrows a control API audit read. ClientID is not set here: a
// ControlClient carries the sandbox in the token it signs, which is what the
// proxy narrows by (see scopeToToken).
type AuditQuery = audit.QueryOptions

// ControlClient reads a proxy's control API. It is the other half of
// ControlHandler, kept in this package so the paths, parameters, token and
// response shape have one owner rather than one on each side of the wire.
//
// It signs its own short-lived token on every call, so it holds the control
// private key — the same custody as whatever holds the proxy's CA keys.
type ControlClient struct {
	baseURL string
	key     ed25519.PrivateKey
	claims  ControlTokenClaims
	http    *http.Client
}

// NewControlClient returns a client for the control API at baseURL. projectID
// and workerID are the claims the proxy's Control config checks when it sets
// them. A nil httpClient uses a default with a timeout.
func NewControlClient(baseURL string, key ed25519.PrivateKey, projectID, workerID string, httpClient *http.Client) *ControlClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &ControlClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		key:     key,
		claims:  ControlTokenClaims{ProjectID: projectID, WorkerID: workerID, Scopes: []string{ScopeAuditRead}},
		http:    httpClient,
	}
}

// ListHTTP returns audited HTTP exchanges, newest first. A non-empty sandboxID
// scopes the token to that sandbox, and the proxy narrows the read to it
// whatever the query says; an empty one is the pool-wide read.
func (c *ControlClient) ListHTTP(ctx context.Context, sandboxID string, query AuditQuery) ([]AuditHTTPExchange, error) {
	claims := c.claims
	claims.SandboxID = sandboxID
	token, err := CreateControlToken(c.key, claims)
	if err != nil {
		return nil, err
	}
	params := url.Values{}
	if query.Host != "" {
		params.Set("host", query.Host)
	}
	if query.UseID != "" {
		params.Set("use_id", query.UseID)
	}
	if !query.Since.IsZero() {
		params.Set("since", query.Since.UTC().Format(time.RFC3339Nano))
	}
	if query.Limit > 0 {
		params.Set("limit", strconv.Itoa(query.Limit))
	}
	target := c.baseURL + "/audit/http"
	if len(params) > 0 {
		target += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("proxy control API answered %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	var rows []AuditHTTPExchange
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return nil, fmt.Errorf("decode proxy audit rows: %w", err)
	}
	return rows, nil
}
