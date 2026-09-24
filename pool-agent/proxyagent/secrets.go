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
	// judge asks the project's judge whether a request may carry what its
	// sentinels stand for. Nil asks nobody, which allows every request that
	// reaches it.
	judge *judgeClient
}

func newSecretResolver(projectID, poolID string, live *activations) *secretResolver {
	contextPath := layout.ProxyResolveContextFile(projectID, poolID)
	client := controlPlaneHTTPClient()
	return &secretResolver{
		contextPath: contextPath,
		client:      client,
		gateClient:  gateHTTPClient(),
		activations: live,
		// Its own client: a verdict takes as long as a model takes, which is
		// nothing like the time a resolve takes.
		judge: &judgeClient{plane: &controlPlaneCredentials{
			contextPath: contextPath,
			client:      judgeHTTPClient(),
		}},
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

// Authorize decides whether a request may carry the credentials its sentinels
// stand for, before any of them is resolved (ADR 0150 §4). It binds every
// sentinel the proxy matched to a live activation of the sandbox that sent the
// request, and those bindings name the approved uses the request is authorized
// against: a request says nothing about which use it is spending, and nothing
// it said about one could be believed.
//
// The project's judge reads the request against those uses, one ask per use,
// and every one of them has to allow before anything is substituted.
//
// A sentinel with no live activation is spending no approved use and keeps the
// policy it already has, which is not one policy but two. An injected harness
// credential goes to the control plane at resolve time and is held to its
// grant and host there. One this process minted whose activation has lapsed,
// or that is being spent against a host the use does not cover, never gets
// that far: Resolve refuses it here (isEphemeralCandidate, below), which is
// what keeps the use window and the host scope from being advisory. Either
// way that check now runs second rather than alone, because resolution happens
// only for a request this allowed.
func (r *secretResolver) Authorize(ctx context.Context, req proxy.SecretAuthorizeRequest) (proxy.SecretVerdict, error) {
	uses := r.uses(req)
	if len(uses) == 0 {
		// Nothing here is being spent under an approved use, so there is no
		// sentence to judge it against and nothing to ask about.
		return proxy.SecretVerdict{Allow: true}, nil
	}
	if r.judge == nil || !r.judge.asking(time.Now()) {
		return proxy.SecretVerdict{Allow: true, UseIDs: uses}, nil
	}
	evidence := evidenceOf(req)
	for _, useID := range uses {
		// Every applicable use has to pass before anything is substituted
		// (ADR 0150 §4): a request spending two credentials is two questions,
		// and one of them saying no is the answer.
		answer, err := r.judge.ask(ctx, judgeAsk{
			SandboxID: req.ClientID,
			UseID:     useID,
			Round:     1,
			Request:   evidence,
		})
		switch {
		case err == nil:
			// A judge answered, which is the only thing that proves this
			// server judges. From here on a silence refuses.
			r.judge.answered()
		case ctx.Err() != nil:
			// The discobox hung up, or its own deadline passed, while the
			// judge was thinking. Nothing is substituted and there is nobody
			// left to answer, and it says nothing about the control plane —
			// so it must not silence the next request, which a sandbox could
			// otherwise arrange one aborted connection at a time.
			return proxy.SecretVerdict{UseIDs: uses}, err
		case outcomeOf(err) == outcomeNobodyJudges:
			// There is nobody to ask, which is not a refusal. Remembered for a
			// few minutes so a server that does not judge is asked once in a
			// while rather than once a request.
			r.judge.disabled(time.Now())
			return proxy.SecretVerdict{Allow: true, UseIDs: uses}, nil
		case outcomeOf(err) == outcomeRefused:
			// The control plane would not take this ask. That refuses the
			// request and nothing else: it neither proves the server judges
			// nor silences the next question, so a request shaped to be
			// refused costs the discobox that sent it and no one else.
			return proxy.SecretVerdict{UseIDs: uses}, err
		default:
			// Nobody answered at all. Whether that refuses depends on
			// something this request cannot see: whether this pool has ever
			// been told the server judges. It must not be the thing that
			// breaks a discobox on a server that never turned judging on.
			if r.judge.unanswered(time.Now()) {
				return proxy.SecretVerdict{UseIDs: uses}, err
			}
			return proxy.SecretVerdict{Allow: true, UseIDs: uses}, nil
		}
		if verdict, ok := refusalFrom(answer, useID); !ok {
			return verdict, nil
		}
	}
	return proxy.SecretVerdict{Allow: true, UseIDs: uses}, nil
}

// refusalFrom reads one answer. It reports ok only for an explicit allow:
// everything else — a deny, an answer that decided nothing, or the judge
// asking to be shown the body — is a request that goes no further.
func refusalFrom(answer judgeAnswer, useID string) (proxy.SecretVerdict, bool) {
	if answer.Need != nil {
		// The judge wants the body. Supplying it is the round the proxy does
		// not run yet, and a question left unanswered is not permission.
		return proxy.SecretVerdict{
			Reason: "the judge asked to see the request body, which this proxy cannot show it yet",
			UseIDs: []string{useID},
		}, false
	}
	if answer.Allow != nil && *answer.Allow {
		return proxy.SecretVerdict{Allow: true, UseIDs: []string{useID}}, true
	}
	reason := strings.TrimSpace(answer.Reason)
	if reason == "" {
		reason = "not an approved use of this credential"
	}
	return proxy.SecretVerdict{Reason: reason, UseIDs: []string{useID}}, false
}

// uses names every approved use this request has to pass: the ones its
// sentinels are being spent under, bound the way resolution binds them — this
// sandbox's activation, live, for a host the use covers — and the ones the
// destination was pinned for, which the proxy hands down from the trust table
// rather than the request carrying them (ADR 0149 §5).
//
// A request to a pinned host has to be asked about whether or not it spends a
// credential, so the trust's uses are here and not only alongside a sentinel.
func (r *secretResolver) uses(req proxy.SecretAuthorizeRequest) []string {
	var ids []string
	seen := make(map[string]struct{}, len(req.Sentinels)+len(req.TrustUseIDs))
	for _, useID := range req.TrustUseIDs {
		if useID == "" {
			continue
		}
		if _, ok := seen[useID]; ok {
			continue
		}
		seen[useID] = struct{}{}
		ids = append(ids, useID)
	}
	for _, sentinel := range req.Sentinels {
		record, ok := r.activation(proxy.SecretResolveRequest{
			ClientID: req.ClientID,
			Sentinel: sentinel,
			Host:     req.Host,
		})
		if !ok || record.UseID == "" {
			continue
		}
		if _, ok := seen[record.UseID]; ok {
			continue
		}
		seen[record.UseID] = struct{}{}
		ids = append(ids, record.UseID)
	}
	return ids
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
