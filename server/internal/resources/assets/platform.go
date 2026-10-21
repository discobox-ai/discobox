package assets

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// platformPins is the platform bundle the release pins, as the digests its
// reference has resolved to while this server runs.
//
// The release pins the platform by reference — a release tag, or a
// development build's :local — and nothing records the digest a box was
// given, so a digest is never dropped once answered: a pool part way through
// fetching the platform it was handed keeps being answered it after the tag
// moves. The set is this process's: a box runs the platform its server ships
// from its next start (ADR 26-10-16-423 §7), which is the digest a restarted
// server resolves.
type platformPins struct {
	ref     string
	resolve func(ctx context.Context, ref string) (string, error)
	now     func() time.Time

	mu       sync.Mutex
	digests  []string
	latest   string
	tried    time.Time
	lastErr  error
	inFlight chan struct{}
}

// platformRetry is how long a resolution is taken as current before the
// reference is asked again — long enough that a pool's reads do not each ask
// a registry, short enough that a rebuilt development platform is answered
// soon after it is built — and platformRetryFailed how long a failure is.
const (
	platformRetry       = time.Minute
	platformRetryFailed = 10 * time.Second
)

// pins answers every digest resolved so far, resolving afresh when the last
// attempt is old, or when fresh is set. A resolution that fails leaves the
// digests already resolved in place, and is the error only while there are
// none.
func (p *platformPins) pins(ctx context.Context, fresh bool) ([]string, error) {
	if p == nil || p.ref == "" {
		return nil, nil
	}
	if _, digest, ok := strings.Cut(p.ref, "@"); ok && digestPattern.MatchString(digest) {
		return []string{digest}, nil
	}
	p.mu.Lock()
	retry := platformRetry
	if p.lastErr != nil {
		retry = platformRetryFailed
	}
	stale := fresh || p.tried.IsZero() || p.now().Sub(p.tried) >= retry
	if stale && p.inFlight == nil {
		p.inFlight = make(chan struct{})
		done := p.inFlight
		p.mu.Unlock()
		// Detached from this read, whose answer others are waiting on.
		resolveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fetchTimeout)
		digest, err := p.resolve(resolveCtx, p.ref)
		cancel()
		p.mu.Lock()
		p.tried, p.lastErr, p.inFlight = p.now(), err, nil
		if err == nil {
			p.latest = digest
			if !slices.Contains(p.digests, digest) {
				p.digests = append(p.digests, digest)
			}
		}
		close(done)
	} else if stale {
		// Another read is resolving: its answer is this one's.
		done := p.inFlight
		p.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		p.mu.Lock()
	}
	defer p.mu.Unlock()
	if len(p.digests) == 0 && p.lastErr != nil {
		return nil, fmt.Errorf("resolve the platform bundle %s: %w", p.ref, p.lastErr)
	}
	return slices.Clone(p.digests), nil
}

// newest is what the reference resolves to now, which a name resolution
// answers: resolved afresh, and an error when that fails.
func (p *platformPins) newest(ctx context.Context) (string, error) {
	if _, err := p.pins(ctx, true); err != nil {
		return "", err
	}
	if _, digest, ok := strings.Cut(p.ref, "@"); ok && digestPattern.MatchString(digest) {
		return digest, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lastErr != nil {
		return "", fmt.Errorf("resolve the platform bundle %s: %w", p.ref, p.lastErr)
	}
	return p.latest, nil
}
