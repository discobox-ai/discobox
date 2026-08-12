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

// stateReportFixture builds a project with one pool and two sandboxes on it,
// each converged and observed running.
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
	project := &model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project"}
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
				State:        model.SandboxStateReady,
			},
			RuntimeState: model.SandboxRuntimeStateRunning,
		}
		if err := appStore.CreateSandbox(ctx, sandbox); err != nil {
			t.Fatalf("create sandbox %s: %v", id, err)
		}
	}
	return ctx, NewService(appStore, nil, "user-1", engine), appStore
}

func loadSandbox(ctx context.Context, t *testing.T, st *store.Store, id string) *model.Sandbox {
	t.Helper()
	sandbox, err := st.GetSandbox(ctx, "project-1", id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return sandbox
}

func runtimeStateOf(ctx context.Context, t *testing.T, st *store.Store, id string) string {
	t.Helper()
	return loadSandbox(ctx, t, st, id).RuntimeState
}

func stateOf(ctx context.Context, t *testing.T, st *store.Store, id string) string {
	t.Helper()
	return loadSandbox(ctx, t, st, id).State
}

// A delta says nothing about the sandboxes it does not mention.
func TestStateReportDeltaTouchesOnlyReportedSandboxes(t *testing.T) {
	ctx, service, st := stateReportFixture(t)

	err := service.ReportSandboxStates(ctx, store.SandboxStateReportBatch{
		PoolID: "pool-1", BootID: "boot-1", Sequence: 1, ReportedAt: time.Now().UTC(),
		Reports: []store.SandboxStateReport{{SandboxID: "sandbox-1", State: model.SandboxRuntimeStateStopped}},
	})
	if err != nil {
		t.Fatalf("report states: %v", err)
	}
	if got := runtimeStateOf(ctx, t, st, "sandbox-1"); got != model.SandboxRuntimeStateStopped {
		t.Fatalf("sandbox-1 runtime state = %q, want stopped", got)
	}
	if got := runtimeStateOf(ctx, t, st, "sandbox-2"); got != model.SandboxRuntimeStateRunning {
		t.Fatalf("sandbox-2 runtime state = %q, want running: a delta must not touch what it omits", got)
	}
}

// A report moves the runtime axis and nothing else. The existence axis is the
// reconciler's, and an agent has no view of whether a sandbox has converged
// (ADR 0034 §§1–2).
func TestStateReportDoesNotWriteTheExistenceState(t *testing.T) {
	ctx, service, st := stateReportFixture(t)

	if err := service.ReportSandboxStates(ctx, store.SandboxStateReportBatch{
		PoolID: "pool-1", BootID: "boot-1", Sequence: 1, ReportedAt: time.Now().UTC(),
		Reports: []store.SandboxStateReport{{SandboxID: "sandbox-1", State: model.SandboxRuntimeStateStopped}},
	}); err != nil {
		t.Fatalf("report states: %v", err)
	}

	if got := stateOf(ctx, t, st, "sandbox-1"); got != model.SandboxStateReady {
		t.Fatalf("sandbox-1 state = %q, want ready: a runtime report must not move the existence axis", got)
	}
}

// The regression this split exists to prevent: a report lands while a slow
// reconcile is in flight, and the reconcile's own write must not carry the
// pre-report value back over it. Before ADR 0034 both facts shared one column,
// so the sandbox was pushed back to `pending` and stayed there until the pool
// agent's next complete sync, up to 60 seconds later.
func TestStateReportSurvivesAConcurrentReconcileWrite(t *testing.T) {
	ctx, service, st := stateReportFixture(t)

	// What a reconcile holds: the sandbox as it looked before the provider call.
	stale := loadSandbox(ctx, t, st, "sandbox-1")
	stale.SetState(model.SandboxStatePending)
	stale.RuntimeState = ""

	if err := service.ReportSandboxStates(ctx, store.SandboxStateReportBatch{
		PoolID: "pool-1", BootID: "boot-1", Sequence: 1, ReportedAt: time.Now().UTC(),
		Reports: []store.SandboxStateReport{{SandboxID: "sandbox-1", State: model.SandboxRuntimeStateRunning}},
	}); err != nil {
		t.Fatalf("report states: %v", err)
	}

	// The reconcile finishes and saves what it was holding.
	stale.SetState(model.SandboxStateReady)
	if err := st.UpdateSandbox(ctx, stale); err != nil {
		t.Fatalf("write stale reconcile result: %v", err)
	}

	after := loadSandbox(ctx, t, st, "sandbox-1")
	if after.RuntimeState != model.SandboxRuntimeStateRunning {
		t.Fatalf("runtime state = %q, want running: a reconcile write must not replay a pre-report observation", after.RuntimeState)
	}
	if after.State != model.SandboxStateReady {
		t.Fatalf("state = %q, want ready: the reconciler still owns the existence axis", after.State)
	}
	if after.StateReportBoot != "boot-1" || after.StateReportSeq != 1 {
		t.Fatalf("report watermark = %q/%d, want boot-1/1: a reconcile write must not reset it",
			after.StateReportBoot, after.StateReportSeq)
	}
}

// A complete sync is the level-triggered half: a sandbox it omits has no
// container, which is the case that went unnoticed for 41 hours under the old
// destroy-only reporting.
func TestStateReportCompleteSyncStopsOmittedSandboxes(t *testing.T) {
	ctx, service, st := stateReportFixture(t)

	err := service.ReportSandboxStates(ctx, store.SandboxStateReportBatch{
		PoolID: "pool-1", BootID: "boot-1", Sequence: 1, ReportedAt: time.Now().UTC(), Complete: true,
		Reports: []store.SandboxStateReport{{SandboxID: "sandbox-1", State: model.SandboxRuntimeStateRunning}},
	})
	if err != nil {
		t.Fatalf("report states: %v", err)
	}
	if got := runtimeStateOf(ctx, t, st, "sandbox-1"); got != model.SandboxRuntimeStateRunning {
		t.Fatalf("sandbox-1 runtime state = %q, want running", got)
	}
	if got := runtimeStateOf(ctx, t, st, "sandbox-2"); got != model.SandboxRuntimeStateStopped {
		t.Fatalf("sandbox-2 runtime state = %q, want stopped: a complete sync reports absence by omission", got)
	}
}

// An archived sandbox has no container by intent, so every complete sync omits
// it — the same signal a lost container sends. Recording that as `stopped` would
// hand the reconciler drift to repair, and it would rebuild the container the
// archive just removed, putting the sandbox back beyond its retention policy
// (ADR 0022 §5). This is the sharpest edge in the archive change.
func TestStateReportCompleteSyncDoesNotResurrectArchivedSandboxes(t *testing.T) {
	ctx, service, st := stateReportFixture(t)

	archived := loadSandbox(ctx, t, st, "sandbox-2")
	archived.DesiredState = model.DesiredStateArchived
	archived.SetState(model.SandboxStateArchived)
	if err := st.UpdateSandbox(ctx, archived); err != nil {
		t.Fatalf("archive sandbox-2: %v", err)
	}

	// A complete sync listing only the live sandbox: sandbox-2 is absent, which
	// for any other sandbox would mean "your container is gone".
	if err := service.ReportSandboxStates(ctx, store.SandboxStateReportBatch{
		PoolID: "pool-1", BootID: "boot-1", Sequence: 1, ReportedAt: time.Now().UTC(), Complete: true,
		Reports: []store.SandboxStateReport{{SandboxID: "sandbox-1", State: model.SandboxRuntimeStateRunning}},
	}); err != nil {
		t.Fatalf("report states: %v", err)
	}

	current := loadSandbox(ctx, t, st, "sandbox-2")
	if current.State != model.SandboxStateArchived {
		t.Fatalf("sandbox-2 state = %q, want archived: a runtime report must not un-archive a sandbox", current.State)
	}
	if current.DesiredState != model.DesiredStateArchived {
		t.Fatalf("sandbox-2 desired state = %q, want archived", current.DesiredState)
	}
	// Its last observation is stale by construction — reports stopped covering
	// it — so nothing may read it as live.
	if model.SandboxIsLive(current) {
		t.Fatal("archived sandbox reads as live: a stale runtime observation must not outrank the archive")
	}
}

// A delayed delta must not overwrite a newer sync.
func TestStateReportIgnoresOutOfOrderBatches(t *testing.T) {
	ctx, service, st := stateReportFixture(t)
	now := time.Now().UTC()

	newer := store.SandboxStateReportBatch{
		PoolID: "pool-1", BootID: "boot-1", Sequence: 5, ReportedAt: now,
		Reports: []store.SandboxStateReport{{SandboxID: "sandbox-1", State: model.SandboxRuntimeStateStopped}},
	}
	if err := service.ReportSandboxStates(ctx, newer); err != nil {
		t.Fatalf("report newer: %v", err)
	}

	stale := store.SandboxStateReportBatch{
		PoolID: "pool-1", BootID: "boot-1", Sequence: 2, ReportedAt: now.Add(-time.Minute),
		Reports: []store.SandboxStateReport{{SandboxID: "sandbox-1", State: model.SandboxRuntimeStateRunning}},
	}
	if err := service.ReportSandboxStates(ctx, stale); err != nil {
		t.Fatalf("report stale: %v", err)
	}
	if got := runtimeStateOf(ctx, t, st, "sandbox-1"); got != model.SandboxRuntimeStateStopped {
		t.Fatalf("sandbox-1 runtime state = %q, want stopped: a lower sequence in the same boot is stale", got)
	}
}

// A restarted agent counts from zero again, so its reports must still land.
func TestStateReportAcceptsANewBootRegardlessOfSequence(t *testing.T) {
	ctx, service, st := stateReportFixture(t)
	now := time.Now().UTC()

	if err := service.ReportSandboxStates(ctx, store.SandboxStateReportBatch{
		PoolID: "pool-1", BootID: "boot-1", Sequence: 9, ReportedAt: now,
		Reports: []store.SandboxStateReport{{SandboxID: "sandbox-1", State: model.SandboxRuntimeStateRunning}},
	}); err != nil {
		t.Fatalf("report first boot: %v", err)
	}
	if err := service.ReportSandboxStates(ctx, store.SandboxStateReportBatch{
		PoolID: "pool-1", BootID: "boot-2", Sequence: 1, ReportedAt: now.Add(time.Second),
		Reports: []store.SandboxStateReport{{SandboxID: "sandbox-1", State: model.SandboxRuntimeStateStopped}},
	}); err != nil {
		t.Fatalf("report second boot: %v", err)
	}
	if got := runtimeStateOf(ctx, t, st, "sandbox-1"); got != model.SandboxRuntimeStateStopped {
		t.Fatalf("sandbox-1 runtime state = %q, want stopped: a new boot's sequence is not comparable to the old one", got)
	}
}

// Reports are observations. They must never be mistaken for intent.
func TestStateReportDoesNotWriteIntent(t *testing.T) {
	ctx, service, st := stateReportFixture(t)
	before := loadSandbox(ctx, t, st, "sandbox-1")

	if err := service.ReportSandboxStates(ctx, store.SandboxStateReportBatch{
		PoolID: "pool-1", BootID: "boot-1", Sequence: 1, ReportedAt: time.Now().UTC(),
		Reports: []store.SandboxStateReport{{SandboxID: "sandbox-1", State: model.SandboxRuntimeStateStopped}},
	}); err != nil {
		t.Fatalf("report states: %v", err)
	}

	after := loadSandbox(ctx, t, st, "sandbox-1")
	if after.DesiredState != before.DesiredState {
		t.Fatalf("desired state moved from %q to %q: an observation is not a request", before.DesiredState, after.DesiredState)
	}
	if after.Generation != before.Generation {
		t.Fatalf("generation moved from %d to %d: an observation is not new intent", before.Generation, after.Generation)
	}
}

// A container that is gone is a different fact from a sandbox that is stopped,
// even though both record the same runtime state. Neither is intent: a report
// never bumps a generation, because a generation versions the spec and nobody
// edited it. The rebuild is driven by the dirty mark and the reconciler's
// idempotent ensure.
func TestCompleteSyncNeverBumpsAGeneration(t *testing.T) {
	ctx, service, st := stateReportFixture(t)
	for _, id := range []string{"sandbox-1", "sandbox-2"} {
		sandbox := loadSandbox(ctx, t, st, id)
		sandbox.ObservedGeneration = sandbox.Generation
		if err := st.UpdateSandbox(ctx, sandbox); err != nil {
			t.Fatalf("converge %s: %v", id, err)
		}
	}

	// sandbox-1 is reported stopped; sandbox-2 is omitted, so its container is gone.
	if err := service.ReportSandboxStates(ctx, store.SandboxStateReportBatch{
		PoolID: "pool-1", BootID: "boot-1", Sequence: 1, ReportedAt: time.Now().UTC(), Complete: true,
		Reports: []store.SandboxStateReport{{SandboxID: "sandbox-1", State: model.SandboxRuntimeStateStopped}},
	}); err != nil {
		t.Fatalf("report states: %v", err)
	}

	for _, id := range []string{"sandbox-1", "sandbox-2"} {
		sandbox := loadSandbox(ctx, t, st, id)
		if !sandbox.Converged() {
			t.Fatalf("%s generations = %d/%d: an observation is not an orchestration event",
				id, sandbox.ObservedGeneration, sandbox.Generation)
		}
		if sandbox.RuntimeState != model.SandboxRuntimeStateStopped {
			t.Fatalf("%s runtime state = %q, want stopped", id, sandbox.RuntimeState)
		}
	}
}
