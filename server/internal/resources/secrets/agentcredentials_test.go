package secrets_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/agentcreds"
	serverapi "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	resourcesecrets "github.com/discobox-ai/discobox/server/internal/resources/secrets"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"github.com/discobox-ai/discobox/wellknown"
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

// How long the agent asks to keep the credential is kept with the ask, for the
// approval to open on. It is not held against any secret's limit here — which
// secret answers is the approval's choice — but a negative one is refused.
func TestAgentCredentialRequestRecordsTheLifetimeAskedFor(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)

	created, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId:       testSandboxID,
		Name:            "github",
		EnvVar:          "GITHUB_TOKEN",
		Host:            "api.github.com",
		Uses:            []apimodel.SecretUse{{Description: "open a pull request"}},
		GrantTTLSeconds: serverapi.NewOptInt64(4 * 3600),
	})
	if err != nil {
		t.Fatalf("create credential request: %v", err)
	}
	stored, _, err := svc.GetSandboxCredentialRequest(ctx, testPoolID, testSandboxID, created.ID)
	if err != nil {
		t.Fatalf("get credential request: %v", err)
	}
	if stored.GrantTTL != 4*3600 {
		t.Fatalf("stored lifetime = %d, want the 14400 seconds asked for", stored.GrantTTL)
	}

	// Bounded on both sides. The ceiling is the half that matters: an ask
	// nobody bounded is shown to a human as the answer already chosen, so a
	// ten-year one — or one large enough to overflow the duration the window
	// converts it to, landing back at the zero that means forever — would be a
	// keystroke from a credential that outlives the project.
	for _, ask := range []int64{-1, int64(agentcreds.MaxGrantTTLSeconds) + 1, 18446744074} {
		_, err = svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
			SandboxId:       testSandboxID,
			Name:            "npm",
			EnvVar:          "NPM_TOKEN",
			Host:            "registry.npmjs.org",
			Uses:            []apimodel.SecretUse{{Description: "publish the package"}},
			GrantTTLSeconds: serverapi.NewOptInt64(ask),
		})
		if err == nil || !strings.Contains(err.Error(), "1 second to") {
			t.Fatalf("asking for %d: err = %v, want it refused as outside what may be asked for", ask, err)
		}
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
		SecretId: serverapi.NewOptString(secret.ID),
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

// An agent may ask to delegate a credential rather than use it. Approving that
// ask mints a delegation grant, which binds nothing: there is nothing for the
// asking discobox to take, and it lists nothing it could run with.
func TestApprovingAnAskToDelegateMintsADelegationGrant(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)

	req, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID,
		Name:      "github",
		EnvVar:    "GITHUB_TOKEN",
		Host:      "api.github.com",
		Uses:      []apimodel.SecretUse{{Description: "triage issues on discobox-ai/discobox"}},
		Purpose:   serverapi.NewOptCreateSandboxCredentialRequestBodyPurpose(serverapi.CreateSandboxCredentialRequestBodyPurposeDelegate),
	})
	if err != nil {
		t.Fatalf("create credential request: %v", err)
	}
	if req.Purpose != model.SecretGrantPurposeDelegate {
		t.Fatalf("request purpose = %q, want the delegation asked for", req.Purpose)
	}

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
	if grant.Purpose != model.SecretGrantPurposeDelegate || len(grant.Uses) != 1 {
		t.Fatalf("grant = %s with uses %#v, want a delegation grant carrying the one use", grant.Purpose, grant.Uses)
	}
	all, err := st.ListSandboxSecrets(ctx, "project-1", testSandboxID)
	if err != nil {
		t.Fatalf("list assignments: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("assignments = %#v, want none; a delegation grant binds nothing", all)
	}
	credentials, err := svc.ListSandboxCredentials(ctx, testPoolID, testSandboxID)
	if err != nil {
		t.Fatalf("list credentials: %v", err)
	}
	if len(credentials) != 0 {
		t.Fatalf("credentials = %#v, want none; a delegation grant authorizes nothing its holder runs", credentials)
	}
}

// An ask to delegate is not a retry of an ask to use the same credential, so
// it is its own question rather than folded into the open one.
func TestAnAskToDelegateIsNotAnAskToUse(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)

	use := createAgentRequest(ctx, t, svc)
	delegate, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID,
		Name:      "github",
		EnvVar:    "GITHUB_TOKEN",
		Host:      "api.github.com",
		Uses:      []apimodel.SecretUse{{Description: "open a pull request"}},
		Purpose:   serverapi.NewOptCreateSandboxCredentialRequestBodyPurpose(serverapi.CreateSandboxCredentialRequestBodyPurposeDelegate),
	})
	if err != nil {
		t.Fatalf("create credential request: %v", err)
	}
	if delegate.ID == use.ID {
		t.Fatalf("ask to delegate reused the open ask to use, %s", use.ID)
	}
	if use.Purpose != model.SecretGrantPurposeUse {
		t.Fatalf("ask naming no purpose = %q, want use", use.Purpose)
	}
}

// The row a credential's own use call writes must resolve back to the grant
// it belongs to, so "every use of one grant" is an ordinary query and not a
// join nobody can make (ADR 0091 §2).
func TestRecordCredentialVerdictResolvesTheGrantFromTheUseID(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	req := createAgentRequest(ctx, t, svc)
	approved, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(secret.ID)})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	grant, err := st.GetSecretGrant(ctx, "project-1", approved.GrantID)
	if err != nil {
		t.Fatalf("get grant: %v", err)
	}
	useID := grant.Uses[0].UseID

	err = svc.RecordCredentialVerdict(ctx, testPoolID, services.RecordCredentialVerdictBody{
		SandboxId: testSandboxID,
		UseId:     useID,
		Command:   []string{"gh", "pr", "create", "--fill"},
		Verdict: apimodel.AgentCredentialVerdict{
			Allow:     true,
			Reason:    serverapi.NewOptString("opens a PR against the approved repo"),
			Role:      "judge",
			Prompt:    "Approved use: open a pull request\n...",
			LatencyMs: serverapi.NewOptInt64(842),
		},
		Volunteered: false,
	})
	if err != nil {
		t.Fatalf("record verdict: %v", err)
	}

	rows, err := st.ListCredentialVerdicts(ctx, "project-1", store.CredentialVerdictFilter{SandboxID: testSandboxID})
	if err != nil {
		t.Fatalf("list verdicts: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("verdicts = %#v, want exactly one", rows)
	}
	row := rows[0]
	if row.GrantID != grant.ID {
		t.Fatalf("grantId = %q, want it resolved to %q from the use ID", row.GrantID, grant.ID)
	}
	if !row.Allow || row.Reason == "" || row.Role != "judge" || row.LatencyMS != 842 {
		t.Fatalf("row = %#v, want the verdict recorded verbatim", row)
	}
	if len(row.Command) != 4 || row.Command[0] != "gh" {
		t.Fatalf("command = %#v, want the declared argv kept in order", row.Command)
	}
	if row.Volunteered {
		t.Fatal("volunteered = true, want false: this verdict rode an issued credential's own use call")
	}
}

// A revoked or unknown use ID must not turn a verdict write into a failure:
// the row is still complete evidence about the command without a grant to
// join it to (ADR 0091 §2's best-effort resolution).
func TestRecordCredentialVerdictSurvivesAnUnresolvableUseID(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)

	err := svc.RecordCredentialVerdict(ctx, testPoolID, services.RecordCredentialVerdictBody{
		SandboxId: testSandboxID,
		UseId:     "use_gone",
		Verdict: apimodel.AgentCredentialVerdict{
			Allow:  false,
			Role:   "judge",
			Prompt: "...",
		},
		Volunteered: true,
	})
	if err != nil {
		t.Fatalf("record verdict: %v", err)
	}
	rows, err := st.ListCredentialVerdicts(ctx, "project-1", store.CredentialVerdictFilter{SandboxID: testSandboxID})
	if err != nil {
		t.Fatalf("list verdicts: %v", err)
	}
	if len(rows) != 1 || rows[0].GrantID != "" {
		t.Fatalf("rows = %#v, want one row with no grant resolved", rows)
	}
	if !rows[0].Volunteered {
		t.Fatal("volunteered = false, want true: the judge refused before any use call, so this is the only record of it")
	}
}

// A verdict is scoped to the sandbox it was recorded for, same as every other
// broker call — a pool may not write a row for a sandbox it does not host.
func TestRecordCredentialVerdictRefusesASandboxTheCallingPoolDoesNotHost(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)

	err := svc.RecordCredentialVerdict(ctx, "some-other-pool", services.RecordCredentialVerdictBody{
		SandboxId:   testSandboxID,
		UseId:       "use_1",
		Verdict:     apimodel.AgentCredentialVerdict{Allow: true, Role: "judge", Prompt: "..."},
		Volunteered: false,
	})
	if err == nil {
		t.Fatal("a pool recorded a verdict for a sandbox it does not host")
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
		SecretId: serverapi.NewOptString(secret.ID),
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
	if _, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(secret.ID)}); err != nil {
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

	if _, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(secret.ID)}); err != nil {
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
	approved, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(secret.ID)})
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
		Type:  serverapi.CreateSecretBodyTypeToken,
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

// A host is matched against what the proxy observed, which it reports
// lowercased and without a port. A grant stored any other way is one nothing
// can ever match, and the symptom — a credential that behaves as though it were
// revoked — says nothing about the typo that caused it.
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
				Host:     serverapi.NewOptString(tc.approved),
			})
			if err != nil {
				t.Fatalf("approve: %v", err)
			}
			grant, err := st.GetSecretGrant(ctx, "project-1", approved.GrantID)
			if err != nil {
				t.Fatalf("get grant: %v", err)
			}
			if grant.Host != "api.github.com" {
				t.Fatalf("grant host = %q, want the host as the proxy reports it", grant.Host)
			}
			// The point of normalizing: the grant this mints actually resolves.
			if _, err := st.FindLiveGrant(ctx, "project-1", secret.ID, "api.github.com",
				[]store.GrantScope{{Scope: model.SecretGrantScopeSandbox, ScopeKey: testSandboxID}}); err != nil {
				t.Fatalf("the minted grant does not match the observed host: %v", err)
			}
		})
	}
}

// What the approver agreed to change on the secret — its binding, its limit —
// is written with the grant or not at all. A binding that sits outside the ask
// and a limit shorter than the lifetime are both fixed by the approval itself.
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

// A refused approval leaves the secret as it was: the binding it would have
// widened and the limit it would have raised are not left behind by an
// approval that never happened.
func TestARefusedApprovalLeavesTheSecretAsItWas(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)

	// GITHUB_TOKEN is already bound to another credential in this discobox,
	// which refuses the second approval after the grant would be minted.
	first := createBoundSecret(ctx, t, svc, "first", "", 0)
	if _, err := svc.ApproveSecretRequest(ctx, "project-1", createAgentRequest(ctx, t, svc).ID, services.ApproveSecretRequestBody{
		SecretId: serverapi.NewOptString(first.ID),
	}); err != nil {
		t.Fatalf("approve first: %v", err)
	}
	secret := createBoundSecret(ctx, t, svc, "github", "www.github.com", 3600)
	req := createAgentRequest(ctx, t, svc)

	_, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{
		SecretId:                 serverapi.NewOptString(secret.ID),
		GrantTTLSeconds:          serverapi.NewOptInt64(7200),
		SecretHost:               serverapi.NewOptString("github.com"),
		SecretMaxGrantTTLSeconds: serverapi.NewOptInt64(7200),
	})
	if err == nil || !strings.Contains(err.Error(), "different secret") {
		t.Fatalf("approve = %v, want the binding conflict", err)
	}
	stored, err := st.GetSecret(ctx, "project-1", secret.ID)
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if stored.Host != "www.github.com" || stored.MaxGrantTTL != 3600 {
		t.Fatalf("secret = %s, limit %d; want it as it was, bound to www.github.com with a 3600s limit", stored.Host, stored.MaxGrantTTL)
	}
	grants, err := st.ListSecretGrants(ctx, "project-1", secret.ID)
	if err != nil {
		t.Fatalf("list grants: %v", err)
	}
	if len(grants) != 0 {
		t.Fatalf("grants = %#v, want none left behind", grants)
	}
	pending, err := st.GetSecretRequest(ctx, "project-1", req.ID)
	if err != nil {
		t.Fatalf("get request: %v", err)
	}
	if pending.Status != model.SecretRequestStatusPending {
		t.Fatalf("request status = %q, want still pending", pending.Status)
	}
}

// A discobox answering the inbox approves with the secret as it is: its role
// changes no secret, and an approval is no way around that.
func TestADiscoboxCannotChangeASecretByApproving(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBoundSecret(ctx, t, svc, "github", "www.github.com", 0)
	req := createAgentRequest(ctx, t, svc)

	asSandbox := auth.WithPrincipal(context.Background(), auth.Principal{
		Type: auth.PrincipalTypeSandbox, SandboxID: "sbx-lead", UserID: "user-1",
	})
	_, err := svc.ApproveSecretRequest(asSandbox, "project-1", req.ID, services.ApproveSecretRequestBody{
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

// A use is named to the judge only when the use, the credential, the discobox
// and the destination all still belong to one live grant. What that check
// reads is the current grant, not what the activation was minted against, so a
// sentence edited at approval time is the sentence the request is judged by.
func TestApprovedUseNamesWhatTheGrantSaysNow(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
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
	if len(grant.Uses) != 1 || grant.Uses[0].UseID == "" {
		t.Fatalf("grant uses = %+v, want one with an ID", grant.Uses)
	}
	useID := grant.Uses[0].UseID

	use, err := svc.ApprovedUse(ctx, testPoolID, testSandboxID, useID, "api.github.com")
	if err != nil {
		t.Fatalf("ApprovedUse() error = %v", err)
	}
	if use.Purpose != "open a pull request" {
		t.Fatalf("purpose = %q, want the sentence somebody approved", use.Purpose)
	}
	if use.Host != "api.github.com" || use.Credential != "github" {
		t.Fatalf("use = %+v, want the grant's host and the credential's name", use)
	}

	// A destination the grant does not cover is refused rather than answered
	// with a use that says nothing about it.
	if _, err := svc.ApprovedUse(ctx, testPoolID, testSandboxID, useID, "evil.example"); err == nil {
		t.Fatal("ApprovedUse() named a use for a host the grant does not cover")
	}
	// And a use nobody granted is refused whatever else is live.
	if _, err := svc.ApprovedUse(ctx, testPoolID, testSandboxID, "use_nobodys", "api.github.com"); err == nil {
		t.Fatal("ApprovedUse() named a use that does not exist")
	}
	// A pool may only ever speak for its own discoboxes.
	if _, err := svc.ApprovedUse(ctx, "pool-somebody-else", testSandboxID, useID, "api.github.com"); err == nil {
		t.Fatal("ApprovedUse() answered a pool that does not host the discobox")
	}
}

// The gate is the case nothing else covers: a sandbox's calls to the discobox
// API are judged like any other use-scoped request, and the host they go to is
// the well-known one. If that did not satisfy the grant's own host check, every
// gate call would be refused the moment judging is turned on — and the pool
// side cannot show it, because there the control plane is a stub.
func TestApprovedUseCoversTheDiscoboxAPIHost(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	known, ok := wellknown.Lookup(wellknown.DiscoboxSandbox)
	if !ok {
		t.Fatal("the discobox API credential is not in the registry")
	}
	req, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId:     testSandboxID,
		ID:            serverapi.NewOptString(wellknown.DiscoboxSandbox),
		Justification: serverapi.NewOptString("the task asks me to start a worker"),
		Uses:          []apimodel.SecretUse{{Description: "create a discobox to run the tests in"}},
	})
	if err != nil {
		t.Fatalf("ask for the discobox API credential: %v", err)
	}
	// Approved without naming a secret: this credential is the server's own to
	// mint, which is the whole point of it being well known.
	approved, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	grant, err := st.GetSecretGrant(ctx, "project-1", approved.GrantID)
	if err != nil {
		t.Fatalf("get grant: %v", err)
	}

	use, err := svc.ApprovedUse(ctx, testPoolID, testSandboxID, grant.Uses[0].UseID, known.Host())
	if err != nil {
		t.Fatalf("ApprovedUse() for %s error = %v", known.Host(), err)
	}
	if use.Purpose != "create a discobox to run the tests in" {
		t.Fatalf("purpose = %q, want the approved sentence", use.Purpose)
	}
}

// A host trust's uses are approved uses too. Every request to a pinned host is
// judged against them, so the judge has to be able to name one — and they live
// on the trust rather than on a credential's grant, which is why naming one is
// a second lookup. Without it every request to every pinned host is refused
// for a use nobody can find.
func TestApprovedUseNamesAHostTrustsUse(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	// Through the path a person's approval takes, so the row is the one the
	// real flow writes.
	// Approved the way a person approves one, so the use ID the judge is asked
	// about is the one approval minted rather than one this test chose.
	trust := approveHostTrust(ctx, t, svc, st, testSandboxID, "kube.internal:6443",
		"read the pods in the prod namespace", time.Hour)
	useID := trust.Uses[0].UseID
	if useID == "" {
		t.Fatal("approval minted no use ID")
	}

	use, err := svc.ApprovedUse(ctx, testPoolID, testSandboxID, useID, "kube.internal")
	if err != nil {
		t.Fatalf("ApprovedUse() error = %v", err)
	}
	if use.Purpose != "read the pods in the prod namespace" {
		t.Fatalf("purpose = %q, want the sentence the trust was approved with", use.Purpose)
	}
	if use.Host != "kube.internal" {
		t.Fatalf("host = %q, want the pinned endpoint's host", use.Host)
	}
	// A pin is for one endpoint, so it does not reach beneath it the way a
	// credential's grant does.
	if _, err := svc.ApprovedUse(ctx, testPoolID, testSandboxID, useID, "api.kube.internal"); err == nil {
		t.Fatal("ApprovedUse() named a trust's use for a host it is not pinned to")
	}
}

// approveHostTrust puts a trust ask and approves it the way a person does, so
// the uses carry the IDs approval mints.
func approveHostTrust(ctx context.Context, t *testing.T, svc *resourcesecrets.Service, st *store.Store,
	sandboxID, host, use string, ttl time.Duration,
) *model.HostTrust {
	t.Helper()
	req := &model.HostTrustRequest{
		ProjectID: "project-1", SandboxID: sandboxID, RequestedBy: "agent:" + sandboxID,
		Host: host, Status: model.HostTrustRequestStatusPending,
		Uses:          []model.SecretUse{{Description: use}},
		ObservedChain: []model.ObservedCertificate{{SHA256: strings.Repeat("cd", 32), SPKISHA256: strings.Repeat("ef", 32)}},
	}
	if err := st.CreateHostTrustRequest(ctx, req); err != nil {
		t.Fatalf("create trust request: %v", err)
	}
	if _, err := svc.ApproveTrustRequest(ctx, "project-1", req.ID, services.ApproveTrustRequestBody{
		GrantTTLSeconds: serverapi.NewOptInt64(int64(ttl.Seconds())),
	}); err != nil {
		t.Fatalf("approve trust request: %v", err)
	}
	trusts, err := st.ListLiveSandboxHostTrusts(ctx, "project-1", sandboxID, time.Now().UTC())
	if err != nil || len(trusts) == 0 {
		t.Fatalf("list host trusts: %v (%d)", err, len(trusts))
	}
	return &trusts[len(trusts)-1]
}

// A trust names a use only while it is this discobox's, and only while it is
// live. Neither had a test, which is the kind of guarantee that stays quietly
// true until it is not.
func TestATrustNamesNoUseOfAnotherDiscoboxOrOnceItLapses(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)

	// A second discobox on the same pool, with its own trust for the same
	// host. Neither may name the other's use.
	other := &model.Sandbox{ID: "sbx_other", ProjectID: "project-1", Name: "other", PoolID: testPoolID}
	if err := st.CreateSandbox(ctx, other); err != nil {
		t.Fatalf("create the other discobox: %v", err)
	}
	theirs := approveHostTrust(ctx, t, svc, st, other.ID, "kube.internal:6443", "read their pods", time.Hour)
	if _, err := svc.ApprovedUse(ctx, testPoolID, testSandboxID, theirs.Uses[0].UseID, "kube.internal"); err == nil {
		t.Fatal("ApprovedUse() named another discobox's trust use")
	}
	// And it names it for the discobox it belongs to, so the refusal above is
	// about whose it is rather than about the trust being unusable.
	if _, err := svc.ApprovedUse(ctx, testPoolID, other.ID, theirs.Uses[0].UseID, "kube.internal"); err != nil {
		t.Fatalf("ApprovedUse() for the discobox that holds the trust: %v", err)
	}

	// Lapsing is the store's to enforce, and ApprovedUse reads it with the
	// clock rather than a time it chooses, so this is where it can be shown.
	live, err := st.ListLiveSandboxHostTrusts(ctx, "project-1", other.ID, theirs.ExpiresAt.Add(time.Second))
	if err != nil {
		t.Fatalf("list host trusts: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("a trust past its expiry is still listed live: %+v", live)
	}
}
