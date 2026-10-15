package proxyagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/discobox-ai/discobox/hostscope"
	"github.com/discobox-ai/discobox/judge"
	"github.com/discobox-ai/discobox/layout"
	"github.com/discobox-ai/discobox/proxy"
	"github.com/discobox-ai/discobox/proxy/egress"
	"github.com/discobox-ai/discobox/secretroute"
	"github.com/discobox-ai/discobox/wire"
)

const (
	secretsPollInterval = 2 * time.Second
	// secretsBackstopInterval re-applies the file periodically in case an
	// fsnotify event is dropped; the watcher handles the immediate path.
	secretsBackstopInterval = 30 * time.Second
	resolveHTTPTimeout      = 10 * time.Second
)

// secretsDoc is the on-disk per-sandbox proxy policy, keyed by sandbox (proxy
// client) ID: each sandbox's sentinel set, and the allow list of each
// restricted sandbox (ADR 26-10-01-032). A sandbox absent from Egress is
// unrestricted; one present reaches only its entries.
type secretsDoc struct {
	Clients map[string][]string `json:"clients"`
	// Routes are, per sandbox, the routes of each injected stable sentinel
	// whose secret carries routes (ADR 26-10-02-924): every request it is
	// sent in must fall on one, with no activation.
	Routes map[string]map[string][]secretroute.Route `json:"routes,omitempty"`
	Egress map[string][]string                       `json:"egress,omitempty"`
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
// updating SecretsFile, with the routes of those whose secret carries routes
// (ADR 26-10-02-924). Passing an empty set removes the sandbox entry.
func UpsertSandboxSentinels(root layout.Root, projectID, poolID string, sandboxID string, sentinels []string, routes map[string][]secretroute.Route) error {
	return updateSecretsDoc(root, projectID, poolID, func(doc *secretsDoc) {
		if len(sentinels) == 0 {
			delete(doc.Clients, sandboxID)
		} else {
			doc.Clients[sandboxID] = sentinels
		}
		held := map[string][]secretroute.Route{}
		for _, sentinel := range sentinels {
			if held[sentinel] = routes[sentinel]; len(held[sentinel]) == 0 {
				delete(held, sentinel)
			}
		}
		if len(held) == 0 {
			delete(doc.Routes, sandboxID)
		} else {
			if doc.Routes == nil {
				doc.Routes = map[string]map[string][]secretroute.Route{}
			}
			doc.Routes[sandboxID] = held
		}
	})
}

// SetSandboxEgress records a sandbox's effective egress policy for the proxy by
// updating SecretsFile. An unrestricted policy removes the sandbox's entry.
func SetSandboxEgress(root layout.Root, projectID, poolID string, sandboxID string, policy egress.Policy) error {
	return updateSecretsDoc(root, projectID, poolID, func(doc *secretsDoc) {
		if !policy.DenyByDefault {
			delete(doc.Egress, sandboxID)
			return
		}
		doc.Egress[sandboxID] = append([]string{}, egress.Normalize(policy.Allow)...)
	})
}

// RemoveSandboxPolicy drops a sandbox's sentinel set, its sentinels' routes,
// and its egress policy from SecretsFile.
func RemoveSandboxPolicy(root layout.Root, projectID, poolID string, sandboxID string) error {
	return updateSecretsDoc(root, projectID, poolID, func(doc *secretsDoc) {
		delete(doc.Clients, sandboxID)
		delete(doc.Routes, sandboxID)
		delete(doc.Egress, sandboxID)
	})
}

func updateSecretsDoc(root layout.Root, projectID, poolID string, update func(*secretsDoc)) error {
	path := root.ProxySecretsFile(projectID, poolID)
	secretsFileMu.Lock()
	defer secretsFileMu.Unlock()
	doc, err := readSecretsDoc(path)
	if err != nil {
		return err
	}
	if doc.Clients == nil {
		doc.Clients = map[string][]string{}
	}
	if doc.Egress == nil {
		doc.Egress = map[string][]string{}
	}
	update(&doc)
	return writeJSONAtomic(path, doc)
}

// WriteResolveContext writes the resolve credential the proxy unit reads.
func WriteResolveContext(root layout.Root, projectID, poolID string, controlPlaneURL, token string) error {
	return writeJSONAtomic(root.ProxyResolveContextFile(projectID, poolID), resolveContext{
		ControlPlaneURL: controlPlaneURL,
		PoolID:          poolID,
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

	// sentinelRoutes are the routes a stable sentinel is held to without an
	// activation, by sandbox, as SecretsFile last said (secretsDoc.Routes).
	routesMu       sync.RWMutex
	sentinelRoutes map[string]map[string][]secretroute.Route
}

func (r *secretResolver) setSentinelRoutes(routes map[string]map[string][]secretroute.Route) {
	r.routesMu.Lock()
	defer r.routesMu.Unlock()
	r.sentinelRoutes = routes
}

// stageSentinelRoutes installs next while keeping the current routes of every
// sentinel next does not name, for the moment the sentinel set takes to catch
// up with it. setSentinelRoutes(next) then drops them.
func (r *secretResolver) stageSentinelRoutes(next map[string]map[string][]secretroute.Route) {
	r.routesMu.Lock()
	defer r.routesMu.Unlock()
	staged := make(map[string]map[string][]secretroute.Route, len(next)+len(r.sentinelRoutes))
	for sandboxID, sentinels := range next {
		staged[sandboxID] = maps.Clone(sentinels)
	}
	for sandboxID, sentinels := range r.sentinelRoutes {
		for sentinel, routes := range sentinels {
			if _, ok := staged[sandboxID][sentinel]; ok {
				continue
			}
			if staged[sandboxID] == nil {
				staged[sandboxID] = map[string][]secretroute.Route{}
			}
			staged[sandboxID][sentinel] = routes
		}
	}
	r.sentinelRoutes = staged
}

func (r *secretResolver) heldRoutes(sandboxID, sentinel string) []secretroute.Route {
	r.routesMu.RLock()
	defer r.routesMu.RUnlock()
	return r.sentinelRoutes[sandboxID][sentinel]
}

func newSecretResolver(root layout.Root, projectID, poolID string, live *activations) *secretResolver {
	contextPath := root.ProxyResolveContextFile(projectID, poolID)
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
	// SecretID is the secret the value is, which the proxy records on every
	// request it swaps the value into (ADR 26-10-01-240). Empty from a control
	// plane that predates it.
	SecretID string `json:"secretId"`
	// ReportScope names the requests that answer for the credential, which the
	// proxy reports a refusal or an acceptance of and no others
	// (ADR 26-10-13-595). Absent is every request.
	ReportScope []reportRuleBody `json:"reportScope,omitempty"`
}

type reportRuleBody struct {
	Host  string   `json:"host"`
	Paths []string `json:"paths,omitempty"`
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
	if resp.StatusCode >= http.StatusInternalServerError || resolveStatusTransient(resp.StatusCode) {
		// No answer about the credential, only about the control plane: treat
		// it as transient, not an authoritative denial, so a cached value
		// keeps serving until its grant expires and a request with nothing
		// cached is held and then refused rather than sent with its sentinel
		// (ADR 26-10-14-482 §2).
		return proxy.SecretResolveResult{}, fmt.Errorf("resolve secret: control plane returned %d", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		// Not found / forbidden: the control plane's answer. Leave the
		// sentinel in place.
		return proxy.SecretResolveResult{}, proxy.ErrSecretResolveDenied
	}
	var out resolveResponseBody
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return proxy.SecretResolveResult{}, err
	}
	if out.Status != "approved" || out.Value == "" {
		return proxy.SecretResolveResult{}, proxy.ErrSecretResolveDenied
	}
	result := proxy.SecretResolveResult{Value: out.Value, UseID: useID, SecretID: out.SecretID}
	for _, rule := range out.ReportScope {
		result.ReportScope = append(result.ReportScope, proxy.SecretReportRule{Host: rule.Host, Paths: rule.Paths})
	}
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

// resolveStatusTransient reports whether a status below 500 says the resolve
// was not answered rather than answered no: the request timed out, came too
// early, or was rate limited. Asked again, any of them can be approved.
func resolveStatusTransient(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
		return true
	}
	return false
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
// stand for, before any of them is resolved (ADR 26-09-22-838 §4). It binds every
// sentinel the proxy matched to a live activation of the sandbox that sent the
// request, and those bindings name the approved uses the request is authorized
// against: a request says nothing about which use it is spending, and nothing
// it said about one could be believed.
//
// The project's judge reads the request against those uses, one ask per use,
// and every one of them has to allow before anything is substituted.
//
// A sentinel whose secret carries routes (ADR 26-10-02-924) must be sent on
// one: a request no route covers is refused here without asking anybody, one
// on a route without a use passes, and one on routes that all carry a use is
// judged against them. A sentinel with no live activation and no routes is
// spending no approved use and keeps the policy it already has, which is not
// one policy but two. An injected harness credential goes to the control plane at resolve time and is held to its
// grant and host there. One this process minted whose activation has lapsed,
// or that is being spent against a host the use does not cover, never gets
// that far: Resolve refuses it here (isEphemeralCandidate, below), which is
// what keeps the use window and the host scope from being advisory. Either
// way that check now runs second rather than alone, because resolution happens
// only for a request this allowed.
func (r *secretResolver) Authorize(ctx context.Context, req proxy.SecretAuthorizeRequest) (proxy.SecretVerdict, error) {
	verdict, err := r.authorize(ctx, req)
	if !verdict.Allow {
		verdict.Refuse = refusalFor(req)
	}
	return verdict, err
}

// refusalFor is how a refused request's own protocol says no, when it has a
// way its client shows (ADR 26-09-26-240 §5), or nil for the proxy's plain
// refusal. It reads the body only if the refusal is written.
func refusalFor(req proxy.SecretAuthorizeRequest) func(context.Context, string) (proxy.SecretRefusal, bool) {
	recognized := recognize(req)
	if recognized.protocol == nil || recognized.protocol.refuse == nil || req.Body == nil {
		return nil
	}
	return func(ctx context.Context, said string) (proxy.SecretRefusal, bool) {
		wait, cancel := context.WithTimeout(ctx, bodyArrivalWait)
		defer cancel()
		raw, complete, err := req.Body.Capture(wait)
		if err != nil {
			return proxy.SecretRefusal{}, false
		}
		decoded, whole, missing := decodeBody(raw, complete, req.Header.Get("Content-Encoding"))
		if missing != "" {
			return proxy.SecretRefusal{}, false
		}
		return recognized.protocol.refuse(decoded, whole, said)
	}
}

// authorize is Authorize's verdict, before how a refusal is said.
func (r *secretResolver) authorize(ctx context.Context, req proxy.SecretAuthorizeRequest) (proxy.SecretVerdict, error) {
	groups, refusal := r.uses(req)
	if refusal != "" {
		// Off every route the credential has: there is no use to judge it
		// against, and it may not be sent.
		return proxy.SecretVerdict{Reason: refusal, UseIDs: flattenUses(groups)}, nil
	}
	if len(groups) == 0 {
		// Nothing here is being spent under an approved use, so there is no
		// sentence to judge it against and nothing to ask about.
		return proxy.SecretVerdict{Allow: true}, nil
	}
	uses := flattenUses(groups)
	if r.judge == nil || !r.judge.asking(time.Now()) {
		return proxy.SecretVerdict{Allow: true, UseIDs: uses}, nil
	}
	for _, group := range groups {
		// Every group has to pass before anything is substituted (ADR
		// 26-09-22-838 §4): a request spending two credentials is two
		// questions, and one of them saying no is the answer. Within a group —
		// the uses of the routes a secret's swap ties on — any one allowing is
		// enough (ADR 26-10-02-924).
		if verdict, settled, err := r.judgeGroup(ctx, req, uses, group); settled {
			return verdict, err
		}
	}
	return proxy.SecretVerdict{Allow: true, UseIDs: uses}, nil
}

// judgeGroup asks about a group's uses in turn until one allows. It is settled
// the way judgeUse is, except that a judge refusing one use moves on to the
// next: only the last refusal is the group's. Anything that is not the judge
// deciding no — a server with nobody to ask, a silence, an ask the control
// plane would not take — is the request's whole answer, as it is for one use.
func (r *secretResolver) judgeGroup(ctx context.Context, req proxy.SecretAuthorizeRequest, uses, group []string) (proxy.SecretVerdict, bool, error) {
	var refusal proxy.SecretVerdict
	for _, useID := range group {
		verdict, settled, err := r.judgeUse(ctx, req, uses, useID)
		if !settled {
			return proxy.SecretVerdict{}, false, nil
		}
		if verdict.Allow || err != nil {
			return verdict, true, err
		}
		refusal = verdict
	}
	return refusal, true, nil
}

func flattenUses(groups [][]string) []string {
	var out []string
	for _, group := range groups {
		out = append(out, group...)
	}
	return out
}

// judgeUse asks the judge about one use until it decides, showing it the body
// when it asks (ADR 26-09-22-838 §6). It reports settled when its answer is the
// request's whole answer — a refusal, or a server with nobody to ask — and
// not settled when the judge allowed and the next use is to be asked about.
//
// The rounds share one deadline, because the request is held open while the
// judge thinks: a judge that keeps asking spends the time the first ask had,
// not three times it. Each ask says how much is left, so the control plane
// answers a later round inside it (judgeHTTPTimeout).
func (r *secretResolver) judgeUse(ctx context.Context, req proxy.SecretAuthorizeRequest, uses []string, useID string) (proxy.SecretVerdict, bool, error) {
	exchange, cancel := context.WithTimeout(ctx, judgeHTTPTimeout)
	defer cancel()
	evidence := evidenceOf(exchange, req)
	for round := 1; ; round++ {
		deadline, _ := exchange.Deadline()
		answer, err := r.judge.ask(exchange, judgeAsk{
			SandboxID:     req.ClientID,
			UseID:         useID,
			Round:         round,
			Request:       evidence,
			TimeoutMillis: max(time.Until(deadline).Milliseconds(), 1),
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
			return proxy.SecretVerdict{UseIDs: uses}, true, err
		case outcomeOf(err) == outcomeNobodyJudges:
			// There is nobody to ask, which is not a refusal. Remembered for a
			// few minutes so a server that does not judge is asked once in a
			// while rather than once a request.
			r.judge.disabled(time.Now())
			return proxy.SecretVerdict{Allow: true, UseIDs: uses}, true, nil
		case outcomeOf(err) == outcomeRefused:
			// The control plane would not take this ask. That refuses the
			// request and nothing else: it neither proves the server judges
			// nor silences the next question, so a request shaped to be
			// refused costs the discobox that sent it and no one else.
			return proxy.SecretVerdict{UseIDs: uses}, true, err
		default:
			// Nobody answered at all — including an exchange whose deadline
			// ran out between rounds. Whether that refuses depends on
			// something this request cannot see: whether this pool has ever
			// been told the server judges. It must not be the thing that
			// breaks a discobox on a server that never turned judging on.
			if r.judge.unanswered(time.Now()) {
				return proxy.SecretVerdict{UseIDs: uses}, true, err
			}
			return proxy.SecretVerdict{Allow: true, UseIDs: uses}, true, nil
		}
		if answer.Need == nil {
			if verdict, ok := refusalFrom(answer, useID); !ok {
				return verdict, true, nil
			}
			return proxy.SecretVerdict{}, false, nil
		}
		// The judge wants the body. Asking is not allowing: a judge that is
		// still asking on the last round, or asks again for what it has
		// already been shown, has decided nothing, and that refuses.
		need := *answer.Need
		switch {
		case round >= judge.MaxRounds:
			return proxy.SecretVerdict{
				Reason: "the judge was still asking to see the body on its last round, and asking is not allowing",
				UseIDs: []string{useID},
			}, true, nil
		case evidence.Body.Answers(need):
			return proxy.SecretVerdict{
				Reason: "the judge asked again for what it had already been shown, and decided nothing",
				UseIDs: []string{useID},
			}, true, nil
		}
		evidence = showBody(exchange, evidence, req, need)
	}
}

// refusalFrom reads an answer that decided. It reports ok only for an explicit
// allow: a deny, or an answer that says neither, is a request that goes no
// further.
func refusalFrom(answer judgeAnswer, useID string) (proxy.SecretVerdict, bool) {
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
//
// Each group is one question, and every group must pass; within a group any
// one use allowing is enough. A grant's use and a host trust's are a group of
// one. A sentinel whose secret carries routes (ADR 26-10-02-924) adds the uses
// of the most specific routes the request falls on, as one group, or nothing
// when one of them carries no use; an activation of an agent-requested
// credential is held to its grant's use as well. A request off every route a
// secret has is refused, and refusal says so.
func (r *secretResolver) uses(req proxy.SecretAuthorizeRequest) (groups [][]string, refusal string) {
	seen := map[string]struct{}{}
	add := func(group []string) {
		var fresh []string
		for _, useID := range group {
			if useID == "" {
				continue
			}
			if _, ok := seen[useID]; ok {
				continue
			}
			seen[useID] = struct{}{}
			fresh = append(fresh, useID)
		}
		if len(fresh) > 0 {
			groups = append(groups, fresh)
		}
	}
	// A path with more than one reading is held to each: every reading must
	// fall on a route, and the uses of every judged one must allow, so a
	// request never lands on a less-judged route than the one it reaches.
	requestPaths := secretroute.RequestPaths(req.URL)
	route := func(routes []secretroute.Route) {
		if len(routes) == 0 {
			return
		}
		for _, requestPath := range requestPaths {
			group, covered := routeUses(routes, req.Host, requestPath)
			if !covered {
				if refusal == "" {
					refusal = fmt.Sprintf("this credential has no route to %s%s", req.Host, requestPath)
				}
				continue
			}
			add(group)
		}
	}
	for _, useID := range req.TrustUseIDs {
		add([]string{useID})
	}
	for _, sentinel := range req.Sentinels {
		record, ok := r.activation(proxy.SecretResolveRequest{
			ClientID: req.ClientID,
			Sentinel: sentinel,
			Host:     req.Host,
		})
		if ok && record.UseID != "" {
			add([]string{record.UseID})
			route(record.SecretRoutes)
			continue
		}
		route(r.heldRoutes(req.ClientID, sentinel))
	}
	return groups, refusal
}

// routeUses are the uses a request to host at requestPath is judged against
// under a secret's routes: those of the most specific routes it falls on, or
// none when one of them carries no use. covered is false when no route does.
func routeUses(routes []secretroute.Route, host, requestPath string) (uses []string, covered bool) {
	matched := secretroute.Match(routes, host, requestPath)
	if len(matched) == 0 {
		return nil, false
	}
	for _, route := range matched {
		if route.UseID == "" {
			return nil, true
		}
		uses = append(uses, route.UseID)
	}
	return uses, true
}

// activation returns the live activation for a resolve request, if the sentinel
// is one this process minted and the destination matches a host the use was
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
	// nothing above it. An activation with no host covers nothing: the
	// control plane mints no hostless use, so one is a server's omission, and
	// the wildcard is never what it meant.
	if len(record.Hosts) == 0 || !hostscope.CoversAny(record.Hosts, req.Host) {
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
	data, err := os.ReadFile(path)
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
//   - SecretsFile, each sandbox's stable sentinels and the allow list of each
//     restricted one, written by the pool-agent process as sandboxes come and
//     go and as their egress policy changes (ADR 26-10-01-032).
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
	file   secretsDoc
	trusts []proxy.HostTrust
	// applyMu makes each publish one read-and-apply, so two that race cannot
	// land an older reading of the sources over a newer one.
	applyMu sync.Mutex
}

func newPolicyPublisher(server *proxy.Server, base proxy.Config, live *activations, onError func(error)) *policyPublisher {
	p := &policyPublisher{server: server, base: base, live: live, onError: onError}
	live.setChangeHandler(p.publish)
	return p
}

// setFile records SecretsFile's sentinel sets and egress policies and
// republishes.
func (p *policyPublisher) setFile(doc secretsDoc) {
	p.mu.Lock()
	p.file = doc
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
	merged := make(map[string][]string, len(p.file.Clients))
	for clientID, sentinels := range p.file.Clients {
		merged[clientID] = append([]string(nil), sentinels...)
	}
	allowlist := make(map[string][]string, len(p.file.Egress))
	for clientID, allow := range p.file.Egress {
		allowlist[clientID] = append([]string(nil), allow...)
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
	cfg.Allowlist = proxy.AllowlistConfig{Clients: allowlist}
	if err := p.server.ApplyConfig(cfg); err != nil && p.onError != nil {
		p.onError(err)
	}
}

// watchSecretsFile watches SecretsFile and feeds its sentinel sets to the
// publisher. It uses fsnotify so a sentinel push takes effect immediately
// (rather than after a poll interval), with a slow ticker backstop in case an
// event is missed.
func watchSecretsFile(ctx context.Context, publisher *policyPublisher, resolver *secretResolver, path string) {
	onError := publisher.onError
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
		// A sentinel's routes bound where it may be swapped, so it is never
		// swappable without the ones it was written with (ADR 26-10-02-924):
		// the new routes go in before the sentinel set moves — keeping the old
		// routes of any sentinel on its way out — and only once it has moved
		// are those dropped. Either order alone leaves a moment in which a
		// sentinel arriving, or one leaving, reads as having no routes.
		resolver.stageSentinelRoutes(doc.Routes)
		publisher.setFile(doc)
		resolver.setSentinelRoutes(doc.Routes)
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
