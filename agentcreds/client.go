package agentcreds

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client speaks the protocol to a base URL. It is the only thing the in-sandbox
// CLI knows: a URL, an optional bearer token, and these five calls.
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
		http:    &http.Client{Timeout: 30 * time.Second},
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
	if err := c.do(ctx, http.MethodGet, PathCredentials, nil, &out); err != nil {
		return nil, err
	}
	return out.Credentials, nil
}

// Request asks for a credential and returns immediately.
func (c *Client) Request(ctx context.Context, body RequestBody) (RequestStatus, error) {
	var out RequestStatus
	err := c.do(ctx, http.MethodPost, PathRequests, body, &out)
	return out, err
}

// RequestStatus reads a request's current status.
func (c *Client) RequestStatus(ctx context.Context, requestID string) (RequestStatus, error) {
	var out RequestStatus
	err := c.do(ctx, http.MethodGet, PathRequests+"/"+requestID, nil, &out)
	return out, err
}

// Get takes a value for one declared command.
func (c *Client) Get(ctx context.Context, body UseBody) (UseResponse, error) {
	var out UseResponse
	err := c.do(ctx, http.MethodPost, PathUse, body, &out)
	return out, err
}

// ReportDenial volunteers a verdict for a command the judge refused, which
// never reached Get. Best-effort by contract (ADR 0091 §3): a caller may, and
// the reference CLI does, ignore this call's own failure rather than let it
// change what the caller reports for the refusal that prompted it.
func (c *Client) ReportDenial(ctx context.Context, body DenialReport) error {
	return c.do(ctx, http.MethodPost, PathDenials, body, nil)
}

// WaitForRequest polls a request until it settles or ctx is done. Blocking is
// the client's job, not the protocol's: approval can take minutes or never
// come, so a long-held connection through the relay chain would be the fragile
// part rather than the poll.
func (c *Client) WaitForRequest(ctx context.Context, requestID string, interval time.Duration) (RequestStatus, error) {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	for {
		status, err := c.RequestStatus(ctx, requestID)
		if err != nil {
			return status, err
		}
		if status.Settled() {
			return status, nil
		}
		select {
		case <-ctx.Done():
			return status, ctx.Err()
		case <-time.After(interval):
		}
	}
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	if c.baseURL == "" {
		return fmt.Errorf("%w: no credentials service URL configured", ErrInvalid)
	}
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
	return json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(out)
}

// statusError maps a response back onto the package's sentinel errors, so a
// relay in the middle of the chain can propagate a denial as a denial rather
// than flattening every failure into a server error.
//
// The body's code wins over the status when both are present: an
// implementation that classified its own failure knows more than the status
// line does, and the code is the part a caller branches on.
func statusError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
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
