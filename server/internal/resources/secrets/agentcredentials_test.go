package secrets_test

import (
	"context"
	"strings"
	"testing"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	resourcesecrets "github.com/discobox-ai/discobox/server/internal/resources/secrets"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

const (
	testPoolID    = "pool-1"
	testSandboxID = "sbx-1"
)

func TestAgentCredentialRequestIsPendingAndRecordsWhatWasAsked(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)

	req := createAgentRequest(ctx, t, svc)
	if req.Status != model.SecretRequestStatusPending {
		t.Fatalf("status = %q, want pending", req.Status)
	}
	if !req.FromProtocol() {
		t.Fatal("request does not read as protocol-originated; declared uses are what separate it from the proxy's reactive path")
	}
	if req.EnvName != "GITHUB_TOKEN" || req.Host != "api.github.com" || req.Justification == "" {
		t.Fatalf("request = %#v, want the ask recorded verbatim", req)
	}
	if len(req.Uses) != 1 || req.Uses[0].UseID != "" {
		t.Fatalf("uses = %#v, want one use with no ID; IDs are minted at approval", req.Uses)
	}
}

// An agent that retries its ask must not fill the approval inbox with copies of
// the same question.
func TestAgentCredentialRequestReusesAnOpenAsk(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)

	first := createAgentRequest(ctx, t, svc)
	second := createAgentRequest(ctx, t, svc)
	if first.ID != second.ID {
		t.Fatalf("second ask created %s, want the open request %s reused", second.ID, first.ID)
	}
}

func TestAgentCredentialRequestRequiresAHost(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)

	_, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID,
		Name:      "github",
		EnvVar:    "GITHUB_TOKEN",
		Uses:      []apimodel.SecretUse{{Description: "open a PR"}},
	})
	if err == nil {
		t.Fatal("hostless ask accepted; approving it could only produce a wildcard grant")
	}
}

func TestApprovingAnAgentRequestMintsUsesAndAnUninjectedBinding(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	req := createAgentRequest(ctx, t, svc)

	approved, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{
		SecretId: secret.ID,
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}

	grant, err := st.GetSecretGrant(ctx, "project-1", approved.GrantID)
	if err != nil {
		t.Fatalf("get grant: %v", err)
	}
	if grant.Scope != model.SecretGrantScopeSandbox || grant.ScopeKey != testSandboxID {
		t.Fatalf("grant scope = %s/%s, want the asking sandbox", grant.Scope, grant.ScopeKey)
	}
	if grant.Host != "api.github.com" {
		t.Fatalf("grant host = %q, want the requested host; this flow never mints a wildcard", grant.Host)
	}
	if len(grant.Uses) != 1 || !strings.HasPrefix(grant.Uses[0].UseID, "use_") {
		t.Fatalf("grant uses = %#v, want one use carrying a minted ID", grant.Uses)
	}

	// The binding exists so the pool agent has something to translate to, and is
	// excluded from everything that reaches the sandbox.
	all, err := st.ListSandboxSecrets(ctx, "project-1", testSandboxID)
	if err != nil {
		t.Fatalf("list assignments: %v", err)
	}
	if len(all) != 1 || !all[0].AgentRequested || all[0].Sentinel == "" {
		t.Fatalf("assignments = %#v, want one agent-requested binding with a sentinel", all)
	}
	injected, err := st.ListInjectedSandboxSecrets(ctx, "project-1", testSandboxID)
	if err != nil {
		t.Fatalf("list injected: %v", err)
	}
	if len(injected) != 0 {
		t.Fatalf("injected = %#v, want none; an agent credential is never written into the sandbox", injected)
	}
}

func TestApprovingAnAgentRequestRefusesABroaderScope(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	req := createAgentRequest(ctx, t, svc)

	_, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{
		SecretId: secret.ID,
		Scope:    serverapi.NewOptApproveSecretRequestBodyScope(serverapi.ApproveSecretRequestBodyScopeProject),
	})
	if err == nil {
		t.Fatal("project-scoped approval accepted; an agent's ask authorizes that agent's sandbox")
	}
}

func TestApproverCanRewriteTheDeclaredUses(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	req := createAgentRequest(ctx, t, svc)

	approved, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{
		SecretId: secret.ID,
		Uses: serverapi.NewOptNilSecretUseArray([]apimodel.SecretUse{
			{Description: "open a PR against this repository only", UseId: serverapi.NewOptString("use_forged")},
		}),
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	grant, err := st.GetSecretGrant(ctx, "project-1", approved.GrantID)
	if err != nil {
		t.Fatalf("get grant: %v", err)
	}
	if len(grant.Uses) != 1 || grant.Uses[0].Description != "open a PR against this repository only" {
		t.Fatalf("grant uses = %#v, want the approver's wording", grant.Uses)
	}
	if grant.Uses[0].UseID == "use_forged" {
		t.Fatal("supplied use ID was kept; IDs must always be minted at approval")
	}
}

func TestGrantedCredentialIsListedForItsSandbox(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	req := createAgentRequest(ctx, t, svc)
	if _, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{SecretId: secret.ID}); err != nil {
		t.Fatalf("approve: %v", err)
	}

	credentials, err := svc.ListSandboxCredentials(ctx, testPoolID, testSandboxID)
	if err != nil {
		t.Fatalf("list credentials: %v", err)
	}
	if len(credentials) != 1 {
		t.Fatalf("credentials = %#v, want the approved one", credentials)
	}
	if credentials[0].Assignment.Sentinel == "" || len(credentials[0].Grant.Uses) != 1 {
		t.Fatalf("credential = %#v, want the stable sentinel and its uses", credentials[0])
	}
}

// A pool may only ever speak for its own sandboxes, and a sandbox may only poll
// its own asks.
func TestAgentCredentialCallsRefuseAnotherPoolsSandbox(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)

	if _, err := svc.ListSandboxCredentials(ctx, "pool-other", testSandboxID); err == nil {
		t.Fatal("listed another pool's sandbox credentials")
	}
	_, err := svc.CreateSandboxCredentialRequest(ctx, "pool-other", services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID,
		Name:      "github",
		EnvVar:    "GITHUB_TOKEN",
		Host:      "api.github.com",
		Uses:      []apimodel.SecretUse{{Description: "open a PR"}},
	})
	if err == nil {
		t.Fatal("recorded a request for another pool's sandbox")
	}
}

func TestPollingReportsGrantedOnceApproved(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	req := createAgentRequest(ctx, t, svc)

	pending, grant, err := svc.GetSandboxCredentialRequest(ctx, testPoolID, testSandboxID, req.ID)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if status := resourcesecrets.AgentCredentialRequestStatus(pending, grant); status != "pending" {
		t.Fatalf("status = %q, want pending", status)
	}

	if _, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{SecretId: secret.ID}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	settled, grant, err := svc.GetSandboxCredentialRequest(ctx, testPoolID, testSandboxID, req.ID)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if status := resourcesecrets.AgentCredentialRequestStatus(settled, grant); status != "granted" {
		t.Fatalf("status = %q, want granted", status)
	}
	if grant == nil || len(grant.Uses) != 1 {
		t.Fatalf("grant = %#v, want the use IDs the agent may present", grant)
	}
}

// Revoking the grant takes the credential away even though the request row
// stays approved: the request is history, the grant is the authorization.
func TestPollingReportsDeniedAfterTheGrantIsRevoked(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	req := createAgentRequest(ctx, t, svc)
	approved, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{SecretId: secret.ID})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := svc.RevokeSecretGrant(ctx, "project-1", approved.GrantID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	settled, grant, err := svc.GetSandboxCredentialRequest(ctx, testPoolID, testSandboxID, req.ID)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if status := resourcesecrets.AgentCredentialRequestStatus(settled, grant); status != "denied" {
		t.Fatalf("status = %q, want denied once the grant is gone", status)
	}
	credentials, err := svc.ListSandboxCredentials(ctx, testPoolID, testSandboxID)
	if err != nil {
		t.Fatalf("list credentials: %v", err)
	}
	if len(credentials) != 0 {
		t.Fatalf("credentials = %#v, want none after revocation", credentials)
	}
}

func createAgentRequest(ctx context.Context, t *testing.T, svc *resourcesecrets.Service) *model.SecretRequest {
	t.Helper()
	req, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId:     testSandboxID,
		Name:          "github",
		EnvVar:        "GITHUB_TOKEN",
		Host:          "api.github.com",
		Justification: serverapi.NewOptString("the task asks me to open a PR"),
		Uses:          []apimodel.SecretUse{{Description: "open a pull request"}},
	})
	if err != nil {
		t.Fatalf("create credential request: %v", err)
	}
	return req
}

func createBearerSecret(ctx context.Context, t *testing.T, svc *resourcesecrets.Service) *model.Secret {
	t.Helper()
	secret, err := svc.CreateSecret(ctx, "project-1", services.CreateSecretBody{
		Name:  "github",
		Type:  serverapi.CreateSecretBodyTypeBearer,
		Value: serverapi.SecretValue{Token: serverapi.NewOptString("ghp_realrealrealrealrealrealrealreal12")},
	})
	if err != nil {
		t.Fatalf("create secret: %v", err)
	}
	return secret
}

// newAgentCredentialService builds a service over a database holding one
// project, one pool, and one sandbox on it, which is the minimum shape every
// broker call re-derives its authorization from.
func newAgentCredentialService(t *testing.T) (*resourcesecrets.Service, *store.Store) {
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
	write := db.Write.WithContext(ctx)
	if err := write.Create(&model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project"}).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := write.Create(&model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "docker", Name: "docker"}).Error; err != nil {
		t.Fatalf("create provider: %v", err)
	}
	if err := write.Create(&model.Pool{
		ID:           testPoolID,
		ProjectID:    "project-1",
		PoolManifest: model.PoolManifest{Name: "pool", ProviderInstanceID: "provider-1"},
	}).Error; err != nil {
		t.Fatalf("create pool: %v", err)
	}
	if err := write.Create(&model.Sandbox{ID: testSandboxID, ProjectID: "project-1", Name: "sandbox", PoolID: testPoolID}).Error; err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	st := store.New(db.Write, db.Read)
	return resourcesecrets.NewService(st), st
}
