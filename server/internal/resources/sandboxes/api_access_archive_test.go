package sandboxes

import (
	"context"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
	"gorm.io/gorm"
)

// Archiving a sandbox removes its runtime, so the agent that asked for API
// access is gone and nothing will read the answer: its open request is closed
// with the archive rather than left pending for a person to answer.
func TestArchivingASandboxClosesItsOpenAPIAccessRequest(t *testing.T) {
	ctx, svc, st, _ := createIntentFixture(t)
	sandbox := &model.Sandbox{ID: "sb-1", ProjectID: "project-1", CreatedByUserID: "user-1", Name: "sb-1", PoolID: "pool-1"}
	if err := st.CreateSandbox(ctx, sandbox); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	req := &model.APIAccessRequest{ProjectID: "project-1", RequesterSandboxID: "sb-1", Justification: "coordinate workers", Status: model.SecretRequestStatusPending}
	if err := st.CreateAPIAccessRequest(context.Background(), req); err != nil {
		t.Fatalf("create request: %v", err)
	}

	if err := svc.DeleteSandbox(ctx, "project-1", "sb-1"); err != nil {
		t.Fatalf("archive sandbox: %v", err)
	}

	got, err := st.GetAPIAccessRequest(ctx, "project-1", req.ID)
	if err != nil {
		t.Fatalf("get request: %v", err)
	}
	if got.Status == model.SecretRequestStatusPending {
		t.Fatalf("API access request of an archived sandbox = %s, want it closed", got.Status)
	}
}

// Archiving closes the sandbox's open asks as denied by system — its secret
// request, its host trust ask, its API access request, and the adoptions it
// asked for or is the child of — and leaves another sandbox's open asks, and
// asks already answered, as they were. An archived sandbox files no new secret
// request or host trust ask.
func TestArchivingASandboxDeniesOnlyItsOwnOpenRequests(t *testing.T) {
	ctx, svc, st, _ := createIntentFixture(t)
	for _, id := range []string{"sb-1", "sb-2", "sb-3"} {
		if err := st.CreateSandbox(ctx, &model.Sandbox{ID: id, ProjectID: "project-1", CreatedByUserID: "user-1", Name: id, PoolID: "pool-1"}); err != nil {
			t.Fatalf("create sandbox: %v", err)
		}
	}
	secretRequest := func(sandboxID, status string) string {
		req := &model.SecretRequest{
			ProjectID: "project-1", RequestedBy: "agent:" + sandboxID, SandboxID: sandboxID,
			Use:    model.SecretRequestPart{Asked: true, Routes: []model.SecretRoute{{Hosts: []string{"api.example.com"}}}},
			Status: status,
		}
		if err := st.CreateSecretRequest(ctx, req); err != nil {
			t.Fatalf("create secret request: %v", err)
		}
		return req.ID
	}
	openSecret := secretRequest("sb-1", model.SecretRequestStatusPending)
	approvedSecret := secretRequest("sb-1", model.SecretRequestStatusApproved)
	otherSecret := secretRequest("sb-2", model.SecretRequestStatusPending)
	apiRequest := func(sandboxID string) string {
		req := &model.APIAccessRequest{ProjectID: "project-1", RequesterSandboxID: sandboxID, Justification: "coordinate workers"}
		if err := st.CreateAPIAccessRequest(ctx, req); err != nil {
			t.Fatalf("create API access request: %v", err)
		}
		return req.ID
	}
	openAPI := apiRequest("sb-1")
	otherAPI := apiRequest("sb-2")
	adoption := func(requester, child string) string {
		req := &model.AdoptionRequest{ProjectID: "project-1", RequesterSandboxID: requester, ChildSandboxID: child, Justification: "lead it", Status: model.SecretRequestStatusPending}
		if err := st.Transaction(ctx, func(_ *store.Store, tx *gorm.DB) error { return tx.Create(req).Error }); err != nil {
			t.Fatalf("create adoption request: %v", err)
		}
		return req.ID
	}
	trustAsk := func(sandboxID string) string {
		req := &model.HostTrustRequest{ProjectID: "project-1", SandboxID: sandboxID, RequestedBy: "agent:" + sandboxID, Host: "internal.example:443", Status: model.HostTrustRequestStatusPending}
		if err := st.CreateHostTrustRequest(ctx, req); err != nil {
			t.Fatalf("create host trust request: %v", err)
		}
		return req.ID
	}
	openTrust := trustAsk("sb-1")
	otherTrust := trustAsk("sb-2")
	adoptsOther := adoption("sb-1", "sb-2")
	adoptedByOther := adoption("sb-2", "sb-1")
	otherAdoption := adoption("sb-2", "sb-3")

	if err := svc.DeleteSandbox(ctx, "project-1", "sb-1"); err != nil {
		t.Fatalf("archive sandbox: %v", err)
	}

	for id, want := range map[string]string{
		openSecret:     model.SecretRequestStatusDenied,
		approvedSecret: model.SecretRequestStatusApproved,
		otherSecret:    model.SecretRequestStatusPending,
	} {
		got, err := st.GetSecretRequest(ctx, "project-1", id)
		if err != nil {
			t.Fatalf("get secret request %s: %v", id, err)
		}
		if got.Status != want {
			t.Errorf("secret request %s = %s, want %s", id, got.Status, want)
		}
		if id == openSecret && (got.DecidedBy != model.SecretBindingActorSystem || got.DecidedAt == nil || got.Use.Outcome != model.SecretRequestPartDenied) {
			t.Errorf("secret request of the archived sandbox decided by %q at %v, use part %q; want denied by %q", got.DecidedBy, got.DecidedAt, got.Use.Outcome, model.SecretBindingActorSystem)
		}
	}
	for id, want := range map[string]string{adoptsOther: model.SecretRequestStatusDenied, adoptedByOther: model.SecretRequestStatusDenied, otherAdoption: model.SecretRequestStatusPending} {
		got, err := st.GetAdoptionRequest(ctx, "project-1", id)
		if err != nil {
			t.Fatalf("get adoption request %s: %v", id, err)
		}
		if got.Status != want {
			t.Errorf("adoption request %s = %s, want %s", id, got.Status, want)
		}
		if want == model.SecretRequestStatusDenied && got.DecidedBy != model.SecretBindingActorSystem {
			t.Errorf("adoption request %s decided by %q, want %q", id, got.DecidedBy, model.SecretBindingActorSystem)
		}
	}
	for id, want := range map[string]string{openTrust: model.HostTrustRequestStatusDenied, otherTrust: model.HostTrustRequestStatusPending} {
		got, err := st.GetHostTrustRequest(ctx, "project-1", id)
		if err != nil {
			t.Fatalf("get host trust request %s: %v", id, err)
		}
		if got.Status != want {
			t.Errorf("host trust request %s = %s, want %s", id, got.Status, want)
		}
	}
	lateTrust := &model.HostTrustRequest{ProjectID: "project-1", SandboxID: "sb-1", RequestedBy: "agent:sb-1", Host: "late.example:443", Status: model.HostTrustRequestStatusPending}
	if err := st.CreateHostTrustRequest(ctx, lateTrust); err == nil {
		t.Error("an archived sandbox asked to trust a host")
	}
	late := &model.SecretRequest{
		ProjectID: "project-1", RequestedBy: "agent:sb-1", SandboxID: "sb-1",
		Use:    model.SecretRequestPart{Asked: true, Routes: []model.SecretRoute{{Hosts: []string{"api.example.com"}}}},
		Status: model.SecretRequestStatusPending,
	}
	if err := st.CreateSecretRequest(ctx, late); err == nil {
		t.Error("an archived sandbox filed a secret request")
	}
	for id, want := range map[string]string{openAPI: model.SecretRequestStatusDenied, otherAPI: model.SecretRequestStatusPending} {
		got, err := st.GetAPIAccessRequest(ctx, "project-1", id)
		if err != nil {
			t.Fatalf("get API access request %s: %v", id, err)
		}
		if got.Status != want {
			t.Errorf("API access request %s = %s, want %s", id, got.Status, want)
		}
		if id == openAPI && (got.DecidedBy != model.SecretBindingActorSystem || got.DecidedAt == nil) {
			t.Errorf("API access request of the archived sandbox decided by %q at %v, want by %q", got.DecidedBy, got.DecidedAt, model.SecretBindingActorSystem)
		}
	}
}
