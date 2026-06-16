package authctx

import (
	"context"
	"fmt"
	"strings"
)

type principalKey struct{}

// Principal identifies the authenticated caller.
type Principal struct {
	Type     string
	UserID   string
	WorkerID string
}

const (
	PrincipalTypeUser   = "user"
	PrincipalTypeWorker = "worker"
)

// WithPrincipal returns a context carrying the authenticated principal.
func WithPrincipal(ctx context.Context, principal Principal) context.Context {
	principal.Type = strings.TrimSpace(principal.Type)
	principal.UserID = strings.TrimSpace(principal.UserID)
	principal.WorkerID = strings.TrimSpace(principal.WorkerID)
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
