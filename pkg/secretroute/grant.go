package secretroute

import (
	"sync"
	"time"
)

// Grant is one binding's say over a credential (ADR 26-11-03-329 §3): the
// route lists a request carrying it must pass, every one — the secret's, the
// binding's, its parent's — and when the binding stops authorizing. A
// credential held under several bindings, its plain one and its additions,
// has a grant for each, and a request passes when some grant passes it.
// Grants never merge into one list: a blocked route refuses only within its
// own grant's lists (ADR 26-10-27-880 §4).
type Grant struct {
	Lists [][]Route `json:"lists"`
	// ExpiresAt is when the grant lapses, the earlier of its binding's and
	// that binding's parent's; nil lasts until a later delivery removes it.
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	// Adds is the control plane's own mark that the grant is an addition's
	// (ADR 26-11-03-329 §1), so what it sends a pool that predates grants
	// leaves it out. It never travels: a pool decides no differently by it.
	Adds bool `json:"-"`
}

// Live reports whether the grant stands at now.
func (g Grant) Live(now time.Time) bool {
	return g.ExpiresAt == nil || now.Before(*g.ExpiresAt)
}

// DecideGrants holds req to grants at now: allowed when some live grant's
// lists all allow it (Decide). A lapsed grant allows nothing, by the caller's
// clock, whether or not a delivery has removed it yet. Refused, the reason is
// the first live grant's, which is the plain binding's when there is one: a
// request no grant allows is told why the credential's standing access
// refused it. Req's body is read at most once across every grant.
func DecideGrants(grants []Grant, now time.Time, req Request) (allowed bool, reason string) {
	req.Body = onceBody(req.Body)
	first := ""
	for _, grant := range grants {
		if !grant.Live(now) {
			continue
		}
		ok, why := Decide(grant.Lists, req)
		if ok {
			return true, ""
		}
		if first == "" {
			first = why
		}
	}
	if first == "" {
		if len(grants) == 0 {
			return false, "this credential has no routes"
		}
		return false, "this credential's grants have expired"
	}
	return false, first
}

// GrantsAllowingHost are the grants live at now that allow some request to
// host (HostAllowed over each one's lists): the check a value is handed out
// by. None is a refusal; the latest of their expiries is how long a value
// handed out for host may be held (LatestExpiry).
func GrantsAllowingHost(grants []Grant, now time.Time, host string) []Grant {
	var out []Grant
	for _, grant := range grants {
		if grant.Live(now) && HostAllowed(grant.Lists, host) {
			out = append(out, grant)
		}
	}
	return out
}

// LatestExpiry is the latest expiry among grants, nil when one of them has
// none or there are none.
func LatestExpiry(grants []Grant) *time.Time {
	var latest *time.Time
	for _, grant := range grants {
		if grant.ExpiresAt == nil {
			return nil
		}
		if latest == nil || grant.ExpiresAt.After(*latest) {
			latest = grant.ExpiresAt
		}
	}
	return latest
}

// onceBody makes body safe to call from each grant's Decide: it is called at
// most once, and every call after returns what the first did.
func onceBody(body func() ([]byte, bool)) func() ([]byte, bool) {
	if body == nil {
		return nil
	}
	var (
		once sync.Once
		data []byte
		ok   bool
	)
	return func() ([]byte, bool) {
		once.Do(func() { data, ok = body() })
		return data, ok
	}
}
