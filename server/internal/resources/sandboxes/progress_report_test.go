package sandboxes

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// recordingPublisher captures what the store fans out, which is the only way to
// tell "written to the database" from "visible to a waiting client".
type recordingPublisher struct {
	mu     sync.Mutex
	events []model.ProjectEvent
}

func (p *recordingPublisher) PublishProjectEvent(_ context.Context, event model.ProjectEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, event)
}

func (p *recordingPublisher) sandboxEvents(sandboxID string) []model.ProjectEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []model.ProjectEvent
	for _, event := range p.events {
		if event.ResourceType == "sandbox" && event.ResourceID == sandboxID {
			out = append(out, event)
		}
	}
	return out
}

func (p *recordingPublisher) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = nil
}

// progressFixture is stateReportFixture with a publisher attached, so the tests
// below can assert on what reached the broker.
func progressFixture(t *testing.T) (context.Context, *Service, *store.Store, *recordingPublisher) {
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
	publisher := &recordingPublisher{}
	appStore := store.New(db.Write, db.Read, store.WithPublisher(publisher))
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
	sandbox := &model.Sandbox{
		ID: "sandbox-1", ProjectID: project.ID, CreatedByUserID: "user-1", Name: "sandbox-1", PoolID: pool.ID,
		ResourceLifecycle: model.ResourceLifecycle{
			DesiredState: model.DesiredStatePresent,
			State:        model.SandboxStateReady,
		},
		RuntimeState: model.SandboxRuntimeStateStopped,
	}
	if err := appStore.CreateSandbox(ctx, sandbox); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	publisher.reset()
	return ctx, NewService(appStore, nil, "user-1", engine), appStore, publisher
}

// A sandbox becoming usable is the transition a client waiting to attach cares
// about most, and it used to reach the database and nothing else: the state
// report path wrote with a raw update that skipped event creation, and
// observationNeedsReconcile deliberately ignores a sandbox that came up. A
// waiting client therefore had nothing to wake on (ADR 0039).
func TestStateReportPublishesTheTransition(t *testing.T) {
	ctx, service, _, publisher := progressFixture(t)

	err := service.ReportSandboxStates(ctx, store.SandboxStateReportBatch{
		PoolID:     "pool-1",
		BootID:     "boot-1",
		Sequence:   1,
		ReportedAt: time.Now().UTC(),
		Reports: []store.SandboxStateReport{
			{SandboxID: "sandbox-1", State: model.SandboxRuntimeStateRunning},
		},
	})
	if err != nil {
		t.Fatalf("ReportSandboxStates: %v", err)
	}

	events := publisher.sandboxEvents("sandbox-1")
	if len(events) != 1 {
		t.Fatalf("published %d events, want 1 for the running transition", len(events))
	}
	if !strings.Contains(string(events[0].Data), model.SandboxRuntimeStateRunning) {
		t.Fatalf("event payload does not carry the new state: %s", events[0].Data)
	}
}

// The complete sync re-reports every sandbox on the pool on its interval. An
// event per sandbox per sync would be a heartbeat wearing a resource event's
// clothes, so only reports that actually change something are published.
func TestRepeatedStateReportsPublishNothingNew(t *testing.T) {
	ctx, service, _, publisher := progressFixture(t)

	report := func(sequence int64) {
		t.Helper()
		err := service.ReportSandboxStates(ctx, store.SandboxStateReportBatch{
			PoolID:     "pool-1",
			BootID:     "boot-1",
			Sequence:   sequence,
			ReportedAt: time.Now().UTC(),
			Complete:   true,
			Reports: []store.SandboxStateReport{
				{SandboxID: "sandbox-1", State: model.SandboxRuntimeStateRunning},
			},
		})
		if err != nil {
			t.Fatalf("ReportSandboxStates: %v", err)
		}
	}
	report(1)
	publisher.reset()
	report(2)
	report(3)

	if events := publisher.sandboxEvents("sandbox-1"); len(events) != 0 {
		t.Fatalf("published %d events for unchanged states, want 0", len(events))
	}
}

// Progress is recorded and published every time, because a client reads it to
// say what it is waiting for. It must not disturb the sandbox's state.
func TestProgressReportIsRecordedAndPublished(t *testing.T) {
	ctx, service, appStore, publisher := progressFixture(t)

	payload := json.RawMessage(`{"pull":{"image":"ghcr.io/example/sandbox:latest","current":50,"total":100,"layers":3,"layersComplete":1}}`)
	at := time.Now().UTC()
	err := service.ReportSandboxProgress(ctx, "pool-1", at, []store.SandboxProgressReport{
		{SandboxID: "sandbox-1", Progress: payload},
	})
	if err != nil {
		t.Fatalf("ReportSandboxProgress: %v", err)
	}

	sandbox, err := appStore.GetSandbox(ctx, "project-1", "sandbox-1")
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if !strings.Contains(string(sandbox.ProvisionProgress), "ghcr.io/example/sandbox:latest") {
		t.Fatalf("progress = %s, want the reported pull", sandbox.ProvisionProgress)
	}
	if sandbox.ProvisionProgressAt == nil {
		t.Fatal("progress timestamp was not recorded")
	}
	// Progress says nothing about power state, and must not be mistaken for it.
	if sandbox.RuntimeState != model.SandboxRuntimeStateStopped {
		t.Fatalf("runtime state = %q, want it untouched at %q", sandbox.RuntimeState, model.SandboxRuntimeStateStopped)
	}
	if events := publisher.sandboxEvents("sandbox-1"); len(events) != 1 {
		t.Fatalf("published %d events, want 1 for the progress report", len(events))
	}
}

// A pool reporting progress for a sandbox it does not host, or one this control
// plane has never heard of, is not an error worth failing the whole batch over —
// but it must not write anything either.
func TestProgressReportIgnoresSandboxesThePoolDoesNotHost(t *testing.T) {
	ctx, service, appStore, publisher := progressFixture(t)

	err := service.ReportSandboxProgress(ctx, "pool-1", time.Now().UTC(), []store.SandboxProgressReport{
		{SandboxID: "sandbox-does-not-exist", Progress: json.RawMessage(`{"pull":{"image":"img"}}`)},
	})
	if err != nil {
		t.Fatalf("ReportSandboxProgress: %v", err)
	}
	sandbox, err := appStore.GetSandbox(ctx, "project-1", "sandbox-1")
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if len(sandbox.ProvisionProgress) != 0 {
		t.Fatalf("unrelated sandbox got progress: %s", sandbox.ProvisionProgress)
	}
	if events := publisher.sandboxEvents("sandbox-1"); len(events) != 0 {
		t.Fatalf("published %d events, want 0", len(events))
	}
}
