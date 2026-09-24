package proxyagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	// gateClient carries a sandbox's calls to the discobox API (gate.go).
	gateClient *http.Client
	// activations translates an ephemeral sentinel back to the stable one the
	// control plane knows. Nil disables the agent credentials path entirely,
	// which is what a resolver built without a broker gets.
	activations *activations
}

func newSecretResolver(projectID, poolID string, live *activations) *secretResolver {
	return &secretResolver{
		contextPath: layout.ProxyResolveContextFile(projectID, poolID),
		client:      controlPlaneHTTPClient(),
		gateClient:  gateHTTPClient(),
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

// rejectionRequestBody reports what an upstream made of a credential this pool
// swapped in. It carries the stable sentinel, never the ephemeral one and never
// the value (ADR 0132 §2).
type rejectionRequestBody struct {
	SandboxID string `json:"sandboxId"`
	Sentinel  string `json:"sentinel"`
	Host      string `json:"host"`
	Outcome   string `json:"outcome"`
	// UseID names the agent-credential use the rejected sentinel was minted
	// for, when it was one. It is what makes a wrapped command's rejection
	// attributable to the use that authorized it (ADR 0079).
	UseID string `json:"useId,omitempty"`
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
	var useID string
	if record, ok := r.activation(req); ok {
		sentinel = record.Stable
		activationExpiry = record.ExpiresAt
		// The approved use this value is being taken under. The proxy records
		// it on the audit row, which is what joins a request that spent a
		// credential to the verdict that authorized it (ADR 0130 §3). Only the
		// agent credentials path has one; an ordinary injected sentinel leaves
		// it empty.
		useID = record.UseID
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
	result := proxy.SecretResolveResult{Value: out.Value, UseID: useID}
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

// Report tells the control plane what an upstream made of a value this resolver
// handed out.
//
// It translates the sentinel the same way Resolve does, and for the same
// reason: the control plane knows stable sentinels, and an ephemeral one is
// this process's own invention. The difference is which activations count —
// Resolve refuses a lapsed one because the use window is an authorization, and
// this accepts it because a rejection is a fact about the credential behind it,
// which the window's ending does not change. A report is also necessarily late:
// it crosses the response path, a queue, and a cooldown.
func (r *secretResolver) Report(ctx context.Context, req proxy.SecretReportRequest) error {
	rc, err := readResolveContext(r.contextPath)
	if err != nil || rc.Token == "" || rc.ControlPlaneURL == "" || rc.PoolID == "" {
		return nil
	}
	sentinel, useID := req.Sentinel, ""
	if record, ok := r.mintedActivation(req.Sentinel); ok {
		if record.SandboxID != req.ClientID {
			// A sentinel this process minted for a different sandbox. Resolve
			// refuses that; there is nothing to say about it either.
			return nil
		}
		sentinel, useID = record.Stable, record.UseID
	}
	payload, err := json.Marshal(rejectionRequestBody{
		SandboxID: req.ClientID,
		Sentinel:  sentinel,
		Host:      req.Host,
		Outcome:   string(req.Outcome),
		UseID:     useID,
	})
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/api/pools/%s/sandbox-secret-rejections", rc.ControlPlaneURL, rc.PoolID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+rc.Token)
	resp, err := r.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("report rejected secret: control plane returned %d", resp.StatusCode)
	}
	return nil
}

// mintedActivation returns the activation an ephemeral sentinel was minted
// under, live or lapsed.
//
// A sentinel this process never minted is a stable one, and is reported as
// itself. That is the same reading `Resolve` takes, and it is why an activation
// outlives its own expiry here by activationReportGrace: without the grace a
// lapsed sentinel would be indistinguishable from a stable one, and the report
// would carry the ephemeral string to a control plane that must never see one
// (ADR 0031 §3) and could not resolve it anyway.
func (r *secretResolver) mintedActivation(sentinel string) (activation, bool) {
	if r.activations == nil {
		return activation{}, false
	}
	return r.activations.lookupAny(sentinel)
}

// Judge decides whether a request may leave carrying the credentials the proxy
// just swapped into it. It is where the pool judge will read the request
// against the uses those credentials were approved for; until then every
// request that reaches it is allowed.
//
// It is not the only check a credential passes. By the time a request is
// judged, its destination has already been held to the host the credential was
// approved for — an activation's here (activation, below), any other
// sentinel's by the control plane's grant match — so the host is enforced
// whatever this answers.
func (r *secretResolver) Judge(context.Context, proxy.SecretJudgeRequest) (proxy.SecretVerdict, error) {
	return proxy.SecretVerdict{Allow: true}, nil
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

// policyPublisher owns the per-client policy the running proxy enforces.
// Three sources feed it and none can be applied alone, because ApplyConfig
// replaces the whole of it:
//
//   - SecretsFile, the sandbox's stable sentinels, written by the pool-agent
//     process as sandboxes come and go.
//   - live activations, the ephemeral sentinels this process mints per use.
//   - host trusts, the pins people approved for this pool's sandboxes
//     (hostTrusts, ADR 0149).
//
// Holding them here is what lets an activation or a newly approved pin take
// effect the instant it is known: publishing is a function call rather than a
// file the proxy has to notice.
type policyPublisher struct {
	server  *proxy.Server
	base    proxy.Config
	live    *activations
	onError func(error)

	mu     sync.Mutex
	file   map[string][]string
	trusts []proxy.HostTrust
	// applyMu makes each publish one read-and-apply, so two that race cannot
	// land an older reading of the sources over a newer one.
	applyMu sync.Mutex
}

func newPolicyPublisher(server *proxy.Server, base proxy.Config, live *activations, onError func(error)) *policyPublisher {
	p := &policyPublisher{server: server, base: base, live: live, onError: onError, file: map[string][]string{}}
	live.setChangeHandler(p.publish)
	return p
}

// setFileSentinels records the stable sentinel set and republishes.
func (p *policyPublisher) setFileSentinels(clients map[string][]string) {
	p.mu.Lock()
	p.file = clients
	p.mu.Unlock()
	p.publish()
}

// setTrusts records the pool's live host trusts and republishes.
func (p *policyPublisher) setTrusts(trusts []proxy.HostTrust) {
	p.mu.Lock()
	p.trusts = trusts
	p.mu.Unlock()
	p.publish()
}

// publish applies every source to the running proxy.
func (p *policyPublisher) publish() {
	p.applyMu.Lock()
	defer p.applyMu.Unlock()
	p.mu.Lock()
	merged := make(map[string][]string, len(p.file))
	for clientID, sentinels := range p.file {
		merged[clientID] = append([]string(nil), sentinels...)
	}
	trusts := p.trusts
	p.mu.Unlock()
	for clientID, sentinels := range p.live.sentinelsByClient() {
		merged[clientID] = append(merged[clientID], sentinels...)
	}

	cfg := p.base
	// Keep the swap tuning (TTLs, refresh interval, query scanning) from the
	// startup config; only the sentinel client set changes per apply.
	cfg.Secrets = p.base.Secrets
	cfg.Secrets.Clients = secretClients(merged)
	cfg.Trusts = trusts
	if err := p.server.ApplyConfig(cfg); err != nil && p.onError != nil {
		p.onError(err)
	}
}

// watchSecretsFile watches SecretsFile and feeds its sentinel sets to the
// publisher. It uses fsnotify so a sentinel push takes effect immediately
// (rather than after a poll interval), with a slow ticker backstop in case an
// event is missed.
func watchSecretsFile(ctx context.Context, publisher *policyPublisher, path string) {
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
