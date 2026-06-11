package workeragent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	defaultRegisterPath = "/api/workers/register"
	defaultStatusPath   = "/api/workers/status"
)

// HTTPClient registers workers through the control plane HTTP API.
type HTTPClient struct {
	baseURL      string
	client       *http.Client
	registerPath string
	statusPath   string
}

type HTTPClientOption func(*HTTPClient)

// NewHTTPClient creates a worker registration HTTP client.
func NewHTTPClient(baseURL string, opts ...HTTPClientOption) *HTTPClient {
	c := &HTTPClient{
		baseURL:      strings.TrimRight(baseURL, "/"),
		client:       http.DefaultClient,
		registerPath: defaultRegisterPath,
		statusPath:   defaultStatusPath,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c
}

// WithHTTPClient overrides the HTTP client used for control plane requests.
func WithHTTPClient(client *http.Client) HTTPClientOption {
	return func(c *HTTPClient) {
		if client != nil {
			c.client = client
		}
	}
}

// WithRegisterPath overrides the worker registration path.
func WithRegisterPath(path string) HTTPClientOption {
	return func(c *HTTPClient) {
		if strings.TrimSpace(path) != "" {
			c.registerPath = path
		}
	}
}

// WithStatusPath overrides the worker status update path.
func WithStatusPath(path string) HTTPClientOption {
	return func(c *HTTPClient) {
		if strings.TrimSpace(path) != "" {
			c.statusPath = path
		}
	}
}

func (c *HTTPClient) RegisterWorker(ctx context.Context, req RegisterRequest) (*RegisterResponse, error) {
	baseURL := strings.TrimRight(firstNonEmpty(req.ControlPlaneURL, c.baseURL), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("control plane URL is required")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+c.registerPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Disco2-Tenant-ID", req.TenantID)
	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("worker registration failed: status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out RegisterResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *HTTPClient) UpdateWorkerStatus(ctx context.Context, req StatusRequest) error {
	baseURL := strings.TrimRight(firstNonEmpty(req.ControlPlaneURL, c.baseURL), "/")
	if baseURL == "" {
		return fmt.Errorf("control plane URL is required")
	}
	if strings.TrimSpace(req.AuthToken) == "" {
		return fmt.Errorf("worker auth token is required")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+c.statusPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+strings.TrimSpace(req.AuthToken))
	httpReq.Header.Set("X-Disco2-Tenant-ID", req.TenantID)
	resp, err := c.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("worker status update failed: status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
