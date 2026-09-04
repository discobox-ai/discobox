package sandboxes_test

import (
	"context"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/resources/sandboxes"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// repairableSandbox creates a present, ready sandbox — the shape repair acts
// on — with the pool and provider rows its foreign keys need.
func repairableSandbox(ctx context.Context, t *testing.T, appStore *store.Store, state string) *model.Sandbox {
	t.Helper()
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "test", Name: "test"}
	if err := appStore.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	pool := &model.Pool{ID: "pool-1", ProjectID: "project-1", PoolManifest: model.PoolManifest{Name: "pool-1", ProviderInstanceID: provider.ID}}
	if err := appStore.CreatePool(ctx, pool); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	sb := &model.Sandbox{
		ID: "sandbox-1", ProjectID: "project-1", PoolID: pool.ID, CreatedByUserID: "user-1", Name: "alpha",
		ResourceLifecycle: model.ResourceLifecycle{
			DesiredState: model.DesiredStatePresent,
			State:        state,
			Generation:   1,
		},
	}
	if err := appStore.CreateSandbox(ctx, sb); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	return sb
}

// recordRepairIntent does what the service's RepairSandbox records: present
// intent whose generation is named by RepairGeneration (ADR 0035).
func recordRepairIntent(ctx context.Context, t *testing.T, appStore *store.Store, sb *model.Sandbox) {
	t.Helper()
	sb.IncrementGeneration()
	sb.RecordIntent(model.DesiredStatePresent)
	sb.RepairGeneration = sb.Generation
	if err := appStore.UpdateSandbox(ctx, sb); err != nil {
		t.Fatalf("record repair intent: %v", err)
	}
}

// Repair tears the runtime down before rebuilding, and the rebuild comes back
// stopped: power is the trailing instruction's job, never the reconciler's.
func TestReconcileRepairTearsDownThenRebuilds(t *testing.T) {
	ctx := context.Background()
	appStore := newExecutorTestStore(t)
	sb := repairableSandbox(ctx, t, appStore, model.SandboxStateReady)
	recordRepairIntent(ctx, t, appStore, sb)

	provider := &archiveTestProvider{}
	reconciler := sandboxes.NewSandboxReconciler(appStore, sandboxes.WithSandboxProvider(provider))
	if _, err := reconciler.ReconcileSandbox(ctx, sb); err != nil {
		t.Fatalf("reconcile repair: %v", err)
	}

	if provider.archiveCalls != 1 {
		t.Fatalf("archive calls = %d, want 1: repair must tear down before rebuilding", provider.archiveCalls)
	}
	if provider.removeCalls != 0 {
		t.Fatalf("repair destroyed the data: remove calls = %d, want 0", provider.removeCalls)
	}
	if provider.createCalls != 1 {
		t.Fatalf("create calls = %d, want 1", provider.createCalls)
	}
	if provider.createStart {
		t.Fatal("repair asked the provider to start the sandbox; the start is a separate instruction")
	}

	updated, err := appStore.GetSandbox(ctx, sb.ProjectID, sb.ID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if updated.State != model.SandboxStateReady {
		t.Fatalf("state = %q, want ready", updated.State)
	}
	if updated.ErrorMessage != nil {
		t.Fatalf("error message = %q, want none", *updated.ErrorMessage)
	}
	if !updated.Converged() {
		t.Fatalf("repair did not settle: generation %d, observed %d", updated.Generation, updated.ObservedGeneration)
	}
}

// RepairGeneration names exactly one generation, so a later, ordinary intent
// must not tear the runtime down again.
func TestReconcileRepairIsOneShot(t *testing.T) {
	ctx := context.Background()
	appStore := newExecutorTestStore(t)
	sb := repairableSandbox(ctx, t, appStore, model.SandboxStateReady)
	recordRepairIntent(ctx, t, appStore, sb)

	provider := &archiveTestProvider{}
	reconciler := sandboxes.NewSandboxReconciler(appStore, sandboxes.WithSandboxProvider(provider))
	if _, err := reconciler.ReconcileSandbox(ctx, sb); err != nil {
		t.Fatalf("reconcile repair: %v", err)
	}

	later, err := appStore.GetSandbox(ctx, sb.ProjectID, sb.ID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	later.IncrementGeneration()
	later.RecordIntent(model.DesiredStatePresent)
	if err := appStore.UpdateSandbox(ctx, later); err != nil {
		t.Fatalf("record later intent: %v", err)
	}
	if _, err := reconciler.ReconcileSandbox(ctx, later); err != nil {
		t.Fatalf("reconcile later intent: %v", err)
	}

	if provider.archiveCalls != 1 {
		t.Fatalf("archive calls = %d, want 1: a later generation must not repeat the teardown", provider.archiveCalls)
	}
	if provider.createCalls != 2 {
		t.Fatalf("create calls = %d, want 2", provider.createCalls)
	}
}

// The wedge repair exists for: a settled failure, converged with its error
// latched (ADR 0017 §4). Recording the repair intent clears the latch and the
// reconcile rebuilds — the same fixture a failed replace leaves behind.
func TestReconcileRepairRecoversASettledFailure(t *testing.T) {
	ctx := context.Background()
	appStore := newExecutorTestStore(t)
	sb := repairableSandbox(ctx, t, appStore, model.SandboxStateReady)

	message := "pool-agent request failed: bind source path does not exist"
	sb.ErrorMessage = &message
	sb.ObservedGeneration = sb.Generation
	if err := appStore.UpdateSandbox(ctx, sb); err != nil {
		t.Fatalf("record settled failure: %v", err)
	}

	provider := &archiveTestProvider{}
	reconciler := sandboxes.NewSandboxReconciler(appStore, sandboxes.WithSandboxProvider(provider))

	// Converged with an error latched: the reconciler must refuse to act on
	// its own (ADR 0017 §4)...
	if _, err := reconciler.ReconcileSandbox(ctx, sb); err != nil {
		t.Fatalf("reconcile settled failure: %v", err)
	}
	if provider.archiveCalls != 0 || provider.createCalls != 0 {
		t.Fatalf("settled failure was re-driven without intent: archive=%d create=%d", provider.archiveCalls, provider.createCalls)
	}

	// ...until repair intent arrives.
	recordRepairIntent(ctx, t, appStore, sb)
	if _, err := reconciler.ReconcileSandbox(ctx, sb); err != nil {
		t.Fatalf("reconcile repair: %v", err)
	}

	updated, err := appStore.GetSandbox(ctx, sb.ProjectID, sb.ID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if updated.ErrorMessage != nil {
		t.Fatalf("error message survived the repair: %q", *updated.ErrorMessage)
	}
	if provider.archiveCalls != 1 || provider.createCalls != 1 {
		t.Fatalf("repair did not rebuild: archive=%d create=%d, want 1 and 1", provider.archiveCalls, provider.createCalls)
	}
}

// A sandbox that has never run has nothing to tear down, and its first create
// is still the one that starts it: repairing it degrades to the ordinary
// first ensure.
func TestReconcileRepairOnANeverRunSandboxSkipsTeardown(t *testing.T) {
	ctx := context.Background()
	appStore := newExecutorTestStore(t)
	sb := repairableSandbox(ctx, t, appStore, model.SandboxStatePending)
	recordRepairIntent(ctx, t, appStore, sb)

	provider := &archiveTestProvider{}
	reconciler := sandboxes.NewSandboxReconciler(appStore, sandboxes.WithSandboxProvider(provider))
	if _, err := reconciler.ReconcileSandbox(ctx, sb); err != nil {
		t.Fatalf("reconcile repair: %v", err)
	}

	if provider.archiveCalls != 0 {
		t.Fatalf("archive calls = %d, want 0: nothing exists to tear down", provider.archiveCalls)
	}
	if provider.createCalls != 1 || !provider.createStart {
		t.Fatalf("create calls = %d (start=%v), want the ordinary starting first create", provider.createCalls, provider.createStart)
	}
}

// Ensure also runs on observation, and those reconciles arrive with the
// generations already in agreement — including on the generation a repair rode.
// Tearing down there re-archived a healthy sandbox on every attach, and the
// sandbox settled as archived with its row still reading `ready`.
func TestReconcileRepairDoesNotTearDownOnceSettled(t *testing.T) {
	ctx := context.Background()
	appStore := newExecutorTestStore(t)
	sb := repairableSandbox(ctx, t, appStore, model.SandboxStateReady)
	recordRepairIntent(ctx, t, appStore, sb)

	provider := &archiveTestProvider{}
	reconciler := sandboxes.NewSandboxReconciler(appStore, sandboxes.WithSandboxProvider(provider))
	if _, err := reconciler.ReconcileSandbox(ctx, sb); err != nil {
		t.Fatalf("reconcile repair: %v", err)
	}

	// What an attach arrives as: the row is reloaded and reconciled again with
	// no new intent, so RepairGeneration still names the current generation.
	settled, err := appStore.GetSandbox(ctx, sb.ProjectID, sb.ID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if settled.RepairGeneration != settled.Generation || !settled.Converged() {
		t.Fatalf("fixture does not exercise the case: repair generation %d, generation %d, observed %d",
			settled.RepairGeneration, settled.Generation, settled.ObservedGeneration)
	}
	if _, err := reconciler.ReconcileSandbox(ctx, settled); err != nil {
		t.Fatalf("reconcile settled repair: %v", err)
	}

	if provider.archiveCalls != 1 {
		t.Fatalf("archive calls = %d, want 1: a settled repair must not tear down again", provider.archiveCalls)
	}
}
