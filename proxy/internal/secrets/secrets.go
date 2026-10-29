// Package secrets swaps sentinel placeholder credentials found in proxied
// requests for their real values. A sandbox is provisioned with a sentinel (a
// convincing fake credential) instead of the real secret; the proxy detects the
// sentinel in outbound requests and, when the destination host is authorized,
// substitutes the real value resolved on demand. The real credential never
// exists inside the sandbox and is never persisted by the proxy.
package secrets

import (
	"context"
	"errors"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/discobox-ai/discobox/pkg/hostscope"
)

// ErrDenied indicates a sentinel is unknown, unapproved, or not permitted for
// the requested host. On this error the proxy leaves the sentinel in place so
// the upstream receives the placeholder and rejects the request. It is the
// only error that does: any other is taken as the resolver being unreachable,
// is asked again within the retry window, and refuses the request with a 503
// if it lasts (ADR 26-10-14-482 §2). A resolver returns it only for an answer
// about the credential, never for a failure to get one.
var ErrDenied = errors.New("secret resolution denied")

// ResolveRequest asks for the real value bound to a sentinel for a destination.
type ResolveRequest struct {
	ClientID string
	Sentinel string
	Host     string
}

// ResolveResult carries a resolved value and the time its grant expires. A zero
// ExpiresAt means the value is usable once but must not be cached.
type ResolveResult struct {
	Value     string
	ExpiresAt time.Time
	// SecretID is the secret the value is: the audit row names what a request
	// spent (ADR 26-10-01-240). It is an identifier, never a sentinel, so it is
	// safe in a trail kept to be read later; empty from a control plane that
	// predates it.
	SecretID string
	// ReportScope names the requests that answer for this credential: only an
	// upstream's verdict on one of them is reported (ADR 26-10-13-595). Empty
	// is every request, which is also what a control plane that predates it
	// says.
	ReportScope ReportScope
}

// ReportRule is one host, and the paths on it, whose answer says whether a
// credential is alive.
type ReportRule struct {
	// Host covers itself and the hosts beneath it, the way a secret's host
	// binding does.
	Host string
	// Paths are prefixes on a segment boundary: "/v1/messages" covers
	// "/v1/messages" and "/v1/messages/batches", never "/v1/messagesX". Empty
	// is every path on the host.
	Paths []string
}

// ReportScope is the set of requests whose refusal or acceptance says something
// about a credential (ADR 26-10-13-595). One credential is sent to many
// endpoints, and they do not all answer for the same thing: an endpoint that
// refuses a login for what it may do says nothing about whether it exists.
type ReportScope []ReportRule

// Covers reports whether a request to host and path is one this scope names.
// An empty scope names every request.
func (s ReportScope) Covers(host, path string) bool {
	if len(s) == 0 {
		return true
	}
	for _, rule := range s {
		if !hostscope.Covers(rule.Host, host) {
			continue
		}
		if len(rule.Paths) == 0 {
			return true
		}
		for _, prefix := range rule.Paths {
			if pathUnder(prefix, path) {
				return true
			}
		}
	}
	return false
}

// pathUnder reports whether path is prefix or lies beneath it, where beneath
// means past a segment boundary.
func pathUnder(prefix, path string) bool {
	prefix = strings.TrimRight(prefix, "/")
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	rest := path[len(prefix):]
	return rest == "" || rest[0] == '/'
}

// Outcome is what an upstream made of a credential the proxy swapped in, as
// the response path saw it. It is the whole vocabulary of a report: the
// resolver is told what happened, never what to do about it.
type Outcome string

const (
	// OutcomeRejected is a 401 on a swapped credential that the proxy had
	// nothing different to retry with — so the value the resolver would hand
	// out now is the one that was refused.
	OutcomeRejected Outcome = "rejected"
	// OutcomeRejectedAfterRetry is a 401 on the retry as well, sent with a
	// credential that differed from the one just refused (ADR 0059).
	OutcomeRejectedAfterRetry Outcome = "rejected-after-retry"
	// OutcomeAccepted is a swapped credential the upstream took. It is reported
	// only where a rejection was reported before it, as the clearance for that
	// rejection; a working credential is otherwise silent.
	OutcomeAccepted Outcome = "accepted"
)

// ReportRequest tells the resolver what an upstream made of the value it
// resolved. It names the sentinel and never the value: the resolver is the side
// that knows which credential the sentinel stands for.
type ReportRequest struct {
	ClientID string
	Sentinel string
	Host     string
	Outcome  Outcome
}

// AuthorizeRequest is a request carrying sentinels, as it is authorized
// before anything in it is resolved. Everything in it is what the sandbox
// sent — nothing has been substituted yet — so it holds sentinels and never a
// credential.
type AuthorizeRequest struct {
	ClientID string
	// Sentinels are the client's sentinels found in this request, literally or
	// inside a base64 token.
	Sentinels []string
	Method    string
	// Host is the destination without its port, stated the way Resolve states
	// it. The port, where there was one, is still in URL.
	Host   string
	URL    string
	Header http.Header
	// Body is the request body for a route's protocol block, or a `_method`
	// its methods must allow, to read (secretroute.Request.Body): called
	// lazily and at most once, it returns
	// the body decoded from its Content-Encoding and true, or false when the
	// body is past the proxy's limit, of undeclared length, unreadable, in
	// another encoding, or the request upgrades its connection. Reading it
	// does not consume it: the request is sent with the body it arrived with.
	// Nil is no body available.
	Body func() ([]byte, bool)
}

// Verdict is an authorizer's answer. A request it does not allow is never
// sent, and nothing in it is resolved.
type Verdict struct {
	Allow  bool
	Reason string
}

// Resolver resolves a sentinel to its real credential value, and hears back
// what the upstream made of it. Implementations live outside the proxy
// (pool-agent/proxyagent) so the proxy stays server-agnostic.
type Resolver interface {
	// Resolve returns the value bound to a sentinel for a host, ErrDenied when
	// the answer is no, or any other error when there is no answer yet — which
	// holds the request rather than sending the sentinel (see ErrDenied).
	Resolve(ctx context.Context, req ResolveRequest) (ResolveResult, error)
	// Report says what an upstream did with a value this resolver returned.
	// It is required rather than optional: a resolver that silently discarded
	// a rejection would be indistinguishable from a credential that never
	// failed, which is the state ADR 0132 exists to end.
	Report(ctx context.Context, req ReportRequest) error
	// Authorize decides whether a request may carry the credentials its
	// sentinels stand for: whether it falls on their secrets' routes (ADR
	// 26-10-02-924). It runs for every request that matched a sentinel, and it
	// runs before Resolve, so a refused request never decrypts a credential
	// and a cached value never carries a request its routes refuse. A
	// resolver that cannot answer returns an error, and nothing is resolved
	// or sent.
	Authorize(ctx context.Context, req AuthorizeRequest) (Verdict, error)
	// Gate answers a request for the gate host, which never goes to the
	// internet (Config.GateHost), from the upstream behind the gate. One it
	// cannot forward is refused with a *GateRefusal saying why, and never
	// sent. Any other error is the gate failing after it let the request in —
	// the upstream unreachable, or gone quiet after the request went out —
	// since the request may have been acted on.
	Gate(ctx context.Context, req GateRequest) (GateAdmission, error)
}

// GateAdmission is a request the gate let in: the upstream's answer.
type GateAdmission struct {
	Response *http.Response
}

// GateRefusal is a gate declining a request, and why. Its text is the reason
// alone, which is what the sandbox is shown: it is an answer, not a failure
// of anything.
type GateRefusal struct {
	Reason string
}

func (r *GateRefusal) Error() string { return r.Reason }

// GateRequest is a request for the gate host, from ClientID.
type GateRequest struct {
	ClientID string
	Request  *http.Request
}

// Config configures a Swapper.
type Config struct {
	// Sentinels maps client (sandbox) ID to the set of sentinel strings to
	// watch for in that client's requests.
	Sentinels map[string][]string
	// ScanQuery enables scanning URL query parameter values in addition to
	// header values.
	ScanQuery bool
	// PositiveTTL caps how long a resolved value is cached. Zero uses the
	// resolver-provided expiry unbounded.
	PositiveTTL time.Duration
	// NegativeTTL is how long a denial is cached before the proxy retries.
	NegativeTTL time.Duration
	// RefreshInterval is the soft TTL: a cached value older than this is
	// refreshed in the background on its next use, while the cached value keeps
	// serving requests until its hard expiry. This keeps values fresh without
	// invalidation, yet a control-plane outage cannot stop a running sandbox
	// from resolving until the grant actually expires. Zero uses a default.
	RefreshInterval time.Duration
	// ResolveRetry is how long a resolve that failed transiently — anything
	// but ErrDenied — is asked again before Apply gives up on it, long enough
	// to outlast a control-plane restart (ADR 26-10-14-482 §2). Zero uses a
	// default.
	ResolveRetry time.Duration
	// GateHost is a host the proxy never sends to the internet: a request for
	// it is answered by the resolver's Gate, which admits one carrying a live
	// use of the credential that opens it (ADR 0140 §2). Empty is no gate.
	GateHost string
}

const (
	defaultNegativeTTL     = 10 * time.Second
	defaultRefreshInterval = 30 * time.Second
	defaultResolveRetry    = 5 * time.Second
	// resolveRetryBackoff is the first pause between asks inside the retry
	// window, doubled after each one up to resolveRetryMaxBackoff: a restart
	// that takes a second is caught within one, and a longer outage is not
	// asked about ten times a second by every waiting request.
	resolveRetryBackoff    = 100 * time.Millisecond
	resolveRetryMaxBackoff = time.Second
	// resolveAttemptFloor is the least an ask is given when the window has
	// all but run out: a request carrying a second sentinel still asks about
	// it once, briefly, after the first used the window up.
	resolveAttemptFloor = time.Second
	// refreshTimeout bounds a background refresh so a hung control plane cannot
	// leak goroutines; the served value is unaffected while it runs.
	refreshTimeout = 30 * time.Second
	// previousValueGrace is how long the value served before a change stays
	// available as a retry fallback. A rotating credential is typically minted
	// well ahead of the moment the old one stops working — the control plane
	// refreshes an OAuth token five minutes before it expires — so the value a
	// rotation displaced is usually still good, and is the only other value
	// there is to try when the new one is rejected.
	previousValueGrace = 2 * time.Minute
)

// Swapper detects sentinels in requests and substitutes resolved values.
//
// One Swapper lives as long as the proxy that holds it: Reconfigure installs a
// new sentinel set and tuning without discarding what has been resolved, so a
// cached value outlives every publish that leaves its sentinel in place (ADR
// 26-10-14-482 §1).
type Swapper struct {
	resolver Resolver
	// settings is what Reconfigure replaces. It is stored under mu, so a
	// store that checks it under mu cannot race the prune that follows a
	// change, and read without it everywhere else.
	settings atomic.Pointer[settings]

	mu    sync.Mutex
	cache map[cacheKey]cacheEntry
	// previous is what each key resolved to before its most recent change,
	// kept for previousValueGrace. It is deliberately not part of cacheEntry:
	// Invalidate drops the entry, and the whole point of this memory is to
	// outlive the value that was just rejected.
	previous   map[cacheKey]previousValue
	refreshing map[cacheKey]struct{}
	now        func() time.Time
}

// settings is a Swapper's configuration: the sentinel set and swap tuning a
// Config carries, cleaned and defaulted.
type settings struct {
	scanQuery    bool
	posTTL       time.Duration
	negTTL       time.Duration
	refreshTTL   time.Duration
	resolveRetry time.Duration
	sentinels    map[string][]string
	gateHost     string
}

// watches reports whether sentinel is one of clientID's.
func (st *settings) watches(clientID, sentinel string) bool {
	return slices.Contains(st.sentinels[clientID], sentinel)
}

// cacheKey is what a resolved value is cached under: a credential is
// authorized per destination, so the same sentinel resolves separately for
// every host it is sent to.
type cacheKey struct {
	clientID string
	sentinel string
	host     string
}

type previousValue struct {
	value    string
	secretID string
	until    time.Time
}

type cacheEntry struct {
	value string
	// secretID rides the cached value because the audit row needs it on every
	// request, not only the one that resolved, and a sentinel stands for one
	// secret.
	secretID string
	// reportScope rides it too: whether a request answers for the credential
	// is asked of every request that carries it.
	reportScope ReportScope
	denied      bool
	// expiresAt is the hard bound: past it the entry is unusable and a request
	// resolves synchronously.
	expiresAt time.Time
	// refreshAt is the soft bound: past it (but before expiresAt) the value is
	// still served, and a background refresh is kicked off. Zero disables
	// background refresh for the entry (denials, or no soft window).
	refreshAt time.Time
}

// New creates a Swapper. A nil resolver produces a Swapper that never swaps.
func New(resolver Resolver, cfg Config) *Swapper {
	s := &Swapper{
		resolver:   resolver,
		cache:      map[cacheKey]cacheEntry{},
		previous:   map[cacheKey]previousValue{},
		refreshing: map[cacheKey]struct{}{},
		now:        time.Now,
	}
	s.settings.Store(newSettings(cfg))
	return s
}

// Reconfigure installs cfg's sentinel set and tuning in place. What was
// resolved for a sentinel the new set still holds is kept — its value, its
// denial, and the value a rotation displaced — so a publish that changes
// trusts, or another client's sentinels, costs nothing that is cached. A
// sentinel that left a client's set is forgotten for that client here, and
// stops resolving with this call (ADR 26-10-14-482 §1).
//
// An entry kept keeps the bounds it was cached with; new tuning applies to
// what is resolved from now on.
func (s *Swapper) Reconfigure(cfg Config) {
	if s == nil {
		return
	}
	next := newSettings(cfg)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settings.Store(next)
	for key := range s.cache {
		if !next.watches(key.clientID, key.sentinel) {
			delete(s.cache, key)
		}
	}
	for key := range s.previous {
		if !next.watches(key.clientID, key.sentinel) {
			delete(s.previous, key)
		}
	}
}

func newSettings(cfg Config) *settings {
	sentinels := make(map[string][]string, len(cfg.Sentinels))
	for clientID, list := range cfg.Sentinels {
		cleaned := make([]string, 0, len(list))
		for _, sentinel := range list {
			if sentinel = strings.TrimSpace(sentinel); sentinel != "" {
				cleaned = append(cleaned, sentinel)
			}
		}
		if len(cleaned) > 0 {
			sentinels[clientID] = cleaned
		}
	}
	negTTL := cfg.NegativeTTL
	if negTTL <= 0 {
		negTTL = defaultNegativeTTL
	}
	refreshTTL := cfg.RefreshInterval
	if refreshTTL <= 0 {
		refreshTTL = defaultRefreshInterval
	}
	resolveRetry := cfg.ResolveRetry
	if resolveRetry <= 0 {
		resolveRetry = defaultResolveRetry
	}
	return &settings{
		scanQuery:    cfg.ScanQuery,
		posTTL:       cfg.PositiveTTL,
		negTTL:       negTTL,
		refreshTTL:   refreshTTL,
		resolveRetry: resolveRetry,
		sentinels:    sentinels,
		gateHost:     extractHost(strings.ToLower(strings.TrimSpace(cfg.GateHost))),
	}
}

// Result describes what a swap did to a request.
type Result struct {
	// Headers is the set of request header names whose values were swapped.
	Headers []string
	// QueryParams is the set of query parameter names whose values were swapped.
	QueryParams []string
	// Sentinels is the set of sentinels whose values were substituted. It is
	// what makes a rejection attributable: a header name says a credential was
	// swapped, and only this says which one. Sentinels are non-secret by
	// construction — the pool publishes them in a plaintext file — so carrying
	// them out of the swap is not carrying the credential.
	Sentinels []string
	// Reportable is the subset of Sentinels this request answers for: the ones
	// whose resolver named no report scope, or named one that covers this
	// request (ADR 26-10-13-595). What the upstream makes of the request is
	// reported for these and for no others. Apply fills it; ApplyPrevious does
	// not, since a retry goes to the URL the first attempt was reported for.
	Reportable []string
	// Errors holds the transient resolution failures that outlasted the retry
	// window: a sentinel left in place for no reason but the resolver being
	// unreachable. A request with any must not be sent, since the upstream
	// would read the sentinel as a bad credential (ADR 26-10-14-482 §2). A
	// denial is not one of these; it leaves the sentinel in place and says
	// nothing here.
	Errors []string
	// Encoded reports that at least one substitution happened inside a
	// base64-encoded token rather than in a value's own text.
	Encoded bool
	// SecretIDs are the secrets the substituted values were (ADR
	// 26-10-01-240). It is plural because one request can carry more than one
	// sentinel: Git sends a username and a password in a single
	// Authorization: Basic token, and each half resolves on its own.
	SecretIDs []string
}

// Swapped reports whether any value in the request was substituted.
func (r Result) Swapped() bool { return len(r.Headers) > 0 || len(r.QueryParams) > 0 }

// Active reports whether the Swapper can swap for clientID.
func (s *Swapper) Active(clientID string) bool {
	return s != nil && s.resolver != nil && len(s.settings.Load().sentinels[clientID]) > 0
}

// Apply swaps any sentinels found in req's headers (and query, when enabled)
// for their resolved values, scoped to the destination host. Sentinels that
// cannot be resolved are left in place: a denied one says nothing more, and
// one that failed transiently, through the retry window, is named in
// Result.Errors. Apply mutates req.
func (s *Swapper) Apply(ctx context.Context, req *http.Request, clientID string) Result {
	if req == nil || !s.Active(clientID) {
		return Result{}
	}
	st := s.settings.Load()
	at := destination{
		host: extractHost(req.Host),
		path: requestPath(req),
		// One window for the whole request, however many sentinels it
		// carries: it bounds how long the request is held, not each ask.
		// Real time rather than s.now, which is the cache's clock.
		retryUntil: time.Now().Add(st.resolveRetry),
	}
	var res Result

	res.Headers, res.QueryParams = eachValue(st, req, func(value string) (string, bool) {
		return s.swapValue(ctx, st, clientID, at, value, &res)
	})

	dedupe(&res.Headers)
	dedupe(&res.QueryParams)
	dedupe(&res.SecretIDs)
	dedupe(&res.Sentinels)
	dedupe(&res.Reportable)
	return res
}

// Match reports which of clientID's sentinels req carries, literally or inside
// a base64 token, and resolves none of them.
//
// It is what lets a request be authorized before anything in it is resolved.
// It walks exactly the surface Apply swaps, through the same scan, because the two drifting apart would be a credential going out on a
// request nothing authorized.
func (s *Swapper) Match(req *http.Request, clientID string) []string {
	if req == nil || !s.Active(clientID) {
		return nil
	}
	st := s.settings.Load()
	sentinels := st.sentinels[clientID]
	var found []string
	eachValue(st, req, func(value string) (string, bool) {
		swapSentinels(value, sentinels, func(sentinel string) (string, bool) {
			found = append(found, sentinel)
			// Substituting nothing is what keeps this a read: the value the
			// scan rebuilds is discarded, and eachValue writes back only what
			// a visit claims to have changed.
			return "", false
		})
		return "", false
	})
	dedupe(&found)
	return found
}

// eachValue calls visit with every header value, and with every query value
// when query scanning is on, writing back the ones visit says it changed. It
// returns the header names and query parameters that were written.
//
// It is the one definition of the surface Match and Apply share, so that what
// is authorized and what is substituted cannot come apart. ApplyPrevious scans
// a subset of it — headers only, from its own loop, because a request whose
// query was swapped is never retried at all.
func eachValue(st *settings, req *http.Request, visit func(value string) (string, bool)) (headers, params []string) {
	for name, values := range req.Header {
		for i, value := range values {
			swapped, ok := visit(value)
			if !ok {
				continue
			}
			req.Header[name][i] = swapped
			headers = append(headers, http.CanonicalHeaderKey(name))
		}
	}

	if st.scanQuery && req.URL != nil && req.URL.RawQuery != "" {
		query := req.URL.Query()
		changed := false
		for name, values := range query {
			for i, value := range values {
				swapped, ok := visit(value)
				if !ok {
					continue
				}
				query[name][i] = swapped
				changed = true
				params = append(params, name)
			}
		}
		if changed {
			req.URL.RawQuery = query.Encode()
		}
	}
	return headers, params
}

func (s *Swapper) swapValue(ctx context.Context, st *settings, clientID string, at destination, value string, res *Result) (string, bool) {
	out := swapSentinels(value, st.sentinels[clientID], func(sentinel string) (string, bool) {
		return s.resolve(ctx, st, clientID, sentinel, at, res)
	})
	if out.encoded {
		res.Encoded = true
	}
	return out.value, out.swapped
}

// lookupFunc returns the value to substitute for a sentinel, and whether there
// is one to substitute. It is what separates a swap from a retry with the
// displaced value: the scanning is identical, only the lookup differs.
type lookupFunc func(sentinel string) (string, bool)

// swapOutcome is the result of scanning one value.
type swapOutcome struct {
	value   string
	swapped bool
	// encoded reports that at least one substitution happened inside a
	// base64-encoded token rather than in the value's own text.
	encoded bool
}

// swapSentinels substitutes every sentinel in value, both where it appears
// literally and where it is hidden inside a base64-encoded token (see
// encoded.go). Encoded tokens are handled first so a substituted real value is
// never itself decoded and rescanned.
func swapSentinels(value string, sentinels []string, lookup lookupFunc) swapOutcome {
	out, encoded := swapEncoded(value, sentinels, lookup)
	out, literal := replaceSentinels(out, sentinels, lookup)
	return swapOutcome{value: out, swapped: encoded || literal, encoded: encoded}
}

// replaceSentinels substitutes every resolvable sentinel appearing literally in
// value.
func replaceSentinels(value string, sentinels []string, lookup lookupFunc) (string, bool) {
	out := value
	swapped := false
	for _, sentinel := range sentinels {
		if !strings.Contains(out, sentinel) {
			continue
		}
		resolved, ok := lookup(sentinel)
		if !ok {
			continue
		}
		out = strings.ReplaceAll(out, sentinel, resolved)
		swapped = true
	}
	return out, swapped
}

func (s *Swapper) resolve(ctx context.Context, st *settings, clientID, sentinel string, at destination, res *Result) (string, bool) {
	host := at.host
	key := cacheKey{clientID: clientID, sentinel: sentinel, host: host}
	now := s.now()

	s.mu.Lock()
	if entry, ok := s.cache[key]; ok && now.Before(entry.expiresAt) {
		s.mu.Unlock()
		if entry.denied {
			return "", false
		}
		// Soft-expired but still within the hard bound: serve the cached value
		// and refresh it in the background, so a control-plane outage cannot stop
		// the running value from resolving before its grant truly expires.
		if !entry.refreshAt.IsZero() && !now.Before(entry.refreshAt) {
			s.triggerRefresh(key)
		}
		noteSwap(res, entry.secretID)
		noteSentinel(res, sentinel, entry.reportScope, at)
		return entry.value, true
	}
	s.mu.Unlock()

	result, err := s.resolveRetrying(ctx, ResolveRequest{ClientID: clientID, Sentinel: sentinel, Host: host}, at.retryUntil)
	now = s.now()
	if err != nil {
		if errors.Is(err, ErrDenied) {
			s.store(key, cacheEntry{denied: true, expiresAt: now.Add(st.negTTL)})
		} else {
			res.Errors = append(res.Errors, err.Error())
		}
		return "", false
	}

	entry, cacheable := entryFor(st, result, now)
	if cacheable {
		s.store(key, entry)
	}
	noteSwap(res, result.SecretID)
	noteSentinel(res, sentinel, result.ReportScope, at)
	return result.Value, true
}

// resolveRetrying asks the resolver, and asks again while the answer is a
// transient failure and retryUntil has not passed. A denial is an answer and is
// never asked again; a failure that outlasts the window is returned for the
// caller to refuse the request on (ADR 26-10-14-482 §2).
func (s *Swapper) resolveRetrying(ctx context.Context, req ResolveRequest, retryUntil time.Time) (ResolveResult, error) {
	return RetryTransient(ctx, retryUntil, func(attempt context.Context) (ResolveResult, error) {
		return s.resolver.Resolve(attempt, req)
	}, func(err error) bool { return errors.Is(err, ErrDenied) })
}

// RetryTransient runs ask, and runs it again while it fails, the failure is
// not an answer (answered), and retryUntil has not passed: the control plane's
// outage rule for anything the proxy asks it for (ADR 26-10-14-482 §2). The
// last failure is returned for the caller to refuse the request on.
//
// Each ask is bounded by the window too, so a control plane that hangs rather
// than refusing holds the request no longer than one that refuses — except
// that an ask is never given less than resolveAttemptFloor.
func RetryTransient[T any](ctx context.Context, retryUntil time.Time, ask func(context.Context) (T, error), answered func(error) bool) (T, error) {
	pause := resolveRetryBackoff
	for {
		attemptUntil := retryUntil
		if floor := time.Now().Add(resolveAttemptFloor); attemptUntil.Before(floor) {
			attemptUntil = floor
		}
		attempt, cancel := context.WithDeadline(ctx, attemptUntil)
		result, err := ask(attempt)
		cancel()
		if err == nil || answered(err) {
			return result, err
		}
		remaining := time.Until(retryUntil)
		if remaining <= 0 {
			return result, err
		}
		timer := time.NewTimer(min(pause, remaining))
		select {
		case <-ctx.Done():
			timer.Stop()
			return result, err
		case <-timer.C:
		}
		pause = min(2*pause, resolveRetryMaxBackoff)
	}
}

// destination is where a request is going, as far as a report scope reads it,
// and how long a value for it may be waited for.
type destination struct {
	host       string
	path       string
	retryUntil time.Time
}

func requestPath(req *http.Request) string {
	if req.URL == nil {
		return ""
	}
	return req.URL.Path
}

// noteSentinel records a substituted sentinel on a swap result, and whether
// the request it was substituted into is one that answers for it.
func noteSentinel(res *Result, sentinel string, scope ReportScope, at destination) {
	res.Sentinels = append(res.Sentinels, sentinel)
	if scope.Covers(at.host, at.path) {
		res.Reportable = append(res.Reportable, sentinel)
	}
}

// noteSwap records the secret one substituted value is on a swap result. An
// older control plane does not say, and an empty ID is not recorded.
func noteSwap(res *Result, secretID string) {
	if secretID != "" {
		res.SecretIDs = append(res.SecretIDs, secretID)
	}
}

// entryFor turns a resolver result into a cache entry, applying the positive TTL
// cap for the hard bound and the refresh interval for the soft bound. cacheable
// is false for a value that must be used once and not stored.
func entryFor(st *settings, result ResolveResult, now time.Time) (cacheEntry, bool) {
	expiresAt := result.ExpiresAt
	if st.posTTL > 0 {
		if capped := now.Add(st.posTTL); expiresAt.IsZero() || capped.Before(expiresAt) {
			expiresAt = capped
		}
	}
	if expiresAt.IsZero() || !expiresAt.After(now) {
		return cacheEntry{}, false
	}
	refreshAt := now.Add(st.refreshTTL)
	if !refreshAt.Before(expiresAt) {
		// The entry hard-expires before the soft window opens: no background
		// refresh, it will resolve synchronously once expired.
		refreshAt = time.Time{}
	}
	return cacheEntry{value: result.Value, secretID: result.SecretID, reportScope: result.ReportScope, expiresAt: expiresAt, refreshAt: refreshAt}, true
}

// triggerRefresh refreshes a soft-expired entry in the background, deduplicated
// per key. On success it replaces the entry; on an authoritative denial it
// caches the denial; on a transient failure it keeps serving the cached value
// and only pushes the soft bound forward so a sustained outage does not refetch
// on every request.
func (s *Swapper) triggerRefresh(key cacheKey) {
	s.mu.Lock()
	if _, inflight := s.refreshing[key]; inflight {
		s.mu.Unlock()
		return
	}
	s.refreshing[key] = struct{}{}
	s.mu.Unlock()

	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.refreshing, key)
			s.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout)
		defer cancel()
		result, err := s.resolver.Resolve(ctx, ResolveRequest{ClientID: key.clientID, Sentinel: key.sentinel, Host: key.host})
		now := s.now()
		st := s.settings.Load()
		if err != nil {
			if errors.Is(err, ErrDenied) {
				// Control plane answered authoritatively: stop serving the value.
				s.store(key, cacheEntry{denied: true, expiresAt: now.Add(st.negTTL)})
				return
			}
			// Transient failure (control plane unreachable): keep the cached value
			// until its hard expiry, backing the soft bound off by one interval.
			s.deferRefresh(key, now.Add(st.refreshTTL))
			return
		}
		entry, cacheable := entryFor(st, result, now)
		if !cacheable {
			s.evict(key)
			return
		}
		s.store(key, entry)
	}()
}

// store caches entry under key, unless the sentinel has left its client's set
// since it was resolved: Reconfigure forgets such a sentinel, and an answer
// that arrives after it must not bring it back.
func (s *Swapper) store(key cacheKey, entry cacheEntry) {
	s.mu.Lock()
	if !s.settings.Load().watches(key.clientID, key.sentinel) {
		s.mu.Unlock()
		return
	}
	if old, ok := s.cache[key]; ok && !old.denied && old.value != "" && old.value != entry.value {
		s.previous[key] = previousValue{value: old.value, secretID: old.secretID, until: s.now().Add(previousValueGrace)}
	}
	s.cache[key] = entry
	s.mu.Unlock()
}

// deferRefresh pushes a still-valid entry's soft bound forward after a failed
// background refresh, leaving its value and hard expiry intact.
func (s *Swapper) deferRefresh(key cacheKey, refreshAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.cache[key]
	if !ok || entry.denied {
		return
	}
	entry.refreshAt = refreshAt
	s.cache[key] = entry
}

// ApplyPrevious substitutes sentinels in req's headers for the value that was
// being served before the most recent change, when there is one and it is still
// within previousValueGrace. It exists for one case: the upstream rejected a
// freshly rotated credential that it has not started honoring yet, where
// resolving again only produces the same rejected value and the displaced one
// is the only thing left to try.
//
// Headers only. A query-param swap rewrites the URL, and a retry is not worth
// reconstructing one.
func (s *Swapper) ApplyPrevious(req *http.Request, clientID string) Result {
	if req == nil || !s.Active(clientID) {
		return Result{}
	}
	sentinels := s.settings.Load().sentinels[clientID]
	host := extractHost(req.Host)
	now := s.now()
	var res Result
	for name, values := range req.Header {
		for i, value := range values {
			out := swapSentinels(value, sentinels, func(sentinel string) (string, bool) {
				prev, ok := s.previousFor(clientID, sentinel, host, now)
				if !ok {
					return "", false
				}
				// The retry spends the same secret the rejected attempt did, so
				// its audit row names it too.
				noteSwap(&res, prev.secretID)
				res.Sentinels = append(res.Sentinels, sentinel)
				return prev.value, true
			})
			if out.swapped {
				req.Header[name][i] = out.value
				res.Headers = append(res.Headers, http.CanonicalHeaderKey(name))
			}
		}
	}
	dedupe(&res.Headers)
	dedupe(&res.SecretIDs)
	dedupe(&res.Sentinels)
	return res
}

func (s *Swapper) previousFor(clientID, sentinel, host string, now time.Time) (previousValue, bool) {
	key := cacheKey{clientID: clientID, sentinel: sentinel, host: host}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok := s.previous[key]
	if !ok {
		return previousValue{}, false
	}
	if !now.Before(prev.until) {
		delete(s.previous, key)
		return previousValue{}, false
	}
	return prev, true
}

// Report hands the response path's verdict to the resolver. It is a
// pass-through: the caller owns whether a report is worth making (see the
// proxy's credential reporter), and this owns nothing but the nil checks a
// Swapper built without a resolver needs.
func (s *Swapper) Report(ctx context.Context, req ReportRequest) error {
	if s == nil || s.resolver == nil {
		return nil
	}
	return s.resolver.Report(ctx, req)
}

// ResolveRetry is how long a transient resolve failure is asked about again
// before Apply gives up on it.
func (s *Swapper) ResolveRetry() time.Duration {
	if s == nil {
		return defaultResolveRetry
	}
	return s.settings.Load().resolveRetry
}

// IsGate reports whether host is the gate host, which is answered by the
// resolver's Gate and never sent to the internet.
func (s *Swapper) IsGate(host string) bool {
	if s == nil {
		return false
	}
	gateHost := s.settings.Load().gateHost
	return gateHost != "" && extractHost(strings.ToLower(host)) == gateHost
}

// Gate hands a request for the gate host to the resolver. A Swapper without a
// resolver admits nothing.
func (s *Swapper) Gate(ctx context.Context, req GateRequest) (GateAdmission, error) {
	if s == nil || s.resolver == nil {
		return GateAdmission{}, &GateRefusal{Reason: "nothing answers for this host"}
	}
	return s.resolver.Gate(ctx, req)
}

// Authorize asks the resolver whether a request may carry what its sentinels
// stand for. A Swapper built without a resolver swaps nothing, so there is
// nothing to authorize.
func (s *Swapper) Authorize(ctx context.Context, req AuthorizeRequest) (Verdict, error) {
	if s == nil || s.resolver == nil {
		return Verdict{Allow: true}, nil
	}
	// The destination as Resolve will state it.
	req.Host = extractHost(req.Host)
	return s.resolver.Authorize(ctx, req)
}

// Invalidate drops whatever this client's sentinels resolved to for host, so
// the next Apply resolves them again rather than serving a cached value.
//
// It exists for the one thing a cache cannot know on its own: the upstream
// rejected the value. A token rotation the proxy has not caught up with looks
// exactly like a valid cache entry until something 401s on it, and only the
// response path sees that.
func (s *Swapper) Invalidate(clientID, host string) {
	if s == nil {
		return
	}
	host = extractHost(host)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sentinel := range s.settings.Load().sentinels[clientID] {
		delete(s.cache, cacheKey{clientID: clientID, sentinel: sentinel, host: host})
	}
}

func (s *Swapper) evict(key cacheKey) {
	s.mu.Lock()
	delete(s.cache, key)
	s.mu.Unlock()
}

func extractHost(hostPort string) string {
	if host, _, err := net.SplitHostPort(hostPort); err == nil {
		return strings.ToLower(host)
	}
	return strings.ToLower(hostPort)
}

func dedupe(list *[]string) {
	if len(*list) < 2 {
		return
	}
	slices.Sort(*list)
	*list = slices.Compact(*list)
}
