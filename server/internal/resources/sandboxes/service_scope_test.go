package sandboxes

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/obot-platform/discobox/server/internal/auth"
	workeragentauth "github.com/obot-platform/discobox/server/internal/auth/workeragent"
)

func TestAuthorizeRequestedScopesRequiresHeldScopes(t *testing.T) {
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{
		Type:   auth.PrincipalTypeUser,
		UserID: "user-1",
		Scopes: []string{
			workeragentauth.ScopeSandboxRead,
		},
	})

	if err := authorizeRequestedScopes(ctx, []string{workeragentauth.ScopeSandboxRead}); err != nil {
		t.Fatalf("authorize read scope: %v", err)
	}

	err := authorizeRequestedScopes(ctx, []string{workeragentauth.ScopeSandboxWrite})
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

	if err := authorizeRequestedScopes(ctx, []string{workeragentauth.ScopeSandboxRead, workeragentauth.ScopeSandboxWrite, workeragentauth.ScopeSandboxHTTP}); err != nil {
		t.Fatalf("authorize all scopes: %v", err)
	}
}
