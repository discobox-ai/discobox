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

	"github.com/discobox-ai/discobox/pkg/poolauth"
	"github.com/discobox-ai/discobox/pool-agent/internalhttp"
)

const (
	defaultStatusPath                   = "/api/pools/{poolId}/status"
	defaultStatesPath                   = "/api/pools/{poolId}/sandbox-states"
	defaultSandboxAgentStatusTokensPath = "/api/pools/{poolId}/sandbox-agent-status-tokens" //nolint:gosec // Route path, not a credential value.
	defaultSandboxAgentStatusPath       = "/api/pools/{poolId}/sandbox-agent-status"
	defaultSandboxInventoriesPath       = "/api/pools/{poolId}/sandbox-inventories"
	defaultPoolResourcesPath            = "/api/pools/{poolId}/resources"
	defaultHeldSandboxesPath            = "/api/pools/{poolId}/sandboxes"
)

// HTTPClient is a pool agent's client of the control plane HTTP API.
type HTTPClient struct {
	baseURL                      string
	client                       *http.Client
	statusPath                   string
	statesPath                   string
	sandboxAgentStatusTokensPath string
	sandboxAgentStatusPath       string
	poolResourcesPath            string
	heldSandboxesPath            string
}

type HTTPClientOption func(*HTTPClient)

// NewHTTPClient creates a pool agent's control plane HTTP client.
func NewHTTPClient(baseURL string, opts ...HTTPClientOption) *HTTPClient {
	c := &HTTPClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		// Never http.DefaultClient: it proxies when the pool runs inside a
		// sandbox, and the control plane is reached directly, never through
		// that sandbox's egress proxy.
		client:                       internalhttp.Client,
		statusPath:                   defaultStatusPath,
		statesPath:                   defaultStatesPath,
		sandboxAgentStatusTokensPath: defaultSandboxAgentStatusTokensPath,
		sandboxAgentStatusPath:       defaultSandboxAgentStatusPath,
		poolResourcesPath:            defaultPoolResourcesPath,
		heldSandboxesPath:            defaultHeldSandboxesPath,
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

// MintSandboxAgentStatusTokens fetches short-lived sandbox-agent tokens for
// the requested sandboxes, good for the status and inventory poll only.
func (c *HTTPClient) MintSandboxAgentStatusTokens(ctx context.Context, req MintSandboxAgentStatusTokensRequest) (*MintSandboxAgentStatusTokensResponse, error) {
	baseURL := strings.TrimRight(firstNonEmpty(req.ControlPlaneURL, c.baseURL), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("control plane URL is required")
	}
	poolID := strings.TrimSpace(req.PoolID)
	if poolID == "" {
		return nil, fmt.Errorf("pool ID is required")
	}
	projectID := strings.TrimSpace(req.ProjectID)
	if projectID == "" {
		return nil, fmt.Errorf("project ID is required")
	}
	token, err := poolauth.CreateToken(req.PrivateKey, poolauth.Claims{ProjectID: projectID, PoolID: poolID})
	if err != nil {
		return nil, fmt.Errorf("create pool assertion: %w", err)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	path := strings.ReplaceAll(c.sandboxAgentStatusTokensPath, "{poolId}", url.PathEscape(poolID))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("mint sandbox-agent status tokens failed: status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out MintSandboxAgentStatusTokensResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ReportSandboxAgentStatus pushes this tick's polled sandbox-agent status
// batch to the control plane.
func (c *HTTPClient) ReportSandboxAgentStatus(ctx context.Context, req SandboxAgentStatusReportRequest) error {
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
	if len(req.Sandboxes) == 0 {
		return nil
	}
	token, err := poolauth.CreateToken(req.PrivateKey, poolauth.Claims{ProjectID: projectID, PoolID: poolID})
	if err != nil {
		return fmt.Errorf("create pool assertion: %w", err)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	path := strings.ReplaceAll(c.sandboxAgentStatusPath, "{poolId}", url.PathEscape(poolID))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, bytes.NewReader(body))
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
		return fmt.Errorf("report sandbox-agent status failed: status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return nil
}

// ReportSandboxInventories pushes the inventories this tick read to the
// control plane, and answers which of those sent by digest alone it wants in
// full (ADR 26-10-11-657 §2).
func (c *HTTPClient) ReportSandboxInventories(ctx context.Context, req SandboxInventoryReportRequest) (*SandboxInventoryReportResponse, error) {
	baseURL := strings.TrimRight(firstNonEmpty(req.ControlPlaneURL, c.baseURL), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("control plane URL is required")
	}
	poolID := strings.TrimSpace(req.PoolID)
	if poolID == "" {
		return nil, fmt.Errorf("pool ID is required")
	}
	projectID := strings.TrimSpace(req.ProjectID)
	if projectID == "" {
		return nil, fmt.Errorf("project ID is required")
	}
	token, err := poolauth.CreateToken(req.PrivateKey, poolauth.Claims{ProjectID: projectID, PoolID: poolID})
	if err != nil {
		return nil, fmt.Errorf("create pool assertion: %w", err)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	path := strings.ReplaceAll(defaultSandboxInventoriesPath, "{poolId}", url.PathEscape(poolID))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("report sandbox inventories failed: status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out SandboxInventoryReportResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ReportPoolResources pushes this report's pool-wide and per-sandbox resource
// accounting to the control plane.
func (c *HTTPClient) ReportPoolResources(ctx context.Context, req PoolResourceReportRequest) error {
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
	path := strings.ReplaceAll(c.poolResourcesPath, "{poolId}", url.PathEscape(poolID))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, bytes.NewReader(body))
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
		return fmt.Errorf("report pool resources failed: status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return nil
}

// ListHeldSandboxes reads every sandbox the control plane holds on this pool.
// A body without the set is an error, never an empty set: the volume reaper
// collects whatever is outside it.
func (c *HTTPClient) ListHeldSandboxes(ctx context.Context, req HeldSandboxesRequest) ([]string, error) {
	baseURL := strings.TrimRight(firstNonEmpty(req.ControlPlaneURL, c.baseURL), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("control plane URL is required")
	}
	poolID := strings.TrimSpace(req.PoolID)
	if poolID == "" {
		return nil, fmt.Errorf("pool ID is required")
	}
	projectID := strings.TrimSpace(req.ProjectID)
	if projectID == "" {
		return nil, fmt.Errorf("project ID is required")
	}
	token, err := poolauth.CreateToken(req.PrivateKey, poolauth.Claims{ProjectID: projectID, PoolID: poolID})
	if err != nil {
		return nil, fmt.Errorf("create pool assertion: %w", err)
	}
	path := strings.ReplaceAll(c.heldSandboxesPath, "{poolId}", url.PathEscape(poolID))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("list held sandboxes failed: status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out struct {
		SandboxIDs *[]string `json:"sandboxIds"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.SandboxIDs == nil {
		return nil, fmt.Errorf("list held sandboxes: the response carries no sandboxIds")
	}
	return *out.SandboxIDs, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
