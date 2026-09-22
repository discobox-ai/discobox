package auth

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

type principalKey struct{}

// Principal identifies the authenticated caller.
type Principal struct {
	Type   string
	UserID string
	PoolID string
	// SandboxID and ProjectID name the sandbox a sandbox principal is, and the
	// project it may act in (ADR 0140 §3). UserID on a sandbox principal is the
	// user who created the sandbox, whose authority what it creates carries.
	SandboxID string
	ProjectID string
	Scopes    []string
}

const (
	PrincipalTypeUser = "user"
	PrincipalTypePool = "pool"
	// PrincipalTypeSandbox is a sandbox's own call, forwarded by the pool that
	// hosts it. It holds the sandbox role and nothing else.
	PrincipalTypeSandbox = "sandbox"

	ScopeAll = "*"
)

// WithPrincipal returns a context carrying the authenticated principal.
func WithPrincipal(ctx context.Context, principal Principal) context.Context {
	principal.Type = strings.TrimSpace(principal.Type)
	principal.UserID = strings.TrimSpace(principal.UserID)
	principal.PoolID = strings.TrimSpace(principal.PoolID)
	principal.SandboxID = strings.TrimSpace(principal.SandboxID)
	principal.ProjectID = strings.TrimSpace(principal.ProjectID)
	principal.Scopes = normalizeScopes(principal.Scopes)
	return context.WithValue(ctx, principalKey{}, principal)
}

// PrincipalFromContext returns the authenticated principal carried by ctx.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	if ctx == nil {
		return Principal{}, false
	}
	principal, ok := ctx.Value(principalKey{}).(Principal)
	return principal, ok
}

// UserID returns the authenticated user ID carried by ctx.
func UserID(ctx context.Context) (string, error) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok || principal.Type != PrincipalTypeUser || principal.UserID == "" {
		return "", fmt.Errorf("authenticated user is required")
	}
	return principal.UserID, nil
}

// ActingUserID is the user a call acts as: a user's own ID, or for a sandbox
// the user who created it. What a sandbox creates is created as that user
// (ADR 0140 §4); the sandbox is recorded as the one that did it.
func ActingUserID(ctx context.Context) (string, error) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok || principal.UserID == "" || (principal.Type != PrincipalTypeUser && principal.Type != PrincipalTypeSandbox) {
		return "", fmt.Errorf("authenticated user or sandbox is required")
	}
	return principal.UserID, nil
}

// HasScope reports whether the principal carries the requested authorization
// scope. ScopeAll grants every scope, and suffix wildcards grant one prefix.
func (p Principal) HasScope(scope string) bool {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return true
	}
	for _, candidate := range p.Scopes {
		switch candidate {
		case ScopeAll, scope:
			return true
		default:
			prefix, ok := strings.CutSuffix(candidate, ":*")
			if ok && strings.HasPrefix(scope, prefix+":") {
				return true
			}
		}
	}
	return false
}

func normalizeScopes(scopes []string) []string {
	out := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		scope = strings.TrimSpace(scope)
		if scope == "" || slices.Contains(out, scope) {
			continue
		}
		out = append(out, scope)
	}
	return out
}
