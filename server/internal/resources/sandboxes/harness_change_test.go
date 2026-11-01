package sandboxes

import (
	"context"
	"errors"
	"testing"

	api "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

func TestHarnessChangeRefusalKeepsEffectiveHarness(t *testing.T) {
	svc, st := newUpgradeEngineFixture(t)
	provider := &recordingProvider{}
	manager := sandbox.NewProviderManager(svc.store)
	manager.RegisterProvider("docker", provider)
	manager.SetDefault("docker")
	svc.sandboxProviders = manager
	target := bundledConfig(t, svc.store, codexBundle, codexBundleV2)
	oldConfig := &model.HarnessConfig{ProjectID: "project-1", Slug: "old", Name: "Old", Configured: true}
	if err := svc.store.CreateHarnessConfig(context.Background(), oldConfig); err != nil {
		t.Fatal(err)
	}
	old := oldConfig.ID
	sb := eligibleSandbox(t, st, old, codexBundle, codexBundleV1)
	var err error
	sb.HarnessConfigID = &old
	sb.State = model.SandboxStateReady
	sb.DesiredState = model.DesiredStatePresent
	sb.PendingHarnessChange = &model.SandboxHarnessChange{ID: "change-1", Manifest: sb.SandboxManifest}
	sb.PendingHarnessChange.Manifest.HarnessConfigID = &target.ID
	sb.IncrementGeneration()
	if err := svc.store.UpdateSandbox(context.Background(), sb); err != nil {
		t.Fatal(err)
	}
	provider.updateErr = sandbox.ErrHarnessChangeRefused
	_, err = svc.NewSandboxReconciler().Reconcile(context.Background(), SandboxDirtyID(sb.ProjectID, sb.ID))
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.store.GetSandbox(context.Background(), sb.ProjectID, sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.HarnessConfigID == nil || *got.HarnessConfigID != old || got.PendingHarnessChange != nil || got.HarnessChangeError == "" {
		t.Fatalf("refused change altered harness or left proposal: %+v", got)
	}
	// A subsequent reconcile must not reconsider a proposal that was refused.
	provider.updateErr = errors.New("should not reserve again")
	if got.PendingHarnessChange != nil {
		t.Fatal("refused proposal remains eligible")
	}
}

func TestHarnessSelectorValidationAndSameConfigNoOp(t *testing.T) {
	svc, st := newBindingFixture(t)
	target := bundledConfig(t, st, codexBundle, codexBundleV2)
	sb := pinnedSandbox(t, st, target.ID, codexBundle, codexBundleV1)
	for _, config := range []api.SandboxUpdateConfig{
		{Harness: api.NewOptString(" ")},
		{Harness: api.NewOptString("codex"), HarnessConfigId: api.NewOptString(target.ID)},
	} {
		if _, err := svc.prepareHarnessChange(context.Background(), sb, config); err == nil {
			t.Fatalf("accepted malformed selector: %+v", config)
		}
	}
	// Same selection doesn't implicitly upgrade or require stopping a live box.
	change, err := svc.prepareHarnessChange(context.Background(), sb, api.SandboxUpdateConfig{Harness: api.NewOptString("codex")})
	if err != nil || change != nil {
		t.Fatalf("same config is not no-op: %+v %v", change, err)
	}
	_, err = svc.UpdateSandbox(context.Background(), sb.ProjectID, sb.ID, services.UpdateSandboxBody{Config: api.NewOptSandboxUpdateConfig(api.SandboxUpdateConfig{Harness: api.NewOptString(target.ID)})})
	if err != nil {
		t.Fatal(err)
	}
}

func TestHarnessChangeAppliesSameBundleDifferentConfigAndFencesReplacement(t *testing.T) {
	svc, st := newUpgradeEngineFixture(t)
	old := &model.HarnessConfig{ProjectID: "project-1", Slug: "old", Name: "Old", Configured: true, Bundle: codexBundle, BundleDigest: codexBundleV2}
	if err := st.CreateHarnessConfig(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	target := bundledConfig(t, st, codexBundle, codexBundleV2)
	sb := eligibleSandbox(t, st, old.ID, codexBundle, codexBundleV2, func(sb *model.Sandbox) { sb.Prompt = []string{"old task"} })
	provider := &recordingProvider{}
	manager := sandbox.NewProviderManager(svc.store)
	manager.RegisterProvider("docker", provider)
	manager.SetDefault("docker")
	svc.sandboxProviders = manager
	updated, err := svc.UpdateSandbox(context.Background(), sb.ProjectID, sb.ID, services.UpdateSandboxBody{Config: api.NewOptSandboxUpdateConfig(api.SandboxUpdateConfig{Harness: api.NewOptString(target.Slug)})})
	if err != nil {
		t.Fatal(err)
	}
	if updated.HarnessConfigID == nil || *updated.HarnessConfigID != target.ID || updated.HarnessChangeID == "" || updated.PendingHarnessChange != nil {
		t.Fatalf("change did not apply: %+v", updated)
	}
	if len(provider.updates) != 1 || provider.updates[0].ReserveHarnessChange != updated.HarnessChangeID {
		t.Fatalf("missing matching runtime fence: %+v", provider.updates)
	}
	if len(updated.Prompt) != 0 {
		t.Fatalf("old prompt would replay into new harness: %v", updated.Prompt)
	}
	if updated.BundleDigest != codexBundleV2 || !updated.Converged() {
		t.Fatalf("replacement did not converge: %+v", updated)
	}
}

func TestPendingHarnessTargetCannotBeDeleted(t *testing.T) {
	svc, st := newUpgradeEngineFixture(t)
	old := &model.HarnessConfig{ProjectID: "project-1", Slug: "old", Name: "Old", Configured: true}
	if err := st.CreateHarnessConfig(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	target := bundledConfig(t, st, codexBundle, codexBundleV2)
	sb := eligibleSandbox(t, st, old.ID, codexBundle, codexBundleV1)
	sb.PendingHarnessConfigID = &target.ID
	sb.PendingHarnessChange = &model.SandboxHarnessChange{ID: "pending", Manifest: sb.SandboxManifest}
	sb.PendingHarnessChange.Manifest.HarnessConfigID = &target.ID
	if err := svc.store.UpdateSandbox(context.Background(), sb); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteHarnessConfig(context.Background(), sb.ProjectID, target.ID, nil); !errors.Is(err, store.ErrInUse) {
		t.Fatalf("delete pending target: %v", err)
	}
}
