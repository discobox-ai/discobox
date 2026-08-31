package secrets_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	resourcesecrets "github.com/discobox-ai/discobox/server/internal/resources/secrets"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

func TestCreateSecretRequestAllowsNoMatchingSecret(t *testing.T) {
	ctx := testPrincipalContext()
	svc := newTestService(t)

	req, err := svc.CreateSecretRequest(ctx, "project-1", services.CreateSecretRequestBody{
		Type: serverapi.CreateSecretRequestBodyTypeToken,
		Host: serverapi.NewOptString("github.com"),
	})
	if err != nil {
		t.Fatalf("create secret request: %v", err)
	}
	if req.Status != model.SecretRequestStatusPending {
		t.Fatalf("status = %q, want %q", req.Status, model.SecretRequestStatusPending)
	}
	if req.SecretID != "" {
		t.Fatalf("secret id = %q, want empty", req.SecretID)
	}
}

func TestApproveSecretRequestUsesSelectedSecretID(t *testing.T) {
	ctx := testPrincipalContext()
	svc := newTestService(t)

	selected, err := svc.CreateSecret(ctx, "project-1", services.CreateSecretBody{
		Name: "selected bearer token",
		Type: serverapi.CreateSecretBodyTypeToken,
		Value: serverapi.SecretValue{
			Token: serverapi.NewOptString("selected-token"),
		},
	})
	if err != nil {
		t.Fatalf("create selected secret: %v", err)
	}

	req, err := svc.CreateSecretRequest(ctx, "project-1", services.CreateSecretRequestBody{
		Type: serverapi.CreateSecretRequestBodyTypeToken,
		Host: serverapi.NewOptString("github.com"),
	})
	if err != nil {
		t.Fatalf("create advisory request: %v", err)
	}

	approved, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{
		SecretId: selected.ID,
	})
	if err != nil {
		t.Fatalf("approve secret request: %v", err)
	}
	if approved.Status != model.SecretRequestStatusApproved {
		t.Fatalf("status = %q, want %q", approved.Status, model.SecretRequestStatusApproved)
	}
	if approved.SecretID != selected.ID {
		t.Fatalf("secret id = %q, want %q", approved.SecretID, selected.ID)
	}
	if approved.GrantID == "" {
		t.Fatal("approved request should reference the minted grant")
	}

	// Approval mints a standing grant. A non-sandbox request defaults to project scope.
	grants, err := svc.ListSecretGrants(ctx, "project-1", selected.ID)
	if err != nil {
		t.Fatalf("list grants: %v", err)
	}
	if len(grants) != 1 {
		t.Fatalf("grants = %d, want 1", len(grants))
	}
	if grants[0].ID != approved.GrantID {
		t.Fatalf("grant id = %q, want %q", grants[0].ID, approved.GrantID)
	}
	if grants[0].Scope != model.SecretGrantScopeProject || grants[0].ScopeKey != "project-1" {
		t.Fatalf("grant scope = %q/%q, want project/project-1", grants[0].Scope, grants[0].ScopeKey)
	}
}

func newTestService(t *testing.T) *resourcesecrets.Service {
	t.Helper()

	ctx := context.Background()
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	if err := db.Write.WithContext(ctx).Create(&model.Project{
		ID:          "project-1",
		OwnerUserID: "user-1",
		Name:        "Project",
	}).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}

	return resourcesecrets.NewService(store.New(db.Write, db.Read))
}

func testPrincipalContext() context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{
		Type:   auth.PrincipalTypeUser,
		UserID: "user-1",
	})
}

// The grant's host is what the proxy enforces, and the secret's host says which
// service the credential is for. When they disagree the grant would swap a real
// credential into requests to another service — with the agent credentials flow
// the host is proposed by the sandbox, so an approval typo is an exfiltration
// primitive. mintGrant is the one place every grant passes through.
func TestGrantRefusesASecretBoundToAnotherHost(t *testing.T) {
	ctx := testPrincipalContext()
	svc := newTestService(t)

	openai, err := svc.CreateSecret(ctx, "project-1", services.CreateSecretBody{
		Name:  "OpenAI key",
		Type:  serverapi.CreateSecretBodyTypeToken,
		Host:  serverapi.NewOptString("api.openai.com"),
		Value: serverapi.SecretValue{Token: serverapi.NewOptString("sk-realrealrealrealrealreal")},
	})
	if err != nil {
		t.Fatalf("create secret: %v", err)
	}

	_, err = svc.CreateSecretGrant(ctx, "project-1", services.CreateSecretGrantBody{
		SecretId: openai.ID,
		Scope:    serverapi.CreateSecretGrantBodyScopeProject,
		Host:     serverapi.NewOptString("api.github.com"),
	})
	if err == nil {
		t.Fatal("granted an OpenAI-bound secret for GitHub; the real key would be swapped into GitHub requests")
	}
	if !strings.Contains(err.Error(), "api.openai.com") || !strings.Contains(err.Error(), "api.github.com") {
		t.Fatalf("error = %v, want it to name both hosts so the approver can see the mismatch", err)
	}
	// Unrelated hosts share no site, so the only remedy is releasing the
	// binding, and the message says exactly that command.
	if !strings.Contains(err.Error(), `discobox secret update`) || !strings.Contains(err.Error(), `--host ""`) {
		t.Fatalf("error = %v, want it to name the command that fixes it", err)
	}

	// A wildcard grant on a host-bound secret is the same hazard, widened.
	if _, err := svc.CreateSecretGrant(ctx, "project-1", services.CreateSecretGrantBody{
		SecretId: openai.ID,
		Scope:    serverapi.CreateSecretGrantBodyScopeProject,
		Host:     serverapi.NewOptString(""),
	}); err == nil {
		t.Fatal("granted a host-bound secret for every host")
	}

	// The host it is bound to still works, and case is not what decides it.
	if _, err := svc.CreateSecretGrant(ctx, "project-1", services.CreateSecretGrantBody{
		SecretId: openai.ID,
		Scope:    serverapi.CreateSecretGrantBodyScopeProject,
		Host:     serverapi.NewOptString("API.OpenAI.com"),
	}); err != nil {
		t.Fatalf("grant for the secret's own host refused: %v", err)
	}
}

// A secret with no host is unconstrained on purpose: the field is often
// inferred from the token's shape, and a credential used against several hosts
// says so by leaving it empty.
func TestGrantAllowsAnyHostForAnUnboundSecret(t *testing.T) {
	ctx := testPrincipalContext()
	svc := newTestService(t)

	secret, err := svc.CreateSecret(ctx, "project-1", services.CreateSecretBody{
		Name:  "multi-host token",
		Type:  serverapi.CreateSecretBodyTypeToken,
		Value: serverapi.SecretValue{Token: serverapi.NewOptString("opaque-token-with-no-provider-shape")},
	})
	if err != nil {
		t.Fatalf("create secret: %v", err)
	}
	if secret.Host != "" {
		t.Fatalf("fixture secret inferred host %q; this test needs an unbound one", secret.Host)
	}
	for _, host := range []string{"api.github.com", "uploads.github.com", ""} {
		if _, err := svc.CreateSecretGrant(ctx, "project-1", services.CreateSecretGrantBody{
			SecretId: secret.ID,
			Scope:    serverapi.CreateSecretGrantBodyScopeProject,
			Host:     serverapi.NewOptString(host),
		}); err != nil {
			t.Fatalf("grant for %q refused: %v", host, err)
		}
	}
}

// A scope covers what is beneath it, so a secret for the site answers a
// request for its API — the case that used to leave the only sensible secret
// unusable, because a ghp_ token's host was inferred as api.github.com while
// the agent asked for github.com.
func TestGrantMayNarrowASecretToASubdomain(t *testing.T) {
	ctx := testPrincipalContext()
	svc := newTestService(t)

	// Bound by a person, which is the only way a secret gets a host: nothing
	// infers one from the token's shape.
	site, err := svc.CreateSecret(ctx, "project-1", services.CreateSecretBody{
		Name:  "gh",
		Type:  serverapi.CreateSecretBodyTypeToken,
		Host:  serverapi.NewOptString("github.com"),
		Value: serverapi.SecretValue{Token: serverapi.NewOptString("ghp_realrealrealrealrealrealrealreal12")},
	})
	if err != nil {
		t.Fatalf("create secret: %v", err)
	}

	grant, err := svc.CreateSecretGrant(ctx, "project-1", services.CreateSecretGrantBody{
		SecretId: site.ID,
		Scope:    serverapi.CreateSecretGrantBodyScopeProject,
		Host:     serverapi.NewOptString("api.github.com"),
	})
	if err != nil {
		t.Fatalf("a grant narrower than the binding was refused: %v", err)
	}
	if grant.Host != "api.github.com" {
		t.Fatalf("grant host = %q", grant.Host)
	}

	// And the other way is still refused: the parent is a different host.
	api, err := svc.CreateSecret(ctx, "project-1", services.CreateSecretBody{
		Name:  "api only",
		Type:  serverapi.CreateSecretBodyTypeToken,
		Host:  serverapi.NewOptString("api.github.com"),
		Value: serverapi.SecretValue{Token: serverapi.NewOptString("opaque-value")},
	})
	if err != nil {
		t.Fatalf("create secret: %v", err)
	}
	if _, err := svc.CreateSecretGrant(ctx, "project-1", services.CreateSecretGrantBody{
		SecretId: api.ID,
		Scope:    serverapi.CreateSecretGrantBodyScopeProject,
		Host:     serverapi.NewOptString("github.com"),
	}); err == nil {
		t.Fatal("a secret bound to the API was granted for the whole site")
	}
}

// A secret bound to a host may be used for that host and the hosts beneath it,
// and nowhere else. The check is at the point the value is handed out, not only
// where a grant is minted: a grant written before the binding existed is
// exactly the one nobody would write today.
func TestABoundSecretResolvesOnlyWithinItsBinding(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)

	secret, err := svc.CreateSecret(ctx, "project-1", services.CreateSecretBody{
		Name:  "gh",
		Type:  serverapi.CreateSecretBodyTypeToken,
		Host:  serverapi.NewOptString("github.com"),
		Value: serverapi.SecretValue{Token: serverapi.NewOptString("ghp_realrealrealrealrealrealrealreal12")},
	})
	if err != nil {
		t.Fatalf("create secret: %v", err)
	}
	// A wildcard grant, the broadest thing an administrator can write.
	if _, err := svc.CreateSecretGrant(ctx, "project-1", services.CreateSecretGrantBody{
		SecretId: secret.ID,
		Scope:    serverapi.CreateSecretGrantBodyScopeSandbox,
		ScopeKey: serverapi.NewOptString(testSandboxID),
		Host:     serverapi.NewOptString("github.com"),
	}); err != nil {
		t.Fatalf("create grant: %v", err)
	}
	if err := st.CreateSandboxSecret(ctx, &model.SandboxSecret{
		ProjectID: "project-1", SandboxID: testSandboxID, SecretID: secret.ID,
		EnvName: "GH_TOKEN", Sentinel: "SENTINEL-GH",
	}); err != nil {
		t.Fatalf("bind sentinel: %v", err)
	}

	for _, tc := range []struct {
		host string
		want string
	}{
		{"github.com", model.SecretRequestStatusApproved},
		{"api.github.com", model.SecretRequestStatusApproved},
		{"uploads.github.com", model.SecretRequestStatusApproved},
		{"example.com", model.SecretRequestStatusDenied},
		{"notgithub.com", model.SecretRequestStatusDenied},
	} {
		res, err := svc.ResolveSandboxSecret(ctx, testPoolID, testSandboxID, "SENTINEL-GH", tc.host)
		if err != nil {
			t.Fatalf("resolve for %s: %v", tc.host, err)
		}
		if res.Status != tc.want {
			t.Fatalf("resolve for %s = %s, want %s", tc.host, res.Status, tc.want)
		}
	}
}

// When the two hosts are the same site, the binding that would have worked is
// the site — so the refusal names it, rather than leaving the reader to work
// out that github.com covers api.github.com.
func TestARefusalNamesTheBindingThatWouldWork(t *testing.T) {
	ctx := testPrincipalContext()
	svc := newTestService(t)

	secret, err := svc.CreateSecret(ctx, "project-1", services.CreateSecretBody{
		Name:  "gh",
		Type:  serverapi.CreateSecretBodyTypeToken,
		Host:  serverapi.NewOptString("api.github.com"),
		Value: serverapi.SecretValue{Token: serverapi.NewOptString("ghp_realrealrealrealrealrealrealreal12")},
	})
	if err != nil {
		t.Fatalf("create secret: %v", err)
	}
	_, err = svc.CreateSecretGrant(ctx, "project-1", services.CreateSecretGrantBody{
		SecretId: secret.ID,
		Scope:    serverapi.CreateSecretGrantBodyScopeProject,
		Host:     serverapi.NewOptString("github.com"),
	})
	if err == nil {
		t.Fatal("a secret bound to the API was granted for the whole site")
	}
	want := "--host github.com"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want it to name the binding that covers both (%q)", err, want)
	}
}

// A project holds as many secrets as it has names for. The uniqueness domain
// includes the name because nothing infers a host any more: without it, one
// unbound token was all a project could have, and a GitHub token and an OpenAI
// key could not coexist.
func TestSecretsWithDifferentNamesCoexistUnbound(t *testing.T) {
	ctx := testPrincipalContext()
	svc := newTestService(t)

	for _, name := range []string{"gh", "openai", "npm"} {
		if _, err := svc.CreateSecret(ctx, "project-1", services.CreateSecretBody{
			Name:  name,
			Type:  serverapi.CreateSecretBodyTypeToken,
			Value: serverapi.SecretValue{Token: serverapi.NewOptString("value-" + name)},
		}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}

	// The same name twice is the collision that remains, and it comes back as
	// something the caller can act on rather than a constraint violation.
	_, err := svc.CreateSecret(ctx, "project-1", services.CreateSecretBody{
		Name:  "gh",
		Type:  serverapi.CreateSecretBodyTypeToken,
		Value: serverapi.SecretValue{Token: serverapi.NewOptString("another")},
	})
	if err == nil {
		t.Fatal("two secrets took the same name")
	}
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != http.StatusConflict {
		t.Fatalf("error = %v, want a 409 the caller can read", err)
	}
	for _, want := range []string{`"gh"`, "with no host", "pick another name"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want it to carry %q", err, want)
		}
	}
}

// A pre-approval in the agent credentials shape: granted before anything asks,
// and readable only through the in-sandbox CLI. The binding it mints is the
// same one approving an agent's request mints — never injected into the
// sandbox, so nothing in the box can read the credential (ADR 0031 §4).
func TestAGrantWithUsesIsTakenOnlyThroughTheCLI(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)

	grant, err := svc.CreateSecretGrant(ctx, "project-1", services.CreateSecretGrantBody{
		SecretId: secret.ID,
		Scope:    serverapi.CreateSecretGrantBodyScopeSandbox,
		ScopeKey: serverapi.NewOptString(testSandboxID),
		Host:     serverapi.NewOptString("api.github.com"),
		EnvVar:   serverapi.NewOptString("GH_TOKEN"),
		Uses: serverapi.NewOptNilSecretUseArray([]serverapi.SecretUse{
			{Description: "Open a pull request against the current repo"},
		}),
	})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if len(grant.Uses) != 1 || !strings.HasPrefix(grant.Uses[0].UseID, "use_") {
		t.Fatalf("uses = %#v, want one carrying a minted ID", grant.Uses)
	}

	// The binding exists for the pool agent to translate to, and reaches the
	// sandbox through nothing.
	all, err := st.ListSandboxSecrets(ctx, "project-1", testSandboxID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 || !all[0].AgentRequested || all[0].EnvName != "GH_TOKEN" || all[0].Sentinel == "" {
		t.Fatalf("bindings = %#v, want one agent-requested binding", all)
	}
	injected, err := st.ListInjectedSandboxSecrets(ctx, "project-1", testSandboxID)
	if err != nil {
		t.Fatalf("list injected: %v", err)
	}
	if len(injected) != 0 {
		t.Fatalf("injected = %#v, want none: nothing in the sandbox may read it", injected)
	}

	// And it is what the CLI can take: the broker lists it with its uses.
	credentials, err := svc.ListSandboxCredentials(ctx, testPoolID, testSandboxID)
	if err != nil {
		t.Fatalf("list credentials: %v", err)
	}
	if len(credentials) != 1 || len(credentials[0].Grant.Uses) != 1 {
		t.Fatalf("credentials = %#v, want the granted use offered to the agent", credentials)
	}
	if credentials[0].Assignment.Sentinel != all[0].Sentinel {
		t.Fatal("the broker offers a different sentinel than the binding holds")
	}
}

// The obligations that shape carries, refused rather than silently relaxed.
func TestAGrantWithUsesRefusesTheWrongShape(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	use := serverapi.NewOptNilSecretUseArray([]serverapi.SecretUse{{Description: "Open a PR"}})

	for _, tc := range []struct {
		name string
		body services.CreateSecretGrantBody
		want string
	}{
		{"no host", services.CreateSecretGrantBody{
			SecretId: secret.ID, Scope: serverapi.CreateSecretGrantBodyScopeSandbox,
			ScopeKey: serverapi.NewOptString(testSandboxID), Host: serverapi.NewOptString(""),
			EnvVar: serverapi.NewOptString("GH_TOKEN"), Uses: use,
		}, "requires a host"},
		{"no environment variable", services.CreateSecretGrantBody{
			SecretId: secret.ID, Scope: serverapi.CreateSecretGrantBodyScopeSandbox,
			ScopeKey: serverapi.NewOptString(testSandboxID), Host: serverapi.NewOptString("api.github.com"), Uses: use,
		}, "environment variable"},
		{"a variable with no uses", services.CreateSecretGrantBody{
			SecretId: secret.ID, Scope: serverapi.CreateSecretGrantBodyScopeSandbox,
			ScopeKey: serverapi.NewOptString(testSandboxID), Host: serverapi.NewOptString("api.github.com"),
			EnvVar: serverapi.NewOptString("GH_TOKEN"),
		}, "only meaningful with uses"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.CreateSecretGrant(ctx, "project-1", tc.body)
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to say %q", err, tc.want)
			}
		})
	}
}

// The same shape, granted wider than one discobox. The binding a credential
// resolves through is per discobox and cannot be written in advance — the boxes
// a project grant covers may not exist yet — so it is minted the first time
// that discobox's agent asks what it may use.
func TestAWiderGrantBindsWhenTheAgentFirstAsks(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)

	if _, err := svc.CreateSecretGrant(ctx, "project-1", services.CreateSecretGrantBody{
		SecretId: secret.ID,
		Scope:    serverapi.CreateSecretGrantBodyScopeProject,
		Host:     serverapi.NewOptString("api.github.com"),
		EnvVar:   serverapi.NewOptString("GH_TOKEN"),
		Uses: serverapi.NewOptNilSecretUseArray([]serverapi.SecretUse{
			{Description: "Open a pull request against the current repo"},
		}),
	}); err != nil {
		t.Fatalf("grant: %v", err)
	}

	// Nothing is bound yet: no agent has asked.
	bindings, err := st.ListSandboxSecrets(ctx, "project-1", testSandboxID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(bindings) != 0 {
		t.Fatalf("bindings = %#v, want none before an agent asks", bindings)
	}

	credentials, err := svc.ListSandboxCredentials(ctx, testPoolID, testSandboxID)
	if err != nil {
		t.Fatalf("list credentials: %v", err)
	}
	if len(credentials) != 1 || credentials[0].Assignment.Sentinel == "" {
		t.Fatalf("credentials = %#v, want the project grant offered with a minted sentinel", credentials)
	}
	if !credentials[0].Assignment.AgentRequested {
		t.Fatal("the binding is injectable; a credential granted this way is never in the sandbox")
	}
	if credentials[0].Assignment.EnvName != "GH_TOKEN" {
		t.Fatalf("bound to %q, want the variable the grant names", credentials[0].Assignment.EnvName)
	}

	// Asking again is the same credential, not a second sentinel: a fresh one
	// would strand every activation taken under the first.
	again, err := svc.ListSandboxCredentials(ctx, testPoolID, testSandboxID)
	if err != nil {
		t.Fatalf("list again: %v", err)
	}
	if len(again) != 1 || again[0].Assignment.Sentinel != credentials[0].Assignment.Sentinel {
		t.Fatalf("second ask = %#v, want the binding it already had", again)
	}
	injected, err := st.ListInjectedSandboxSecrets(ctx, "project-1", testSandboxID)
	if err != nil {
		t.Fatalf("list injected: %v", err)
	}
	if len(injected) != 0 {
		t.Fatalf("injected = %#v, want none", injected)
	}
}

// Two grants naming one variable: the narrower one keeps it, and the wider is
// passed over rather than swapping the credential underneath it.
func TestTheNarrowerGrantKeepsTheVariable(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	mine := createBearerSecret(ctx, t, svc)
	theirs, err := svc.CreateSecret(ctx, "project-1", services.CreateSecretBody{
		Name:  "everyone's github",
		Type:  serverapi.CreateSecretBodyTypeToken,
		Host:  serverapi.NewOptString("api.github.com"),
		Value: serverapi.SecretValue{Token: serverapi.NewOptString("ghp_projectwidevalue00000000000000000")},
	})
	if err != nil {
		t.Fatalf("create secret: %v", err)
	}
	use := serverapi.NewOptNilSecretUseArray([]serverapi.SecretUse{{Description: "Open a PR"}})

	for _, g := range []services.CreateSecretGrantBody{
		{SecretId: theirs.ID, Scope: serverapi.CreateSecretGrantBodyScopeProject,
			Host: serverapi.NewOptString("api.github.com"), EnvVar: serverapi.NewOptString("GH_TOKEN"), Uses: use},
		{SecretId: mine.ID, Scope: serverapi.CreateSecretGrantBodyScopeSandbox,
			ScopeKey: serverapi.NewOptString(testSandboxID),
			Host:     serverapi.NewOptString("api.github.com"), EnvVar: serverapi.NewOptString("GH_TOKEN"), Uses: use},
	} {
		if _, err := svc.CreateSecretGrant(ctx, "project-1", g); err != nil {
			t.Fatalf("grant: %v", err)
		}
	}

	credentials, err := svc.ListSandboxCredentials(ctx, testPoolID, testSandboxID)
	if err != nil {
		t.Fatalf("list credentials: %v", err)
	}
	if len(credentials) != 1 {
		t.Fatalf("credentials = %#v, want one: two grants cannot share a variable", credentials)
	}
	if credentials[0].Assignment.SecretID != mine.ID {
		t.Fatalf("offered %s, want the discobox's own grant to keep the variable", credentials[0].Assignment.SecretID)
	}
}

// Registering an OAuth credential by hand: everything the control plane needs
// to renew it goes in, and what comes back says what the credential is without
// being it.
func TestAnOAuthSecretIsRegisteredAndDescribed(t *testing.T) {
	ctx := testPrincipalContext()
	svc := newTestService(t)

	created, err := svc.CreateSecret(ctx, "project-1", services.CreateSecretBody{
		Name: "claude",
		Type: serverapi.CreateSecretBodyTypeOAuth,
		Host: serverapi.NewOptString("api.anthropic.com"),
		Value: serverapi.SecretValue{
			Token:                serverapi.NewOptString("sk-ant-oat01-access"),
			RefreshToken:         serverapi.NewOptString("sk-ant-ort01-refresh"),
			TokenUrl:             serverapi.NewOptString("https://console.anthropic.com/v1/oauth/token"),
			ClientId:             serverapi.NewOptString("client-123"),
			Scopes:               serverapi.NewOptNilStringArray([]string{"user:profile", "user:inference"}),
			SubscriptionType:     serverapi.NewOptString("max"),
			AccessTokenExpiresAt: serverapi.NewOptInt64(1893456000000),
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := svc.GetSecret(ctx, "project-1", created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.OAuth == nil {
		t.Fatal("the credential says nothing about itself")
	}
	if got.OAuth.TokenURL != "https://console.anthropic.com/v1/oauth/token" || got.OAuth.ClientID != "client-123" {
		t.Fatalf("oauth = %#v, want where it renews and whose it is", got.OAuth)
	}
	if len(got.OAuth.Scopes) != 2 || got.OAuth.SubscriptionType != "max" {
		t.Fatalf("oauth = %#v, want what the grant may do", got.OAuth)
	}
	if got.OAuth.AccessTokenExpiresAt != 1893456000000 || !got.OAuth.Refreshable {
		t.Fatalf("oauth = %#v, want the expiry and that it can renew", got.OAuth)
	}

	// What it must never say: the credential, or the thing that mints one.
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, secret := range []string{"sk-ant-oat01-access", "sk-ant-ort01-refresh"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("the secret leaks %q: %s", secret, encoded)
		}
	}

	// And a listing says the same, since that is where somebody looks first.
	listed, err := svc.ListSecrets(ctx, "project-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 || listed[0].OAuth == nil || !listed[0].OAuth.Refreshable {
		t.Fatalf("listed = %#v, want the same summary", listed)
	}
}

// An oauth secret is one that renews itself. Without the material for that,
// calling it oauth promises a refresh nothing can perform.
func TestOAuthWithoutRefreshMaterialIsRefused(t *testing.T) {
	ctx := testPrincipalContext()
	svc := newTestService(t)

	_, err := svc.CreateSecret(ctx, "project-1", services.CreateSecretBody{
		Name:  "half an oauth",
		Type:  serverapi.CreateSecretBodyTypeOAuth,
		Value: serverapi.SecretValue{Token: serverapi.NewOptString("sk-ant-oat01-access")},
	})
	if err == nil {
		t.Fatal("accepted an oauth secret that cannot renew itself")
	}
	if !strings.Contains(err.Error(), "refreshToken") || !strings.Contains(err.Error(), "store it as a token") {
		t.Fatalf("error = %v, want it to say what is missing and what to do", err)
	}
}

// The limit is a ceiling, not a pre-filled suggestion. It has to bite wherever
// a grant is minted, because the lifetime arrives from three places — an
// approval, a pre-approval, and the in-sandbox flow — and a rule enforced in
// one of them is a rule the other two walk around.
func TestGrantMayNotOutliveTheSecretsLimit(t *testing.T) {
	ctx := testPrincipalContext()
	svc := newTestService(t)

	secret, err := svc.CreateSecret(ctx, "project-1", services.CreateSecretBody{
		Name:               "github",
		Type:               serverapi.CreateSecretBodyTypeToken,
		Host:               serverapi.NewOptString("github.com"),
		MaxGrantTTLSeconds: serverapi.NewOptInt64(3600),
		Value:              serverapi.SecretValue{Token: serverapi.NewOptString("ghp_token")},
	})
	if err != nil {
		t.Fatalf("create secret: %v", err)
	}
	if secret.MaxGrantTTL != 3600 {
		t.Fatalf("limit = %d, want 3600", secret.MaxGrantTTL)
	}

	_, err = svc.CreateSecretGrant(ctx, "project-1", services.CreateSecretGrantBody{
		SecretId:        secret.ID,
		Scope:           serverapi.CreateSecretGrantBodyScopeProject,
		GrantTTLSeconds: serverapi.NewOptInt64(7200),
	})
	if err == nil {
		t.Fatal("a grant twice the secret's limit was minted")
	}
	if msg := err.Error(); !strings.Contains(msg, "at most 1h0m0s") || !strings.Contains(msg, "--max-grant-ttl 7200") {
		t.Fatalf("refusal = %q, want the limit and the command that raises it", msg)
	}

	// Forever is the case worth naming separately: it is not "longer than the
	// limit" arithmetically, and it is the one an approver reaches for.
	_, err = svc.CreateSecretGrant(ctx, "project-1", services.CreateSecretGrantBody{
		SecretId:        secret.ID,
		Scope:           serverapi.CreateSecretGrantBodyScopeProject,
		GrantTTLSeconds: serverapi.NewOptInt64(0),
	})
	if err == nil {
		t.Fatal("a grant that never expires was minted on a limited secret")
	}
	if msg := err.Error(); !strings.Contains(msg, "cannot be granted forever") {
		t.Fatalf("refusal = %q, want it to say forever is what was refused", msg)
	}

	// Inside the limit, and nobody naming one at all, both stand.
	within, err := svc.CreateSecretGrant(ctx, "project-1", services.CreateSecretGrantBody{
		SecretId:        secret.ID,
		Scope:           serverapi.CreateSecretGrantBodyScopeProject,
		GrantTTLSeconds: serverapi.NewOptInt64(600),
	})
	if err != nil {
		t.Fatalf("grant inside the limit: %v", err)
	}
	if within.ExpiresAt == nil {
		t.Fatal("a grant with a lifetime should expire")
	}
	if _, err := svc.CreateSecretGrant(ctx, "project-1", services.CreateSecretGrantBody{
		SecretId: secret.ID,
		Scope:    serverapi.CreateSecretGrantBodyScopeProject,
	}); err != nil {
		t.Fatalf("grant taking the secret's limit: %v", err)
	}
}

// Zero on the secret is the meaningful value "no limit", not an unset field:
// grants on such a credential may live forever, which is what an unlimited
// credential has to be able to say out loud.
func TestASecretWithNoLimitGrantsForever(t *testing.T) {
	ctx := testPrincipalContext()
	svc := newTestService(t)

	secret, err := svc.CreateSecret(ctx, "project-1", services.CreateSecretBody{
		Name:               "unlimited",
		Type:               serverapi.CreateSecretBodyTypeToken,
		MaxGrantTTLSeconds: serverapi.NewOptInt64(0),
		Value:              serverapi.SecretValue{Token: serverapi.NewOptString("token")},
	})
	if err != nil {
		t.Fatalf("create secret: %v", err)
	}
	if secret.MaxGrantTTL != 0 {
		t.Fatalf("limit = %d, want 0 — an explicit zero is the answer, not an omission", secret.MaxGrantTTL)
	}

	grant, err := svc.CreateSecretGrant(ctx, "project-1", services.CreateSecretGrantBody{
		SecretId:        secret.ID,
		Scope:           serverapi.CreateSecretGrantBodyScopeProject,
		GrantTTLSeconds: serverapi.NewOptInt64(0),
	})
	if err != nil {
		t.Fatalf("grant forever on an unlimited secret: %v", err)
	}
	if grant.ExpiresAt != nil {
		t.Fatalf("expires at = %v, want a grant that never expires", grant.ExpiresAt)
	}
}

// Approving a request is the other door into minting, and it defaults to the
// secret's limit rather than to an hour nobody chose.
func TestApprovalTakesTheSecretsLimitAndIsBoundByIt(t *testing.T) {
	ctx := testPrincipalContext()
	svc := newTestService(t)

	secret, err := svc.CreateSecret(ctx, "project-1", services.CreateSecretBody{
		Name:               "short-lived",
		Type:               serverapi.CreateSecretBodyTypeToken,
		MaxGrantTTLSeconds: serverapi.NewOptInt64(300),
		Value:              serverapi.SecretValue{Token: serverapi.NewOptString("token")},
	})
	if err != nil {
		t.Fatalf("create secret: %v", err)
	}

	req, err := svc.CreateSecretRequest(ctx, "project-1", services.CreateSecretRequestBody{
		Type: serverapi.CreateSecretRequestBodyTypeToken,
		Host: serverapi.NewOptString("github.com"),
	})
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	if _, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{
		SecretId:        secret.ID,
		GrantTTLSeconds: serverapi.NewOptInt64(86400),
	}); err == nil {
		t.Fatal("an approval outlived the secret's limit")
	}

	approved, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{SecretId: secret.ID})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	grants, err := svc.ListSecretGrants(ctx, "project-1", secret.ID)
	if err != nil {
		t.Fatalf("list grants: %v", err)
	}
	if len(grants) != 1 || grants[0].ID != approved.GrantID {
		t.Fatalf("grants = %#v, want the one the approval minted", grants)
	}
	if grants[0].ExpiresAt == nil {
		t.Fatal("a grant on a limited secret should expire")
	}
	if got := time.Until(*grants[0].ExpiresAt); got > 5*time.Minute+time.Minute {
		t.Fatalf("expires in %v, want the secret's 5m limit", got)
	}
}
