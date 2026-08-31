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

	"github.com/discobox-ai/discobox/hostscope"
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
	// activations translates an ephemeral sentinel back to the stable one the
	// control plane knows. Nil disables the agent credentials path entirely,
	// which is what a resolver built without a broker gets.
	activations *activations
}

func newSecretResolver(projectID, poolID string, live *activations) *secretResolver {
	return &secretResolver{
		contextPath: layout.ProxyResolveContextFile(projectID, poolID),
		client:      controlPlaneHTTPClient(),
		activations: live,
	}
}

// controlPlaneHTTPClient builds the client the proxy unit uses to reach the
// control plane: same URL and same transport resolution as the agent itself,
// which the unit inherits from the unit environment file.
func controlPlaneHTTPClient() *http.Client {
	client := &http.Client{Timeout: resolveHTTPTimeout}
	if url := strings.TrimSpace(os.Getenv(envControlPlaneURL)); url != "" {
		if _, resolved, err := wire.HTTPClient(url, resolveHTTPTimeout); err == nil {
			client = resolved
		}
	}
	return client
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
	rc, err := readResolveContext(r.contextPath)
	if err != nil || rc.Token == "" || rc.ControlPlaneURL == "" || rc.PoolID == "" {
		// No usable credential yet: fail closed so the sentinel is left in place.
		return proxy.SecretResolveResult{}, proxy.ErrSecretResolveDenied
	}
	// An ephemeral sentinel is checked and translated here, before the control
	// plane is asked anything. The control plane never learns that ephemeral
	// sentinels exist: it is handed the stable one and answers the same question
	// it always did (ADR 0031 §3).
	sentinel := req.Sentinel
	var activationExpiry time.Time
	if record, ok := r.activation(req); ok {
		sentinel = record.Stable
		activationExpiry = record.ExpiresAt
	} else if r.isEphemeralCandidate(req) {
		// The proxy matched a string this process handed out, but the activation
		// behind it is gone or was never for this destination. Fail closed:
		// translating it anyway would make the use window and the host scope
		// advisory.
		return proxy.SecretResolveResult{}, proxy.ErrSecretResolveDenied
	}
	url := fmt.Sprintf("%s/api/pools/%s/resolve-sandbox-secret", rc.ControlPlaneURL, rc.PoolID)
	payload, err := json.Marshal(resolveRequestBody{SandboxID: req.ClientID, Sentinel: sentinel, Host: req.Host})
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
	// Cap the proxy's positive cache at the activation window as well as the
	// grant. The grant is the longer of the two by construction, and a value
	// cached to grant expiry would keep serving an ephemeral sentinel long after
	// the one command it was minted for finished.
	if !activationExpiry.IsZero() && (result.ExpiresAt.IsZero() || activationExpiry.Before(result.ExpiresAt)) {
		result.ExpiresAt = activationExpiry
	}
	return result, nil
}

// activation returns the live activation for a resolve request, if the sentinel
// is one this process minted and the destination matches the host the use was
// approved for.
//
// The host check is repeated here rather than left to the control plane's grant
// match because it is the cheaper and earlier of the two, and because it is the
// check that ties this specific activation to this specific destination: the
// grant only knows the credential may go to that host at all.
func (r *secretResolver) activation(req proxy.SecretResolveRequest) (activation, bool) {
	if r.activations == nil {
		return activation{}, false
	}
	record, ok := r.activations.lookup(req.Sentinel)
	if !ok {
		return activation{}, false
	}
	if record.SandboxID != req.ClientID {
		return activation{}, false
	}
	// The same reading the control plane uses: a use approved for github.com
	// covers api.github.com, and one approved for api.github.com covers
	// nothing above it.
	if !hostscope.Covers(record.Host, req.Host) {
		return activation{}, false
	}
	return record, true
}

// isEphemeralCandidate reports whether a sentinel looks like one this process
// minted for some sandbox, even though no live activation covers this request.
// It separates "expired or wrong host" from "an ordinary injected sentinel",
// so the former is refused rather than forwarded to the control plane as if it
// were a stable binding.
func (r *secretResolver) isEphemeralCandidate(req proxy.SecretResolveRequest) bool {
	if r.activations == nil {
		return false
	}
	_, minted := r.activations.lookupAny(req.Sentinel)
	return minted
}

func readResolveContext(path string) (resolveContext, error) {
	data, err := os.ReadFile(resolve(path))
	if err != nil {
		return resolveContext{}, err
	}
	var rc resolveContext
	if err := json.Unmarshal(data, &rc); err != nil {
		return resolveContext{}, err
	}
	return rc, nil
}

// sentinelPublisher owns what the running proxy watches for. Two sources feed
// it and neither can be applied alone, because ApplyConfig replaces the whole
// per-client set:
//
//   - SecretsFile, the sandbox's stable sentinels, written by the pool-agent
//     process as sandboxes come and go.
//   - live activations, the ephemeral sentinels this process mints per use.
//
// Holding both here is what lets an activation take effect the instant it is
// minted: publishing is a function call rather than a file the proxy has to
// notice.
type sentinelPublisher struct {
	server  *proxy.Server
	base    proxy.Config
	live    *activations
	onError func(error)

	mu   sync.Mutex
	file map[string][]string
}

func newSentinelPublisher(server *proxy.Server, base proxy.Config, live *activations, onError func(error)) *sentinelPublisher {
	p := &sentinelPublisher{server: server, base: base, live: live, onError: onError, file: map[string][]string{}}
	live.setChangeHandler(p.publish)
	return p
}

// setFileSentinels records the stable sentinel set and republishes.
func (p *sentinelPublisher) setFileSentinels(clients map[string][]string) {
	p.mu.Lock()
	p.file = clients
	p.mu.Unlock()
	p.publish()
}

// publish applies the union of both sources to the running proxy.
func (p *sentinelPublisher) publish() {
	p.mu.Lock()
	merged := make(map[string][]string, len(p.file))
	for clientID, sentinels := range p.file {
		merged[clientID] = append([]string(nil), sentinels...)
	}
	p.mu.Unlock()
	for clientID, sentinels := range p.live.sentinelsByClient() {
		merged[clientID] = append(merged[clientID], sentinels...)
	}

	cfg := p.base
	// Keep the swap tuning (TTLs, refresh interval, query scanning) from the
	// startup config; only the sentinel client set changes per apply.
	cfg.Secrets = p.base.Secrets
	cfg.Secrets.Clients = secretClients(merged)
	if err := p.server.ApplyConfig(cfg); err != nil && p.onError != nil {
		p.onError(err)
	}
}

// watchSecretsFile watches SecretsFile and feeds its sentinel sets to the
// publisher. It uses fsnotify so a sentinel push takes effect immediately
// (rather than after a poll interval), with a slow ticker backstop in case an
// event is missed.
func watchSecretsFile(ctx context.Context, publisher *sentinelPublisher, path string) {
	onError := publisher.onError
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
		publisher.setFileSentinels(doc.Clients)
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

func secretClients(byClient map[string][]string) []proxy.SecretClient {
	clients := make([]proxy.SecretClient, 0, len(byClient))
	for clientID, sentinels := range byClient {
		if len(sentinels) == 0 {
			continue
		}
		clients = append(clients, proxy.SecretClient{ClientID: clientID, Sentinels: sentinels})
	}
	return clients
}
