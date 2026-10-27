package secrets_test

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	serverapi "github.com/discobox-ai/discobox/api/gen"
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

// sentinelMarks records which sandboxes an approval sent a new binding to.
type sentinelMarks struct {
	sandboxes []string
	secrets   []string
}

func (m *sentinelMarks) MarkSecretHoldersDirtyTx(_ context.Context, _ *store.Store, _ *gorm.DB, _, secretID string) error {
	m.secrets = append(m.secrets, secretID)
	return nil
}

func (m *sentinelMarks) MarkSandboxSentinelsDirtyTx(_ context.Context, _ *gorm.DB, _, sandboxID string) error {
	m.sandboxes = append(m.sandboxes, sandboxID)
	return nil
}

func TestAgentCredentialRequestIsPendingAndRecordsWhatWasAsked(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)

	req := createAgentRequest(ctx, t, svc)
	if req.Status != model.SecretRequestStatusPending {
		t.Fatalf("status = %q, want pending", req.Status)
	}
	if !req.FromProtocol() {
		t.Fatal("request does not read as protocol-originated; the credential it names is what separates it from the proxy's reactive path")
	}
	if req.Name != "github" || req.EnvName != "GITHUB_TOKEN" || !slices.Equal(req.Hosts, []string{"api.github.com"}) ||
		req.Justification == "" || req.Purpose != model.SecretGrantPurposeUse {
		t.Fatalf("request = %#v, want the ask recorded verbatim", req)
	}
}

// A use is delivered in the variable it names, so one names a variable; a
// delegation delivers nothing to the discobox that asks, so one that names a
// variable is a mistake about what it asks for.
func TestAgentCredentialRequestNamesAVariableOnlyForAUse(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	delegate := serverapi.NewOptCreateSandboxCredentialRequestBodyPurpose(serverapi.CreateSandboxCredentialRequestBodyPurposeDelegate)
	for _, tc := range []struct {
		name string
		body services.CreateSandboxCredentialRequestBody
		want int
	}{
		{"a use with no variable", services.CreateSandboxCredentialRequestBody{Name: "github", Hosts: []string{"api.github.com"}}, http.StatusBadRequest},
		{"a use with a malformed variable", services.CreateSandboxCredentialRequestBody{Name: "github", EnvVar: serverapi.NewOptString("A=B"), Hosts: []string{"api.github.com"}}, http.StatusBadRequest},
		{"a delegation naming a variable", services.CreateSandboxCredentialRequestBody{Name: "github", EnvVar: serverapi.NewOptString("GITHUB_TOKEN"), Hosts: []string{"github.com"}, Purpose: delegate}, http.StatusBadRequest},
		{"a delegation", services.CreateSandboxCredentialRequestBody{Name: "github", Hosts: []string{"github.com"}, Purpose: delegate}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.body.SandboxId = testSandboxID
			_, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, tc.body)
			if tc.want == 0 {
				if err != nil {
					t.Fatalf("create = %v, want it filed", err)
				}
				return
			}
			if !isStatus(err, tc.want) {
				t.Fatalf("create = %v, want %d", err, tc.want)
			}
		})
	}
}

// An agent that asks again — a retry, a wrapper asking on each attempt —
// finds its open ask rather than filing another; an ask for anything approving
// would grant differently is its own.
func TestAgentCredentialRequestReusesAnOpenAsk(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	first := createAgentRequest(ctx, t, svc)
	again := createAgentRequest(ctx, t, svc)
	if again.ID != first.ID {
		t.Fatalf("second ask = %s, want the open %s", again.ID, first.ID)
	}
	other, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID, Name: "github", EnvVar: serverapi.NewOptString("GITHUB_TOKEN"), Hosts: []string{"api.github.com"},
		GrantTTLSeconds: serverapi.NewOptInt64(7200),
	})
	if err != nil {
		t.Fatalf("ask for another lifetime: %v", err)
	}
	if other.ID == first.ID {
		t.Fatal("an ask for another lifetime was folded into the open one")
	}
}

func TestAgentCredentialRequestRecordsTheLifetimeAskedFor(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	for _, tc := range []struct {
		name    string
		seconds int64
		ok      bool
	}{
		{"a day", 86400, true},
		{"thirty days", 30 * 86400, true},
		{"past thirty days", 30*86400 + 1, false},
		{"negative", -1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
				SandboxId: testSandboxID, Name: "github-" + tc.name, EnvVar: serverapi.NewOptString("GITHUB_TOKEN"),
				Hosts: []string{"api.github.com"}, GrantTTLSeconds: serverapi.NewOptInt64(tc.seconds),
			})
			if tc.ok {
				if err != nil || req.GrantTTL != tc.seconds {
					t.Fatalf("request = %+v, %v; want the lifetime recorded", req, err)
				}
				return
			}
			if !isStatus(err, http.StatusBadRequest) {
				t.Fatalf("create = %v, want 400", err)
			}
		})
	}
}

func TestAgentCredentialRequestRequiresAHost(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	_, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID, Name: "github", EnvVar: serverapi.NewOptString("GITHUB_TOKEN"),
	})
	if !isStatus(err, http.StatusBadRequest) {
		t.Fatalf("create = %v, want 400: a grant this flow mints is never host-unscoped", err)
	}
}

// Approving an agent's use mints a sandbox grant for the hosts approved and
// binds the variable it named on the discobox like any credential, marking
// that discobox's sentinel delivery so the binding reaches it while it runs
// (ADR 26-10-27-880).
func TestApprovingAnAgentRequestMintsAGrantAndAnInjectedBinding(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	marks := &sentinelMarks{}
	svc.SetSecretHolders(marks)
	secret := createBearerSecret(ctx, t, svc)
	req := createAgentRequest(ctx, t, svc)

	approved, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{
		SecretId: serverapi.NewOptString(secret.ID),
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	grant, err := st.GetSecretGrant(ctx, "project-1", approved.GrantID)
	if err != nil {
		t.Fatalf("get grant: %v", err)
	}
	if grant.Scope != model.SecretGrantScopeSandbox || grant.ScopeKey != testSandboxID || grant.Purpose != model.SecretGrantPurposeUse {
		t.Fatalf("grant = %+v, want a use grant for the asking sandbox", grant)
	}
	if !slices.Equal(grant.Hosts, []string{"api.github.com"}) {
		t.Fatalf("grant hosts = %q, want the requested host; this flow never mints a wildcard", grant.Hosts)
	}
	bindings, err := st.ListSandboxSecrets(ctx, "project-1", testSandboxID)
	if err != nil {
		t.Fatalf("list bindings: %v", err)
	}
	if len(bindings) != 1 || bindings[0].EnvName != "GITHUB_TOKEN" || bindings[0].SecretID != secret.ID || bindings[0].Sentinel == "" {
		t.Fatalf("bindings = %#v, want GITHUB_TOKEN bound to the secret with a sentinel", bindings)
	}
	if !slices.Equal(marks.sandboxes, []string{testSandboxID}) {
		t.Fatalf("sentinel deliveries marked = %v, want the asking sandbox's", marks.sandboxes)
	}
}

// An agent may ask to delegate a credential rather than use it. Approving that
// ask mints a delegation grant, which binds nothing: there is nothing for the
// asking discobox to send. It is listed as what it may hand on.
func TestApprovingAnAskToDelegateMintsADelegationGrant(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	req, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID, Name: "github", Hosts: []string{"github.com"},
		Purpose: serverapi.NewOptCreateSandboxCredentialRequestBodyPurpose(serverapi.CreateSandboxCredentialRequestBodyPurposeDelegate),
	})
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	approved, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(secret.ID)})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	grant, err := st.GetSecretGrant(ctx, "project-1", approved.GrantID)
	if err != nil || grant.Purpose != model.SecretGrantPurposeDelegate || grant.MayUse() {
		t.Fatalf("grant = %+v, %v; want a delegation grant", grant, err)
	}
	if bindings, _ := st.ListSandboxSecrets(ctx, "project-1", testSandboxID); len(bindings) != 0 {
		t.Fatalf("bindings = %#v, want none: a delegation delivers nothing", bindings)
	}
	listed, err := svc.ListSandboxCredentials(ctx, testPoolID, testSandboxID)
	if err != nil || len(listed) != 1 || listed[0].Purpose != model.SecretGrantPurposeDelegate || listed[0].EnvName != "" {
		t.Fatalf("listed = %+v, %v; want the delegation, with no variable", listed, err)
	}
}

func TestApprovingAnAgentRequestRefusesABroaderScope(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	req := createAgentRequest(ctx, t, svc)
	_, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{
		SecretId: serverapi.NewOptString(secret.ID),
		Scope:    serverapi.NewOptApproveSecretRequestBodyScope(serverapi.ApproveSecretRequestBodyScopeProject),
	})
	if !isStatus(err, http.StatusBadRequest) {
		t.Fatalf("approve = %v, want 400: an agent asked on behalf of one sandbox", err)
	}
}

// What a sandbox holds is listed for it: each variable a live use grant
// authorizes, with the hosts it may be sent to, and nothing once the grant
// is revoked.
func TestGrantedCredentialIsListedForItsSandbox(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	approved, err := svc.ApproveSecretRequest(ctx, "project-1", createAgentRequest(ctx, t, svc).ID, services.ApproveSecretRequestBody{
		SecretId: serverapi.NewOptString(secret.ID),
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	credentials, err := svc.ListSandboxCredentials(ctx, testPoolID, testSandboxID)
	if err != nil {
		t.Fatalf("list credentials: %v", err)
	}
	if len(credentials) != 1 || credentials[0].Name != "github" || credentials[0].EnvName != "GITHUB_TOKEN" ||
		credentials[0].GrantID != approved.GrantID || !slices.Equal(credentials[0].Hosts, []string{"api.github.com"}) {
		t.Fatalf("credentials = %+v, want the one granted", credentials)
	}
	if err := st.DeleteSecretGrant(ctx, "project-1", approved.GrantID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	credentials, err = svc.ListSandboxCredentials(ctx, testPoolID, testSandboxID)
	if err != nil || len(credentials) != 0 {
		t.Fatalf("credentials = %+v, %v; want none after revocation", credentials, err)
	}
}

func TestAgentCredentialCallsRefuseAnotherPoolsSandbox(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	if _, err := svc.ListSandboxCredentials(ctx, "pool-other", testSandboxID); !isStatus(err, http.StatusNotFound) {
		t.Fatalf("list = %v, want 404", err)
	}
	if _, err := svc.CreateSandboxCredentialRequest(ctx, "pool-other", services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID, Name: "github", EnvVar: serverapi.NewOptString("GITHUB_TOKEN"), Hosts: []string{"api.github.com"},
	}); !isStatus(err, http.StatusNotFound) {
		t.Fatalf("create = %v, want 404", err)
	}
	req := createAgentRequest(ctx, t, svc)
	if _, _, err := svc.GetSandboxCredentialRequest(ctx, "pool-other", testSandboxID, req.ID); !isStatus(err, http.StatusNotFound) {
		t.Fatalf("poll = %v, want 404", err)
	}
}

func TestPollingReportsGrantedOnceApprovedAndDeniedOnceRevoked(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	req := createAgentRequest(ctx, t, svc)
	got, grant, err := svc.GetSandboxCredentialRequest(ctx, testPoolID, testSandboxID, req.ID)
	if err != nil || services.AgentCredentialRequestStatus(got, grant) != model.SecretRequestStatusPending {
		t.Fatalf("poll before approval = %+v, %v; want pending", got, err)
	}
	approved, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(secret.ID)})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	got, grant, err = svc.GetSandboxCredentialRequest(ctx, testPoolID, testSandboxID, req.ID)
	if err != nil || services.AgentCredentialRequestStatus(got, grant) != "granted" || grant.ID != approved.GrantID {
		t.Fatalf("poll after approval = %+v, %+v, %v; want granted", got, grant, err)
	}
	if err := st.DeleteSecretGrant(ctx, "project-1", approved.GrantID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	got, grant, err = svc.GetSandboxCredentialRequest(ctx, testPoolID, testSandboxID, req.ID)
	if err != nil || services.AgentCredentialRequestStatus(got, grant) != model.SecretRequestStatusDenied {
		t.Fatalf("poll after revocation = %+v, %v; want denied", got, err)
	}
}

func createAgentRequest(ctx context.Context, t *testing.T, svc *resourcesecrets.Service) *model.SecretRequest {
	t.Helper()
	req, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId:     testSandboxID,
		Name:          "github",
		EnvVar:        serverapi.NewOptString("GITHUB_TOKEN"),
		Hosts:         []string{"api.github.com"},
		Justification: serverapi.NewOptString("the task asks me to open a PR"),
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
		Type:  serverapi.CreateSecretBodyTypeToken,
		Value: serverapi.SecretValue{Token: serverapi.NewOptString("ghp_realrealrealrealrealrealrealreal12")},
	})
	if err != nil {
		t.Fatalf("create secret: %v", err)
	}
	return secret
}

// newAgentCredentialService builds a service over a database holding one
// project, one pool, the sandbox that asks, and the lead the delegation tests
// act as, which created it.
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
	if err := write.Create(&model.Sandbox{ID: leadID, ProjectID: "project-1", Name: "lead", PoolID: testPoolID}).Error; err != nil {
		t.Fatalf("create lead sandbox: %v", err)
	}
	lead := leadID
	if err := write.Create(&model.Sandbox{ID: testSandboxID, ProjectID: "project-1", Name: "sandbox", PoolID: testPoolID, CreatedBySandboxID: &lead}).Error; err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	st := store.New(db.Write, db.Read)
	return resourcesecrets.NewService(st), st
}

// A host is matched against what the proxy observed, which it reports
// lowercased and without a port. A grant stored any other way is one nothing
// can ever match.
func TestApprovedHostIsNormalizedToWhatTheProxyReports(t *testing.T) {
	for _, tc := range []struct{ name, approved string }{
		{"mixed case", "API.GitHub.com"},
		{"padded", "  api.github.com  "},
		{"with a port", "api.github.com:443"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testPrincipalContext()
			svc, st := newAgentCredentialService(t)
			secret := createBearerSecret(ctx, t, svc)
			req := createAgentRequest(ctx, t, svc)
			approved, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{
				SecretId: serverapi.NewOptString(secret.ID),
				Hosts:    []string{tc.approved},
			})
			if err != nil {
				t.Fatalf("approve: %v", err)
			}
			grant, err := st.GetSecretGrant(ctx, "project-1", approved.GrantID)
			if err != nil {
				t.Fatalf("get grant: %v", err)
			}
			if !slices.Equal(grant.Hosts, []string{"api.github.com"}) {
				t.Fatalf("grant host = %q, want the host as the proxy reports it", grant.Hosts)
			}
			if _, err := st.FindLiveGrant(ctx, "project-1", secret.ID, "api.github.com",
				[]store.GrantScope{{Scope: model.SecretGrantScopeSandbox, ScopeKey: testSandboxID}}); err != nil {
				t.Fatalf("the minted grant does not match the observed host: %v", err)
			}
		})
	}
}

// What the approver agreed to change on the secret — its binding, its limit —
// is written with the grant or not at all.
func TestAnApprovalChangesTheSecretItAgreedTo(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBoundSecret(ctx, t, svc, "github", "www.github.com", 3600)
	req := createAgentRequest(ctx, t, svc)

	approved, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{
		SecretId:                 serverapi.NewOptString(secret.ID),
		GrantTTLSeconds:          serverapi.NewOptInt64(7200),
		SecretHost:               serverapi.NewOptString("github.com"),
		SecretMaxGrantTTLSeconds: serverapi.NewOptInt64(7200),
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	stored, err := st.GetSecret(ctx, "project-1", secret.ID)
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if stored.Host != "github.com" || stored.MaxGrantTTL != 7200 {
		t.Fatalf("secret = %s, limit %d; want bound to github.com with a 7200s limit", stored.Host, stored.MaxGrantTTL)
	}
	if approved.Status != model.SecretRequestStatusApproved || approved.GrantID == "" {
		t.Fatalf("request = %#v, want approved with a grant", approved)
	}
}

// A refused approval leaves the secret as it was, and the request pending: a
// variable already holding another credential in the discobox is not taken
// over by approving.
func TestARefusedApprovalLeavesTheSecretAsItWas(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)

	first := createBoundSecret(ctx, t, svc, "first", "", 0)
	if _, err := svc.ApproveSecretRequest(ctx, "project-1", createAgentRequest(ctx, t, svc).ID, services.ApproveSecretRequestBody{
		SecretId: serverapi.NewOptString(first.ID),
	}); err != nil {
		t.Fatalf("approve first: %v", err)
	}
	secret := createBoundSecret(ctx, t, svc, "github", "www.github.com", 3600)
	req, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID, Name: "github", EnvVar: serverapi.NewOptString("GITHUB_TOKEN"), Hosts: []string{"api.github.com"},
		Justification: serverapi.NewOptString("again"), GrantTTLSeconds: serverapi.NewOptInt64(60),
	})
	if err != nil {
		t.Fatalf("ask again: %v", err)
	}

	_, err = svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{
		SecretId:                 serverapi.NewOptString(secret.ID),
		GrantTTLSeconds:          serverapi.NewOptInt64(7200),
		SecretHost:               serverapi.NewOptString("github.com"),
		SecretMaxGrantTTLSeconds: serverapi.NewOptInt64(7200),
	})
	if !isStatus(err, http.StatusConflict) || !strings.Contains(err.Error(), "another credential") {
		t.Fatalf("approve = %v, want the binding conflict", err)
	}
	stored, err := st.GetSecret(ctx, "project-1", secret.ID)
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if stored.Host != "www.github.com" || stored.MaxGrantTTL != 3600 {
		t.Fatalf("secret = %s, limit %d; want it as it was", stored.Host, stored.MaxGrantTTL)
	}
	if grants, _ := st.ListSecretGrants(ctx, "project-1", secret.ID); len(grants) != 0 {
		t.Fatalf("grants = %#v, want none left behind", grants)
	}
	if pending, _ := st.GetSecretRequest(ctx, "project-1", req.ID); pending == nil || pending.Status != model.SecretRequestStatusPending {
		t.Fatalf("request = %+v, want still pending", pending)
	}
}

// A variable already bound to the secret approved keeps its binding and
// sentinel: approving again delivers nothing new and swaps nothing.
func TestApprovingAVariableAlreadyBoundToTheSecretKeepsItsSentinel(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	if _, err := svc.ApproveSecretRequest(ctx, "project-1", createAgentRequest(ctx, t, svc).ID, services.ApproveSecretRequestBody{
		SecretId: serverapi.NewOptString(secret.ID),
	}); err != nil {
		t.Fatalf("approve first: %v", err)
	}
	before, _ := st.ListSandboxSecrets(ctx, "project-1", testSandboxID)
	req, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID, Name: "github", EnvVar: serverapi.NewOptString("GITHUB_TOKEN"), Hosts: []string{"github.com"},
	})
	if err != nil {
		t.Fatalf("ask again: %v", err)
	}
	if _, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(secret.ID)}); err != nil {
		t.Fatalf("approve again: %v", err)
	}
	after, _ := st.ListSandboxSecrets(ctx, "project-1", testSandboxID)
	if len(before) != 1 || len(after) != 1 || after[0].Sentinel != before[0].Sentinel {
		t.Fatalf("bindings %#v became %#v, want the one binding kept", before, after)
	}
}

// A discobox answering the inbox approves with the secret as it is: its role
// changes no secret, and an approval is no way around that.
func TestADiscoboxCannotChangeASecretByApproving(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBoundSecret(ctx, t, svc, "github", "www.github.com", 0)
	req := createAgentRequest(ctx, t, svc)
	// It holds a delegation of the secret, so the refusal is about changing
	// it and not about what it may hand on.
	delegate(t, st, secret, "api.github.com", time.Hour)

	_, err := svc.ApproveSecretRequest(asLead(), "project-1", req.ID, services.ApproveSecretRequestBody{
		SecretId:   serverapi.NewOptString(secret.ID),
		SecretHost: serverapi.NewOptString(""),
	})
	if err == nil || !strings.Contains(err.Error(), "a person changes") {
		t.Fatalf("approve = %v, want a discobox refused a change to the secret", err)
	}
	if stored, _ := st.GetSecret(ctx, "project-1", secret.ID); stored == nil || stored.Host != "www.github.com" {
		t.Fatalf("secret = %#v, want its binding kept", stored)
	}
}

func createBoundSecret(ctx context.Context, t *testing.T, svc *resourcesecrets.Service, name, host string, limit int64) *model.Secret {
	t.Helper()
	body := services.CreateSecretBody{
		Name:  name,
		Type:  serverapi.CreateSecretBodyTypeToken,
		Value: serverapi.SecretValue{Token: serverapi.NewOptString("ghp_realrealrealrealrealrealrealreal12")},
	}
	if host != "" {
		body.Host = serverapi.NewOptString(host)
	}
	if limit > 0 {
		body.MaxGrantTTLSeconds = serverapi.NewOptInt64(limit)
	}
	secret, err := svc.CreateSecret(ctx, "project-1", body)
	if err != nil {
		t.Fatalf("create secret: %v", err)
	}
	return secret
}

// A discobox listing requests sees only those filed by discoboxes it created;
// a person sees every one (ADR 26-09-30-782 §2).
func TestADiscoboxListsOnlyItsOwnDiscoboxesRequests(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	if err := st.CreateSandbox(ctx, &model.Sandbox{ID: "sbx-other", ProjectID: "project-1", Name: "other", PoolID: testPoolID}); err != nil {
		t.Fatalf("create other: %v", err)
	}
	for id, sandbox := range map[string]string{"sreq-worker": testSandboxID, "sreq-other": "sbx-other"} {
		if err := st.CreateSecretRequest(ctx, &model.SecretRequest{
			ID: id, ProjectID: "project-1", SandboxID: sandbox, RequestedBy: "agent:" + sandbox,
			Type: "token", Status: model.SecretRequestStatusPending,
		}); err != nil {
			t.Fatalf("create request %s: %v", id, err)
		}
	}
	owned, err := svc.ListSecretRequests(asLead(), "project-1", "")
	if err != nil || len(owned) != 1 || owned[0].ID != "sreq-worker" {
		t.Fatalf("listed as the lead = %+v, %v; want only its worker's request", owned, err)
	}
	all, err := svc.ListSecretRequests(ctx, "project-1", "")
	if err != nil || len(all) != 2 {
		t.Fatalf("listed as a person = %d, %v; want both", len(all), err)
	}
}

// An approval that names no lifetime grants what the agent asked for, else an
// hour — what the window opens on — and never past the secret's limit, so
// approving needs nothing read first.
func TestAnApprovalNamingNoLifetimeGrantsWhatWasAsked(t *testing.T) {
	for _, tc := range []struct {
		name         string
		asked, limit int64
		want         time.Duration
	}{
		{"what was asked", 7200, 86400, 2 * time.Hour},
		{"an hour when nothing was asked", 0, 86400, time.Hour},
		{"within the secret's limit", 7200, 600, 10 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testPrincipalContext()
			svc, st := newAgentCredentialService(t)
			secret := createBoundSecret(ctx, t, svc, "github", "", tc.limit)
			req, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
				SandboxId: testSandboxID, Name: "github", EnvVar: serverapi.NewOptString("GITHUB_TOKEN"), Hosts: []string{"api.github.com"},
				GrantTTLSeconds: serverapi.NewOptInt64(tc.asked),
			})
			if err != nil {
				t.Fatalf("create request: %v", err)
			}
			approved, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(secret.ID)})
			if err != nil {
				t.Fatalf("approve: %v", err)
			}
			grant, err := st.GetSecretGrant(ctx, "project-1", approved.GrantID)
			if err != nil || grant.ExpiresAt == nil {
				t.Fatalf("grant = %+v, %v; want one that expires", grant, err)
			}
			if got := time.Until(*grant.ExpiresAt); got > tc.want || got < tc.want-time.Minute {
				t.Fatalf("expires in %v, want %v", got, tc.want)
			}
		})
	}
}
