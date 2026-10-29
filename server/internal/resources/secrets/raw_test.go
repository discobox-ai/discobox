package secrets_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/server/internal/model"
	resourcesecrets "github.com/discobox-ai/discobox/server/internal/resources/secrets"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// allowRaw lets a secret be handed over raw, as a person does.
func allowRaw(ctx context.Context, t *testing.T, svc *resourcesecrets.Service, secret *model.Secret) {
	t.Helper()
	if _, err := svc.UpdateSecret(ctx, "project-1", secret.ID, services.UpdateSecretBody{AllowRaw: serverapi.NewOptBool(true)}); err != nil {
		t.Fatalf("allow raw: %v", err)
	}
}

func rawBindBody(raw serverapi.CreateSecretBindingBodyRaw) services.CreateSecretBindingBody {
	return services.CreateSecretBindingBody{
		Secret: "github", TargetKind: serverapi.CreateSecretBindingBodyTargetKindSandbox, Target: serverapi.NewOptString(testSandboxID),
		EnvVar: serverapi.NewOptString("GH_TOKEN"), Raw: serverapi.NewOptCreateSecretBindingBodyRaw(raw),
	}
}

// A binding is raw only when its secret allows it, it is for use, and a
// person makes it (ADR 26-10-27-840 §§1-2).
func TestARawBindingNeedsTheSecretsLeaveAndAPerson(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	if _, err := svc.CreateSecretBinding(ctx, "project-1", rawBindBody(serverapi.CreateSecretBindingBodyRawEnv)); !isStatus(err, http.StatusForbidden) {
		t.Fatalf("raw of a secret that does not allow it = %v, want 403", err)
	}
	if _, err := svc.UpdateSecret(asLead(), "project-1", secret.ID, services.UpdateSecretBody{AllowRaw: serverapi.NewOptBool(true)}); !isStatus(err, http.StatusForbidden) {
		t.Fatalf("a discobox allowing raw = %v, want 403", err)
	}
	allowRaw(ctx, t, svc, secret)
	delegateOnly := rawBindBody(serverapi.CreateSecretBindingBodyRawFile)
	delegateOnly.EnvVar, delegateOnly.Delegate, delegateOnly.Use = serverapi.OptString{}, serverapi.NewOptBool(true), serverapi.NewOptBool(false)
	if _, err := svc.CreateSecretBinding(ctx, "project-1", delegateOnly); !isStatus(err, http.StatusBadRequest) {
		t.Fatalf("a raw delegation = %v, want 400", err)
	}
	binding, err := svc.CreateSecretBinding(ctx, "project-1", rawBindBody(serverapi.CreateSecretBindingBodyRawFile))
	if err != nil {
		t.Fatalf("raw binding: %v", err)
	}
	if binding.Raw != "file" || !binding.Use {
		t.Fatalf("binding = %+v, want a raw file use", binding)
	}
	updated, err := svc.UpdateSecretBinding(ctx, "project-1", binding.ID, services.UpdateSecretBindingBody{Raw: serverapi.NewOptUpdateSecretBindingBodyRaw(serverapi.UpdateSecretBindingBodyRawNone)})
	if err != nil || updated.Raw != "" {
		t.Fatalf("back to a sentinel = %+v, %v", updated, err)
	}
}

// A discobox never hands a value on raw: not at a create, and nothing it
// approves is raw, since no request asks for the value (ADR 26-10-27-840 §2).
func TestADiscoboxNeverHandsAValueOnRaw(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	allowRaw(ctx, t, svc, secret)
	mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: leadID, Delegate: true})
	newBox := &model.Sandbox{ID: "sbx-new", ProjectID: "project-1"}
	if _, err := svc.PrepareSandboxBindings(asLead(), "project-1", newBox, []apimodel.SandboxBindingInput{{
		Secret: "github", EnvVar: serverapi.NewOptString("GH"), Raw: serverapi.NewOptSandboxBindingInputRaw(serverapi.SandboxBindingInputRawEnv),
	}}); !isStatus(err, http.StatusForbidden) {
		t.Fatalf("a discobox binding raw at a create = %v, want 403", err)
	}
	prepared, err := svc.PrepareSandboxBindings(ctx, "project-1", newBox, []apimodel.SandboxBindingInput{{
		Secret: "github", EnvVar: serverapi.NewOptString("GH"), Raw: serverapi.NewOptSandboxBindingInputRaw(serverapi.SandboxBindingInputRawEnv),
	}})
	if err != nil || len(prepared.Bindings) != 1 || prepared.Bindings[0].Raw != "env" {
		t.Fatalf("a person binding raw at a create = %+v, %v", prepared.Bindings, err)
	}
}

// A sentinel approval never renews a raw binding: the ask a person approves is for a
// sentinel, and extending the handover would widen what nobody looked at.
func TestASentinelApprovalNeverRenewsARawBinding(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	allowRaw(ctx, t, svc, secret)
	standing := mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID, Use: true, EnvName: "GH_TOKEN", Raw: "file"})
	req, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID, Name: "github", EnvVar: serverapi.NewOptString("GH_TOKEN"), Hosts: []string{"github.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(secret.ID)}); !isStatus(err, http.StatusConflict) {
		t.Fatalf("approving over a raw binding = %v, want 409", err)
	}
	if got, err := st.SecretBindingByExactID(ctx, "project-1", standing.ID); err != nil || got.ExpiresAt != standing.ExpiresAt {
		t.Fatalf("raw binding after a refused approval = %+v, %v, want it untouched", got, err)
	}
	credentials, err := svc.ListSandboxCredentials(ctx, testPoolID, testSandboxID)
	if err != nil || len(credentials) != 1 || credentials[0].Raw != "file" {
		t.Fatalf("credentials = %+v, %v, want the raw file binding listed", credentials, err)
	}
}

// The value of a raw binding leaves by its own door, with no host, once per
// value recorded; the swap door refuses its sentinel, and taking raw away
// from the secret takes the binding with it.
func TestARawValueIsResolvedOnlyRawAndGoesWhenRawIsTakenAway(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	marks := &sentinelMarks{}
	svc.SetSecretHolders(marks)
	secret := createBearerSecret(ctx, t, svc)
	allowRaw(ctx, t, svc, secret)
	binding := mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID, Use: true, EnvName: "GH_TOKEN", Raw: "env"})
	if err := st.CreateSandboxSecret(ctx, &model.SandboxSecret{
		ProjectID: "project-1", SandboxID: testSandboxID, SecretID: secret.ID, EnvName: "GH_TOKEN", Sentinel: "raw-handle", BindingID: binding.ID, Raw: "env",
	}); err != nil {
		t.Fatal(err)
	}
	if swapped, err := svc.ResolveSandboxSecret(ctx, testPoolID, testSandboxID, "raw-handle", "github.com"); err != nil || swapped.Status != model.SecretRequestStatusDenied {
		t.Fatalf("swapping a raw handle = %+v, %v, want denied", swapped, err)
	}
	if _, err := svc.ResolveSandboxRawSecret(ctx, "pool-other", testSandboxID, "raw-handle"); !isStatus(err, http.StatusNotFound) {
		t.Fatalf("another pool's resolve = %v, want 404", err)
	}
	for range 2 {
		got, err := svc.ResolveSandboxRawSecret(ctx, testPoolID, testSandboxID, "raw-handle")
		if err != nil || got.Status != model.SecretRequestStatusApproved || got.Value == nil || got.Value.Token != "ghp_realrealrealrealrealrealrealreal12" || got.BindingID != binding.ID {
			t.Fatalf("raw resolve = %+v, %v", got, err)
		}
	}
	events, err := st.ListSecretBindingEvents(ctx, "project-1", store.SecretBindingEventFilter{BindingID: binding.ID})
	if err != nil {
		t.Fatal(err)
	}
	delivered := 0
	for _, event := range events {
		if event.Event == model.SecretBindingEventDelivered {
			delivered++
			if event.Cause != testSandboxID {
				t.Fatalf("delivered event cause = %q, want the discobox", event.Cause)
			}
		}
	}
	if delivered != 1 {
		t.Fatalf("delivered events = %d, want one for the one value", delivered)
	}
	if _, err := svc.UpdateSecret(ctx, "project-1", secret.ID, services.UpdateSecretBody{AllowRaw: serverapi.NewOptBool(false)}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SecretBindingByExactID(ctx, "project-1", binding.ID); err == nil {
		t.Fatal("the raw binding outlived its secret allowing raw")
	}
	if len(marks.bindings) == 0 {
		t.Fatal("taking raw away marked nobody")
	}
	if got, err := svc.ResolveSandboxRawSecret(ctx, testPoolID, testSandboxID, "raw-handle"); err != nil || got.Status != model.SecretRequestStatusDenied {
		t.Fatalf("raw resolve after raw was taken away = %+v, %v, want denied", got, err)
	}
}

// A sentinel binding's sentinel never resolves raw.
func TestASentinelIsNeverResolvedRaw(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	allowRaw(ctx, t, svc, secret)
	binding := mustDeliverAs(t, st, testSandboxID, secret.ID, "GH_TOKEN")
	if err := st.CreateSandboxSecret(ctx, &model.SandboxSecret{
		ProjectID: "project-1", SandboxID: testSandboxID, SecretID: secret.ID, EnvName: "GH_TOKEN", Sentinel: "plain-sentinel", BindingID: binding.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := svc.ResolveSandboxRawSecret(ctx, testPoolID, testSandboxID, "plain-sentinel"); err != nil || got.Status != model.SecretRequestStatusDenied {
		t.Fatalf("raw resolve of a sentinel = %+v, %v, want denied", got, err)
	}
}

// A delivery is recorded against the row and binding as they stand when it
// is written: a row no longer raw hands nothing out (ADR 26-10-27-840 §5).
func TestARawDeliveryIsRecordedOnlyWhileTheRowIsRaw(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	allowRaw(ctx, t, svc, secret)
	binding := mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID, Use: true, EnvName: "GH_TOKEN", Raw: "env"})
	row := &model.SandboxSecret{ProjectID: "project-1", SandboxID: testSandboxID, SecretID: secret.ID, EnvName: "GH_TOKEN", Sentinel: "raw-handle", BindingID: binding.ID, Raw: "env"}
	if err := st.CreateSandboxSecret(ctx, row); err != nil {
		t.Fatal(err)
	}
	stale := *row
	row.Raw = ""
	if err := st.UpdateSandboxSecret(ctx, row); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordRawDelivery(ctx, &stale, binding.ID, secret.Revision); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("recording a delivery for a row no longer raw = %v, want not found", err)
	}
}

// A person may grant a request the value itself, of a secret that allows it
// (ADR 26-10-27-840 §2); a discobox may not, and granting it raw again renews
// a raw binding.
func TestAPersonMayGrantARequestRaw(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	ask := func() *model.SecretRequest {
		t.Helper()
		req, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
			SandboxId: testSandboxID, Name: "github", EnvVar: serverapi.NewOptString("GH_TOKEN"), Hosts: []string{"github.com"},
		})
		if err != nil {
			t.Fatal(err)
		}
		return req
	}
	raw := services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(secret.ID), Raw: serverapi.NewOptApproveSecretRequestBodyRaw(serverapi.ApproveSecretRequestBodyRawFile)}
	req := ask()
	if _, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, raw); !isStatus(err, http.StatusForbidden) {
		t.Fatalf("granting raw of a secret that does not allow it = %v, want 403", err)
	}
	allowRaw(ctx, t, svc, secret)
	mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: leadID, Delegate: true})
	if _, err := svc.ApproveSecretRequest(asLead(), "project-1", req.ID, services.ApproveSecretRequestBody{Raw: raw.Raw}); !isStatus(err, http.StatusForbidden) {
		t.Fatalf("a discobox granting raw = %v, want 403", err)
	}
	approved, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, raw)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := st.SecretBindingByExactID(ctx, "project-1", approved.BindingID)
	if err != nil || binding.Raw != "file" || binding.RequestID != req.ID {
		t.Fatalf("approved binding = %+v, %v, want a raw file binding answering the request", binding, err)
	}
	again, err := svc.ApproveSecretRequest(ctx, "project-1", ask().ID, raw)
	if err != nil || again.BindingID != binding.ID {
		t.Fatalf("granting raw again = %+v, %v, want the raw binding renewed", again, err)
	}
	if _, err := svc.ApproveSecretRequest(ctx, "project-1", ask().ID, services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(secret.ID)}); !isStatus(err, http.StatusConflict) {
		t.Fatalf("a sentinel approval over the raw binding = %v, want 409", err)
	}
}

// A request to delegate is delivered nothing, so it is granted no raw value.
func TestADelegationIsGrantedNoRawValue(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	allowRaw(ctx, t, svc, secret)
	req, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID, Name: "github", Hosts: []string{"github.com"},
		Purpose: serverapi.NewOptCreateSandboxCredentialRequestBodyPurpose(serverapi.CreateSandboxCredentialRequestBodyPurposeDelegate),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{
		SecretId: serverapi.NewOptString(secret.ID), Raw: serverapi.NewOptApproveSecretRequestBodyRaw(serverapi.ApproveSecretRequestBodyRawEnv),
	}); !isStatus(err, http.StatusBadRequest) {
		t.Fatalf("granting a delegation raw = %v, want 400", err)
	}
}

// A delegation approved over a binding that both holds the value and
// delegates it is refused, pointing at the binding rather than at a raw grant
// a delegation cannot take.
func TestADelegationOverARawBindingPointsAtTheBinding(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	allowRaw(ctx, t, svc, secret)
	mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID, Use: true, Delegate: true, EnvName: "GH_TOKEN", Raw: "env"})
	req, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID, Name: "github", Hosts: []string{"github.com"},
		Purpose: serverapi.NewOptCreateSandboxCredentialRequestBodyPurpose(serverapi.CreateSandboxCredentialRequestBodyPurposeDelegate),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(secret.ID)})
	if !isStatus(err, http.StatusConflict) || !strings.Contains(err.Error(), "admin binding update") {
		t.Fatalf("a delegation over a raw binding = %v, want a 409 pointing at the binding", err)
	}
}
