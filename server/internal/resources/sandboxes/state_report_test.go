package sandboxes

import (
	"context"
	"testing"
	"time"

	"github.com/obot-platform/discobox/server/internal/database"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/reconcile"
	"github.com/obot-platform/discobox/server/internal/store"
)

// stateReportFixture builds a project with one pool and two sandboxes on it.
func stateReportFixture(t *testing.T) (context.Context, *Service, *store.Store) {
	t.Helper()
	ctx := context.Background()
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	appStore := store.New(db.Write, db.Read)
	engine, err := reconcile.New(db.Write, reconcile.Options{SingleNode: true})
	if err != nil {
		t.Fatalf("create reconcile engine: %v", err)
	}
	project := &model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project", Slug: "project"}
	if err := db.Write.WithContext(ctx).Create(project).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: project.ID, Type: "test", Name: "Test"}
	if err := appStore.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	pool := &model.Pool{ID: "pool-1", ProjectID: project.ID, PoolManifest: model.PoolManifest{Name: "pool-1", ProviderInstanceID: provider.ID}}
	if err := appStore.CreatePool(ctx, pool); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	for _, id := range []string{"sandbox-1", "sandbox-2"} {
		sandbox := &model.Sandbox{
			ID: id, ProjectID: project.ID, CreatedByUserID: "user-1", Name: id, PoolID: pool.ID,
			ResourceLifecycle: model.ResourceLifecycle{
				DesiredState: model.DesiredStatePresent,
				State:        model.SandboxStateRunning,
			},
		}
		if err := appStore.CreateSandbox(ctx, sandbox); err != nil {
			t.Fatalf("create sandbox %s: %v", id, err)
		}
	}
	return ctx, NewService(appStore, nil, "user-1", engine), appStore
}

func stateOf(ctx context.Context, t *testing.T, st *store.Store, id string) string {
	t.Helper()
	sandbox, err := st.GetSandbox(ctx, "project-1", id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return sandbox.State
}

// A delta says nothing about the sandboxes it does not mention.
func TestStateReportDeltaTouchesOnlyReportedSandboxes(t *testing.T) {
	ctx, service, st := stateReportFixture(t)

	err := service.ReportSandboxStates(ctx, store.SandboxStateReportBatch{
		PoolID: "pool-1", BootID: "boot-1", Sequence: 1, ReportedAt: time.Now().UTC(),
		Reports: []store.SandboxStateReport{{SandboxID: "sandbox-1", State: model.SandboxStateStopped}},
	})
	if err != nil {
		t.Fatalf("report states: %v", err)
	}
	if got := stateOf(ctx, t, st, "sandbox-1"); got != model.SandboxStateStopped {
		t.Fatalf("sandbox-1 state = %q, want stopped", got)
	}
	if got := stateOf(ctx, t, st, "sandbox-2"); got != model.SandboxStateRunning {
		t.Fatalf("sandbox-2 state = %q, want running: a delta must not touch what it omits", got)
	}
}

// A complete sync is the level-triggered half: a sandbox it omits has no
// container, which is the case that went unnoticed for 41 hours under the old
// destroy-only reporting.
func TestStateReportCompleteSyncStopsOmittedSandboxes(t *testing.T) {
	ctx, service, st := stateReportFixture(t)

	err := service.ReportSandboxStates(ctx, store.SandboxStateReportBatch{
		PoolID: "pool-1", BootID: "boot-1", Sequence: 1, ReportedAt: time.Now().UTC(), Complete: true,
		Reports: []store.SandboxStateReport{{SandboxID: "sandbox-1", State: model.SandboxStateRunning}},
	})
	if err != nil {
		t.Fatalf("report states: %v", err)
	}
	if got := stateOf(ctx, t, st, "sandbox-1"); got != model.SandboxStateRunning {
		t.Fatalf("sandbox-1 state = %q, want running", got)
	}
	if got := stateOf(ctx, t, st, "sandbox-2"); got != model.SandboxStateStopped {
		t.Fatalf("sandbox-2 state = %q, want stopped: a complete sync reports absence by omission", got)
	}
}

// A delayed delta must not overwrite a newer sync.
func TestStateReportIgnoresOutOfOrderBatches(t *testing.T) {
	ctx, service, st := stateReportFixture(t)
	now := time.Now().UTC()

	newer := store.SandboxStateReportBatch{
		PoolID: "pool-1", BootID: "boot-1", Sequence: 5, ReportedAt: now,
		Reports: []store.SandboxStateReport{{SandboxID: "sandbox-1", State: model.SandboxStateStopped}},
	}
	if err := service.ReportSandboxStates(ctx, newer); err != nil {
		t.Fatalf("report newer: %v", err)
	}

	stale := store.SandboxStateReportBatch{
		PoolID: "pool-1", BootID: "boot-1", Sequence: 2, ReportedAt: now.Add(-time.Minute),
		Reports: []store.SandboxStateReport{{SandboxID: "sandbox-1", State: model.SandboxStateRunning}},
	}
	if err := service.ReportSandboxStates(ctx, stale); err != nil {
		t.Fatalf("report stale: %v", err)
	}
	if got := stateOf(ctx, t, st, "sandbox-1"); got != model.SandboxStateStopped {
		t.Fatalf("sandbox-1 state = %q, want stopped: a lower sequence in the same boot is stale", got)
	}
}

// A restarted agent counts from zero again, so its reports must still land.
func TestStateReportAcceptsANewBootRegardlessOfSequence(t *testing.T) {
	ctx, service, st := stateReportFixture(t)
	now := time.Now().UTC()

	if err := service.ReportSandboxStates(ctx, store.SandboxStateReportBatch{
		PoolID: "pool-1", BootID: "boot-1", Sequence: 9, ReportedAt: now,
		Reports: []store.SandboxStateReport{{SandboxID: "sandbox-1", State: model.SandboxStateRunning}},
	}); err != nil {
		t.Fatalf("report first boot: %v", err)
	}
	if err := service.ReportSandboxStates(ctx, store.SandboxStateReportBatch{
		PoolID: "pool-1", BootID: "boot-2", Sequence: 1, ReportedAt: now.Add(time.Second),
		Reports: []store.SandboxStateReport{{SandboxID: "sandbox-1", State: model.SandboxStateStopped}},
	}); err != nil {
		t.Fatalf("report second boot: %v", err)
	}
	if got := stateOf(ctx, t, st, "sandbox-1"); got != model.SandboxStateStopped {
		t.Fatalf("sandbox-1 state = %q, want stopped: a new boot's sequence is not comparable to the old one", got)
	}
}

// Reports are observations. They must never be mistaken for intent.
func TestStateReportDoesNotWriteIntent(t *testing.T) {
	ctx, service, st := stateReportFixture(t)
	before, err := st.GetSandbox(ctx, "project-1", "sandbox-1")
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}

	if err := service.ReportSandboxStates(ctx, store.SandboxStateReportBatch{
		PoolID: "pool-1", BootID: "boot-1", Sequence: 1, ReportedAt: time.Now().UTC(),
		Reports: []store.SandboxStateReport{{SandboxID: "sandbox-1", State: model.SandboxStateStopped}},
	}); err != nil {
		t.Fatalf("report states: %v", err)
	}

	after, err := st.GetSandbox(ctx, "project-1", "sandbox-1")
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if after.DesiredState != before.DesiredState {
		t.Fatalf("desired state moved from %q to %q: an observation is not a request", before.DesiredState, after.DesiredState)
	}
	if after.Generation != before.Generation {
		t.Fatalf("generation moved from %d to %d: an observation is not new intent", before.Generation, after.Generation)
	}
}

// A container that is gone is a different fact from a sandbox that is stopped,
// even though both record the same state. Neither is intent: a report never
// bumps a generation, because a generation versions the spec and nobody edited
// it. The rebuild is driven by the dirty mark and the reconciler's idempotent
// ensure.
func TestCompleteSyncNeverBumpsAGeneration(t *testing.T) {
	ctx, service, st := stateReportFixture(t)
	for _, id := range []string{"sandbox-1", "sandbox-2"} {
		sandbox, err := st.GetSandbox(ctx, "project-1", id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		sandbox.ObservedGeneration = sandbox.Generation
		if err := st.UpdateSandbox(ctx, sandbox); err != nil {
			t.Fatalf("converge %s: %v", id, err)
		}
	}

	// sandbox-1 is reported stopped; sandbox-2 is omitted, so its container is gone.
	if err := service.ReportSandboxStates(ctx, store.SandboxStateReportBatch{
		PoolID: "pool-1", BootID: "boot-1", Sequence: 1, ReportedAt: time.Now().UTC(), Complete: true,
		Reports: []store.SandboxStateReport{{SandboxID: "sandbox-1", State: model.SandboxStateStopped}},
	}); err != nil {
		t.Fatalf("report states: %v", err)
	}

	for _, id := range []string{"sandbox-1", "sandbox-2"} {
		sandbox, err := st.GetSandbox(ctx, "project-1", id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if !sandbox.Converged() {
			t.Fatalf("%s generations = %d/%d: an observation is not an orchestration event",
				id, sandbox.ObservedGeneration, sandbox.Generation)
		}
		if sandbox.State != model.SandboxStateStopped {
			t.Fatalf("%s state = %q, want stopped", id, sandbox.State)
		}
	}
}
