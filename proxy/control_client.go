package proxy

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/auditid"
	"github.com/discobox-ai/discobox/proxy/internal/audit"
)

// AuditHTTPExchange is one audited HTTP exchange, as the control API returns it.
type AuditHTTPExchange = audit.HTTPExchange

// AuditDNSQuery is one audited DNS query, as the control API returns it.
type AuditDNSQuery = audit.DNSQuery

// AuditDNSQueryOptions narrows a control API DNS audit read; ClientID is not
// set here, as with AuditQuery.
type AuditDNSQueryOptions = audit.DNSQueryOptions

// AuditQuery narrows a control API audit read. ClientID is not set here: a
// ControlClient takes the sandbox separately and puts it both in the token it
// signs and in the query (see sandboxParams).
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
		// No overall timeout: an artifact is a body or a stream of unbounded
		// length, and a whole-request timeout would cut one off part way. The
		// header wait is bounded here; everything after is the caller's context.
		httpClient = &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 30 * time.Second}}
	}
	return &ControlClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		key:     key,
		claims:  ControlTokenClaims{ProjectID: projectID, WorkerID: workerID, Scopes: []string{ScopeAuditRead}},
		http:    httpClient,
	}
}

// ListHTTP returns audited HTTP exchanges, newest first unless the query asks
// for ascending. A non-empty sandboxID scopes the read to that sandbox; an empty
// one is the pool-wide read.
func (c *ControlClient) ListHTTP(ctx context.Context, sandboxID string, query AuditQuery) ([]AuditHTTPExchange, error) {
	claims := c.claims
	claims.SandboxID = sandboxID
	token, err := CreateControlToken(c.key, claims)
	if err != nil {
		return nil, err
	}
	params := sandboxParams(sandboxID)
	if query.Host != "" {
		params.Set("host", query.Host)
	}
	if query.UseID != "" {
		params.Set("use_id", query.UseID)
	}
	if !query.Since.IsZero() {
		params.Set("since", query.Since.UTC().Format(time.RFC3339Nano))
	}
	if !query.Until.IsZero() {
		params.Set("until", query.Until.UTC().Format(time.RFC3339Nano))
	}
	if query.Limit > 0 {
		params.Set("limit", strconv.Itoa(query.Limit))
	}
	if query.Ascending {
		params.Set("order", "asc")
	}
	if query.AfterID > 0 {
		params.Set("after_id", query.AfterID.String())
	}
	if query.MinStatus > 0 {
		params.Set("min_status", strconv.Itoa(query.MinStatus))
	}
	if query.MaxStatus > 0 {
		params.Set("max_status", strconv.Itoa(query.MaxStatus))
	}
	if query.Blocked != nil {
		params.Set("blocked", strconv.FormatBool(*query.Blocked))
	}
	target := c.baseURL + "/audit/http"
	if len(params) > 0 {
		target += "?" + params.Encode()
	}
	resp, err := c.get(ctx, target, token)
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

// ListDNS returns audited DNS queries, newest first unless the query asks for
// ascending. sandboxID scopes the read as it does ListHTTP's.
func (c *ControlClient) ListDNS(ctx context.Context, sandboxID string, query AuditDNSQueryOptions) ([]AuditDNSQuery, error) {
	claims := c.claims
	claims.SandboxID = sandboxID
	token, err := CreateControlToken(c.key, claims)
	if err != nil {
		return nil, err
	}
	params := sandboxParams(sandboxID)
	if query.Name != "" {
		params.Set("name", query.Name)
	}
	if !query.Since.IsZero() {
		params.Set("since", query.Since.UTC().Format(time.RFC3339Nano))
	}
	if !query.Until.IsZero() {
		params.Set("until", query.Until.UTC().Format(time.RFC3339Nano))
	}
	if query.Limit > 0 {
		params.Set("limit", strconv.Itoa(query.Limit))
	}
	if query.Ascending {
		params.Set("order", "asc")
	}
	if query.AfterID > 0 {
		params.Set("after_id", query.AfterID.String())
	}
	if query.ID > 0 {
		params.Set("id", query.ID.String())
	}
	target := c.baseURL + "/audit/dns"
	if len(params) > 0 {
		target += "?" + params.Encode()
	}
	resp, err := c.get(ctx, target, token)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("proxy control API answered %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	var rows []AuditDNSQuery
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return nil, fmt.Errorf("decode proxy dns audit rows: %w", err)
	}
	return rows, nil
}

// Artifacts an HTTP audit row can have recorded beside it.
const (
	AuditArtifactRequestBody  = "request-body"
	AuditArtifactResponseBody = "response-body"
	AuditArtifactStream       = "stream"
)

// ErrAuditArtifactNotFound is an artifact the proxy has no record of: the row
// does not exist, belongs to another sandbox, recorded no such body, or its
// spool has been reclaimed by retention.
var ErrAuditArtifactNotFound = errors.New("audit artifact not found")

// AuditArtifact is a recorded body or upgraded stream, still being read.
type AuditArtifact struct {
	Body io.ReadCloser
	// Format is the spool format the proxy recorded it in: a body is raw bytes;
	// a stream is framed.
	Format      string
	ContentType string
}

// GetHTTP reads one audited exchange in full: every field the recorder wrote,
// rather than the summary a list returns. A non-empty sandboxID scopes the
// read, so a row belonging to another sandbox reads as not found.
func (c *ControlClient) GetHTTP(ctx context.Context, sandboxID string, id auditid.ExchangeID) (*AuditHTTPExchange, error) {
	claims := c.claims
	claims.SandboxID = sandboxID
	token, err := CreateControlToken(c.key, claims)
	if err != nil {
		return nil, err
	}
	target := fmt.Sprintf("%s/audit/http/%s", c.baseURL, id)
	if params := sandboxParams(sandboxID); len(params) > 0 {
		target += "?" + params.Encode()
	}
	resp, err := c.get(ctx, target, token)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrAuditArtifactNotFound
	}
	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("proxy control API answered %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	var row AuditHTTPExchange
	if err := json.NewDecoder(resp.Body).Decode(&row); err != nil {
		return nil, fmt.Errorf("decode audited exchange %d: %w", id, err)
	}
	return &row, nil
}

// OpenHTTPArtifact streams one artifact recorded beside audit row id. A
// non-empty sandboxID scopes the token, so a row belonging to another sandbox
// reads as not found.
func (c *ControlClient) OpenHTTPArtifact(ctx context.Context, sandboxID string, id auditid.ExchangeID, artifact string) (*AuditArtifact, error) {
	switch artifact {
	case AuditArtifactRequestBody, AuditArtifactResponseBody, AuditArtifactStream:
	default:
		return nil, fmt.Errorf("unknown audit artifact %q", artifact)
	}
	claims := c.claims
	claims.SandboxID = sandboxID
	token, err := CreateControlToken(c.key, claims)
	if err != nil {
		return nil, err
	}
	target := fmt.Sprintf("%s/audit/http/%s/%s", c.baseURL, id, artifact)
	if params := sandboxParams(sandboxID); len(params) > 0 {
		target += "?" + params.Encode()
	}
	resp, err := c.get(ctx, target, token)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, ErrAuditArtifactNotFound
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("proxy control API answered %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	format := resp.Header.Get("X-Discobox-Body-Format")
	if artifact == AuditArtifactStream {
		format = resp.Header.Get("X-Discobox-Stream-Format")
	}
	return &AuditArtifact{Body: resp.Body, Format: format, ContentType: resp.Header.Get("Content-Type")}, nil
}

// sandboxParams is the query half of a sandbox scope. The token half is what an
// authenticated proxy narrows by, and it rewrites client_id to match; this half
// is what narrows a proxy serving its control API without authentication, which
// would otherwise ignore the token and answer for every sandbox. Sending both
// means the scope holds whichever way the proxy is configured.
func sandboxParams(sandboxID string) url.Values {
	params := url.Values{}
	if sandboxID != "" {
		params.Set("client_id", sandboxID)
	}
	return params
}

func (c *ControlClient) get(ctx context.Context, target, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return c.http.Do(req)
}
