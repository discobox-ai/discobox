package proxyagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/obot-platform/discobox/proxy"
)

const (
	// SecretsFile holds the per-sandbox sentinel sets the proxy watches. The
	// worker-agent process writes it; the proxy unit polls and applies it. It
	// contains only non-secret sentinel placeholders.
	SecretsFile = Root + "/secrets.json"

	// ResolveContextFile holds the control-plane URL, worker ID, and the scoped
	// resolve token the proxy unit uses to fetch real secret values. The
	// worker-agent process writes and refreshes it.
	ResolveContextFile = Root + "/resolve-context.json"

	secretsPollInterval = 2 * time.Second
	resolveHTTPTimeout  = 10 * time.Second
)

// secretsDoc is the on-disk sentinel registry keyed by sandbox (proxy client) ID.
type secretsDoc struct {
	Clients map[string][]string `json:"clients"`
}

// resolveContext is the on-disk resolve credential the proxy unit reads.
type resolveContext struct {
	ControlPlaneURL string `json:"controlPlaneUrl"`
	WorkerID        string `json:"workerId"`
	Token           string `json:"token"`
}

// secretsFileMu serializes read-modify-write updates to SecretsFile within the
// worker-agent process. The proxy unit only reads the file.
var secretsFileMu sync.Mutex

// UpsertSandboxSentinels registers a sandbox's sentinel set with the proxy by
// updating SecretsFile. Passing an empty set removes the sandbox entry.
func UpsertSandboxSentinels(hostDirFor HostPathResolver, sandboxID string, sentinels []string) error {
	if hostDirFor == nil {
		hostDirFor = func(p string) string { return p }
	}
	path := hostDirFor(SecretsFile)
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
func RemoveSandboxSentinels(hostDirFor HostPathResolver, sandboxID string) error {
	return UpsertSandboxSentinels(hostDirFor, sandboxID, nil)
}

// WriteResolveContext writes the resolve credential the proxy unit reads.
func WriteResolveContext(hostDirFor HostPathResolver, controlPlaneURL, workerID, token string) error {
	if hostDirFor == nil {
		hostDirFor = func(p string) string { return p }
	}
	return writeJSONAtomic(hostDirFor(ResolveContextFile), resolveContext{
		ControlPlaneURL: controlPlaneURL,
		WorkerID:        workerID,
		Token:           token,
	})
}

func readSecretsDoc(path string) (secretsDoc, error) {
	data, err := os.ReadFile(path)
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

func newSecretResolver(hostDirFor HostPathResolver) *secretResolver {
	if hostDirFor == nil {
		hostDirFor = func(p string) string { return p }
	}
	return &secretResolver{
		contextPath: hostDirFor(ResolveContextFile),
		client:      &http.Client{Timeout: resolveHTTPTimeout},
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
	if err != nil || rc.Token == "" || rc.ControlPlaneURL == "" || rc.WorkerID == "" {
		// No usable credential yet: fail closed so the sentinel is left in place.
		return proxy.SecretResolveResult{}, proxy.ErrSecretResolveDenied
	}
	url := fmt.Sprintf("%s/api/workers/%s/resolve-sandbox-secret", rc.ControlPlaneURL, rc.WorkerID)
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
	if resp.StatusCode != http.StatusOK {
		// Not found / forbidden / server error: leave the sentinel in place.
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
	data, err := os.ReadFile(r.contextPath)
	if err != nil {
		return resolveContext{}, err
	}
	var rc resolveContext
	if err := json.Unmarshal(data, &rc); err != nil {
		return resolveContext{}, err
	}
	return rc, nil
}

// watchSecretsFile polls SecretsFile and applies sentinel sets to the running
// proxy. base carries the startup config whose runtime policy fields are
// replaced on each apply.
func watchSecretsFile(ctx context.Context, server *proxy.Server, base proxy.Config, path string, onError func(error)) {
	ticker := time.NewTicker(secretsPollInterval)
	defer ticker.Stop()
	var lastMod time.Time
	apply := func() {
		info, err := os.Stat(path)
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
		cfg.Secrets = proxy.SecretsConfig{Clients: secretClientsFromDoc(doc)}
		if err := server.ApplyConfig(cfg); err != nil {
			if onError != nil {
				onError(err)
			}
			return
		}
		lastMod = info.ModTime()
	}
	apply()
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
