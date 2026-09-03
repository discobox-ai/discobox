package sandboxes_test

import (
	"context"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/resources/sandboxes"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// archiveTestProvider records which existence call it received, so a test can
// tell "keep the data" from "destroy it" — the distinction the provider
// contract could not express before ADR 0022 §6.
type archiveTestProvider struct {
	sandboxes.Provider
	createCalls  int
	createStart  bool
	archiveCalls int
	removeCalls  int
}

// Create is what unarchiving runs: the container is rebuilt against the tree
// that was retained. createStart records whether it was asked to start, which
// unarchive must not do.
func (p *archiveTestProvider) Create(_ context.Context, _ sandboxes.SandboxRef, state []byte, opts sandboxes.CreateOptions) (*sandboxes.Sandbox, []byte, error) {
	p.createCalls++
	p.createStart = opts.Start
	return &sandboxes.Sandbox{ID: "runtime-1"}, state, nil
}

func (p *archiveTestProvider) Archive(context.Context, sandboxes.SandboxRef, []byte) ([]byte, error) {
	p.archiveCalls++
	return nil, nil
}

func (p *archiveTestProvider) Remove(context.Context, sandboxes.SandboxRef, []byte) ([]byte, error) {
	p.removeCalls++
	return nil, nil
}

// archivableSandbox creates a sandbox asking to be archived, with its state
// anchor placed archivedFor in the past. The anchor is what the retention
// deadline derives from, so backdating it is how a test reaches expiry without
// waiting for one.
func archivableSandbox(ctx context.Context, t *testing.T, appStore *store.Store, state string, archivedFor time.Duration) *model.Sandbox {
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
			DesiredState: model.DesiredStateArchived,
			State:        state,
			Generation:   1,
		},
	}
	if err := appStore.CreateSandbox(ctx, sb); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if archivedFor > 0 {
		sb.StateChangedAt = time.Now().UTC().Add(-archivedFor)
		if err := appStore.UpdateSandbox(ctx, sb); err != nil {
			t.Fatalf("backdate state anchor: %v", err)
		}
	}
	return sb
}

// Archiving asks the provider to drop the runtime and keep the data. The row
// survives, which is what makes the sandbox restorable and what gives the
// retention sweep something to find.
func TestReconcileArchiveKeepsTheSandboxAndItsData(t *testing.T) {
	ctx := context.Background()
	appStore := newExecutorTestStore(t)
	sb := archivableSandbox(ctx, t, appStore, model.SandboxStateReady, 0)

	provider := &archiveTestProvider{}
	reconciler := sandboxes.NewSandboxReconciler(appStore, sandboxes.WithSandboxProvider(provider))
	if _, err := reconciler.ReconcileSandbox(ctx, sb); err != nil {
		t.Fatalf("reconcile archive: %v", err)
	}

	if provider.archiveCalls != 1 {
		t.Fatalf("archive calls = %d, want 1", provider.archiveCalls)
	}
	if provider.removeCalls != 0 {
		t.Fatalf("archive destroyed the data: remove calls = %d, want 0", provider.removeCalls)
	}
	updated, err := appStore.GetSandbox(ctx, sb.ProjectID, sb.ID)
	if err != nil {
		t.Fatalf("get sandbox after archive: %v", err)
	}
	if updated.State != model.SandboxStateArchived {
		t.Fatalf("state = %q, want archived", updated.State)
	}
	if !updated.Converged() {
		t.Fatalf("archive did not settle: generation %d, observed %d", updated.Generation, updated.ObservedGeneration)
	}
}

// Within retention, re-reconciling an archived sandbox must leave it alone. The
// reconcile happens on every wake-up the expiry timer schedules, so a version
// that re-archived or re-stamped the anchor each time would push the deadline
// out forever and the sandbox would never be purged.
func TestReconcileArchivedWithinRetentionIsInert(t *testing.T) {
	ctx := context.Background()
	appStore := newExecutorTestStore(t)
	sb := archivableSandbox(ctx, t, appStore, model.SandboxStateArchived, time.Hour)
	anchor := sb.StateChangedAt

	provider := &archiveTestProvider{}
	reconciler := sandboxes.NewSandboxReconciler(appStore, sandboxes.WithSandboxProvider(provider))
	if _, err := reconciler.ReconcileSandbox(ctx, sb); err != nil {
		t.Fatalf("reconcile archived: %v", err)
	}

	if provider.removeCalls != 0 {
		t.Fatalf("an unexpired archive was purged: remove calls = %d, want 0", provider.removeCalls)
	}
	updated, err := appStore.GetSandbox(ctx, sb.ProjectID, sb.ID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if updated.State != model.SandboxStateArchived {
		t.Fatalf("state = %q, want archived", updated.State)
	}
	if !updated.StateChangedAt.Equal(anchor) {
		t.Fatalf("reconcile moved the retention anchor %v -> %v; the deadline would never arrive",
			anchor, updated.StateChangedAt)
	}
}

// Past retention the sandbox is purged: the provider is asked to destroy the
// data, and the row goes only after that call returns.
func TestReconcileArchivedPastRetentionPurges(t *testing.T) {
	ctx := context.Background()
	appStore := newExecutorTestStore(t)
	sb := archivableSandbox(ctx, t, appStore, model.SandboxStateArchived, sandboxes.DefaultArchiveRetention+time.Hour)

	provider := &archiveTestProvider{}
	reconciler := sandboxes.NewSandboxReconciler(appStore, sandboxes.WithSandboxProvider(provider))
	if _, err := reconciler.ReconcileSandbox(ctx, sb); err != nil {
		t.Fatalf("reconcile expired archive: %v", err)
	}

	if provider.removeCalls != 1 {
		t.Fatalf("remove calls = %d, want 1", provider.removeCalls)
	}
	if _, err := appStore.GetSandbox(ctx, sb.ProjectID, sb.ID); err == nil {
		t.Fatal("sandbox still present after its retention expired")
	}
}

// Unarchiving rebuilds the container, so the sandbox must stop reading
// `archived` as soon as that happens. Waiting for the pool agent's next
// complete sync would report an archived sandbox that has a container for up to
// a full sync interval — observed live as ~15s of `discobox ls` showing `archived`
// for a sandbox that had already come back.
func TestReconcileUnarchiveLeavesTheSandboxStopped(t *testing.T) {
	ctx := context.Background()
	appStore := newExecutorTestStore(t)
	sb := archivableSandbox(ctx, t, appStore, model.SandboxStateArchived, time.Hour)

	// Unarchive is intent like any other: ask for it to be present again.
	sb.IncrementGeneration()
	sb.RecordIntent(model.DesiredStatePresent)
	if err := appStore.UpdateSandbox(ctx, sb); err != nil {
		t.Fatalf("record unarchive intent: %v", err)
	}

	provider := &archiveTestProvider{}
	reconciler := sandboxes.NewSandboxReconciler(appStore, sandboxes.WithSandboxProvider(provider))
	if _, err := reconciler.ReconcileSandbox(ctx, sb); err != nil {
		t.Fatalf("reconcile unarchive: %v", err)
	}

	if provider.createCalls != 1 {
		t.Fatalf("create calls = %d, want 1", provider.createCalls)
	}
	if provider.createStart {
		t.Fatal("unarchive asked the provider to start the sandbox; it must come back stopped")
	}

	updated, err := appStore.GetSandbox(ctx, sb.ProjectID, sb.ID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if updated.State != model.SandboxStateReady {
		t.Fatalf("state after unarchive = %q, want ready: the container exists again", updated.State)
	}
	if updated.DesiredState != model.DesiredStatePresent {
		t.Fatalf("desired state = %q, want present", updated.DesiredState)
	}
}

// A project's own retention wins over the server default, in both directions:
// a short one expires a sandbox the default would still be holding.
func TestArchiveRetentionFollowsTheProjectSetting(t *testing.T) {
	ctx := context.Background()
	appStore := newExecutorTestStore(t)
	sb := archivableSandbox(ctx, t, appStore, model.SandboxStateArchived, time.Hour)

	project, err := appStore.GetProject(ctx, "project-1")
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	project.ArchiveRetentionSeconds = int64(time.Minute / time.Second)
	if err := appStore.UpsertProject(ctx, project); err != nil {
		t.Fatalf("set project retention: %v", err)
	}

	provider := &archiveTestProvider{}
	reconciler := sandboxes.NewSandboxReconciler(appStore, sandboxes.WithSandboxProvider(provider))
	if _, err := reconciler.ReconcileSandbox(ctx, sb); err != nil {
		t.Fatalf("reconcile expired archive: %v", err)
	}

	if provider.removeCalls != 1 {
		t.Fatalf("remove calls = %d, want 1: a one-minute project retention must expire a one-hour-old archive", provider.removeCalls)
	}
}

// A server-wide default shortens the window for every project that has not
// chosen one, which is what `task dev` sets: a development tree is as large as
// a production one and is discarded many times a day.
func TestArchiveRetentionFollowsTheConfiguredServerDefault(t *testing.T) {
	ctx := context.Background()
	appStore := newExecutorTestStore(t)
	sb := archivableSandbox(ctx, t, appStore, model.SandboxStateArchived, time.Hour)

	provider := &archiveTestProvider{}
	reconciler := sandboxes.NewSandboxReconciler(appStore,
		sandboxes.WithSandboxProvider(provider),
		sandboxes.WithArchiveRetention(time.Minute))
	if _, err := reconciler.ReconcileSandbox(ctx, sb); err != nil {
		t.Fatalf("reconcile expired archive: %v", err)
	}

	if provider.removeCalls != 1 {
		t.Fatalf("remove calls = %d, want 1: a one-minute server default must expire a one-hour-old archive", provider.removeCalls)
	}
}

// The server default is the value a project defers to, not a ceiling on it: a
// project that asked for longer keeps its data for longer.
func TestProjectArchiveRetentionWinsOverTheServerDefault(t *testing.T) {
	ctx := context.Background()
	appStore := newExecutorTestStore(t)
	sb := archivableSandbox(ctx, t, appStore, model.SandboxStateArchived, time.Hour)

	project, err := appStore.GetProject(ctx, "project-1")
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	project.ArchiveRetentionSeconds = int64(24 * time.Hour / time.Second)
	if err := appStore.UpsertProject(ctx, project); err != nil {
		t.Fatalf("set project retention: %v", err)
	}

	provider := &archiveTestProvider{}
	reconciler := sandboxes.NewSandboxReconciler(appStore,
		sandboxes.WithSandboxProvider(provider),
		sandboxes.WithArchiveRetention(time.Minute))
	result, err := reconciler.ReconcileSandbox(ctx, sb)
	if err != nil {
		t.Fatalf("reconcile archived: %v", err)
	}

	if provider.removeCalls != 0 {
		t.Fatalf("remove calls = %d, want 0: the project asked for 24h and the server default must not override it", provider.removeCalls)
	}
	want := sb.StateChangedAt.Add(24 * time.Hour)
	if !result.RequeueAt.Equal(want) {
		t.Fatalf("RequeueAt = %s, want %s (the project's own deadline)", result.RequeueAt, want)
	}
}

// The expiry sweep is the backstop for a lost future-dated mark. An archived
// sandbox has converged, so the ordinary needs-reconcile scan cannot see it, and
// without this a lost mark would mean data kept forever.
func TestExpiredArchiveScanFindsOnlyExpiredSandboxes(t *testing.T) {
	ctx := context.Background()
	appStore := newExecutorTestStore(t)
	sb := archivableSandbox(ctx, t, appStore, model.SandboxStateArchived, time.Hour)

	now := time.Now().UTC()
	refs, err := appStore.ListArchivedSandboxRefsExpiredBefore(ctx, 24*time.Hour, now)
	if err != nil {
		t.Fatalf("scan expired archives: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("scan returned %d unexpired sandboxes, want 0", len(refs))
	}

	refs, err = appStore.ListArchivedSandboxRefsExpiredBefore(ctx, time.Minute, now)
	if err != nil {
		t.Fatalf("scan expired archives: %v", err)
	}
	if len(refs) != 1 || refs[0].SandboxID != sb.ID {
		t.Fatalf("scan = %#v, want just %s", refs, sb.ID)
	}
}

// TestReconcileArchivedArmsTheDeadlineInsteadOfSpinning is the regression test
// for a reconcile loop that burned two CPUs for seventeen hours.
//
// An archived sandbox within retention is converged, so it reports the deadline
// as a Result and the engine parks the row until then. The bug it replaces did
// the same job by marking the sandbox dirty from inside its own reconcile,
// which the engine cannot tell apart from newer intent: the row could never be
// deleted, was never delayed, and re-ran at ~146/sec, forever.
func TestReconcileArchivedArmsTheDeadlineInsteadOfSpinning(t *testing.T) {
	ctx := context.Background()
	appStore := newExecutorTestStore(t)
	archivedFor := time.Hour
	sb := archivableSandbox(ctx, t, appStore, model.SandboxStateArchived, archivedFor)

	provider := &archiveTestProvider{}
	reconciler := sandboxes.NewSandboxReconciler(appStore, sandboxes.WithSandboxProvider(provider))
	result, err := reconciler.ReconcileSandbox(ctx, sb)
	if err != nil {
		t.Fatalf("reconcile archived: %v", err)
	}

	if result.RequeueAt.IsZero() {
		t.Fatal("no deadline armed: the sandbox would wait on the scan backstop, not its own retention")
	}
	want := sb.StateChangedAt.Add(sandboxes.DefaultArchiveRetention)
	if !result.RequeueAt.Equal(want) {
		t.Fatalf("RequeueAt = %s, want %s (the derived retention deadline)", result.RequeueAt, want)
	}
	// The whole point: the next run is a long way off, not immediately.
	if !result.RequeueAt.After(time.Now().Add(time.Hour)) {
		t.Fatalf("RequeueAt = %s, want well into the future", result.RequeueAt)
	}
}

// TestReconcileArchivedIsStableAcrossRepeatedRuns pins the property the loop
// violated: the armed deadline is derived from an anchor no reconcile moves, so
// running again yields the same instant rather than pushing it out.
func TestReconcileArchivedIsStableAcrossRepeatedRuns(t *testing.T) {
	ctx := context.Background()
	appStore := newExecutorTestStore(t)
	sb := archivableSandbox(ctx, t, appStore, model.SandboxStateArchived, time.Hour)

	provider := &archiveTestProvider{}
	reconciler := sandboxes.NewSandboxReconciler(appStore, sandboxes.WithSandboxProvider(provider))

	var first time.Time
	for i := range 3 {
		current, err := appStore.GetSandbox(ctx, sb.ProjectID, sb.ID)
		if err != nil {
			t.Fatalf("get sandbox: %v", err)
		}
		result, err := reconciler.ReconcileSandbox(ctx, current)
		if err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
		if i == 0 {
			first = result.RequeueAt
			continue
		}
		if !result.RequeueAt.Equal(first) {
			t.Fatalf("run %d armed %s, want the stable deadline %s", i, result.RequeueAt, first)
		}
	}
	if provider.removeCalls != 0 {
		t.Fatalf("repeated reconciles purged the data: remove calls = %d", provider.removeCalls)
	}
}
