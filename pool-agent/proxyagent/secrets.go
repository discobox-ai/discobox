package proxyagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/discobox-ai/discobox/layout"
	"github.com/discobox-ai/discobox/pool-agent/wire"
	"github.com/discobox-ai/discobox/proxy"
)

const (
	secretsPollInterval = 2 * time.Second
	// secretsBackstopInterval re-applies the file periodically in case an
	// fsnotify event is dropped; the watcher handles the immediate path.
	secretsBackstopInterval = 30 * time.Second
	resolveHTTPTimeout      = 10 * time.Second
)

// secretsDoc is the on-disk sentinel registry keyed by sandbox (proxy client) ID.
type secretsDoc struct {
	Clients map[string][]string `json:"clients"`
}

// resolveContext is the on-disk resolve credential the proxy unit reads.
type resolveContext struct {
	ControlPlaneURL string `json:"controlPlaneUrl"`
	PoolID          string `json:"poolId"`
	Token           string `json:"token"`
}

// secretsFileMu serializes read-modify-write updates to SecretsFile within the
// pool-agent process. The proxy unit only reads the file.
var secretsFileMu sync.Mutex

// UpsertSandboxSentinels registers a sandbox's sentinel set with the proxy by
// updating SecretsFile. Passing an empty set removes the sandbox entry.
func UpsertSandboxSentinels(projectID, poolID string, sandboxID string, sentinels []string) error {
	path := layout.ProxySecretsFile(projectID, poolID)
	secretsFileMu.Lock()
	defer secretsFileMu.Unlock()
	doc, err := readSecretsDoc(path)
	if err != nil {
		return err
	}
	if doc.Clients == nil {
		doc.Clients = map[string][]string{}
	}
	if len(sentinels) == 0 {
		delete(doc.Clients, sandboxID)
	} else {
		doc.Clients[sandboxID] = sentinels
	}
	return writeJSONAtomic(path, doc)
}

// RemoveSandboxSentinels drops a sandbox's sentinel set from SecretsFile.
func RemoveSandboxSentinels(projectID, poolID string, sandboxID string) error {
	return UpsertSandboxSentinels(projectID, poolID, sandboxID, nil)
}

// WriteResolveContext writes the resolve credential the proxy unit reads.
func WriteResolveContext(projectID, poolID string, controlPlaneURL, token string) error {
	return writeJSONAtomic(layout.ProxyResolveContextFile(projectID, poolID), resolveContext{
		ControlPlaneURL: controlPlaneURL,
		PoolID:          poolID,
		Token:           token,
	})
}

func readSecretsDoc(path string) (secretsDoc, error) {
	data, err := os.ReadFile(resolve(path))
	if err != nil {
		if os.IsNotExist(err) {
			return secretsDoc{Clients: map[string][]string{}}, nil
		}
		return secretsDoc{}, err
	}
	var doc secretsDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return secretsDoc{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return doc, nil
}

func writeJSONAtomic(path string, value any) error {
	// Resolve once: every operation below must act on the same location, and
	// resolving each argument separately would rename across roots.
	path = resolve(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// secretResolver implements proxy.SecretResolver by calling the control-plane
// resolve endpoint with the scoped token from ResolveContextFile. It reads the
// context file per call so token refreshes take effect without a restart.
type secretResolver struct {
	contextPath string
	client      *http.Client
}

func newSecretResolver(projectID, poolID string) *secretResolver {
	// Same URL, same transport resolution as the agent itself; the proxy unit
	// inherits it from the unit environment file.
	client := &http.Client{Timeout: resolveHTTPTimeout}
	if url := strings.TrimSpace(os.Getenv(envControlPlaneURL)); url != "" {
		if _, resolved, err := wire.HTTPClient(url, resolveHTTPTimeout); err == nil {
			client = resolved
		}
	}
	return &secretResolver{
		contextPath: layout.ProxyResolveContextFile(projectID, poolID),
		client:      client,
	}
}

type resolveRequestBody struct {
	SandboxID string `json:"sandboxId"`
	Sentinel  string `json:"sentinel"`
	Host      string `json:"host"`
}

type resolveResponseBody struct {
	Status    string     `json:"status"`
	Value     string     `json:"value"`
	ExpiresAt *time.Time `json:"expiresAt"`
}

func (r *secretResolver) Resolve(ctx context.Context, req proxy.SecretResolveRequest) (proxy.SecretResolveResult, error) {
	rc, err := r.readContext()
	if err != nil || rc.Token == "" || rc.ControlPlaneURL == "" || rc.PoolID == "" {
		// No usable credential yet: fail closed so the sentinel is left in place.
		return proxy.SecretResolveResult{}, proxy.ErrSecretResolveDenied
	}
	url := fmt.Sprintf("%s/api/pools/%s/resolve-sandbox-secret", rc.ControlPlaneURL, rc.PoolID)
	payload, err := json.Marshal(resolveRequestBody{SandboxID: req.ClientID, Sentinel: req.Sentinel, Host: req.Host})
	if err != nil {
		return proxy.SecretResolveResult{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return proxy.SecretResolveResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+rc.Token)
	resp, err := r.client.Do(httpReq)
	if err != nil {
		return proxy.SecretResolveResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusInternalServerError {
		// Control plane is up but erroring: treat as transient (not an
		// authoritative denial) so a cached value keeps serving until its grant
		// expires rather than being invalidated by a blip.
		return proxy.SecretResolveResult{}, fmt.Errorf("resolve secret: control plane returned %d", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		// Not found / forbidden: leave the sentinel in place.
		return proxy.SecretResolveResult{}, proxy.ErrSecretResolveDenied
	}
	var out resolveResponseBody
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return proxy.SecretResolveResult{}, err
	}
	if out.Status != "approved" || out.Value == "" {
		return proxy.SecretResolveResult{}, proxy.ErrSecretResolveDenied
	}
	result := proxy.SecretResolveResult{Value: out.Value}
	if out.ExpiresAt != nil {
		result.ExpiresAt = *out.ExpiresAt
	}
	return result, nil
}

func (r *secretResolver) readContext() (resolveContext, error) {
	data, err := os.ReadFile(resolve(r.contextPath))
	if err != nil {
		return resolveContext{}, err
	}
	var rc resolveContext
	if err := json.Unmarshal(data, &rc); err != nil {
		return resolveContext{}, err
	}
	return rc, nil
}

// watchSecretsFile watches SecretsFile and applies sentinel sets to the running
// proxy. It uses fsnotify so a sentinel push takes effect immediately (rather
// than after a poll interval), with a slow ticker backstop in case an event is
// missed. base carries the startup config whose runtime policy fields are
// replaced on each apply.
func watchSecretsFile(ctx context.Context, server *proxy.Server, base proxy.Config, path string, onError func(error)) {
	var lastMod time.Time
	apply := func() {
		info, err := os.Stat(resolve(path))
		if err != nil {
			if !os.IsNotExist(err) && onError != nil {
				onError(err)
			}
			return
		}
		if !info.ModTime().After(lastMod) {
			return
		}
		doc, err := readSecretsDoc(path)
		if err != nil {
			if onError != nil {
				onError(err)
			}
			return
		}
		cfg := base
		// Keep the swap tuning (TTLs, refresh interval, query scanning) from the
		// startup config; only the sentinel client set changes per apply.
		cfg.Secrets = base.Secrets
		cfg.Secrets.Clients = secretClientsFromDoc(doc)
		if err := server.ApplyConfig(cfg); err != nil {
			if onError != nil {
				onError(err)
			}
			return
		}
		lastMod = info.ModTime()
	}
	apply()

	// SecretsFile is written atomically (write temp + rename), so watch the
	// containing directory for the rename rather than the file itself.
	watcher, err := fsnotify.NewWatcher()
	if err == nil {
		if addErr := watcher.Add(filepath.Dir(path)); addErr != nil {
			_ = watcher.Close()
			watcher = nil
			if onError != nil {
				onError(fmt.Errorf("watch secrets dir: %w", addErr))
			}
		}
	} else if onError != nil {
		onError(fmt.Errorf("create secrets watcher: %w", err))
	}
	if watcher == nil {
		pollSecretsFile(ctx, apply) // fall back to polling when fsnotify is unavailable
		return
	}
	defer watcher.Close()

	// Backstop the event stream in case an event is dropped.
	ticker := time.NewTicker(secretsBackstopInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-watcher.Events:
			if filepath.Clean(event.Name) == filepath.Clean(path) {
				apply()
			}
		case watchErr := <-watcher.Errors:
			if watchErr != nil && onError != nil {
				onError(watchErr)
			}
		case <-ticker.C:
			apply()
		}
	}
}

// pollSecretsFile applies the secrets file on a fixed interval. It is the
// fallback when an fsnotify watcher cannot be established.
func pollSecretsFile(ctx context.Context, apply func()) {
	ticker := time.NewTicker(secretsPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			apply()
		}
	}
}

func secretClientsFromDoc(doc secretsDoc) []proxy.SecretClient {
	clients := make([]proxy.SecretClient, 0, len(doc.Clients))
	for clientID, sentinels := range doc.Clients {
		if len(sentinels) == 0 {
			continue
		}
		clients = append(clients, proxy.SecretClient{ClientID: clientID, Sentinels: sentinels})
	}
	return clients
}
