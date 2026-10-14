package sandboxes

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/auth"
	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	services "github.com/discobox-ai/discobox/server/internal/services"
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

// A sandbox holds no scopes. The calls of its the sandbox role lets reach a
// sandbox are a push into the origin of a discobox in its project (ADR
// 26-09-24-630 §2), its terminals and services (ADR 26-10-01-397 §1, ADR
// 26-10-14-320 §3), and HTTP to one of its ports (ADR 26-10-14-320 §3), so
// those scopes are all it is allowed here: a route admitted by mistake still
// reaches no legacy terminal or tunnel.
func TestAuthorizeRequestedScopesAllowsASandboxOnlyAPushItsTerminalsAndHTTP(t *testing.T) {
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{
		Type: auth.PrincipalTypeSandbox, SandboxID: "sbx-lead", ProjectID: "proj-1", UserID: "user-1",
	})
	for _, scope := range []string{poolagentauth.ScopeSandboxWrite, poolagentauth.ScopeExecRead, poolagentauth.ScopeExecWrite, poolagentauth.ScopeSandboxHTTP} {
		if err := authorizeRequestedScopes(ctx, []string{scope}); err != nil {
			t.Fatalf("authorize a sandbox for %s: %v", scope, err)
		}
	}
	for _, scope := range []string{poolagentauth.ScopeSandboxRead, poolagentauth.ScopeTerminalRead,
		poolagentauth.ScopeTerminalWrite, poolagentauth.ScopeTCPConnect, poolagentauth.ScopeUDPConnect} {
		err := authorizeRequestedScopes(ctx, []string{scope})
		var statusErr interface{ StatusCode() int }
		if !errors.As(err, &statusErr) || statusErr.StatusCode() != http.StatusForbidden {
			t.Fatalf("authorize a sandbox for %s: err = %v, want forbidden", scope, err)
		}
	}
}

// A discobox never attaches to another (ADR 26-10-01-397 §2): an acquire for an
// attach refuses it before asking anything of the sandbox, whatever scopes it
// names.
func TestAcquireForAnAttachRefusesASandbox(t *testing.T) {
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{
		Type: auth.PrincipalTypeSandbox, SandboxID: "sbx-lead", ProjectID: "proj-1", UserID: "user-1",
	})
	attach := services.SandboxAction("exec attach")
	attach.Attach = true
	_, _, err := (&Service{}).AcquireSandboxHTTPClient(ctx, "proj-1", "sbx-worker", []string{poolagentauth.ScopeExecWrite, poolagentauth.ScopeExecRead}, attach)
	var statusErr interface{ StatusCode() int }
	if !errors.As(err, &statusErr) || statusErr.StatusCode() != http.StatusForbidden {
		t.Fatalf("attach by a sandbox: err = %v, want forbidden", err)
	}
}
