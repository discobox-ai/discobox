package poolagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/obot-platform/discobox/pool-agent/poolauth"
)

const (
	defaultRegisterPath = "/api/pools/register"
	defaultStatusPath   = "/api/pools/{poolId}/status"
	defaultStatesPath   = "/api/pools/{poolId}/sandbox-states"
)

// HTTPClient registers pools through the control plane HTTP API.
type HTTPClient struct {
	baseURL      string
	client       *http.Client
	registerPath string
	statusPath   string
	statesPath   string
}

type HTTPClientOption func(*HTTPClient)

// NewHTTPClient creates a pool registration HTTP client.
func NewHTTPClient(baseURL string, opts ...HTTPClientOption) *HTTPClient {
	c := &HTTPClient{
		baseURL:      strings.TrimRight(baseURL, "/"),
		client:       http.DefaultClient,
		registerPath: defaultRegisterPath,
		statusPath:   defaultStatusPath,
		statesPath:   defaultStatesPath,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c
}

// ReportSandboxStates publishes observed sandbox states to the control plane
// using the pool's signed runtime identity.
func (c *HTTPClient) ReportSandboxStates(ctx context.Context, req SandboxStateRequest) error {
	baseURL := firstNonEmpty(strings.TrimRight(req.ControlPlaneURL, "/"), c.baseURL)
	if baseURL == "" {
		return fmt.Errorf("control plane URL is required")
	}
	poolID := strings.TrimSpace(req.PoolID)
	if poolID == "" {
		return fmt.Errorf("pool ID is required")
	}
	projectID := strings.TrimSpace(req.ProjectID)
	if projectID == "" {
		return fmt.Errorf("project ID is required")
	}
	token, err := poolauth.CreateToken(req.PrivateKey, poolauth.Claims{ProjectID: projectID, PoolID: poolID})
	if err != nil {
		return fmt.Errorf("create pool assertion: %w", err)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	statesPath := strings.ReplaceAll(c.statesPath, "{poolId}", url.PathEscape(poolID))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+statesPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("sandbox state report failed: status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return nil
}

// WithHTTPClient overrides the HTTP client used for control plane requests.
func WithHTTPClient(client *http.Client) HTTPClientOption {
	return func(c *HTTPClient) {
		if client != nil {
			c.client = client
		}
	}
}

// WithRegisterPath overrides the pool registration path.
func WithRegisterPath(path string) HTTPClientOption {
	return func(c *HTTPClient) {
		if strings.TrimSpace(path) != "" {
			c.registerPath = path
		}
	}
}

// WithStatusPath overrides the pool status update path.
func WithStatusPath(path string) HTTPClientOption {
	return func(c *HTTPClient) {
		if strings.TrimSpace(path) != "" {
			c.statusPath = path
		}
	}
}

func (c *HTTPClient) RegisterPool(ctx context.Context, req RegisterRequest) (*RegisterResponse, error) {
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
	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("pool registration failed: status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out RegisterResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *HTTPClient) UpdatePoolStatus(ctx context.Context, req StatusRequest) error {
	baseURL := strings.TrimRight(firstNonEmpty(req.ControlPlaneURL, c.baseURL), "/")
	if baseURL == "" {
		return fmt.Errorf("control plane URL is required")
	}
	poolID := strings.TrimSpace(req.PoolID)
	if poolID == "" {
		return fmt.Errorf("pool ID is required")
	}
	projectID := strings.TrimSpace(req.ProjectID)
	if projectID == "" {
		return fmt.Errorf("project ID is required")
	}
	token, err := poolauth.CreateToken(req.PrivateKey, poolauth.Claims{ProjectID: projectID, PoolID: poolID})
	if err != nil {
		return fmt.Errorf("create pool assertion: %w", err)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	statusPath := strings.ReplaceAll(c.statusPath, "{poolId}", url.PathEscape(poolID))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+statusPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("pool status update failed: status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
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
