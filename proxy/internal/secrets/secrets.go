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
}

// Resolver resolves a sentinel to its real credential value. Implementations
// live outside the proxy (worker-agent) so the proxy stays server-agnostic.
type Resolver interface {
	Resolve(ctx context.Context, req ResolveRequest) (ResolveResult, error)
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
	until time.Time
}

type cacheEntry struct {
	value  string
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
	// Errors holds non-fatal resolution error strings (transient failures).
	Errors []string
	// Encoded reports that at least one substitution happened inside a
	// base64-encoded token rather than in a value's own text.
	Encoded bool
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

	for name, values := range req.Header {
		for i, value := range values {
			swapped, ok := s.swapValue(ctx, clientID, host, value, sentinels, &res)
			if ok {
				req.Header[name][i] = swapped
				res.Headers = append(res.Headers, http.CanonicalHeaderKey(name))
			}
		}
	}

	if s.scanQuery && req.URL != nil && req.URL.RawQuery != "" {
		query := req.URL.Query()
		changed := false
		for name, values := range query {
			for i, value := range values {
				swapped, ok := s.swapValue(ctx, clientID, host, value, sentinels, &res)
				if ok {
					query[name][i] = swapped
					changed = true
					res.QueryParams = append(res.QueryParams, name)
				}
			}
		}
		if changed {
			req.URL.RawQuery = query.Encode()
		}
	}

	dedupe(&res.Headers)
	dedupe(&res.QueryParams)
	return res
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
	return result.Value, true
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
	return cacheEntry{value: result.Value, expiresAt: expiresAt, refreshAt: refreshAt}, true
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
		s.previous[key] = previousValue{value: old.value, until: s.now().Add(previousValueGrace)}
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
				return s.previousFor(clientID, sentinel, host, now)
			})
			if out.swapped {
				req.Header[name][i] = out.value
				res.Headers = append(res.Headers, http.CanonicalHeaderKey(name))
			}
		}
	}
	dedupe(&res.Headers)
	return res
}

func (s *Swapper) previousFor(clientID, sentinel, host string, now time.Time) (string, bool) {
	key := clientID + "\x00" + sentinel + "\x00" + host
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok := s.previous[key]
	if !ok {
		return "", false
	}
	if !now.Before(prev.until) {
		delete(s.previous, key)
		return "", false
	}
	return prev.value, true
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
