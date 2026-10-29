package agentcreds

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client speaks the protocol to a base URL. It is the only thing the in-sandbox
// CLI knows: a URL, an optional bearer token, and the protocol's calls.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// ClientOption configures a Client.
type ClientOption func(*Client)

// WithToken sends an Authorization bearer header on every request. Discobox
// needs none (the transport is sandbox loopback), so it is optional.
func WithToken(token string) ClientOption {
	return func(c *Client) { c.token = strings.TrimSpace(token) }
}

// WithHTTPClient overrides the HTTP client, which is how a caller supplies its
// own transport — an mTLS one, for the sandbox-to-pool relay.
func WithHTTPClient(client *http.Client) ClientOption {
	return func(c *Client) {
		if client != nil {
			c.http = client
		}
	}
}

// NewClient creates a protocol client for baseURL.
func NewClient(baseURL string, opts ...ClientOption) *Client {
	c := &Client{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		// No client-wide timeout: each call is bounded by its own (do).
		http: &http.Client{},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c
}

// List returns the credentials the caller may use.
func (c *Client) List(ctx context.Context) ([]Credential, error) {
	var out ListResponse
	if err := c.do(ctx, callTimeout, http.MethodGet, PathCredentials, nil, &out); err != nil {
		return nil, err
	}
	return out.Credentials, nil
}

// Request asks for a credential and returns immediately.
func (c *Client) Request(ctx context.Context, body RequestBody) (RequestStatus, error) {
	var out RequestStatus
	err := c.do(ctx, callTimeout, http.MethodPost, PathRequests, body, &out)
	return out, err
}

// RequestStatus reads a request's current status.
func (c *Client) RequestStatus(ctx context.Context, requestID string) (RequestStatus, error) {
	var out RequestStatus
	err := c.do(ctx, callTimeout, http.MethodGet, PathRequests+"/"+requestID, nil, &out)
	return out, err
}

// Environment reads the variables credentials are delivered in now.
func (c *Client) Environment(ctx context.Context) (map[string]string, error) {
	var out EnvironmentResponse
	if err := c.do(ctx, callTimeout, http.MethodGet, PathEnvironment, nil, &out); err != nil {
		return nil, err
	}
	return out.Variables, nil
}

// Trusts lists the host trusts the caller holds.
func (c *Client) Trusts(ctx context.Context) ([]Trust, error) {
	var out TrustListResponse
	if err := c.do(ctx, callTimeout, http.MethodGet, PathTrusts, nil, &out); err != nil {
		return nil, err
	}
	return out.Trusts, nil
}

// RequestTrust asks for a host to be trusted and returns immediately.
func (c *Client) RequestTrust(ctx context.Context, body TrustRequestBody) (TrustRequestStatus, error) {
	var out TrustRequestStatus
	err := c.do(ctx, callTimeout, http.MethodPost, PathTrustRequests, body, &out)
	return out, err
}

// TrustRequestStatus reads a trust request's current status.
func (c *Client) TrustRequestStatus(ctx context.Context, requestID string) (TrustRequestStatus, error) {
	var out TrustRequestStatus
	err := c.do(ctx, callTimeout, http.MethodGet, PathTrustRequests+"/"+requestID, nil, &out)
	return out, err
}

// AuditHTTP reads the caller's own audited HTTP exchanges, newest first.
func (c *Client) AuditHTTP(ctx context.Context, query AuditQuery) ([]AuditExchange, error) {
	path := PathAuditHTTP
	if values := query.values(); len(values) > 0 {
		path += "?" + values.Encode()
	}
	var out AuditHTTPResponse
	if err := c.do(ctx, callTimeout, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out.Exchanges, nil
}

// WaitForTrustRequest is WaitForRequest for a trust request.
func (c *Client) WaitForTrustRequest(ctx context.Context, known TrustRequestStatus, interval time.Duration) (TrustRequestStatus, error) {
	requestID := known.RequestID
	return waitFor(ctx, known, known.Status != "", interval, func() (TrustRequestStatus, bool, error) {
		status, err := c.TrustRequestStatus(ctx, requestID)
		return status, status.Settled(), err
	})
}

// WaitForRequest polls a request until it settles or ctx is done. Blocking is
// the client's job, not the protocol's: approval can take minutes or never
// come, so a long-held connection through the relay chain would be the fragile
// part rather than the poll. known is what the caller already holds for the
// request: the answer that created it, or only its ID to resume one. See
// waitFor for what it answers when ctx ends first, and which poll failures it
// waits through.
func (c *Client) WaitForRequest(ctx context.Context, known RequestStatus, interval time.Duration) (RequestStatus, error) {
	requestID := known.RequestID
	return waitFor(ctx, known, known.Status != "", interval, func() (RequestStatus, bool, error) {
		status, err := c.RequestStatus(ctx, requestID)
		return status, status.Settled(), err
	})
}

// waitFor polls until poll reports a settled answer or a failure that another
// poll would only repeat, or ctx is done.
//
// A poll that fails for any other reason — it timed out, the relay dropped
// it, the service answered 5xx — says nothing about the request, which is
// still waiting on a person: it is polled again. A request outlives any one
// wait, so a wait that ends before the answer comes is not a failure either:
// it answers with the last status it knew, still pending, for the caller to
// report as such and resume later — the one it started with (known, when
// read) if no poll answered. Only a wait that never knew a status answers
// with an error.
func waitFor[T any](ctx context.Context, known T, read bool, interval time.Duration, poll func() (T, bool, error)) (T, error) {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	last := known
	var lastErr error
	for {
		status, settled, err := poll()
		switch {
		case err == nil && settled:
			return status, nil
		case err == nil:
			last, read = status, true
		case permanent(err):
			return status, err
		default:
			lastErr = err
		}
		select {
		case <-ctx.Done():
			if read {
				return last, nil
			}
			if lastErr != nil {
				return last, lastErr
			}
			return last, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// permanent reports whether a poll failure would only be repeated by polling
// again: the service does not know the request, will not show it to this
// caller, or refuses the call itself.
func permanent(err error) bool {
	return errors.Is(err, ErrNotFound) || errors.Is(err, ErrDenied) || errors.Is(err, ErrInvalid)
}

// callTimeout bounds every call but a use, which waits for UseTimeout.
const callTimeout = 30 * time.Second

func (c *Client) do(ctx context.Context, timeout time.Duration, method, path string, body, out any) error {
	if c.baseURL == "" {
		return fmt.Errorf("%w: no credentials service URL configured", ErrInvalid)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return statusError(resp)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, MaxResponseBytes)).Decode(out)
}

// statusError maps a response back onto the package's sentinel errors, so a
// relay in the middle of the chain can propagate a denial as a denial rather
// than flattening every failure into a server error.
//
// The body's code wins over the status when both are present: an
// implementation that classified its own failure knows more than the status
// line does, and the code is the part a caller branches on.
func statusError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, MaxBodyBytes))
	message := strings.TrimSpace(string(data))
	var parsed ErrorResponse
	if json.Unmarshal(data, &parsed) == nil && strings.TrimSpace(parsed.Error) != "" {
		message = strings.TrimSpace(parsed.Error)
	}
	if message == "" {
		message = http.StatusText(resp.StatusCode)
	}
	code := strings.TrimSpace(parsed.Code)
	if code == "" {
		code = statusCode(resp.StatusCode)
	}
	switch code {
	case CodeNotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, message)
	case CodeDenied:
		return fmt.Errorf("%w: %s", ErrDenied, message)
	case CodeInvalid:
		return fmt.Errorf("%w: %s", ErrInvalid, message)
	default:
		return fmt.Errorf("credentials service returned %d: %s", resp.StatusCode, message)
	}
}

// statusCode classifies a response that carried no code of its own.
func statusCode(status int) string {
	switch status {
	case http.StatusNotFound:
		return CodeNotFound
	case http.StatusForbidden, http.StatusUnauthorized:
		return CodeDenied
	case http.StatusBadRequest, http.StatusConflict:
		return CodeInvalid
	default:
		return CodeUnavailable
	}
}
