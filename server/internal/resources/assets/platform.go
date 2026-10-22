package assets

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/pkg/imagecache"
)

// platformPins is the platform bundle the release pins, as the digests its
// reference has resolved to while this server runs.
//
// The release names the platform by reference — a release tag, or a
// development build's :local — and a digest is never dropped once answered:
// a pool part way through fetching the platform it was handed keeps being
// answered it after the tag moves. What a box was given lasts longer than
// this process, as its sandbox's pin (ADR 26-10-16-423 §7), which the scope
// reads beside these.
type platformPins struct {
	ref     string
	resolve func(ctx context.Context, ref string) (string, error)
	now     func() time.Time
	// moved is told, off the resolving read's path, each digest the
	// reference resolves to that differs from the one before, the first
	// included: sandboxes are pinned to the platform, and a stopped one
	// follows it (ADR 26-10-16-423 §7).
	moved func(digest string)

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
	if digest := p.fixed(); digest != "" {
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
		if err == nil && digest != p.latest && p.moved != nil {
			go p.moved(digest)
		}
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

// fixed is the digest a reference pinned by digest names, empty for one
// that names a tag.
func (p *platformPins) fixed() string {
	if _, digest, ok := strings.Cut(p.ref, "@"); ok && imagecache.IsDigest(digest) {
		return digest
	}
	return ""
}

// current is what the reference last resolved to, without asking again.
func (p *platformPins) current() string {
	if p == nil || p.ref == "" {
		return ""
	}
	if digest := p.fixed(); digest != "" {
		return digest
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.latest
}

// newest is what the reference resolves to now, which a name resolution
// answers: resolved afresh, and an error when that fails.
func (p *platformPins) newest(ctx context.Context) (string, error) {
	if _, err := p.pins(ctx, true); err != nil {
		return "", err
	}
	if digest := p.fixed(); digest != "" {
		return digest, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lastErr != nil {
		return "", fmt.Errorf("resolve the platform bundle %s: %w", p.ref, p.lastErr)
	}
	return p.latest, nil
}
