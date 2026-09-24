package sandboxes

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/auth"
	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
)

func TestAuthorizeRequestedScopesRequiresHeldScopes(t *testing.T) {
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{
		Type:   auth.PrincipalTypeUser,
		UserID: "user-1",
		Scopes: []string{
			poolagentauth.ScopeSandboxRead,
		},
	})

	if err := authorizeRequestedScopes(ctx, []string{poolagentauth.ScopeSandboxRead}); err != nil {
		t.Fatalf("authorize read scope: %v", err)
	}

	err := authorizeRequestedScopes(ctx, []string{poolagentauth.ScopeSandboxWrite})
	if err == nil {
		t.Fatal("authorize write scope succeeded, want failure")
	}
	var statusErr interface{ StatusCode() int }
	if !errors.As(err, &statusErr) || statusErr.StatusCode() != http.StatusForbidden {
		t.Fatalf("authorize write scope error = %v, want forbidden status", err)
	}
}

func TestAuthorizeRequestedScopesAllowsAllScope(t *testing.T) {
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{
		Type:   auth.PrincipalTypeUser,
		UserID: "user-1",
		Scopes: []string{
			auth.ScopeAll,
		},
	})

	if err := authorizeRequestedScopes(ctx, []string{poolagentauth.ScopeSandboxRead, poolagentauth.ScopeSandboxWrite, poolagentauth.ScopeSandboxHTTP, poolagentauth.ScopeTerminalRead, poolagentauth.ScopeTerminalWrite, poolagentauth.ScopeExecRead, poolagentauth.ScopeExecWrite}); err != nil {
		t.Fatalf("authorize all scopes: %v", err)
	}
}

// A sandbox holds no scopes: what reaches a sandbox from one was decided by
// the sandbox role, which admits only a push into the origin of a discobox it
// created (ADR 0149 §2).
func TestAuthorizeRequestedScopesLeavesASandboxToItsRole(t *testing.T) {
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{
		Type: auth.PrincipalTypeSandbox, SandboxID: "sbx-lead", ProjectID: "proj-1", UserID: "user-1",
	})
	if err := authorizeRequestedScopes(ctx, []string{poolagentauth.ScopeSandboxWrite}); err != nil {
		t.Fatalf("authorize a sandbox's push: %v", err)
	}
}
