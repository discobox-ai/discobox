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
	"time"
)

// ErrDenied indicates a sentinel is unknown, unapproved, or not permitted for
// the requested host. On this error the proxy leaves the sentinel in place so
// the upstream receives the placeholder and rejects the request.
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
	// UseID names the approved use this value was taken under, when the
	// sentinel was one minted by the agent credentials protocol (ADR 0130 §3).
	// It is the join between a request that spent a credential and the verdict
	// that authorized it. A resolver with no use to name leaves it empty, which
	// is the ordinary injected-sentinel case rather than a failure.
	//
	// It is an identifier, never a sentinel: it authorizes nothing, so it is
	// safe in a trail kept to be read later.
	UseID string
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

<<<<<<< HEAD
// AuthorizeRequest is a request carrying sentinels, or one bound for a host
// the client trusts by a pin, as it is authorized before anything in it is
// resolved. Everything in it is what the sandbox sent — nothing has been
// substituted yet — so it holds sentinels and never a credential
// (ADR 0150 §4).
=======
// AuthorizeRequest is a request carrying sentinels, as it is authorized before
// any of them is resolved. Everything in it is what the sandbox sent — nothing
// has been substituted yet — so it holds sentinels and never a credential
// (ADR 0148 §4).
>>>>>>> c12e9e6d (docs(adr): the judge ADR is 0148, because 0141 was taken)
type AuthorizeRequest struct {
	ClientID string
	// Sentinels are the client's sentinels found in this request, literally or
	// inside a base64 token. They are what the resolver binds to the uses the
	// request is authorized against; the request itself says nothing about
	// which use it is spending, and could not be believed if it did.
	Sentinels []string
	// TrustUseIDs are the uses the destination was trusted for, when the
	// client reaches it by a pin (ADR 0149 §5). A request to a trusted host is
	// authorized against them whether or not it carries a credential, and they
	// come from the pin rather than from anything the request said.
	TrustUseIDs []string
	Method      string
	// Host is the destination without its port, stated the way Resolve states
	// it, so that a use bound here is the use the value is taken under. The
	// port, where there was one, is still in URL.
	Host   string
	URL    string
	Header http.Header
}

// Verdict is an authorizer's answer. A request it does not allow is never
// sent, and nothing in it is resolved.
type Verdict struct {
	Allow  bool
	Reason string
	// UseIDs are the approved uses the resolver bound this request's sentinels
	// to, empty for sentinels that carry no use. They name what the request
	// was authorized — or refused — under, so a refusal that resolved nothing
	// is still recorded against the use it was about.
	UseIDs []string
}

// Resolver resolves a sentinel to its real credential value, and hears back
// what the upstream made of it. Implementations live outside the proxy
// (pool-agent/proxyagent) so the proxy stays server-agnostic.
type Resolver interface {
	Resolve(ctx context.Context, req ResolveRequest) (ResolveResult, error)
	// Report says what an upstream did with a value this resolver returned.
	// It is required rather than optional: a resolver that silently discarded
	// a rejection would be indistinguishable from a credential that never
	// failed, which is the state ADR 0132 exists to end.
	Report(ctx context.Context, req ReportRequest) error
	// Authorize decides whether a request may carry the credentials its
	// sentinels stand for: whether sending it is what their uses were approved
	// for. It runs for every request that matched a sentinel, and it runs
	// before Resolve, so a refused request never decrypts a credential and a
	// cached value never authorizes a new operation (ADR 0148 §4). A resolver
	// that cannot answer returns an error, and nothing is resolved or sent.
	Authorize(ctx context.Context, req AuthorizeRequest) (Verdict, error)
	// Gate answers a request for the gate host, which never goes to the
	// internet (Config.GateHost). It admits the request only when it carries
	// a live use of the credential that opens the gate and the judge allows
	// it, and answers it from the upstream behind the gate. One it does not
	// admit is refused with a *GateRefusal saying why, and never sent. Any
	// other error is the gate failing after it let the request in — the
	// upstream unreachable, or gone quiet after the request went out — and
	// the admission returned with it still names the use, since the request
	// may have been acted on.
	Gate(ctx context.Context, req GateRequest) (GateAdmission, error)
}

// GateAdmission is a request the gate let in: the upstream's answer, and the
// approved use it was let in under. The use is what joins the audited request
// to the approval that allowed it, as a swapped credential's use does.
type GateAdmission struct {
	Response *http.Response
	UseID    string
}

// GateRefusal is a gate declining a request, and why. Its text is the reason
// alone, which is what the sandbox is shown: it is an answer, not a failure
// of anything. UseID is the use the request carried, when it carried one the
// gate recognized and still refused — the judge's refusal, say.
type GateRefusal struct {
	Reason string
	UseID  string
}

func (r *GateRefusal) Error() string { return r.Reason }

// GateRequest is a request for the gate host, from ClientID, with the
// sentinel it carries still in it.
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
	// GateHost is a host the proxy never sends to the internet: a request for
	// it is answered by the resolver's Gate, which admits one carrying a live
	// use of the credential that opens it (ADR 0140 §2). Empty is no gate.
	GateHost string
}

const (
	defaultNegativeTTL     = 10 * time.Second
	defaultRefreshInterval = 30 * time.Second
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
type Swapper struct {
	resolver   Resolver
	scanQuery  bool
	posTTL     time.Duration
	negTTL     time.Duration
	refreshTTL time.Duration
	sentinels  map[string][]string
	gateHost   string

	mu    sync.Mutex
	cache map[string]cacheEntry
	// previous is what each key resolved to before its most recent change,
	// kept for previousValueGrace. It is deliberately not part of cacheEntry:
	// Invalidate drops the entry, and the whole point of this memory is to
	// outlive the value that was just rejected.
	previous   map[string]previousValue
	refreshing map[string]struct{}
	now        func() time.Time
}

type previousValue struct {
	value string
	useID string
	until time.Time
}

type cacheEntry struct {
	value string
	// useID rides the cached value because the audit row needs it on every
	// request, not only the one that resolved. The cache key includes the
	// sentinel, and an ephemeral sentinel belongs to exactly one activation, so
	// an entry can never be shared by two uses.
	useID  string
	denied bool
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
	return &Swapper{
		resolver:   resolver,
		scanQuery:  cfg.ScanQuery,
		posTTL:     cfg.PositiveTTL,
		negTTL:     negTTL,
		refreshTTL: refreshTTL,
		sentinels:  sentinels,
		gateHost:   extractHost(strings.ToLower(strings.TrimSpace(cfg.GateHost))),
		cache:      map[string]cacheEntry{},
		previous:   map[string]previousValue{},
		refreshing: map[string]struct{}{},
		now:        time.Now,
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
	// Errors holds non-fatal resolution error strings (transient failures).
	Errors []string
	// Encoded reports that at least one substitution happened inside a
	// base64-encoded token rather than in a value's own text.
	Encoded bool
	// UseIDs are the approved uses the substituted values were taken under.
	// It is plural because one request can carry more than one sentinel: Git
	// sends a username and a password in a single Authorization: Basic token,
	// and each half resolves on its own.
	UseIDs []string
}

// Swapped reports whether any value in the request was substituted.
func (r Result) Swapped() bool { return len(r.Headers) > 0 || len(r.QueryParams) > 0 }

// Active reports whether the Swapper can swap for clientID.
func (s *Swapper) Active(clientID string) bool {
	return s != nil && s.resolver != nil && len(s.sentinels[clientID]) > 0
}

// Apply swaps any sentinels found in req's headers (and query, when enabled)
// for their resolved values, scoped to the destination host. Sentinels that
// cannot be resolved are left in place. Apply mutates req.
func (s *Swapper) Apply(ctx context.Context, req *http.Request, clientID string) Result {
	if req == nil || !s.Active(clientID) {
		return Result{}
	}
	sentinels := s.sentinels[clientID]
	host := extractHost(req.Host)
	var res Result

	res.Headers, res.QueryParams = s.eachValue(req, func(value string) (string, bool) {
		return s.swapValue(ctx, clientID, host, value, sentinels, &res)
	})

	dedupe(&res.Headers)
	dedupe(&res.QueryParams)
	dedupe(&res.UseIDs)
	dedupe(&res.Sentinels)
	return res
}

// Redact replaces every sentinel in value with marker, wherever the scan finds
// one — literally, and inside a base64 token, where it is re-encoded so the
// token stays a token.
//
// It exists because the values a sentinel hides in are the same ones a swap
// would substitute, and something that shows a request to somebody else has to
// cover exactly that set. Going through the same scan is what keeps the two
// from drifting (proxy/REVIEW.md).
func Redact(value string, sentinels []string, marker string) string {
	// An empty sentinel matches between every byte, and this lookup — unlike
	// the resolving one — can never decline, so it would rewrite the value
	// into nothing but markers. Apply is safe from that only because an empty
	// sentinel never resolves.
	usable := make([]string, 0, len(sentinels))
	for _, sentinel := range sentinels {
		if sentinel != "" {
			usable = append(usable, sentinel)
		}
	}
	if len(usable) == 0 {
		return value
	}
	out := swapSentinels(value, usable, func(string) (string, bool) {
		return marker, true
	})
	return out.value
}

// Match reports which of clientID's sentinels req carries, literally or inside
// a base64 token, and resolves none of them.
//
// It is what lets a request be authorized before anything in it is resolved
// (ADR 0148 §4). It walks exactly the surface Apply swaps, through the same
// scan, because the two drifting apart would be a credential going out on a
// request nothing authorized.
func (s *Swapper) Match(req *http.Request, clientID string) []string {
	if req == nil || !s.Active(clientID) {
		return nil
	}
	sentinels := s.sentinels[clientID]
	var found []string
	s.eachValue(req, func(value string) (string, bool) {
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
func (s *Swapper) eachValue(req *http.Request, visit func(value string) (string, bool)) (headers, params []string) {
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

	if s.scanQuery && req.URL != nil && req.URL.RawQuery != "" {
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

func (s *Swapper) swapValue(ctx context.Context, clientID, host, value string, sentinels []string, res *Result) (string, bool) {
	out := swapSentinels(value, sentinels, func(sentinel string) (string, bool) {
		return s.resolve(ctx, clientID, sentinel, host, res)
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

func (s *Swapper) resolve(ctx context.Context, clientID, sentinel, host string, res *Result) (string, bool) {
	key := clientID + "\x00" + sentinel + "\x00" + host
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
			s.triggerRefresh(clientID, sentinel, host, key)
		}
		noteUseID(res, entry.useID)
		res.Sentinels = append(res.Sentinels, sentinel)
		return entry.value, true
	}
	s.mu.Unlock()

	result, err := s.resolver.Resolve(ctx, ResolveRequest{ClientID: clientID, Sentinel: sentinel, Host: host})
	if err != nil {
		if errors.Is(err, ErrDenied) {
			s.store(key, cacheEntry{denied: true, expiresAt: now.Add(s.negTTL)})
		} else {
			res.Errors = append(res.Errors, err.Error())
		}
		return "", false
	}

	entry, cacheable := s.entryFor(result, now)
	if cacheable {
		s.store(key, entry)
	}
	noteUseID(res, result.UseID)
	res.Sentinels = append(res.Sentinels, sentinel)
	return result.Value, true
}

// noteUseID records one approved use on a swap result, ignoring the empty ID a
// sentinel outside the agent credentials protocol resolves with.
func noteUseID(res *Result, useID string) {
	if useID == "" {
		return
	}
	res.UseIDs = append(res.UseIDs, useID)
}

// entryFor turns a resolver result into a cache entry, applying the positive TTL
// cap for the hard bound and the refresh interval for the soft bound. cacheable
// is false for a value that must be used once and not stored.
func (s *Swapper) entryFor(result ResolveResult, now time.Time) (cacheEntry, bool) {
	expiresAt := result.ExpiresAt
	if s.posTTL > 0 {
		if capped := now.Add(s.posTTL); expiresAt.IsZero() || capped.Before(expiresAt) {
			expiresAt = capped
		}
	}
	if expiresAt.IsZero() || !expiresAt.After(now) {
		return cacheEntry{}, false
	}
	refreshAt := now.Add(s.refreshTTL)
	if !refreshAt.Before(expiresAt) {
		// The entry hard-expires before the soft window opens: no background
		// refresh, it will resolve synchronously once expired.
		refreshAt = time.Time{}
	}
	return cacheEntry{value: result.Value, useID: result.UseID, expiresAt: expiresAt, refreshAt: refreshAt}, true
}

// triggerRefresh refreshes a soft-expired entry in the background, deduplicated
// per key. On success it replaces the entry; on an authoritative denial it
// caches the denial; on a transient failure it keeps serving the cached value
// and only pushes the soft bound forward so a sustained outage does not refetch
// on every request.
func (s *Swapper) triggerRefresh(clientID, sentinel, host, key string) {
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
		result, err := s.resolver.Resolve(ctx, ResolveRequest{ClientID: clientID, Sentinel: sentinel, Host: host})
		now := s.now()
		if err != nil {
			if errors.Is(err, ErrDenied) {
				// Control plane answered authoritatively: stop serving the value.
				s.store(key, cacheEntry{denied: true, expiresAt: now.Add(s.negTTL)})
				return
			}
			// Transient failure (control plane unreachable): keep the cached value
			// until its hard expiry, backing the soft bound off by one interval.
			s.deferRefresh(key, now.Add(s.refreshTTL))
			return
		}
		entry, cacheable := s.entryFor(result, now)
		if !cacheable {
			s.evict(key)
			return
		}
		s.store(key, entry)
	}()
}

func (s *Swapper) store(key string, entry cacheEntry) {
	s.mu.Lock()
	if old, ok := s.cache[key]; ok && !old.denied && old.value != "" && old.value != entry.value {
		s.previous[key] = previousValue{value: old.value, useID: old.useID, until: s.now().Add(previousValueGrace)}
	}
	s.cache[key] = entry
	s.mu.Unlock()
}

// deferRefresh pushes a still-valid entry's soft bound forward after a failed
// background refresh, leaving its value and hard expiry intact.
func (s *Swapper) deferRefresh(key string, refreshAt time.Time) {
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
	sentinels := s.sentinels[clientID]
	host := extractHost(req.Host)
	now := s.now()
	var res Result
	for name, values := range req.Header {
		for i, value := range values {
			out := swapSentinels(value, sentinels, func(sentinel string) (string, bool) {
				value, useID, ok := s.previousFor(clientID, sentinel, host, now)
				// The retry spends the same approved use the rejected attempt
				// did, so its audit row names it too.
				noteUseID(&res, useID)
				if ok {
					res.Sentinels = append(res.Sentinels, sentinel)
				}
				return value, ok
			})
			if out.swapped {
				req.Header[name][i] = out.value
				res.Headers = append(res.Headers, http.CanonicalHeaderKey(name))
			}
		}
	}
	dedupe(&res.Headers)
	dedupe(&res.UseIDs)
	dedupe(&res.Sentinels)
	return res
}

func (s *Swapper) previousFor(clientID, sentinel, host string, now time.Time) (string, string, bool) {
	key := clientID + "\x00" + sentinel + "\x00" + host
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok := s.previous[key]
	if !ok {
		return "", "", false
	}
	if !now.Before(prev.until) {
		delete(s.previous, key)
		return "", "", false
	}
	return prev.value, prev.useID, true
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

// IsGate reports whether host is the gate host, which is answered by the
// resolver's Gate and never sent to the internet.
func (s *Swapper) IsGate(host string) bool {
	return s != nil && s.gateHost != "" && extractHost(strings.ToLower(host)) == s.gateHost
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
	// The destination as Resolve will state it, so that a sentinel bound to a
	// use here is bound to the same one when its value is fetched.
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
	for _, sentinel := range s.sentinels[clientID] {
		delete(s.cache, clientID+"\x00"+sentinel+"\x00"+host)
	}
}

func (s *Swapper) evict(key string) {
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
