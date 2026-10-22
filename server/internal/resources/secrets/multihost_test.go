package secrets_test

import (
	"slices"
	"strings"
	"testing"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/pkg/wellknown"
	"github.com/discobox-ai/discobox/server/internal/model"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// copilotHosts are the two sites Copilot CLI sends one GitHub token to, which
// share no parent short of a public suffix (ADR 26-10-02-393).
var copilotHosts = []string{"api.github.com", "api.githubcopilot.com"}

// One ask names both sites, one approval grants both, and one use is spent at
// either — and nowhere else.
func TestOneRequestAndGrantNameSeveralHosts(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)

	req, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID, Name: "github", EnvVar: "GH_TOKEN",
		Hosts:  []string{"API.GitHub.com", "api.githubcopilot.com", "api.github.com"},
		Uses:   []apimodel.SecretUse{{Description: "run copilot against org/repo"}},
		Policy: policyAsked(t),
	})
	if err != nil {
		t.Fatalf("create credential request: %v", err)
	}
	if !slices.Equal(req.Hosts, copilotHosts) {
		t.Fatalf("request hosts = %q, want %q: normalized and deduplicated, in the order given", req.Hosts, copilotHosts)
	}

	approved, err := approveAsAsked(ctx, t, svc, "project-1", req.ID, services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(secret.ID)})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	grant, err := st.GetSecretGrant(ctx, "project-1", approved.GrantID)
	if err != nil {
		t.Fatalf("get grant: %v", err)
	}
	if !slices.Equal(grant.Hosts, copilotHosts) {
		t.Fatalf("grant hosts = %q, want the hosts the request named", grant.Hosts)
	}

	scopes := []store.GrantScope{{Scope: model.SecretGrantScopeSandbox, ScopeKey: testSandboxID}}
	useID := grant.Uses[0].UseID
	for _, host := range []string{"api.github.com", "api.githubcopilot.com"} {
		if _, err := st.FindLiveGrant(ctx, "project-1", secret.ID, host, scopes); err != nil {
			t.Errorf("no live grant resolves at %s: %v", host, err)
		}
		use, err := svc.ApprovedUse(ctx, testPoolID, testSandboxID, useID, host)
		if err != nil {
			t.Errorf("ApprovedUse at %s: %v", host, err)
			continue
		}
		// The judge is told where this request is approved for: the grant's
		// host that covers it, not the list.
		if use.Host != host {
			t.Errorf("ApprovedUse at %s names %q", host, use.Host)
		}
	}
	if _, err := st.FindLiveGrant(ctx, "project-1", secret.ID, "github.com", scopes); err == nil {
		t.Error("a grant for api.github.com resolved at its parent")
	}
	if _, err := svc.ApprovedUse(ctx, testPoolID, testSandboxID, useID, "evil.example.com"); err == nil {
		t.Error("ApprovedUse named a use at a host the grant does not cover")
	}
}

// Approving either of two asks for the same hosts in another order grants the
// same thing, so the second is the first (ADR 26-10-02-393 §3); an ask for a
// different set is its own.
func TestARetryNamingTheSameHostsInAnotherOrderIsTheSameRequest(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	ask := func(hosts ...string) *model.SecretRequest {
		t.Helper()
		req, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
			SandboxId: testSandboxID, Name: "github", EnvVar: "GH_TOKEN", Hosts: hosts,
			Uses:   []apimodel.SecretUse{{Description: "run copilot against org/repo"}},
			Policy: policyAsked(t),
		})
		if err != nil {
			t.Fatalf("create credential request: %v", err)
		}
		return req
	}
	first := ask("api.github.com", "api.githubcopilot.com")
	if again := ask("api.githubcopilot.com", "api.github.com"); again.ID != first.ID {
		t.Fatalf("the same hosts in another order opened %s beside %s", again.ID, first.ID)
	}
	if narrower := ask("api.github.com"); narrower.ID == first.ID {
		t.Fatal("an ask for one of the hosts was folded into the ask for both")
	}
}

// A grant's hosts must each sit inside the secret's binding. A GitHub secret
// bound to github.com cannot be granted for Copilot's host too, and the
// refusal names the host it cannot cover.
func TestEveryHostOfAGrantMustSitInsideTheSecretsBinding(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	bound := createBoundSecret(ctx, t, svc, "github", "github.com", 0)

	_, err := svc.CreateSecretGrant(ctx, "project-1", services.CreateSecretGrantBody{
		SecretId: bound.ID,
		Scope:    serverapi.CreateSecretGrantBodyScopeProject,
		Hosts:    copilotHosts,
	})
	if err == nil || !strings.Contains(err.Error(), "api.githubcopilot.com") {
		t.Fatalf("err = %v, want the grant refused at the host the binding does not cover", err)
	}

	unbound := createBoundSecret(ctx, t, svc, "github-anywhere", "", 0)
	grant, err := svc.CreateSecretGrant(ctx, "project-1", services.CreateSecretGrantBody{
		SecretId: unbound.ID,
		Scope:    serverapi.CreateSecretGrantBodyScopeProject,
		Hosts:    []string{"api.github.com", "api.githubcopilot.com"},
	})
	if err != nil {
		t.Fatalf("a secret bound to nothing refused a grant for two hosts: %v", err)
	}
	if !slices.Equal(grant.Hosts, copilotHosts) {
		t.Fatalf("grant hosts = %q, want %q", grant.Hosts, copilotHosts)
	}

	// An empty list is how a grant asks for every host; leaving it out takes
	// the secret's own.
	wildcard, err := svc.CreateSecretGrant(ctx, "project-1", services.CreateSecretGrantBody{
		SecretId: unbound.ID,
		Scope:    serverapi.CreateSecretGrantBodyScopeProject,
		Hosts:    []string{},
	})
	if err != nil || len(wildcard.Hosts) != 0 {
		t.Fatalf("wildcard grant = %v, %v; want one with no hosts", wildcard, err)
	}
}

// com.github.api may be sent to Copilot's site, but an ask by the ID that
// names no host is for GitHub alone: listing a place a credential may go does
// not widen every ask to it (ADR 26-10-02-393 §5).
func TestAWellKnownAskNamesItsFirstHostUnlessItNamesMore(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	ask := func(hosts ...string) (*model.SecretRequest, error) {
		return svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
			SandboxId: testSandboxID, ID: serverapi.NewOptString(wellknown.GitHubAPI), Hosts: hosts,
			Uses:   []apimodel.SecretUse{{Description: "run copilot against org/repo"}},
			Policy: policyAsked(t),
		})
	}
	plain, err := ask()
	if err != nil {
		t.Fatalf("ask by ID: %v", err)
	}
	if !slices.Equal(plain.Hosts, []string{"github.com"}) {
		t.Fatalf("an ask by ID alone named %q, want github.com alone", plain.Hosts)
	}
	both, err := ask(copilotHosts...)
	if err != nil {
		t.Fatalf("ask by ID for GitHub and Copilot: %v", err)
	}
	if !slices.Equal(both.Hosts, copilotHosts) {
		t.Fatalf("hosts = %q, want %q", both.Hosts, copilotHosts)
	}
	if _, err := ask("api.github.com", "gitlab.com"); err == nil || !strings.Contains(err.Error(), "gitlab.com") {
		t.Fatalf("err = %v, want the host the ID is not sent to named", err)
	}
}
