package proxyagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/discobox-ai/discobox/pkg/layout"
	"github.com/discobox-ai/discobox/pkg/secretroute"
	"github.com/discobox-ai/discobox/pkg/wire"
	"github.com/discobox-ai/discobox/proxy"
	"github.com/discobox-ai/discobox/proxy/egress"
	"github.com/discobox-ai/discobox/proxy/gateway"
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
	// Version is the document's shape; see secretsDocVersion.
	Version int                 `json:"version,omitempty"`
	Clients map[string][]string `json:"clients"`
	// Routes are, per sandbox, the route lists each injected sentinel is held
	// to (ADR 26-10-27-880 §4): its secret's, its binding's when the binding
	// has any, and its parent's. Every request it is sent in must be allowed
	// by every list (secretroute.Decide). A sentinel with no lists is sent
	// nowhere.
	Routes map[string]map[string][][]secretroute.Route `json:"routes,omitempty"`
	Egress map[string][]string                         `json:"egress,omitempty"`
	// Gateways are, per sandbox, its project's gateways (ADR 26-10-16-470).
	// A header value from the server's environment is its variable's name.
	Gateways map[string][]gateway.Route `json:"gateways,omitempty"`
	// Environment is, per sandbox, each variable a credential is delivered
	// in and the sentinel delivered there: the sandbox's secret environment
	// less what its harness reads from a file. The credentials broker answers
	// the protocol's environment read from it, for a process that started
	// before a grant (agentcreds.PathEnvironment).
	Environment map[string]map[string]string `json:"environment,omitempty"`
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

// secretsDocVersion is the shape this pool agent writes SecretsFile in. 2 is
// a sentinel held to route lists (ADR 26-10-27-880 §4), and no lists sending
// it nowhere. A document with no version was written before: one route list a
// sentinel, and a sentinel with none held to no route at all (readSecretsDoc).
const secretsDocVersion = 2

// UpsertSandboxSentinels registers a sandbox's sentinel set with the proxy by
// updating SecretsFile, with the route lists each sentinel is held to (ADR
// 26-10-27-880 §4). Passing an empty set removes the sandbox entry. A
// sentinel routes names no lists for is sent nowhere.
func UpsertSandboxSentinels(root layout.Root, projectID, poolID string, sandboxID string, sentinels []string, routes map[string][][]secretroute.Route) error {
	return updateSecretsDoc(root, projectID, poolID, func(doc *secretsDoc) {
		if len(sentinels) == 0 {
			delete(doc.Clients, sandboxID)
		} else {
			doc.Clients[sandboxID] = sentinels
		}
		held := map[string][][]secretroute.Route{}
		for _, sentinel := range sentinels {
			if held[sentinel] = routes[sentinel]; len(held[sentinel]) == 0 {
				delete(held, sentinel)
			}
		}
		if len(held) == 0 {
			delete(doc.Routes, sandboxID)
		} else {
			if doc.Routes == nil {
				doc.Routes = map[string]map[string][][]secretroute.Route{}
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

// SetSandboxGateways records a sandbox's gateways for the proxy by updating
// SecretsFile. None removes the sandbox's entry.
func SetSandboxGateways(root layout.Root, projectID, poolID string, sandboxID string, routes []gateway.Route) error {
	return updateSecretsDoc(root, projectID, poolID, func(doc *secretsDoc) {
		if len(routes) == 0 {
			delete(doc.Gateways, sandboxID)
			return
		}
		doc.Gateways[sandboxID] = slices.Clone(routes)
	})
}

// SetSandboxEnvironment records the variables a sandbox's credentials are
// delivered in, each with its sentinel, by updating SecretsFile. None removes
// the sandbox's entry.
func SetSandboxEnvironment(root layout.Root, projectID, poolID string, sandboxID string, env map[string]string) error {
	return updateSecretsDoc(root, projectID, poolID, func(doc *secretsDoc) {
		if len(env) == 0 {
			delete(doc.Environment, sandboxID)
			return
		}
		doc.Environment[sandboxID] = maps.Clone(env)
	})
}

// RemoveSandboxPolicy drops a sandbox's sentinel set, its sentinels' routes,
// its egress policy, its gateways, and its credential environment from
// SecretsFile.
func RemoveSandboxPolicy(root layout.Root, projectID, poolID string, sandboxID string) error {
	return updateSecretsDoc(root, projectID, poolID, func(doc *secretsDoc) {
		delete(doc.Clients, sandboxID)
		delete(doc.Routes, sandboxID)
		delete(doc.Egress, sandboxID)
		delete(doc.Gateways, sandboxID)
		delete(doc.Environment, sandboxID)
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
	if doc.Gateways == nil {
		doc.Gateways = map[string][]gateway.Route{}
	}
	if doc.Environment == nil {
		doc.Environment = map[string]map[string]string{}
	}
	update(&doc)
	doc.Version = secretsDocVersion
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

// secretsDocOnDisk is SecretsFile as any pool agent may have written it. The
// file outlives the binary that wrote it, so a field this one no longer has,
// or one whose shape changed, is read here and converted rather than dropped
// or refused (pool-agent/proxyagent/REVIEW.md).
type secretsDocOnDisk struct {
	secretsDoc
	Routes map[string]map[string]json.RawMessage `json:"routes,omitempty"`
}

// persistedRoute is a route as written to SecretsFile. UseID is what a pool
// agent from before ADR 26-10-27-880 wrote for a route a judge read: a request
// on one was sent only on the judge's yes. It reads as blocked, so a pool
// upgraded under a running sandbox never sends one unjudged before the control
// plane pushes the route again.
type persistedRoute struct {
	secretroute.Route
	UseID string `json:"useId,omitempty"`
}

func (r persistedRoute) route() secretroute.Route {
	route := r.Route
	if strings.TrimSpace(r.UseID) != "" {
		route.Blocked = true
	}
	return route
}

// persistedLists reads one sentinel's routes as the document's version wrote
// them: route lists from version 2, one list before it.
func persistedLists(version int, raw json.RawMessage) ([][]secretroute.Route, error) {
	var lists [][]persistedRoute
	if version >= secretsDocVersion {
		if err := json.Unmarshal(raw, &lists); err != nil {
			return nil, err
		}
	} else {
		var one []persistedRoute
		if err := json.Unmarshal(raw, &one); err != nil {
			return nil, err
		}
		lists = [][]persistedRoute{one}
	}
	out := make([][]secretroute.Route, 0, len(lists))
	for _, list := range lists {
		routes := make([]secretroute.Route, 0, len(list))
		for _, route := range list {
			routes = append(routes, route.route())
		}
		out = append(out, routes)
	}
	return out, nil
}

// anyHost is the route list a sentinel from a document before version 2 is
// held to when that document named no routes for it: such a sentinel was held
// to none, so it reads as held to any host (ADR 26-10-27-939) until the
// control plane pushes its lists, rather than as sent nowhere. The control
// plane still hands its value out only for a host its routes allow.
var anyHost = [][]secretroute.Route{{{Host: secretroute.AnyHost}}}

func readSecretsDoc(path string) (secretsDoc, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return secretsDoc{Clients: map[string][]string{}}, nil
		}
		return secretsDoc{}, err
	}
	var onDisk secretsDocOnDisk
	if err := json.Unmarshal(data, &onDisk); err != nil {
		return secretsDoc{}, fmt.Errorf("parse %s: %w", path, err)
	}
	doc := onDisk.secretsDoc
	doc.Routes = nil
	set := func(sandboxID, sentinel string, lists [][]secretroute.Route) {
		if doc.Routes == nil {
			doc.Routes = map[string]map[string][][]secretroute.Route{}
		}
		if doc.Routes[sandboxID] == nil {
			doc.Routes[sandboxID] = map[string][][]secretroute.Route{}
		}
		doc.Routes[sandboxID][sentinel] = lists
	}
	for sandboxID, sentinels := range onDisk.Routes {
		for sentinel, raw := range sentinels {
			lists, err := persistedLists(doc.Version, raw)
			if err != nil {
				return secretsDoc{}, fmt.Errorf("parse %s: routes of a sentinel of %s: %w", path, sandboxID, err)
			}
			set(sandboxID, sentinel, lists)
		}
	}
	if doc.Version < secretsDocVersion {
		for sandboxID, sentinels := range doc.Clients {
			for _, sentinel := range sentinels {
				if _, ok := doc.Routes[sandboxID][sentinel]; !ok {
					set(sandboxID, sentinel, anyHost)
				}
			}
		}
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

	// sentinelRoutes are the route lists a sentinel is held to, by sandbox,
	// as SecretsFile last said (secretsDoc.Routes).
	routesMu       sync.RWMutex
	sentinelRoutes map[string]map[string][][]secretroute.Route
}

func (r *secretResolver) setSentinelRoutes(routes map[string]map[string][][]secretroute.Route) {
	r.routesMu.Lock()
	defer r.routesMu.Unlock()
	r.sentinelRoutes = routes
}

// stageSentinelRoutes installs next while keeping the current routes of every
// sentinel next does not name, for the moment the sentinel set takes to catch
// up with it. setSentinelRoutes(next) then drops them.
func (r *secretResolver) stageSentinelRoutes(next map[string]map[string][][]secretroute.Route) {
	r.routesMu.Lock()
	defer r.routesMu.Unlock()
	staged := make(map[string]map[string][][]secretroute.Route, len(next)+len(r.sentinelRoutes))
	for sandboxID, sentinels := range next {
		staged[sandboxID] = maps.Clone(sentinels)
	}
	for sandboxID, sentinels := range r.sentinelRoutes {
		for sentinel, routes := range sentinels {
			if _, ok := staged[sandboxID][sentinel]; ok {
				continue
			}
			if staged[sandboxID] == nil {
				staged[sandboxID] = map[string][][]secretroute.Route{}
			}
			staged[sandboxID][sentinel] = routes
		}
	}
	r.sentinelRoutes = staged
}

func (r *secretResolver) heldRoutes(sandboxID, sentinel string) [][]secretroute.Route {
	r.routesMu.RLock()
	defer r.routesMu.RUnlock()
	return r.sentinelRoutes[sandboxID][sentinel]
}

func newSecretResolver(root layout.Root, projectID, poolID string) *secretResolver {
	return &secretResolver{
		contextPath: root.ProxyResolveContextFile(projectID, poolID),
		client:      controlPlaneHTTPClient(),
		gateClient:  gateHTTPClient(),
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
// swapped in. It carries the sentinel, never the value (ADR 0132 §2).
type rejectionRequestBody struct {
	SandboxID string `json:"sandboxId"`
	Sentinel  string `json:"sentinel"`
	Host      string `json:"host"`
	Outcome   string `json:"outcome"`
}

func (r *secretResolver) Resolve(ctx context.Context, req proxy.SecretResolveRequest) (proxy.SecretResolveResult, error) {
	rc, err := readResolveContext(r.contextPath)
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
	result := proxy.SecretResolveResult{Value: out.Value, SecretID: out.SecretID}
	for _, rule := range out.ReportScope {
		result.ReportScope = append(result.ReportScope, proxy.SecretReportRule{Host: rule.Host, Paths: rule.Paths})
	}
	if out.ExpiresAt != nil {
		result.ExpiresAt = *out.ExpiresAt
	}
	return result, nil
}

type gatewayHeadersBody struct {
	SandboxID string   `json:"sandboxId"`
	Gateway   string   `json:"gateway"`
	Env       []string `json:"env"`
}

type gatewayHeadersResponse struct {
	Values map[string]string `json:"values"`
}

// ResolveGatewayHeaders asks the control plane for the server environment
// values a sandbox's gateway sets headers from (ADR 26-10-16-470 §4). The
// control plane checks the sandbox is this pool's, that its project's gateway
// is one the server configuration file keeps (ADR 26-10-17-271 §3), and that
// it names each variable; any answer but 200 is no answer, which the
// proxy holds the request on rather than sending it without its headers.
func (r *secretResolver) ResolveGatewayHeaders(ctx context.Context, req proxy.GatewayHeadersRequest) (map[string]string, error) {
	rc, err := readResolveContext(r.contextPath)
	if err != nil || rc.Token == "" || rc.ControlPlaneURL == "" || rc.PoolID == "" {
		return nil, errors.New("resolve gateway headers: no control plane credential yet")
	}
	url := fmt.Sprintf("%s/api/pools/%s/resolve-gateway-headers", rc.ControlPlaneURL, rc.PoolID)
	payload, err := json.Marshal(gatewayHeadersBody{SandboxID: req.ClientID, Gateway: req.Gateway, Env: req.Env})
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+rc.Token)
	resp, err := r.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("resolve gateway headers: control plane returned %d", resp.StatusCode)
	}
	var out gatewayHeadersResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Values, nil
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
func (r *secretResolver) Report(ctx context.Context, req proxy.SecretReportRequest) error {
	rc, err := readResolveContext(r.contextPath)
	if err != nil || rc.Token == "" || rc.ControlPlaneURL == "" || rc.PoolID == "" {
		return nil
	}
	payload, err := json.Marshal(rejectionRequestBody{
		SandboxID: req.ClientID,
		Sentinel:  req.Sentinel,
		Host:      req.Host,
		Outcome:   string(req.Outcome),
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

// Authorize decides whether a request may carry the credentials its sentinels
// stand for, before any of them is resolved. Every sentinel holds the request
// to each route list it was sent with (ADR 26-10-27-880 §4,
// secretroute.Decide): in each, it must fall on a route, and the most specific
// routes it falls on must not be blocked. A sentinel with no lists is sent
// nowhere. The first refusal is the answer; nothing is asked of the control
// plane.
func (r *secretResolver) Authorize(_ context.Context, req proxy.SecretAuthorizeRequest) (proxy.SecretVerdict, error) {
	for _, sentinel := range req.Sentinels {
		if allowed, reason := secretroute.Decide(r.heldRoutes(req.ClientID, sentinel), req.Host, req.URL); !allowed {
			return proxy.SecretVerdict{Reason: reason}, nil
		}
	}
	return proxy.SecretVerdict{Allow: true}, nil
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
// Two sources feed it and neither can be applied alone, because ApplyConfig
// replaces the whole of it:
//
//   - SecretsFile, each sandbox's sentinels and the allow list of each
//     restricted one, written by the pool-agent process as sandboxes come and
//     go and as their egress policy changes (ADR 26-10-01-032).
//   - host trusts, the pins people approved for this pool's sandboxes
//     (hostTrusts, ADR 0149).
//
// Holding them here is what lets a newly approved pin take effect the instant
// it is known: publishing is a function call rather than a file the proxy has
// to notice.
type policyPublisher struct {
	server  *proxy.Server
	base    proxy.Config
	onError func(error)

	mu     sync.Mutex
	file   secretsDoc
	trusts []proxy.HostTrust
	// applyMu makes each publish one read-and-apply, so two that race cannot
	// land an older reading of the sources over a newer one.
	applyMu sync.Mutex
}

func newPolicyPublisher(server *proxy.Server, base proxy.Config, onError func(error)) *policyPublisher {
	return &policyPublisher{server: server, base: base, onError: onError}
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
	if err := p.apply(); err != nil && p.onError != nil {
		p.onError(err)
	}
}

// loadFile applies SecretsFile as it stands before the proxy serves anything,
// and says when it cannot. A restricted sandbox's allow list is in that file,
// and the proxy filters only the clients it has a policy for, so serving
// before it is applied — or after reading it failed — would let every
// restricted sandbox out unfiltered (ADR 26-10-01-032 §6). A missing file is
// a pool with no sandboxes yet, not a failure.
//
// The sentinels' routes are staged and set around the publish in the order
// the watcher keeps (watchSecretsFile), so no sentinel is ever swappable
// without the routes it was written with (ADR 26-10-02-924).
func (p *policyPublisher) loadFile(path string, resolver *secretResolver) error {
	doc, err := readSecretsDoc(path)
	if err != nil {
		return fmt.Errorf("read proxy policy file: %w", err)
	}
	if resolver != nil {
		resolver.stageSentinelRoutes(doc.Routes)
	}
	p.mu.Lock()
	p.file = doc
	p.mu.Unlock()
	if err := p.apply(); err != nil {
		return fmt.Errorf("apply proxy policy file: %w", err)
	}
	if resolver != nil {
		resolver.setSentinelRoutes(doc.Routes)
	}
	return nil
}

// apply applies every source to the running proxy. A failed apply leaves the
// proxy on the last policy it took.
func (p *policyPublisher) apply() error {
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
	gateways := make(map[string][]gateway.Route, len(p.file.Gateways))
	for clientID, routes := range p.file.Gateways {
		gateways[clientID] = slices.Clone(routes)
	}
	trusts := p.trusts
	p.mu.Unlock()

	cfg := p.base
	// Keep the swap tuning (TTLs, refresh interval, query scanning) from the
	// startup config; only the sentinel client set changes per apply.
	cfg.Secrets = p.base.Secrets
	cfg.Secrets.Clients = secretClients(merged)
	cfg.Trusts = trusts
	cfg.Allowlist = proxy.AllowlistConfig{Clients: allowlist}
	cfg.Gateways = proxy.GatewaysConfig{Clients: gateways}
	return p.server.ApplyConfig(cfg)
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
