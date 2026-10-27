package secrets_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	resourcesecrets "github.com/discobox-ai/discobox/server/internal/resources/secrets"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// The lead discobox answering its workers' requests in these tests. It created
// testSandboxID.
const leadID = "sbx-lead"

func asLead() context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{
		Type: auth.PrincipalTypeSandbox, SandboxID: leadID, ProjectID: "project-1", UserID: "user-1",
	})
}

func requireStatus(t *testing.T, err error, want int) {
	t.Helper()
	if !isStatus(err, want) {
		t.Fatalf("err = %v, want status %d", err, want)
	}
}

// delegate gives the lead a delegation grant of secret to host, lapsing after
// lifetime (none when zero), as a person approving its ask to delegate does.
func delegate(t *testing.T, st *store.Store, secret *model.Secret, host string, lifetime time.Duration) *model.SecretGrant {
	t.Helper()
	grant := &model.SecretGrant{
		ProjectID: "project-1", SecretID: secret.ID, Scope: model.SecretGrantScopeSandbox, ScopeKey: leadID,
		Hosts: []string{host}, GrantedBy: "user-1", Purpose: model.SecretGrantPurposeDelegate,
	}
	if lifetime > 0 {
		expires := time.Now().UTC().Add(lifetime)
		grant.ExpiresAt = &expires
	}
	if err := st.CreateSecretGrant(context.Background(), grant); err != nil {
		t.Fatalf("create delegation grant: %v", err)
	}
	return grant
}

// workerRequest is the lead's worker's ask, as discobox-access files it.
func workerRequest(t *testing.T, svc *resourcesecrets.Service, change func(*services.CreateSandboxCredentialRequestBody)) *model.SecretRequest {
	t.Helper()
	body := services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID, Name: "github", EnvVar: serverapi.NewOptString("GITHUB_TOKEN"), Hosts: []string{"api.github.com"},
		Justification: serverapi.NewOptString("read issue 43"),
	}
	if change != nil {
		change(&body)
	}
	req, err := svc.CreateSandboxCredentialRequest(testPrincipalContext(), testPoolID, body)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	return req
}

func approveAsLead(svc *resourcesecrets.Service, req *model.SecretRequest, input services.ApproveSecretRequestBody) (*model.SecretRequest, error) {
	return svc.ApproveSecretRequest(asLead(), "project-1", req.ID, input)
}

// A discobox approves within a delegation grant it holds, with that grant's
// secret — it names none — for no longer than the delegation lasts, and the
// grant records the delegation it was made under (ADR 26-10-27-880 §3). The
// worker is delivered the credential like any other: a binding of the
// variable it named.
func TestADiscoboxApprovesWithinItsDelegation(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBoundSecret(ctx, t, svc, "github", "", 86400)
	delegation := delegate(t, st, secret, "github.com", 30*time.Minute)

	// The agent asked for two hours; the delegation has half an hour left.
	req := workerRequest(t, svc, func(b *services.CreateSandboxCredentialRequestBody) {
		b.GrantTTLSeconds = serverapi.NewOptInt64(7200)
	})
	approved, err := approveAsLead(svc, req, services.ApproveSecretRequestBody{})
	if err != nil {
		t.Fatalf("approve within the delegation: %v", err)
	}
	grant, err := st.GetSecretGrant(ctx, "project-1", approved.GrantID)
	if err != nil {
		t.Fatalf("get grant: %v", err)
	}
	if grant.SecretID != secret.ID || grant.GrantedBy != leadID || grant.Purpose != model.SecretGrantPurposeUse || grant.DelegationID != delegation.ID {
		t.Fatalf("grant = %+v, want a use of the delegated secret, granted by the lead under its delegation", grant)
	}
	if grant.ExpiresAt == nil || time.Until(*grant.ExpiresAt) > 30*time.Minute {
		t.Fatalf("grant expires %v, want no later than the delegation", grant.ExpiresAt)
	}
	bindings, _ := st.ListSandboxSecrets(ctx, "project-1", testSandboxID)
	if len(bindings) != 1 || bindings[0].EnvName != "GITHUB_TOKEN" || bindings[0].SecretID != secret.ID {
		t.Fatalf("bindings = %#v, want the worker's variable bound", bindings)
	}

	// A lifetime the lead names that outlasts the delegation is refused, not
	// shortened: it is what the lead said, and it is not the lead's to give.
	again := workerRequest(t, svc, func(b *services.CreateSandboxCredentialRequestBody) { b.Name = "github-again" })
	_, err = approveAsLead(svc, again, services.ApproveSecretRequestBody{GrantTTLSeconds: serverapi.NewOptInt64(3600)})
	requireStatus(t, err, http.StatusForbidden)
}

// A delegation grant that never lapses bounds nothing in time; the grant takes
// the lifetime it would have from a person.
func TestADelegationThatNeverLapsesBoundsNoLifetime(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBoundSecret(ctx, t, svc, "github", "", 86400)
	delegate(t, st, secret, "github.com", 0)
	req := workerRequest(t, svc, func(b *services.CreateSandboxCredentialRequestBody) {
		b.GrantTTLSeconds = serverapi.NewOptInt64(7200)
	})
	approved, err := approveAsLead(svc, req, services.ApproveSecretRequestBody{})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	grant, _ := st.GetSecretGrant(ctx, "project-1", approved.GrantID)
	if grant == nil || grant.ExpiresAt == nil || time.Until(*grant.ExpiresAt) < 2*time.Hour-time.Minute {
		t.Fatalf("grant = %+v, want the two hours asked for", grant)
	}
}

// What a discobox was not delegated it does not hand on: no delegation at all,
// one for another host, one that has lapsed, or another secret than the one
// it names.
func TestADiscoboxHandsOnNothingItWasNotDelegated(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, st *store.Store, delegated, other *model.Secret)
		input func(other *model.Secret) services.ApproveSecretRequestBody
	}{
		{name: "no delegation"},
		{name: "a delegation to another host", setup: func(t *testing.T, st *store.Store, delegated, _ *model.Secret) {
			delegate(t, st, delegated, "gitlab.com", time.Hour)
		}},
		{name: "a lapsed delegation", setup: func(t *testing.T, st *store.Store, delegated, _ *model.Secret) {
			grant := delegate(t, st, delegated, "github.com", time.Hour)
			lapsed := time.Now().UTC().Add(-time.Minute)
			grant.ExpiresAt = &lapsed
			if err := st.UpdateSecretGrant(context.Background(), grant); err != nil {
				t.Fatalf("lapse delegation: %v", err)
			}
		}},
		{name: "naming a secret it was not delegated",
			setup: func(t *testing.T, st *store.Store, delegated, _ *model.Secret) {
				delegate(t, st, delegated, "github.com", time.Hour)
			},
			input: func(other *model.Secret) services.ApproveSecretRequestBody {
				return services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(other.ID)}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testPrincipalContext()
			svc, st := newAgentCredentialService(t)
			delegated := createBoundSecret(ctx, t, svc, "github", "", 86400)
			other := createBoundSecret(ctx, t, svc, "github-admin", "", 86400)
			if tc.setup != nil {
				tc.setup(t, st, delegated, other)
			}
			input := services.ApproveSecretRequestBody{}
			if tc.input != nil {
				input = tc.input(other)
			}
			_, err := approveAsLead(svc, workerRequest(t, svc, nil), input)
			requireStatus(t, err, http.StatusForbidden)
		})
	}
}

// Delegated more than one secret that fits, a discobox names which — and only
// among them.
func TestADiscoboxDelegatedTwoSecretsNamesWhich(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	first := createBoundSecret(ctx, t, svc, "github", "", 86400)
	second := createBoundSecret(ctx, t, svc, "github-bot", "", 86400)
	delegate(t, st, first, "github.com", time.Hour)
	delegate(t, st, second, "github.com", time.Hour)

	_, err := approveAsLead(svc, workerRequest(t, svc, nil), services.ApproveSecretRequestBody{})
	requireStatus(t, err, http.StatusBadRequest)

	approved, err := approveAsLead(svc, workerRequest(t, svc, nil), services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(second.ID)})
	if err != nil {
		t.Fatalf("approve naming a delegated secret: %v", err)
	}
	if approved.SecretID != second.ID {
		t.Fatalf("approved with %s, want the one named", approved.SecretID)
	}
}

// A discobox never approves a request to delegate, nor one the proxy opened,
// however much it was delegated: both are a person's to answer.
func TestADiscoboxNeverApprovesADelegationOrAReactiveRequest(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBoundSecret(ctx, t, svc, "github", "", 86400)
	delegate(t, st, secret, "github.com", time.Hour)

	toDelegate := workerRequest(t, svc, func(b *services.CreateSandboxCredentialRequestBody) {
		b.EnvVar = serverapi.OptString{}
		b.Purpose = serverapi.NewOptCreateSandboxCredentialRequestBodyPurpose(serverapi.CreateSandboxCredentialRequestBodyPurposeDelegate)
	})
	_, err := approveAsLead(svc, toDelegate, services.ApproveSecretRequestBody{})
	requireStatus(t, err, http.StatusForbidden)

	reactive := &model.SecretRequest{
		ID: "sreq-reactive", ProjectID: "project-1", SandboxID: testSandboxID, RequestedBy: "pool:" + testPoolID,
		Type: "token", Hosts: []string{"api.github.com"}, Status: model.SecretRequestStatusPending,
	}
	if err := st.CreateSecretRequest(ctx, reactive); err != nil {
		t.Fatalf("create reactive request: %v", err)
	}
	_, err = approveAsLead(svc, reactive, services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(secret.ID)})
	requireStatus(t, err, http.StatusForbidden)
}

// Of a discobox's delegations of a secret, an approval is made under the one
// that lets the grant last longest, not the newest: a short-lived delegation
// granted last does not cut short a grant an older one that never lapses
// allows.
func TestADiscoboxApprovesUnderItsLongestLivedDelegation(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBoundSecret(ctx, t, svc, "github", "", 86400)
	lasting := delegate(t, st, secret, "github.com", 0)
	time.Sleep(10 * time.Millisecond) // granted after it, so newest first
	delegate(t, st, secret, "github.com", 5*time.Minute)

	asked := workerRequest(t, svc, func(b *services.CreateSandboxCredentialRequestBody) {
		b.GrantTTLSeconds = serverapi.NewOptInt64(7200)
	})
	approved, err := approveAsLead(svc, asked, services.ApproveSecretRequestBody{})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	grant, _ := st.GetSecretGrant(ctx, "project-1", approved.GrantID)
	if grant == nil || grant.ExpiresAt == nil || time.Until(*grant.ExpiresAt) < 2*time.Hour-time.Minute || grant.DelegationID != lasting.ID {
		t.Fatalf("grant = %+v, want the two hours the lasting delegation allows, made under it", grant)
	}
}

// A discobox listing secrets sees only those it holds a live delegation grant
// of — what it may hand on, and so may name — and a person sees every one.
func TestADiscoboxListsOnlyTheSecretsItWasDelegated(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	delegated := createBoundSecret(ctx, t, svc, "github", "", 86400)
	lapsed := createBoundSecret(ctx, t, svc, "gitlab", "", 86400)
	createBoundSecret(ctx, t, svc, "npm", "", 86400)
	delegate(t, st, delegated, "github.com", time.Hour)
	grant := delegate(t, st, lapsed, "gitlab.com", time.Hour)
	expired := time.Now().UTC().Add(-time.Minute)
	grant.ExpiresAt = &expired
	if err := st.UpdateSecretGrant(ctx, grant); err != nil {
		t.Fatalf("lapse delegation: %v", err)
	}

	listed, err := svc.ListSecrets(asLead(), "project-1")
	if err != nil || len(listed) != 1 || listed[0].ID != delegated.ID {
		t.Fatalf("listed as the lead = %+v, %v; want only the secret it holds a live delegation of", listed, err)
	}
	all, err := svc.ListSecrets(ctx, "project-1")
	if err != nil || len(all) != 3 {
		t.Fatalf("listed as a person = %d, %v; want every secret", len(all), err)
	}
}

// A secret a discobox was not delegated answers its --secret the same as one
// that does not exist, so naming IDs cannot tell it which the project holds.
func TestASecretNotDelegatedAnswersAsIfItDidNotExist(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	delegated := createBoundSecret(ctx, t, svc, "github", "", 86400)
	other := createBoundSecret(ctx, t, svc, "github-admin", "", 86400)
	delegate(t, st, delegated, "github.com", time.Hour)

	for _, named := range []string{other.ID, other.ID[:len(other.ID)-3], "sec_doesnotexist"} {
		_, err := approveAsLead(svc, workerRequest(t, svc, nil), services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(named)})
		requireStatus(t, err, http.StatusForbidden)
	}
	// A prefix of the delegated secret's ID names it.
	if _, err := approveAsLead(svc, workerRequest(t, svc, nil), services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(delegated.ID[:len(delegated.ID)-3])}); err != nil {
		t.Fatalf("approve naming the delegated secret by a prefix: %v", err)
	}
}

// The grants a discobox gives on a create are made under its delegations and
// held to them once more in the create's transaction: one lapsed between
// preparing the grants and storing them refuses the create. Each is bound on
// the new discobox like any credential.
func TestACreatesGrantsAreHeldToTheirDelegationsWhenStored(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBoundSecret(ctx, t, svc, "github", "", 86400)
	delegation := delegate(t, st, secret, "github.com", time.Hour)

	grants, err := svc.PrepareSandboxGrants(asLead(), "project-1", &model.Sandbox{ID: "sbx-new", ProjectID: "project-1"}, []apimodel.SandboxGrant{{
		SecretId: serverapi.NewOptString(secret.ID), EnvVar: serverapi.NewOptString("GH_TOKEN"), Hosts: []string{"api.github.com"},
	}})
	if err != nil {
		t.Fatalf("prepare grants: %v", err)
	}
	if len(grants.Delegations) != 1 || grants.Delegations[0].ID != delegation.ID || grants.Grantor != leadID ||
		grants.Grants[0].DelegationID != delegation.ID {
		t.Fatalf("grants = %+v, want the one made under the lead's delegation", grants)
	}
	if len(grants.Bindings) != 1 || grants.Bindings[0].EnvName != "GH_TOKEN" || grants.Bindings[0].Sentinel == "" {
		t.Fatalf("bindings = %+v, want GH_TOKEN bound", grants.Bindings)
	}
	if grants.Grants[0].ExpiresAt == nil || grants.Grants[0].ExpiresAt.After(*delegation.ExpiresAt) {
		t.Fatalf("grant expires %v, want no later than the delegation", grants.Grants[0].ExpiresAt)
	}
	if err := resourcesecrets.HoldDelegations(ctx, st, grants); err != nil {
		t.Fatalf("hold a live delegation: %v", err)
	}

	lapsed := time.Now().UTC().Add(-time.Minute)
	delegation.ExpiresAt = &lapsed
	if err := st.UpdateSecretGrant(ctx, delegation); err != nil {
		t.Fatalf("lapse delegation: %v", err)
	}
	requireStatus(t, resourcesecrets.HoldDelegations(ctx, st, grants), http.StatusForbidden)
}

// A secret a discobox gives on a create is resolved among what it was
// delegated before anything reads it: one it was not delegated — by full ID
// or by prefix — answers the same as one that does not exist.
func TestACreatesSecretNotDelegatedAnswersAsIfItDidNotExist(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	delegated := createBoundSecret(ctx, t, svc, "github", "", 86400)
	other := createBoundSecret(ctx, t, svc, "github-admin", "api.github.com", 86400)
	delegate(t, st, delegated, "github.com", time.Hour)
	grant := func(secretID string) []apimodel.SandboxGrant {
		return []apimodel.SandboxGrant{{
			SecretId: serverapi.NewOptString(secretID), EnvVar: serverapi.NewOptString("GH_TOKEN"), Hosts: []string{"api.github.com"},
		}}
	}
	for _, named := range []string{other.ID, other.ID[:len(other.ID)-3], "sec_doesnotexist"} {
		_, err := svc.PrepareSandboxGrants(asLead(), "project-1", &model.Sandbox{ID: "sbx-new", ProjectID: "project-1"}, grant(named))
		requireStatus(t, err, http.StatusForbidden)
	}
	if _, err := svc.PrepareSandboxGrants(asLead(), "project-1", &model.Sandbox{ID: "sbx-new", ProjectID: "project-1"}, grant(delegated.ID[:len(delegated.ID)-3])); err != nil {
		t.Fatalf("give the delegated secret named by a prefix: %v", err)
	}
}

// A person giving grants on a create holds no delegation, and the grants name
// none.
func TestAPersonsCreateGrantsAreMadeUnderNoDelegation(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	secret := createBoundSecret(ctx, t, svc, "github", "", 86400)
	grants, err := svc.PrepareSandboxGrants(ctx, "project-1", &model.Sandbox{ID: "sbx-new", ProjectID: "project-1"}, []apimodel.SandboxGrant{{
		SecretId: serverapi.NewOptString(secret.ID), EnvVar: serverapi.NewOptString("GH_TOKEN"), Hosts: []string{"api.github.com"},
	}, {
		SecretId: serverapi.NewOptString(secret.ID), EnvVar: serverapi.NewOptString("GH_TOKEN"), Hosts: []string{"github.com"},
	}})
	if err != nil {
		t.Fatalf("prepare grants: %v", err)
	}
	if len(grants.Grants) != 2 || grants.Grants[0].DelegationID != "" || grants.Delegations[0] != nil || len(grants.Bindings) != 1 {
		t.Fatalf("grants = %+v, want two grants, one binding, and no delegation", grants)
	}
}

// A delegation with less than a second left fits no lifetime: rounded down it
// is zero, which is forever, so it is refused rather than minted as a grant
// that never expires.
func TestADelegationWithNothingLeftHandsNothingOn(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBoundSecret(ctx, t, svc, "github", "", 0)
	delegate(t, st, secret, "github.com", 1500*time.Millisecond)
	req := workerRequest(t, svc, nil)
	time.Sleep(700 * time.Millisecond) // under a second left, still live
	_, err := approveAsLead(svc, req, services.ApproveSecretRequestBody{})
	requireStatus(t, err, http.StatusForbidden)
}
