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
}

const defaultNegativeTTL = 10 * time.Second

// Swapper detects sentinels in requests and substitutes resolved values.
type Swapper struct {
	resolver  Resolver
	scanQuery bool
	posTTL    time.Duration
	negTTL    time.Duration
	sentinels map[string][]string

	mu    sync.Mutex
	cache map[string]cacheEntry
	now   func() time.Time
}

type cacheEntry struct {
	value     string
	denied    bool
	expiresAt time.Time
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
	return &Swapper{
		resolver:  resolver,
		scanQuery: cfg.ScanQuery,
		posTTL:    cfg.PositiveTTL,
		negTTL:    negTTL,
		sentinels: sentinels,
		cache:     map[string]cacheEntry{},
		now:       time.Now,
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
	out := value
	swapped := false
	for _, sentinel := range sentinels {
		if !strings.Contains(out, sentinel) {
			continue
		}
		value, ok := s.resolve(ctx, clientID, sentinel, host, res)
		if !ok {
			continue
		}
		out = strings.ReplaceAll(out, sentinel, value)
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

	expiresAt := result.ExpiresAt
	if s.posTTL > 0 {
		if capped := now.Add(s.posTTL); expiresAt.IsZero() || capped.Before(expiresAt) {
			expiresAt = capped
		}
	}
	if expiresAt.IsZero() || !expiresAt.After(now) {
		// Usable now, but not cacheable.
		return result.Value, true
	}
	s.store(key, cacheEntry{value: result.Value, expiresAt: expiresAt})
	return result.Value, true
}

func (s *Swapper) store(key string, entry cacheEntry) {
	s.mu.Lock()
	s.cache[key] = entry
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
