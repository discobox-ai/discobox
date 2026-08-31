// Package hostscope answers one question, the same way everywhere it is asked:
// does this scope cover this destination?
//
// A credential's authorization is written as a host — the host a grant is
// limited to, the host a secret belongs to, the host an approved use named —
// and three separate places compare one of those against the destination the
// proxy actually observed: the control plane's grant lookup, the pool agent's
// activation check, and the check that a grant may not point a secret at a
// host it does not belong to. They must agree. A rule that is subtly different
// in one of them is either a credential that stops working for no visible
// reason or one that travels somewhere nobody approved.
package hostscope

import (
	"net"
	"strings"
)

// Normalize is the one reading of a host string: lowercased, trimmed, and
// without a port. The proxy reports the destination this way, so anything
// stored differently is a scope that can never match.
func Normalize(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if trimmed, _, err := net.SplitHostPort(host); err == nil {
		return trimmed
	}
	return host
}

// Covers reports whether scope authorizes traffic to host.
//
// A scope covers itself and anything beneath it: github.com covers
// api.github.com and uploads.github.com. It does not cover its parent —
// api.github.com is not authority over github.com, which is a different host
// serving different things — so the relation is deliberately one-way.
//
// An empty scope covers everything. That is the wildcard grant, which stays an
// explicit administrative act; nothing in the agent credentials flow can
// produce one.
func Covers(scope, host string) bool {
	scope, host = Normalize(scope), Normalize(host)
	if scope == "" {
		return true
	}
	if host == "" {
		// A destination nothing named cannot be shown to be inside a scope,
		// and a check that cannot be made fails closed.
		return false
	}
	if scope == host {
		return true
	}
	return strings.HasSuffix(host, "."+scope)
}

// Specificity ranks how closely a scope answers for a host, smallest first:
// the host itself, then a parent of it, then the wildcard. It is what picks
// between two grants that both cover a destination — the narrower one is the
// one whose author was thinking about this host.
func Specificity(scope, host string) int {
	scope, host = Normalize(scope), Normalize(host)
	switch {
	case scope != "" && scope == host:
		return 0
	case scope != "" && Covers(scope, host):
		return 1
	default:
		return 2
	}
}

// TooBroad reports whether a scope is so wide that it cannot have been meant
// as one: a single label, like "com" or "internal", covers every host under a
// public suffix. It is a guard on what a person may type, not a security
// boundary — the wildcard scope is the honest way to say "anywhere", and it is
// already an explicit act.
func TooBroad(scope string) bool {
	scope = Normalize(scope)
	return scope != "" && !strings.Contains(scope, ".")
}

// CommonParent is the scope that would cover both hosts: the deeper of the two
// when one is already beneath the other, and the shared suffix when they are
// siblings. It is empty when they share nothing but a public suffix, which is
// not a scope anybody should be offered.
//
// It exists so a refusal can name the binding that would have worked —
// api.github.com and github.com share github.com — rather than only saying
// which one did not.
func CommonParent(a, b string) string {
	a, b = Normalize(a), Normalize(b)
	if a == "" || b == "" {
		return ""
	}
	if Covers(a, b) {
		return a
	}
	if Covers(b, a) {
		return b
	}
	// Siblings: walk the shared labels back from the end. api.github.com and
	// uploads.github.com give github.com.
	al, bl := strings.Split(a, "."), strings.Split(b, ".")
	var shared []string
	for i, j := len(al)-1, len(bl)-1; i >= 0 && j >= 0 && al[i] == bl[j]; i, j = i-1, j-1 {
		shared = append([]string{al[i]}, shared...)
	}
	if len(shared) < 2 {
		// One label is a public suffix, not a site.
		return ""
	}
	return strings.Join(shared, ".")
}
