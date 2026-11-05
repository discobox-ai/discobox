package secrets_test

import (
	"github.com/discobox-ai/discobox/pkg/secretroute"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"net/http"
	"testing"
	"time"

	serverapi "github.com/discobox-ai/discobox/api/gen"
)

func TestApprovalCanGrantForeverInEitherUseMode(t *testing.T) {
	for _, mode := range []serverapi.SecretRequestPartDecisionAs{
		serverapi.SecretRequestPartDecisionAsReplace,
		serverapi.SecretRequestPartDecisionAsAdd,
	} {
		t.Run(string(mode), func(t *testing.T) {
			ctx := testPrincipalContext()
			svc, st := newAgentCredentialService(t)
			secret := createRoutedSecret(ctx, t, svc, "github", "api.github.com", 0)
			req := askToAdd(ctx, t, svc)
			answer := approveAs(secret.ID, mode)
			part := answer.Use.Value
			part.TtlSeconds = serverapi.NewOptInt64(0)
			answer.Use = serverapi.NewOptSecretRequestPartDecision(part)
			approved, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, answer)
			if err != nil {
				t.Fatalf("approve %s forever: %v", mode, err)
			}
			binding, err := st.SecretBindingByExactID(ctx, "project-1", approved.Use.BindingID)
			if err != nil {
				t.Fatal(err)
			}
			if binding.ExpiresAt != nil {
				t.Fatalf("expiry = %v, want no expiry", binding.ExpiresAt)
			}
		})
	}
}

func TestForeverAdditionStillRespectsLifetimeBounds(t *testing.T) {
	for _, bound := range []string{"secret", "parent", "unlimited parent"} {
		t.Run(bound, func(t *testing.T) {
			ctx := testPrincipalContext()
			svc, st := newAgentCredentialService(t)
			maximum := int64(0)
			if bound == "secret" {
				maximum = 3600
			}
			secret := createRoutedSecret(ctx, t, svc, "github", "api.github.com", maximum)
			var parent *model.SecretBinding
			if bound != "secret" {
				parent = &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: leadID, Delegate: true}
				if bound == "parent" {
					deadline := time.Now().UTC().Add(time.Hour)
					parent.ExpiresAt = &deadline
				}
				parent = mustBindAs(t, st, parent)
				ctx = asLead()
			}
			req := askToAdd(testPrincipalContext(), t, svc)
			answer := approveAs(secret.ID, serverapi.SecretRequestPartDecisionAsAdd)
			part := answer.Use.Value
			part.TtlSeconds = serverapi.NewOptInt64(0)
			answer.Use = serverapi.NewOptSecretRequestPartDecision(part)
			approved, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, answer)
			if bound != "unlimited parent" {
				status := http.StatusBadRequest
				if bound == "parent" {
					status = http.StatusForbidden
				}
				if !isStatus(err, status) {
					t.Fatalf("forever under %s = %v, want %d", bound, err, status)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			child, err := st.SecretBindingByExactID(ctx, "project-1", approved.Use.BindingID)
			if err != nil || child.ParentID != parent.ID || child.ExpiresAt != nil {
				t.Fatalf("child = %+v, %v", child, err)
			}
			if _, err := svc.DeleteSecretBinding(testPrincipalContext(), "project-1", parent.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := st.SecretBindingByExactID(ctx, "project-1", child.ID); err == nil {
				t.Fatal("addition survived its parent deletion")
			}
		})
	}
}

func TestAdditionCanClearAndRestoreExpiry(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	secret := createRoutedSecret(ctx, t, svc, "github", "api.github.com", 0)
	req := askToAdd(ctx, t, svc)
	approved, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, approveAs(secret.ID, serverapi.SecretRequestPartDecisionAsAdd))
	if err != nil {
		t.Fatal(err)
	}
	binding, err := svc.GetSecretBinding(ctx, "project-1", approved.Use.BindingID)
	if err != nil {
		t.Fatal(err)
	}
	if !binding.Adds || binding.ExpiresAt == nil {
		t.Fatalf("created = %+v", binding)
	}

	for _, seconds := range []int64{0, 3600, 0} {
		binding, err = svc.UpdateSecretBinding(ctx, "project-1", binding.ID, services.UpdateSecretBindingBody{TtlSeconds: serverapi.NewOptInt64(seconds)})
		if err != nil {
			t.Fatal(err)
		}
		if (binding.ExpiresAt == nil) != (seconds == 0) {
			t.Fatalf("updated for %d = %+v", seconds, binding)
		}
	}
}

func TestPermanentAdditionOutlivesFiniteGrantsAndCanBeRevoked(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createRoutedSecret(ctx, t, svc, "github", "api.github.com", 0)
	deadline := time.Now().UTC().Add(time.Hour)
	base := mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID, Use: true, EnvName: "GH_TOKEN", Routes: []model.SecretRoute{readsRoute}, ExpiresAt: &deadline})
	row := deliverRow(t, st, secret.ID, "GH_TOKEN", "SENT-GH")
	finite := mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID, Use: true, Adds: true, EnvName: "GH_TOKEN", Routes: []model.SecretRoute{{Hosts: []string{"api.github.com"}, Methods: []string{"DELETE"}}}, ExpiresAt: &deadline})
	req := askToAdd(ctx, t, svc)
	answer := approveAs(secret.ID, serverapi.SecretRequestPartDecisionAsAdd)
	part := answer.Use.Value
	part.TtlSeconds = serverapi.NewOptInt64(0)
	answer.Use = serverapi.NewOptSecretRequestPartDecision(part)
	approved, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, answer)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := svc.ListSandboxCredentials(ctx, testPoolID, testSandboxID)
	if err != nil || len(listed) != 1 || len(listed[0].Grants) != 3 || listed[0].ExpiresAt != nil {
		t.Fatalf("listed = %+v, %v", listed, err)
	}
	// Once both finite bindings lapse, only the permanent addition's routes remain.
	past := time.Now().UTC().Add(-time.Second)
	for _, binding := range []*model.SecretBinding{base, finite} {
		binding.ExpiresAt = &past
		if err := st.UpdateSecretBinding(ctx, binding, store.BindingChange{Actor: "user-1"}); err != nil {
			t.Fatal(err)
		}
	}
	sandbox, err := st.GetSandbox(ctx, "project-1", testSandboxID)
	if err != nil {
		t.Fatal(err)
	}
	// Repoint the materialized sentinel as sandbox reconciliation does when
	// the binding delivering its variable changes after expiry.
	effective, err := st.ResolveSandboxBindings(ctx, sandbox, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	applied, ok := effective.UseFor("GH_TOKEN")
	if !ok || applied.Binding.ID != approved.Use.BindingID {
		t.Fatalf("effective = %+v, want the permanent addition to deliver", effective)
	}
	row.BindingID = applied.Binding.ID
	if err := st.UpdateSandboxSecret(ctx, row); err != nil {
		t.Fatal(err)
	}

	grants, err := st.SentinelGrants(ctx, sandbox, []model.SandboxSecret{*row}, time.Now().UTC())
	if err != nil || len(grants[row.Sentinel]) != 1 || grants[row.Sentinel][0].ExpiresAt != nil {
		t.Fatalf("grants = %+v, %v", grants, err)
	}
	for method, want := range map[string]bool{"POST": true, "GET": false, "DELETE": false} {
		request := secretroute.Request{Method: method, Host: "api.github.com", URL: "https://api.github.com/repos/acme/web/pulls", Body: func() ([]byte, bool) { return nil, true }}
		if allowed, reason := secretroute.DecideGrants(grants[row.Sentinel], time.Now(), request); allowed != want {
			t.Fatalf("%s allowed = %v: %s", method, allowed, reason)
		}
	}
	resolved, err := svc.ResolveSandboxSecret(ctx, testPoolID, testSandboxID, row.Sentinel, "api.github.com")
	if err != nil || resolved.Status != model.SecretRequestStatusApproved || resolved.ExpiresAt != nil {
		t.Fatalf("resolved = %+v, %v", resolved, err)
	}
	if _, err := svc.DeleteSecretBinding(ctx, "project-1", approved.Use.BindingID); err != nil {
		t.Fatal(err)
	}
	resolved, err = svc.ResolveSandboxSecret(ctx, testPoolID, testSandboxID, row.Sentinel, "api.github.com")
	if err != nil || resolved.Status == model.SecretRequestStatusApproved {
		t.Fatalf("after revocation = %+v, %v", resolved, err)
	}
}
