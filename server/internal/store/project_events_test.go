package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// projectEvents is every event the log holds for project-1, oldest first.
func projectEvents(t *testing.T, s *store.Store) []model.ProjectEvent {
	t.Helper()
	events, err := s.ListProjectEventsAfter(context.Background(), "project-1", 0, 1000)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	return events
}

func sandboxEvent(t *testing.T, event model.ProjectEvent) model.SandboxEvent {
	t.Helper()
	var decoded model.SandboxEvent
	if err := json.Unmarshal(event.Data, &decoded); err != nil {
		t.Fatalf("decode %s: %v", event.Type, err)
	}
	return decoded
}

// requireNewEvents returns the events written since the last call saw
// through seen, and moves seen on.
func requireNewEvents(t *testing.T, s *store.Store, seen *int, want ...string) []model.ProjectEvent {
	t.Helper()
	events := projectEvents(t, s)
	fresh := events[*seen:]
	*seen = len(events)
	types := make([]string, len(fresh))
	for i, event := range fresh {
		types[i] = event.Type
	}
	if !slices.Equal(types, want) {
		t.Fatalf("new events = %v, want %v", types, want)
	}
	return fresh
}

func agentStatus(t *testing.T, state, kind string) model.RawJSON {
	t.Helper()
	program := map[string]any{"state": state, "reportedAt": time.Now().UTC()}
	if kind != "" {
		program["kind"] = kind
	}
	encoded, err := json.Marshal(map[string]any{
		"sessions": []any{
			map[string]any{"terminalId": "t-1", "primary": true, "startedAt": time.Now().UTC(), "programStatus": program},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func reportRuntime(t *testing.T, s *store.Store, seq int64, complete bool, state string) {
	t.Helper()
	batch := store.SandboxStateReportBatch{
		PoolID: "pool-1", BootID: "boot-1", Sequence: seq, ReportedAt: time.Now().UTC(), Complete: complete,
		Reports: []store.SandboxStateReport{{SandboxID: "sandbox-1", State: state}},
	}
	if _, err := s.ApplySandboxStateReports(context.Background(), batch); err != nil {
		t.Fatalf("apply state report: %v", err)
	}
}

// Every writer of a projected field writes an event exactly when the
// projection moves, and the loud writers that move nothing projected — the
// complete sync's restamp, a repeated agent status, a description — write
// none (ADR 26-10-30-868 §4).
func TestSandboxEventsFollowTheProjection(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")
	seen := 0

	sandbox := &model.Sandbox{
		ID: "sandbox-1", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "alpha",
		ResourceLifecycle: model.ResourceLifecycle{DesiredState: model.DesiredStatePresent, State: "pending"},
	}
	if err := s.CreateSandbox(ctx, sandbox); err != nil {
		t.Fatalf("create: %v", err)
	}
	created := sandboxEvent(t, requireNewEvents(t, s, &seen, model.ProjectEventSandboxCreated)[0])
	if created.SandboxID != "sandbox-1" || created.Name != "alpha" || created.State != "pending" || created.DesiredState != model.DesiredStatePresent || created.Agent != nil || created.At.IsZero() {
		t.Fatalf("created = %+v", created)
	}

	// A write that moves nothing projected.
	description := "a description"
	sandbox.Description = &description
	sandbox.Generation++
	if err := s.UpdateSandbox(ctx, sandbox); err != nil {
		t.Fatalf("update: %v", err)
	}
	requireNewEvents(t, s, &seen)

	sandbox.SetState("ready")
	if err := s.UpdateSandbox(ctx, sandbox); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := sandboxEvent(t, requireNewEvents(t, s, &seen, model.ProjectEventSandboxUpdated)[0]); got.State != "ready" {
		t.Fatalf("state = %q, want ready", got.State)
	}

	// The agent is not read while the box is not running.
	if err := s.UpdateSandboxAgentStatus(ctx, "project-1", "sandbox-1", agentStatus(t, "working", ""), time.Now().UTC(), nil); err != nil {
		t.Fatalf("agent status: %v", err)
	}
	requireNewEvents(t, s, &seen)

	reportRuntime(t, s, 1, false, model.SandboxRuntimeStateRunning)
	running := sandboxEvent(t, requireNewEvents(t, s, &seen, model.ProjectEventSandboxUpdated)[0])
	if running.RuntimeState != model.SandboxRuntimeStateRunning || running.Agent == nil || running.Agent.State != "working" {
		t.Fatalf("running = %+v, agent %+v", running, running.Agent)
	}

	// The complete sync restamps a box whose power did not move.
	reportRuntime(t, s, 2, true, model.SandboxRuntimeStateRunning)
	requireNewEvents(t, s, &seen)

	// The same program state again is no change.
	if err := s.UpdateSandboxAgentStatus(ctx, "project-1", "sandbox-1", agentStatus(t, "working", ""), time.Now().UTC(), nil); err != nil {
		t.Fatalf("agent status: %v", err)
	}
	requireNewEvents(t, s, &seen)

	if err := s.UpdateSandboxAgentStatus(ctx, "project-1", "sandbox-1", agentStatus(t, "blocked", "permission"), time.Now().UTC(), nil); err != nil {
		t.Fatalf("agent status: %v", err)
	}
	blocked := sandboxEvent(t, requireNewEvents(t, s, &seen, model.ProjectEventSandboxUpdated)[0])
	if blocked.Agent == nil || blocked.Agent.State != "blocked" || blocked.Agent.Kind != "permission" {
		t.Fatalf("agent = %+v, want blocked on permission", blocked.Agent)
	}

	// Stopping clears the agent in the same event.
	reportRuntime(t, s, 3, false, model.SandboxRuntimeStateStopped)
	stopped := sandboxEvent(t, requireNewEvents(t, s, &seen, model.ProjectEventSandboxUpdated)[0])
	if stopped.RuntimeState != model.SandboxRuntimeStateStopped || stopped.Agent != nil {
		t.Fatalf("stopped = %+v, agent %+v", stopped, stopped.Agent)
	}

	sandbox.RecordIntent(model.DesiredStateArchived)
	sandbox.Generation++
	if err := s.UpdateSandbox(ctx, sandbox); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if got := sandboxEvent(t, requireNewEvents(t, s, &seen, model.ProjectEventSandboxUpdated)[0]); got.DesiredState != model.DesiredStateArchived {
		t.Fatalf("desiredState = %q, want archived", got.DesiredState)
	}

	if err := s.DeleteSandbox(ctx, "project-1", "sandbox-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	deleted := sandboxEvent(t, requireNewEvents(t, s, &seen, model.ProjectEventSandboxDeleted)[0])
	if deleted.SandboxID != "sandbox-1" || deleted.DesiredState != model.DesiredStateArchived {
		t.Fatalf("deleted = %+v, want the last projection", deleted)
	}

	events := projectEvents(t, s)
	for i := 1; i < len(events); i++ {
		if events[i].ID != events[i-1].ID+1 {
			t.Fatalf("event IDs %d then %d: one project's only writer leaves no gap", events[i-1].ID, events[i].ID)
		}
	}
	head, _, err := s.ProjectEventLogBounds(ctx)
	if err != nil || head != events[len(events)-1].ID {
		t.Fatalf("head = %d (%v), want %d", head, err, events[len(events)-1].ID)
	}
}

// An event commits with its change or not at all, a savepoint that rolls back
// included, and a caller's transaction appends its events only when it
// commits.
func TestSandboxEventsCommitWithTheirChange(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")
	seen := 0
	sandbox := &model.Sandbox{ID: "sandbox-1", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "alpha"}
	if err := s.CreateSandbox(ctx, sandbox); err != nil {
		t.Fatalf("create: %v", err)
	}
	requireNewEvents(t, s, &seen, model.ProjectEventSandboxCreated)

	failed := errors.New("caller failed")
	err := s.Transaction(ctx, func(txStore *store.Store, _ *gorm.DB) error {
		sandbox.SetState("ready")
		if err := txStore.UpdateSandbox(ctx, sandbox); err != nil {
			return err
		}
		return failed
	})
	if !errors.Is(err, failed) {
		t.Fatalf("transaction = %v, want the caller's error", err)
	}
	requireNewEvents(t, s, &seen)

	err = s.Transaction(ctx, func(txStore *store.Store, _ *gorm.DB) error {
		sandbox.SetState("ready")
		if err := txStore.UpdateSandbox(ctx, sandbox); err != nil {
			return err
		}
		inner := txStore.Transaction(ctx, func(innerStore *store.Store, _ *gorm.DB) error {
			sandbox.Name = "renamed"
			if err := innerStore.UpdateSandbox(ctx, sandbox); err != nil {
				return err
			}
			return failed
		})
		if !errors.Is(inner, failed) {
			t.Fatalf("savepoint = %v, want the caller's error", inner)
		}
		sandbox.Name = "alpha"
		return nil
	})
	if err != nil {
		t.Fatalf("transaction: %v", err)
	}
	events := requireNewEvents(t, s, &seen, model.ProjectEventSandboxUpdated)
	if got := sandboxEvent(t, events[0]); got.State != "ready" || got.Name != "alpha" {
		t.Fatalf("event = %+v, want the outer change only", got)
	}
}

// Retention removes a prefix of the log and records where it ended, which is
// how a Last-Event-ID it removed is known to be gone.
func TestPruneProjectEventsRecordsWhatItRemoved(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")
	for _, id := range []string{"sandbox-1", "sandbox-2"} {
		if err := s.CreateSandbox(ctx, &model.Sandbox{ID: id, ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: id}); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	events := projectEvents(t, s)
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	if removed, err := s.PruneProjectEvents(ctx, events[0].At.Add(-time.Hour)); err != nil || removed != 0 {
		t.Fatalf("prune before everything = %d, %v", removed, err)
	}
	removed, err := s.PruneProjectEvents(ctx, time.Now().Add(time.Hour))
	if err != nil || removed != 2 {
		t.Fatalf("prune = %d, %v; want 2", removed, err)
	}
	head, prunedThrough, err := s.ProjectEventLogBounds(ctx)
	if err != nil || head != events[1].ID || prunedThrough != events[1].ID {
		t.Fatalf("bounds = %d, %d (%v); want both %d", head, prunedThrough, err, events[1].ID)
	}
	oldest, err := s.OldestProjectEventAt(ctx)
	if err != nil || oldest != nil {
		t.Fatalf("oldest = %v, %v; want an empty log", oldest, err)
	}
}

// A sandbox from before the log has its projection recorded by the upgrade,
// so its first write after it is not an event announcing a change that did
// not happen.
func TestUpgradeRecordsTheProjectionOfExistingSandboxes(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStoreWithDB(t, nil)
	createTestPool(t, s, "project-1", "pool-1")
	if err := s.CreateSandbox(ctx, &model.Sandbox{ID: "sandbox-1", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "alpha"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := db.Write.Model(&model.Sandbox{}).Where("id = ?", "sandbox-1").UpdateColumn("recorded_projection", "").Error; err != nil {
		t.Fatal(err)
	}
	// A database from before the log has neither its events nor its counter.
	for _, table := range []any{&model.ProjectEvent{}, &model.ProjectEventCounter{}} {
		if err := db.Write.Where("1 = 1").Delete(table).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := (&database.DB{Write: db.Write, Read: db.Read}).Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	seen := len(projectEvents(t, s))
	if err := s.UpdateSandboxAgentStatus(ctx, "project-1", "sandbox-1", agentStatus(t, "idle", ""), time.Now().UTC(), nil); err != nil {
		t.Fatalf("agent status: %v", err)
	}
	requireNewEvents(t, s, &seen)
	head, _, err := s.ProjectEventLogBounds(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var counter model.ProjectEventCounter
	if err := db.Write.First(&counter, "id = ?", model.ProjectEventCounterID).Error; err != nil || counter.Value != head {
		t.Fatalf("counter = %+v (%v), want the upgrade to have made it", counter, err)
	}
}

// The telemetry writers move nothing an event projects, and write none: a
// pull's progress and a resource sample are each reported every few seconds
// for every box (ADR 26-10-30-868 §4).
func TestTelemetryWritesNoEvents(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")
	if err := s.CreateSandbox(ctx, &model.Sandbox{ID: "sandbox-1", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "alpha"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	seen := len(projectEvents(t, s))
	for i := range 3 {
		progress := model.RawJSON(`{"phase":"pull","current":` + string(rune('1'+i)) + `}`)
		if err := s.ApplySandboxProgressReports(ctx, "pool-1", time.Now().UTC(), []store.SandboxProgressReport{{SandboxID: "sandbox-1", Progress: progress}}); err != nil {
			t.Fatalf("progress: %v", err)
		}
		if err := s.UpdateSandboxResources(ctx, "project-1", "sandbox-1", model.RawJSON(`{"cpu":1}`), time.Now().UTC()); err != nil {
			t.Fatalf("resources: %v", err)
		}
	}
	requireNewEvents(t, s, &seen)
}

// A state report compares every row it writes with what the last event
// recorded, not with its own unlocked read of the row: a batch accepted on a
// stale read can write back a state another batch has just recorded the
// opposite of, and its own read calls that no change.
func TestStateReportAnnouncesWhatItWritesBackOverARecordedState(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStoreWithDB(t, nil)
	createTestPool(t, s, "project-1", "pool-1")
	if err := s.CreateSandbox(ctx, &model.Sandbox{ID: "sandbox-1", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "alpha"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	reportRuntime(t, s, 1, false, model.SandboxRuntimeStateStopped)
	seen := len(projectEvents(t, s))

	// The row reads running, while the last event recorded stopped: what the
	// lost race leaves.
	if err := db.Write.Model(&model.Sandbox{}).Where("id = ?", "sandbox-1").UpdateColumn("runtime_state", model.SandboxRuntimeStateRunning).Error; err != nil {
		t.Fatal(err)
	}
	reportRuntime(t, s, 2, true, model.SandboxRuntimeStateRunning)
	if got := sandboxEvent(t, requireNewEvents(t, s, &seen, model.ProjectEventSandboxUpdated)[0]); got.RuntimeState != model.SandboxRuntimeStateRunning {
		t.Fatalf("runtimeState = %q, want running announced", got.RuntimeState)
	}
	reportRuntime(t, s, 3, true, model.SandboxRuntimeStateRunning)
	requireNewEvents(t, s, &seen)
}
