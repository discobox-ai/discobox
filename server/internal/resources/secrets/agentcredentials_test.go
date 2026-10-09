package secrets_test

import (
	"context"
	"slices"
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
	if req.EnvName != "GITHUB_TOKEN" || !slices.Equal(req.Hosts, []string{"api.github.com"}) || req.Justification == "" {
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

// An ask for other uses of the same credential is its own request. Folding it
// into the open one would answer it with that request's ID while dropping the
// uses it asked for: the agent believes it asked, and the person approving
// never sees it.
func TestAgentCredentialRequestForOtherUsesIsItsOwn(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	ask := func(ttl int64, uses ...string) *model.SecretRequest {
		t.Helper()
		body := services.CreateSandboxCredentialRequestBody{
			SandboxId: testSandboxID, Name: "github", EnvVar: "GITHUB_TOKEN", Hosts: []string{"api.github.com"},
		}
		for _, use := range uses {
			body.Uses = append(body.Uses, apimodel.SecretUse{Description: use})
		}
		if ttl > 0 {
			body.GrantTTLSeconds = serverapi.NewOptInt64(ttl)
		}
		created, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, body)
		if err != nil {
			t.Fatalf("create credential request: %v", err)
		}
		return created
	}

	first := ask(0, "push the branch issue-43")
	other := ask(0, "fetch main", "mark the pull request ready")
	if other.ID == first.ID {
		t.Fatalf("an ask for other uses was answered with the open request %s", first.ID)
	}
	if len(other.Uses) != 2 || other.Uses[1].Description != "mark the pull request ready" {
		t.Fatalf("uses = %#v, want the ones this ask named", other.Uses)
	}
	if longer := ask(3600, "push the branch issue-43"); longer.ID == first.ID {
		t.Fatal("an ask for a different lifetime was answered with the open request")
	}
	if again := ask(0, "fetch main", "mark the pull request ready"); again.ID != other.ID {
		t.Fatalf("a retry created %s, want the open request %s it repeats", again.ID, other.ID)
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
		Hosts:           []string{"api.github.com"},
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
			Hosts:           []string{"registry.npmjs.org"},
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
	if !slices.Equal(grant.Hosts, []string{"api.github.com"}) {
		t.Fatalf("grant host = %q, want the requested host; this flow never mints a wildcard", grant.Hosts)
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
		Hosts:     []string{"api.github.com"},
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
		Hosts:     []string{"api.github.com"},
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

// A command is judged against the use it names as the live grant says it
// now, with the grant's hosts and credential, so the verdict joins back to the
// grant (ADR 26-09-22-838 §3).
func TestACommandsUseIsReadFromTheLiveGrant(t *testing.T) {
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

	use, err := svc.ApprovedCredentialUse(ctx, testPoolID, testSandboxID, grant.Uses[0].UseID)
	if err != nil {
		t.Fatalf("ApprovedCredentialUse() error = %v", err)
	}
	if use.GrantID != grant.ID || use.Purpose != grant.Uses[0].Description || use.Host != strings.Join(grant.Hosts, ", ") {
		t.Fatalf("use = %#v, want the grant's use, host and ID", use)
	}
	if _, err := svc.ApprovedCredentialUse(ctx, testPoolID, testSandboxID, "use_gone"); err == nil {
		t.Fatal("a use no live grant has was approved")
	}
}

// Like every broker call, a pool may only ask about a sandbox it hosts.
func TestACommandsUseIsNotReadForASandboxTheCallingPoolDoesNotHost(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)

	if _, err := svc.ApprovedCredentialUse(ctx, "some-other-pool", testSandboxID, "use_1"); err == nil {
		t.Fatal("a pool read a use for a sandbox it does not host")
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
		Hosts:     []string{"api.github.com"},
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
	if status := services.AgentCredentialRequestStatus(pending, grant); status != "pending" {
		t.Fatalf("status = %q, want pending", status)
	}

	if _, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(secret.ID)}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	settled, grant, err := svc.GetSandboxCredentialRequest(ctx, testPoolID, testSandboxID, req.ID)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if status := services.AgentCredentialRequestStatus(settled, grant); status != "granted" {
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
	if status := services.AgentCredentialRequestStatus(settled, grant); status != "denied" {
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
		Hosts:         []string{"api.github.com"},
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
	if err == nil || !strings.Contains(err.Error(), "another secret") {
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
	// It holds a delegation of the secret, so the refusal is about changing
	// it and not about what it may hand on.
	delegate(t, st, secret, "api.github.com", time.Hour)
	svc.SetJudge(&delegationJudge{allow: true})

	asSandbox := auth.WithPrincipal(context.Background(), auth.Principal{
		Type: auth.PrincipalTypeSandbox, SandboxID: "sbx-lead", ProjectID: "project-1", UserID: "user-1",
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

	// And a trust that has lapsed names nothing. The service will not mint one
	// already expired, so this writes it the way approval does and asks
	// ApprovedUse itself — asserting the store's predicate instead would pin
	// the one thing that was never at risk.
	expired := &model.HostTrustRequest{
		ProjectID: "project-1", SandboxID: testSandboxID, RequestedBy: "agent:" + testSandboxID,
		Host: "old.internal:6443", Status: model.HostTrustRequestStatusPending,
		Uses: []model.SecretUse{{Description: "read the old thing"}},
	}
	if err := st.CreateHostTrustRequest(ctx, expired); err != nil {
		t.Fatalf("create trust request: %v", err)
	}
	lapsed := &model.HostTrust{
		ProjectID: "project-1", SandboxID: testSandboxID, Host: "old.internal:6443",
		Pin:       model.TrustPin{Kind: model.TrustPinKindLeafSPKI, SHA256: strings.Repeat("ab", 32)},
		Uses:      []model.SecretUse{{UseID: "use_lapsed", Description: "read the old thing"}},
		ExpiresAt: time.Now().Add(-time.Minute),
		GrantedBy: "user-1", RequestID: expired.ID,
	}
	if err := st.ApproveHostTrustRequest(ctx, expired, lapsed); err != nil {
		t.Fatalf("write the lapsed trust: %v", err)
	}
	if _, err := svc.ApprovedUse(ctx, testPoolID, testSandboxID, "use_lapsed", "old.internal"); err == nil {
		t.Fatal("ApprovedUse() named a use of a trust that has lapsed")
	}
}

// A discobox listing requests sees only those filed by discoboxes it created;
// a person sees every one (ADR 26-09-30-782 §2).
func TestADiscoboxListsOnlyItsOwnDiscoboxesRequests(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	lead := "sbx-lead"
	if err := st.CreateSandbox(ctx, &model.Sandbox{ID: "sbx-worker", ProjectID: "project-1", Name: "worker", PoolID: testPoolID, CreatedBySandboxID: &lead}); err != nil {
		t.Fatalf("create worker: %v", err)
	}
	for id, sandbox := range map[string]string{"sreq-worker": "sbx-worker", "sreq-other": testSandboxID} {
		if err := st.CreateSecretRequest(ctx, &model.SecretRequest{
			ID: id, ProjectID: "project-1", SandboxID: sandbox, RequestedBy: "agent:" + sandbox,
			Type: "token", Status: model.SecretRequestStatusPending,
		}); err != nil {
			t.Fatalf("create request %s: %v", id, err)
		}
	}
	asLead := auth.WithPrincipal(context.Background(), auth.Principal{
		Type: auth.PrincipalTypeSandbox, SandboxID: lead, ProjectID: "project-1", UserID: "user-1",
	})
	owned, err := svc.ListSecretRequests(asLead, "project-1", "")
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
// approving needs nothing read first (ADR 26-09-30-782 §4).
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
				SandboxId: testSandboxID, Name: "github", EnvVar: "GITHUB_TOKEN", Hosts: []string{"api.github.com"},
				Uses:            []apimodel.SecretUse{{Description: "open a pull request"}},
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
