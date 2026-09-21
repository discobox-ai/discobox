package secrets_test

import (
	"net/http"
	"testing"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/server/internal/model"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// delegationGrant is a person's grant to the test discobox of a GitHub token
// for it to hand on, in the shape delegation requires: one discobox, with uses.
func delegationGrant(secretID string) services.CreateSecretGrantBody {
	return services.CreateSecretGrantBody{
		SecretId: secretID,
		Scope:    serverapi.CreateSecretGrantBodyScopeSandbox,
		ScopeKey: serverapi.NewOptString(testSandboxID),
		Host:     serverapi.NewOptString("github.com"),
		EnvVar:   serverapi.NewOptString("GH_TOKEN"),
		Uses:     serverapi.NewOptNilSecretUseArray([]serverapi.SecretUse{{Description: "push a branch to org/repo"}}),
		Purpose:  serverapi.NewOptCreateSecretGrantBodyPurpose(serverapi.CreateSecretGrantBodyPurposeDelegate),
	}
}

// A grant is for using the credential unless it says otherwise, which is every
// grant there was before delegation.
func TestAGrantIsForUseUnlessItSaysOtherwise(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)

	body := delegationGrant(secret.ID)
	body.Purpose = serverapi.OptCreateSecretGrantBodyPurpose{}
	grant, err := svc.CreateSecretGrant(ctx, "project-1", body)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if grant.Purpose != model.SecretGrantPurposeUse || !grant.MayUse() {
		t.Fatalf("grant = %+v, want a use grant", grant)
	}
}

// A delegation grant is for handing the credential on, and for nothing else:
// it is bound to nothing, offered to nobody, and found by no lookup that hands
// a credential out. A grant is one or the other, so the person approving it is
// agreeing to one thing.
func TestADelegationGrantIsNotTheHoldersToUse(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)

	grant, err := svc.CreateSecretGrant(ctx, "project-1", delegationGrant(secret.ID))
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if grant.Purpose != model.SecretGrantPurposeDelegate || grant.MayUse() {
		t.Fatalf("grant = %+v, want a delegation grant its holder may not use", grant)
	}
	bindings, err := st.ListSandboxSecrets(ctx, "project-1", testSandboxID)
	if err != nil || len(bindings) != 0 {
		t.Fatalf("bindings = %#v, %v; want nothing bound for a holder that may not use it", bindings, err)
	}
	credentials, err := svc.ListSandboxCredentials(ctx, testPoolID, testSandboxID)
	if err != nil || len(credentials) != 0 {
		t.Fatalf("credentials = %#v, %v; want nothing offered to its holder", credentials, err)
	}
	if found, err := st.FindLiveGrant(ctx, "project-1", secret.ID, "github.com",
		[]store.GrantScope{{Scope: model.SecretGrantScopeSandbox, ScopeKey: testSandboxID}}); err == nil {
		t.Fatalf("live grant = %+v, want none: a delegation grant authorizes nothing its holder sends", found)
	}
}

// Delegation is one discobox's, a use at a time; anything else is refused
// rather than minted as something other than was asked for.
func TestADelegationGrantRefusesTheWrongShape(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)

	projectWide := delegationGrant(secret.ID)
	projectWide.Scope = serverapi.CreateSecretGrantBodyScopeProject
	projectWide.ScopeKey = serverapi.OptString{}
	projectWide.Uses = serverapi.OptNilSecretUseArray{}
	projectWide.EnvVar = serverapi.OptString{}

	noUses := delegationGrant(secret.ID)
	noUses.Uses = serverapi.OptNilSecretUseArray{}
	noUses.EnvVar = serverapi.OptString{}

	nobody := delegationGrant(secret.ID)
	nobody.ScopeKey = serverapi.NewOptString("sbx-nobody")

	for name, tc := range map[string]struct {
		body services.CreateSecretGrantBody
		want int
	}{
		"wider than one discobox":             {projectWide, http.StatusBadRequest},
		"without uses":                        {noUses, http.StatusBadRequest},
		"held by a discobox that isn't there": {nobody, http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.CreateSecretGrant(ctx, "project-1", tc.body)
			requireStatus(t, err, tc.want)
		})
	}
}
