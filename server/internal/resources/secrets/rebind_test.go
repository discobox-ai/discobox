package secrets_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/server/internal/model"
	resourcesecrets "github.com/discobox-ai/discobox/server/internal/resources/secrets"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// A variable whose grant was revoked is free to carry another credential: the
// binding left behind gives the discobox nothing, so the agent's next ask is
// answered with whichever secret the person chooses. The binding is rebound
// under a fresh sentinel, so one minted under the old secret resolves to
// nothing rather than to the new one.
func TestAnAgentCredentialIsReboundOnceItsGrantIsRevoked(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	first := createTokenSecret(ctx, t, svc, "first", "ghp_firstfirstfirstfirstfirstfirstfir1")
	second := createTokenSecret(ctx, t, svc, "second", "ghp_secondsecondsecondsecondsecondsec2")

	approved := approveAgentRequest(ctx, t, svc, first)
	old := agentBinding(ctx, t, st)
	if err := svc.RevokeSecretGrant(ctx, "project-1", approved.GrantID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	approveAgentRequest(ctx, t, svc, second)
	rebound := agentBinding(ctx, t, st)
	if rebound.SecretID != second.ID {
		t.Fatalf("binding secret = %q, want rebound to %q", rebound.SecretID, second.ID)
	}
	if rebound.ID != old.ID || rebound.Sentinel == old.Sentinel {
		t.Fatalf("binding = %s/%s, want the same row %s under a fresh sentinel (was %s)", rebound.ID, rebound.Sentinel, old.ID, old.Sentinel)
	}

	if _, err := svc.ResolveSandboxSecret(ctx, testPoolID, testSandboxID, old.Sentinel, "api.github.com"); !isStatus(err, http.StatusNotFound) {
		t.Fatalf("resolve old sentinel = %v, want not found; it must never resolve to the new secret", err)
	}
	resolved, err := svc.ResolveSandboxSecret(ctx, testPoolID, testSandboxID, rebound.Sentinel, "api.github.com")
	if err != nil {
		t.Fatalf("resolve new sentinel: %v", err)
	}
	if resolved.Status != model.SecretRequestStatusApproved || resolved.Value == nil || resolved.Value.Token != "ghp_secondsecondsecondsecondsecondsec2" {
		t.Fatalf("resolution = %+v, want the second secret's value", resolved)
	}
}

// A grant that lapsed delivers nothing either, so it holds the variable no
// more than a revoked one does — through any path that binds: an approval, and
// a wider grant bound lazily the first time the agent lists what it may use.
func TestAnAgentCredentialIsReboundOnceItsGrantExpires(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	first := createTokenSecret(ctx, t, svc, "first", "ghp_firstfirstfirstfirstfirstfirstfir1")
	second := createTokenSecret(ctx, t, svc, "second", "ghp_secondsecondsecondsecondsecondsec2")

	approved := approveAgentRequest(ctx, t, svc, first)
	old := agentBinding(ctx, t, st)
	expireGrant(ctx, t, st, approved.GrantID)

	if _, err := svc.CreateSecretGrant(ctx, "project-1", projectUseGrant(t, second.ID)); err != nil {
		t.Fatalf("grant: %v", err)
	}
	credentials, err := svc.ListSandboxCredentials(ctx, testPoolID, testSandboxID)
	if err != nil {
		t.Fatalf("list credentials: %v", err)
	}
	if len(credentials) != 1 || credentials[0].Assignment.SecretID != second.ID {
		t.Fatalf("credentials = %#v, want the project grant of the second secret", credentials)
	}
	if credentials[0].Assignment.Sentinel == old.Sentinel {
		t.Fatal("the rebound credential kept the sentinel minted under the first secret")
	}
}

// While a live grant still delivers the binding, at any scope covering the
// discobox, the variable stays with its secret: rebinding would swap the
// credential under a live activation. Revoking that grant is what frees it,
// which is what the refusal says to do.
func TestAnAgentCredentialIsNotReboundWhileAGrantDeliversIt(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	first := createTokenSecret(ctx, t, svc, "first", "ghp_firstfirstfirstfirstfirstfirstfir1")
	second := createTokenSecret(ctx, t, svc, "second", "ghp_secondsecondsecondsecondsecondsec2")

	// The binding came from an approval whose grant is revoked, but a project
	// grant of the same secret for the same variable still delivers it.
	approved := approveAgentRequest(ctx, t, svc, first)
	project, err := svc.CreateSecretGrant(ctx, "project-1", projectUseGrant(t, first.ID))
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if err := svc.RevokeSecretGrant(ctx, "project-1", approved.GrantID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	req := createAgentRequest(ctx, t, svc)
	_, err = approveAsAsked(ctx, t, svc, "project-1", req.ID, services.ApproveSecretRequestBody{
		SecretId: serverapi.NewOptString(second.ID),
	})
	if !isStatus(err, http.StatusConflict) || !strings.Contains(err.Error(), project.ID) {
		t.Fatalf("approve = %v, want a conflict naming the grant to revoke, %s", err, project.ID)
	}
	if binding := agentBinding(ctx, t, st); binding.SecretID != first.ID {
		t.Fatalf("binding secret = %q, want still %q", binding.SecretID, first.ID)
	}

	if err := svc.RevokeSecretGrant(ctx, "project-1", project.ID); err != nil {
		t.Fatalf("revoke project grant: %v", err)
	}
	if _, err := approveAsAsked(ctx, t, svc, "project-1", req.ID, services.ApproveSecretRequestBody{
		SecretId: serverapi.NewOptString(second.ID),
	}); err != nil {
		t.Fatalf("approve after revoking the grant the refusal named: %v", err)
	}
	if binding := agentBinding(ctx, t, st); binding.SecretID != second.ID {
		t.Fatalf("binding secret = %q, want rebound to %q", binding.SecretID, second.ID)
	}
}

// Two grants of one scope may name one variable for different secrets. The
// one that holds it is delivered and the other is passed over, as a wider
// grant is: a contested variable does not cost the discobox every credential
// it has.
func TestAContestedVariableIsPassedOverNotFailed(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	first := createTokenSecret(ctx, t, svc, "first", "ghp_firstfirstfirstfirstfirstfirstfir1")
	second := createTokenSecret(ctx, t, svc, "second", "ghp_secondsecondsecondsecondsecondsec2")

	if _, err := svc.CreateSecretGrant(ctx, "project-1", projectUseGrant(t, first.ID)); err != nil {
		t.Fatalf("grant first: %v", err)
	}
	if _, err := svc.ListSandboxCredentials(ctx, testPoolID, testSandboxID); err != nil {
		t.Fatalf("list credentials: %v", err)
	}
	if _, err := svc.CreateSecretGrant(ctx, "project-1", projectUseGrant(t, second.ID)); err != nil {
		t.Fatalf("grant second: %v", err)
	}
	credentials, err := svc.ListSandboxCredentials(ctx, testPoolID, testSandboxID)
	if err != nil {
		t.Fatalf("list credentials with a contested variable: %v", err)
	}
	if len(credentials) != 1 || credentials[0].Assignment.SecretID != first.ID {
		t.Fatalf("credentials = %#v, want the first secret, which holds the variable", credentials)
	}
}

func createTokenSecret(ctx context.Context, t *testing.T, svc *resourcesecrets.Service, name, token string) *model.Secret {
	t.Helper()
	secret, err := svc.CreateSecret(ctx, "project-1", services.CreateSecretBody{
		Name:  name,
		Type:  serverapi.CreateSecretBodyTypeToken,
		Value: serverapi.SecretValue{Token: serverapi.NewOptString(token)},
	})
	if err != nil {
		t.Fatalf("create secret: %v", err)
	}
	return secret
}

// approveAgentRequest is the test discobox asking for GITHUB_TOKEN and a
// person answering with secret.
func approveAgentRequest(ctx context.Context, t *testing.T, svc *resourcesecrets.Service, secret *model.Secret) *model.SecretRequest {
	t.Helper()
	approved, err := approveAsAsked(ctx, t, svc, "project-1", createAgentRequest(ctx, t, svc).ID, services.ApproveSecretRequestBody{
		SecretId: serverapi.NewOptString(secret.ID),
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	return approved
}

// projectUseGrant is a pre-approval of secret as GITHUB_TOKEN for every
// discobox in the project.
func projectUseGrant(t *testing.T, secretID string) services.CreateSecretGrantBody {
	return services.CreateSecretGrantBody{
		SecretId: secretID,
		Scope:    serverapi.CreateSecretGrantBodyScopeProject,
		Hosts:    []string{"api.github.com"},
		EnvVar:   serverapi.NewOptString("GITHUB_TOKEN"),
		Uses:     serverapi.NewOptNilSecretUseArray([]serverapi.SecretUse{{Description: "open a pull request"}}),
		Policy:   policyAsked(t),
	}
}

func agentBinding(ctx context.Context, t *testing.T, st *store.Store) *model.SandboxSecret {
	t.Helper()
	binding, err := st.FindAgentSandboxSecret(ctx, "project-1", testSandboxID, "GITHUB_TOKEN")
	if err != nil {
		t.Fatalf("find binding: %v", err)
	}
	return binding
}

func expireGrant(ctx context.Context, t *testing.T, st *store.Store, grantID string) {
	t.Helper()
	grant, err := st.GetSecretGrant(ctx, "project-1", grantID)
	if err != nil {
		t.Fatalf("get grant: %v", err)
	}
	lapsed := time.Now().UTC().Add(-time.Minute)
	grant.ExpiresAt = &lapsed
	if err := st.UpdateSecretGrant(ctx, grant); err != nil {
		t.Fatalf("expire grant: %v", err)
	}
}
