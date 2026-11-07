package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/pkg/sandboxmeta"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

func sandboxEvents(t *testing.T, s *store.Store, after int64) []model.SandboxEvent {
	t.Helper()
	events, err := s.ListSandboxEventsAfter(context.Background(), "project-1", after, 100)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	return events
}

func eventData(t *testing.T, event model.SandboxEvent) model.SandboxEventData {
	t.Helper()
	var data model.SandboxEventData
	if err := json.Unmarshal(event.Data, &data); err != nil {
		t.Fatalf("decode event %d: %v", event.Seq, err)
	}
	return data
}

func agentStatusWithProgram(state, kind, msg string) model.RawJSON {
	program := map[string]any{"state": state, "app": "claude-code", "msg": msg, "reportedAt": time.Now().UTC()}
	if kind != "" {
		program["kind"] = kind
	}
	status, err := json.Marshal(map[string]any{
		"sessions": []any{
			map[string]any{"terminalId": "t-old", "primary": true, "startedAt": "2026-01-01T00:00:00Z", "programStatus": map[string]any{"state": "error"}},
			map[string]any{"terminalId": "t-1", "primary": true, "startedAt": "2026-02-01T00:00:00Z", "programStatus": program},
			map[string]any{"terminalId": "t-2", "primary": false, "startedAt": "2026-03-01T00:00:00Z", "programStatus": map[string]any{"state": "working"}},
		},
	})
	if err != nil {
		panic(err)
	}
	return status
}

// Every writer of a column the events report records an event when the
// state changed, and only then.
func TestSandboxEventsFollowEachWriter(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")

	sandbox := &model.Sandbox{ID: "sandbox-1", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "alpha"}
	sandbox.DesiredState = model.DesiredStatePresent
	sandbox.State = "pending"
	if err := s.CreateSandbox(ctx, sandbox); err != nil {
		t.Fatalf("create: %v", err)
	}
	events := sandboxEvents(t, s, 0)
	if len(events) != 1 || events[0].Type != model.SandboxEventCreated {
		t.Fatalf("after create: %+v, want one sandbox.created", events)
	}
	cursor := events[0].Seq

	// A save that moves nothing the events report records nothing.
	sandbox.ObservedGeneration++
	if err := s.UpdateSandbox(ctx, sandbox); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := sandboxEvents(t, s, cursor); len(got) != 0 {
		t.Fatalf("save of an unreported column recorded %+v", got)
	}

	sandbox.SetState(model.SandboxStateReady)
	if err := s.UpdateSandbox(ctx, sandbox, store.WithGeneration(sandbox.Generation)); err != nil {
		t.Fatalf("update state: %v", err)
	}
	events = sandboxEvents(t, s, cursor)
	if len(events) != 1 || events[0].Type != model.SandboxEventUpdated || eventData(t, events[0]).State != model.SandboxStateReady {
		t.Fatalf("after state change: %+v, want one sandbox.updated to active", events)
	}
	cursor = events[0].Seq

	report := func(seq int64, complete bool, state string) {
		t.Helper()
		batch := store.SandboxStateReportBatch{PoolID: "pool-1", BootID: "boot-1", Sequence: seq, ReportedAt: time.Now().UTC(), Complete: complete}
		if state != "" {
			batch.Reports = []store.SandboxStateReport{{SandboxID: sandbox.ID, State: state}}
		}
		if _, err := s.ApplySandboxStateReports(ctx, batch); err != nil {
			t.Fatalf("state report: %v", err)
		}
	}
	report(1, false, model.SandboxRuntimeStateRunning)
	events = sandboxEvents(t, s, cursor)
	if len(events) != 1 || eventData(t, events[0]).RuntimeState != model.SandboxRuntimeStateRunning {
		t.Fatalf("after running report: %+v", events)
	}
	cursor = events[0].Seq
	// A complete sync restamps every sandbox it hosts; one already running
	// has nothing to report.
	report(2, true, model.SandboxRuntimeStateRunning)
	if got := sandboxEvents(t, s, cursor); len(got) != 0 {
		t.Fatalf("restamp recorded %+v", got)
	}

	if err := s.UpdateSandboxAgentStatus(ctx, "project-1", sandbox.ID, agentStatusWithProgram("blocked", "permission", "may I?"), time.Now().UTC(), nil); err != nil {
		t.Fatalf("agent status: %v", err)
	}
	events = sandboxEvents(t, s, cursor)
	if len(events) != 1 {
		t.Fatalf("after blocked status: %+v", events)
	}
	program := eventData(t, events[0]).Program
	if program == nil || *program != (model.SandboxEventProgram{State: "blocked", Kind: "permission", App: "claude-code"}) {
		t.Fatalf("program = %+v, want the newest primary session's blocked/permission", program)
	}
	cursor = events[0].Seq
	// The message changes while the state does not.
	if err := s.UpdateSandboxAgentStatus(ctx, "project-1", sandbox.ID, agentStatusWithProgram("blocked", "permission", "may I, please?"), time.Now().UTC(), nil); err != nil {
		t.Fatalf("agent status: %v", err)
	}
	if got := sandboxEvents(t, s, cursor); len(got) != 0 {
		t.Fatalf("a new message recorded %+v", got)
	}

	// Stopped, the program's last report says nothing about now.
	report(3, false, model.SandboxRuntimeStateStopped)
	events = sandboxEvents(t, s, cursor)
	if len(events) != 1 || eventData(t, events[0]).Program != nil {
		t.Fatalf("after stop: %+v, want one event with no program", events)
	}
	cursor = events[0].Seq

	if err := s.DeleteSandbox(ctx, "project-1", sandbox.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	events = sandboxEvents(t, s, cursor)
	if len(events) != 1 || events[0].Type != model.SandboxEventDeleted {
		t.Fatalf("after delete: %+v, want one sandbox.deleted", events)
	}
	if data := eventData(t, events[0]); data.SandboxID != sandbox.ID || data.RuntimeState != model.SandboxRuntimeStateStopped {
		t.Fatalf("deleted data = %+v, want the sandbox's last state", data)
	}

	position, err := s.GetSandboxEventPosition(ctx)
	if err != nil {
		t.Fatalf("position: %v", err)
	}
	if position.Head != events[0].Seq {
		t.Fatalf("head = %d, want the last event's %d", position.Head, events[0].Seq)
	}
}

// A write rolled back with its savepoint records nothing, nor does a whole
// transaction rolled back.
func TestSandboxEventsRollBackWithTheirWrite(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")
	sandbox := &model.Sandbox{ID: "sandbox-1", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "alpha"}
	if err := s.CreateSandbox(ctx, sandbox); err != nil {
		t.Fatalf("create: %v", err)
	}
	cursor := sandboxEvents(t, s, 0)[0].Seq

	rollback := errors.New("roll back")
	err := s.Transaction(ctx, func(txStore *store.Store, _ *gorm.DB) error {
		_ = txStore.Transaction(ctx, func(inner *store.Store, _ *gorm.DB) error {
			if err := inner.DeleteSandbox(ctx, "project-1", sandbox.ID); err != nil {
				return err
			}
			return rollback
		})
		return nil
	})
	if err != nil {
		t.Fatalf("outer transaction: %v", err)
	}
	err = s.Transaction(ctx, func(txStore *store.Store, _ *gorm.DB) error {
		sandbox.SetState("failed")
		if err := txStore.UpdateSandbox(ctx, sandbox); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatalf("transaction error = %v", err)
	}
	if got := sandboxEvents(t, s, cursor); len(got) != 0 {
		t.Fatalf("rolled-back writes recorded %+v", got)
	}
}

// The current state is every live sandbox's latest event and every deletion
// after the cursor it was asked from.
func TestListLatestSandboxEvents(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")
	for _, id := range []string{"sandbox-a", "sandbox-b", "sandbox-c"} {
		if err := s.CreateSandbox(ctx, &model.Sandbox{ID: id, ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: id}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	a, err := s.GetSandbox(ctx, "project-1", "sandbox-a")
	if err != nil {
		t.Fatal(err)
	}
	a.SetState(model.SandboxStateReady)
	if err := s.UpdateSandbox(ctx, a); err != nil {
		t.Fatal(err)
	}
	position, err := s.GetSandboxEventPosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSandbox(ctx, "project-1", "sandbox-b"); err != nil {
		t.Fatal(err)
	}

	latest, err := s.ListLatestSandboxEvents(ctx, "project-1", position.Head)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	var got []string
	for _, event := range latest {
		got = append(got, event.SandboxID+" "+event.Type)
	}
	want := []string{"sandbox-c sandbox.created", "sandbox-a sandbox.updated", "sandbox-b sandbox.deleted"}
	if len(got) != len(want) {
		t.Fatalf("latest = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("latest = %v, want %v", got, want)
		}
	}
	// A deletion at or before the cursor is no part of the state after it.
	head, _ := s.GetSandboxEventPosition(ctx)
	latest, err = s.ListLatestSandboxEvents(ctx, "project-1", head.Head)
	if err != nil {
		t.Fatal(err)
	}
	if len(latest) != 2 {
		t.Fatalf("latest after the deletion = %d events, want the two live sandboxes", len(latest))
	}
}

// Retention keeps every sandbox's latest event and moves the watermark past
// what it removes.
func TestPruneSandboxEvents(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStoreWithDB(t, false)
	createTestPool(t, s, "project-1", "pool-1")
	for _, id := range []string{"sandbox-a", "sandbox-b"} {
		if err := s.CreateSandbox(ctx, &model.Sandbox{ID: id, ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: id}); err != nil {
			t.Fatal(err)
		}
	}
	a, err := s.GetSandbox(ctx, "project-1", "sandbox-a")
	if err != nil {
		t.Fatal(err)
	}
	a.SetState(model.SandboxStateReady)
	if err := s.UpdateSandbox(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSandbox(ctx, "project-1", "sandbox-b"); err != nil {
		t.Fatal(err)
	}
	if oldest, err := s.OldestPrunableSandboxEvent(ctx); err != nil || oldest == nil {
		t.Fatalf("oldest prunable = %v, %v; want a's created and b's events", oldest, err)
	}
	// Everything is old.
	if err := db.Write.Model(&model.SandboxEvent{}).Where("1 = 1").Update("at", time.Now().Add(-48*time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	removed, err := s.PruneSandboxEvents(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	// a's created, b's created, b's deleted.
	if removed != 3 {
		t.Fatalf("removed %d, want 3", removed)
	}
	left := sandboxEvents(t, s, 0)
	if len(left) != 1 || left[0].SandboxID != "sandbox-a" || left[0].Type != model.SandboxEventUpdated {
		t.Fatalf("left %+v, want a's latest", left)
	}
	position, err := s.GetSandboxEventPosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if position.PrunedThrough != position.Head {
		t.Fatalf("pruned through %d, want %d, b's deletion", position.PrunedThrough, position.Head)
	}
	if oldest, err := s.OldestPrunableSandboxEvent(ctx); err != nil || oldest != nil {
		t.Fatalf("oldest prunable after prune = %v, %v; want none", oldest, err)
	}
}

// A sandbox from before events gets one, as an update, and only once.
func TestBackfillSandboxEvents(t *testing.T) {
	s, db := newTestStoreWithDB(t, false)
	createTestPool(t, s, "project-1", "pool-1")
	old := &model.Sandbox{ID: "sandbox-old", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "old"}
	old.State = model.SandboxStateReady
	if err := db.Write.Create(old).Error; err != nil {
		t.Fatalf("insert: %v", err)
	}
	for range 2 {
		if err := store.BackfillSandboxEvents(db.Write); err != nil {
			t.Fatalf("backfill: %v", err)
		}
	}
	events := sandboxEvents(t, s, 0)
	if len(events) != 1 || events[0].Type != model.SandboxEventUpdated || eventData(t, events[0]).State != model.SandboxStateReady {
		t.Fatalf("backfilled %+v, want one sandbox.updated", events)
	}
}

// An archived sandbox has no container and no pool reports on it, so the
// runtime state recorded before the archive is not reported, nor is its
// program.
func TestSandboxEventsReportNoPowerForAnArchivedSandbox(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")
	sandbox := &model.Sandbox{ID: "sandbox-1", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "alpha"}
	sandbox.SetState(model.SandboxStateReady)
	if err := s.CreateSandbox(ctx, sandbox); err != nil {
		t.Fatal(err)
	}
	batch := store.SandboxStateReportBatch{PoolID: "pool-1", BootID: "boot-1", Sequence: 1, ReportedAt: time.Now().UTC(),
		Reports: []store.SandboxStateReport{{SandboxID: sandbox.ID, State: model.SandboxRuntimeStateRunning}}}
	if _, err := s.ApplySandboxStateReports(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateSandboxAgentStatus(ctx, "project-1", sandbox.ID, agentStatusWithProgram("working", "", "busy"), time.Now().UTC(), nil); err != nil {
		t.Fatal(err)
	}
	cursor := sandboxEvents(t, s, 0)[2].Seq

	current, err := s.GetSandbox(ctx, "project-1", sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	current.DesiredState = model.DesiredStateArchived
	current.SetState(model.SandboxStateArchived)
	if err := s.UpdateSandbox(ctx, current); err != nil {
		t.Fatal(err)
	}
	events := sandboxEvents(t, s, cursor)
	if len(events) != 1 {
		t.Fatalf("after archive: %+v", events)
	}
	if data := eventData(t, events[0]); data.State != model.SandboxStateArchived || data.RuntimeState != "" || data.Program != nil {
		t.Fatalf("archived data = %+v, want no power and no program", data)
	}
	cursor = events[0].Seq

	// A delta that does not mention it says nothing; the complete sync that
	// omits it puts its observation down, and hands nothing to the reconciler.
	sync := func(seq int64, complete bool) []store.SandboxObservation {
		t.Helper()
		observed, err := s.ApplySandboxStateReports(ctx, store.SandboxStateReportBatch{PoolID: "pool-1", BootID: "boot-1", Sequence: seq, ReportedAt: time.Now().UTC(), Complete: complete})
		if err != nil {
			t.Fatal(err)
		}
		return observed
	}
	sync(2, false)
	if current, _ := s.GetSandbox(ctx, "project-1", sandbox.ID); current.RuntimeState != model.SandboxRuntimeStateRunning {
		t.Fatalf("runtime state after a delta = %q, want it untouched", current.RuntimeState)
	}
	if observed := sync(3, true); len(observed) != 0 {
		t.Fatalf("a complete sync handed the archived sandbox on: %+v", observed)
	}
	current, err = s.GetSandbox(ctx, "project-1", sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.RuntimeState != "" {
		t.Fatalf("runtime state after a complete sync = %q, want unobserved", current.RuntimeState)
	}

	// Unarchived, it reads as not yet reported until its pool reports again.
	current.DesiredState = model.DesiredStatePresent
	current.SetState(model.SandboxStatePending)
	if err := s.UpdateSandbox(ctx, current); err != nil {
		t.Fatal(err)
	}
	events = sandboxEvents(t, s, cursor)
	if len(events) != 1 {
		t.Fatalf("after unarchive: %+v", events)
	}
	if data := eventData(t, events[0]); data.RuntimeState != "" || data.Program != nil {
		t.Fatalf("unarchived data = %+v, want no power and no program from before the archive", data)
	}
}

// A state report compares every row it writes with the sandbox's latest
// event, not with its own unlocked read of the row: a batch accepted on a
// stale read can write back a state another batch has just recorded the
// opposite of, and its own read calls that no change.
func TestStateReportAnnouncesWhatItWritesBackOverARecordedState(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStoreWithDB(t, false)
	createTestPool(t, s, "project-1", "pool-1")
	if err := s.CreateSandbox(ctx, &model.Sandbox{ID: "sandbox-1", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "alpha"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	report := func(seq int64, state string) {
		t.Helper()
		batch := store.SandboxStateReportBatch{PoolID: "pool-1", BootID: "boot-1", Sequence: seq, ReportedAt: time.Now().UTC(), Complete: true,
			Reports: []store.SandboxStateReport{{SandboxID: "sandbox-1", State: state}}}
		if _, err := s.ApplySandboxStateReports(ctx, batch); err != nil {
			t.Fatalf("state report: %v", err)
		}
	}
	report(1, model.SandboxRuntimeStateStopped)
	events := sandboxEvents(t, s, 0)
	cursor := events[len(events)-1].Seq

	// The row reads running while its latest event says stopped: what the
	// lost race leaves.
	if err := db.Write.Model(&model.Sandbox{}).Where("id = ?", "sandbox-1").UpdateColumn("runtime_state", model.SandboxRuntimeStateRunning).Error; err != nil {
		t.Fatal(err)
	}
	report(2, model.SandboxRuntimeStateRunning)
	events = sandboxEvents(t, s, cursor)
	if len(events) != 1 || eventData(t, events[0]).RuntimeState != model.SandboxRuntimeStateRunning {
		t.Fatalf("events = %+v, want running announced", events)
	}
	report(3, model.SandboxRuntimeStateRunning)
	if events := sandboxEvents(t, s, events[0].Seq); len(events) != 0 {
		t.Fatalf("an unchanged restamp recorded %d events", len(events))
	}
}

// The stream reads the event log on the write handle: a read replica lagging
// another would let a page and the head it moves to disagree, and skip events.
// A store whose read handle is a replica that has seen none of them still
// streams them.
func TestSandboxEventLogIsReadOnTheWriteHandle(t *testing.T) {
	ctx := context.Background()
	primary, db := newTestStoreWithDB(t, false)
	createTestPool(t, primary, "project-1", "pool-1")
	if err := primary.CreateSandbox(ctx, &model.Sandbox{ID: "sandbox-1", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "alpha"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, replica := newTestStoreWithDB(t, false)
	s := store.New(db.Write, replica.Write)

	position, err := s.GetSandboxEventPosition(ctx)
	if err != nil || position.Head == 0 {
		t.Fatalf("position = %+v, %v; want the primary's head", position, err)
	}
	if events, err := s.ListSandboxEventsAfter(ctx, "project-1", 0, 10); err != nil || len(events) == 0 {
		t.Fatalf("events after = %d, %v; want the primary's", len(events), err)
	}
	if events, err := s.ListLatestSandboxEvents(ctx, "project-1", 0); err != nil || len(events) == 0 {
		t.Fatalf("latest events = %d, %v; want the primary's", len(events), err)
	}
}

// An event carries the sandbox's tags and the sandbox that created it, and
// the meta write records one when the tags change, and only then (ADR
// 26-11-01-560 §2).
func TestSandboxEventsCarryTagsAndCreator(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStoreWithDB(t, false)
	createTestPool(t, s, "project-1", "pool-1")
	lead := "sandbox-lead"
	if err := db.Write.Create(&model.Sandbox{ID: lead, Name: "lead", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1"}).Error; err != nil {
		t.Fatal(err)
	}
	sandbox := &model.Sandbox{ID: "sandbox-1", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "alpha", CreatedBySandboxID: &lead}
	sandbox.DesiredState = model.DesiredStatePresent
	sandbox.State = "pending"
	if err := s.CreateSandbox(ctx, sandbox); err != nil {
		t.Fatalf("create: %v", err)
	}
	events := sandboxEvents(t, s, 0)
	if len(events) != 1 {
		t.Fatalf("after create: %+v", events)
	}
	if data := eventData(t, events[0]); data.CreatedBySandboxID != lead || data.Tags != nil || data.DisplayState != "starting" {
		t.Fatalf("created data = %+v, want creator %s, no tags, and displayState starting", data, lead)
	}
	cursor := events[0].Seq

	at := time.Now().UTC()
	if err := s.UpdateSandboxMeta(ctx, "project-1", sandbox.ID, sandboxmeta.Meta{Name: "worker", Tags: map[string]string{"issue": "427"}}, at); err != nil {
		t.Fatalf("meta: %v", err)
	}
	events = sandboxEvents(t, s, cursor)
	if len(events) != 1 || events[0].Type != model.SandboxEventUpdated {
		t.Fatalf("after tagging: %+v, want one sandbox.updated", events)
	}
	if data := eventData(t, events[0]); data.Tags["issue"] != "427" || data.CreatedBySandboxID != lead {
		t.Fatalf("tagged data = %+v", data)
	}
	cursor = events[0].Seq

	// A name and a description are not an event's; a report that changes
	// only them, or nothing, records nothing.
	at = at.Add(time.Second)
	if err := s.UpdateSandboxMeta(ctx, "project-1", sandbox.ID, sandboxmeta.Meta{Name: "renamed", Description: "d", Tags: map[string]string{"issue": "427"}}, at); err != nil {
		t.Fatalf("meta: %v", err)
	}
	if got := sandboxEvents(t, s, cursor); len(got) != 0 {
		t.Fatalf("a rename recorded %+v", got)
	}
	// An older report loses, and records nothing.
	if err := s.UpdateSandboxMeta(ctx, "project-1", sandbox.ID, sandboxmeta.Meta{}, at.Add(-time.Minute)); err != nil {
		t.Fatalf("meta: %v", err)
	}
	if got := sandboxEvents(t, s, cursor); len(got) != 0 {
		t.Fatalf("a stale report recorded %+v", got)
	}
	// Clearing the tags is a change.
	if err := s.UpdateSandboxMeta(ctx, "project-1", sandbox.ID, sandboxmeta.Meta{}, at.Add(time.Second)); err != nil {
		t.Fatalf("meta: %v", err)
	}
	events = sandboxEvents(t, s, cursor)
	if len(events) != 1 || eventData(t, events[0]).Tags != nil {
		t.Fatalf("after clearing tags: %+v, want one event with no tags", events)
	}
}

// An event's displayState is the server's one rule over the row
// (Sandbox.DisplayState), so a failure is an event, and reads as an error,
// though neither axis moved.
func TestSandboxEventsCarryTheDisplayState(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")
	sandbox := &model.Sandbox{ID: "sandbox-1", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "alpha"}
	sandbox.DesiredState = model.DesiredStatePresent
	sandbox.State = model.SandboxStateReady
	if err := s.CreateSandbox(ctx, sandbox); err != nil {
		t.Fatalf("create: %v", err)
	}
	cursor := sandboxEvents(t, s, 0)[0].Seq
	message := "the pool is gone"
	sandbox.ErrorMessage = &message
	if err := s.UpdateSandbox(ctx, sandbox); err != nil {
		t.Fatalf("update: %v", err)
	}
	events := sandboxEvents(t, s, cursor)
	if len(events) != 1 {
		t.Fatalf("after an error: %+v, want one event", events)
	}
	if data := eventData(t, events[0]); data.DisplayState != "error" || data.State != model.SandboxStateReady {
		t.Fatalf("data = %+v, want displayState error over state ready", data)
	}
}

// A latest event written before the projection gained a field is brought up
// to date by the backfill, once; one that matches is left alone.
func TestBackfillSandboxEventsUpdatesAStaleLatestEvent(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStoreWithDB(t, false)
	createTestPool(t, s, "project-1", "pool-1")
	for _, id := range []string{"sandbox-tagged", "sandbox-plain"} {
		sandbox := &model.Sandbox{ID: id, ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: id}
		sandbox.DesiredState = model.DesiredStatePresent
		sandbox.State = model.SandboxStateReady
		if err := s.CreateSandbox(ctx, sandbox); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	// Tags written as an older server wrote them, with no event.
	if err := db.Write.Model(&model.Sandbox{}).Where("id = ?", "sandbox-tagged").
		UpdateColumn("tags", `{"role":"worker"}`).Error; err != nil {
		t.Fatalf("raw tags: %v", err)
	}
	cursor := sandboxEvents(t, s, 0)[1].Seq
	for range 2 {
		if err := store.BackfillSandboxEvents(db.Write); err != nil {
			t.Fatalf("backfill: %v", err)
		}
	}
	events := sandboxEvents(t, s, cursor)
	if len(events) != 1 || events[0].SandboxID != "sandbox-tagged" || events[0].Type != model.SandboxEventUpdated {
		t.Fatalf("backfilled %+v, want one sandbox.updated for sandbox-tagged", events)
	}
	if data := eventData(t, events[0]); data.Tags["role"] != "worker" {
		t.Fatalf("backfilled data = %+v, want its tags", data)
	}
}

func TestSandboxEventsCarryReconcileWarnings(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStoreWithDB(t, false)
	createTestPool(t, s, "project-1", "pool-1")
	box := &model.Sandbox{ID: "sandbox-warning", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "warning"}
	box.DesiredState, box.State, box.RuntimeState = "present", "failed", "running"
	if err := s.CreateSandbox(ctx, box); err != nil {
		t.Fatal(err)
	}
	cursor := sandboxEvents(t, s, 0)[0].Seq
	for _, message := range []string{"ensure failed", "ensure failed differently", ""} {
		box.ErrorMessage = nil
		if message != "" {
			box.ErrorMessage = &message
		}
		if err := s.UpdateSandbox(ctx, box); err != nil {
			t.Fatal(err)
		}
		events := sandboxEvents(t, s, cursor)
		if len(events) != 1 || events[0].Type != model.SandboxEventUpdated {
			t.Fatalf("message %q: events %+v", message, events)
		}
		data := eventData(t, events[0])
		if data.State != "failed" || data.RuntimeState != "running" || data.DisplayState != "running" {
			t.Fatalf("axes changed: %+v", data)
		}
		var wire map[string]any
		if err := json.Unmarshal(events[0].Data, &wire); err != nil {
			t.Fatal(err)
		}
		if message == "" {
			if _, ok := wire["errorMessage"]; ok {
				t.Fatal("cleared error was not omitted")
			}
		} else if wire["errorMessage"] != message {
			t.Fatalf("wire error: %+v", wire)
		}
		cursor = events[0].Seq
		if err := s.UpdateSandbox(ctx, box); err != nil {
			t.Fatal(err)
		}
		if events := sandboxEvents(t, s, cursor); len(events) != 0 {
			t.Fatalf("unchanged warning emitted %+v", events)
		}
	}
	// Old JSON is retained. Startup appends a snapshot carrying the current
	// message even when all the existing fields and display state agree.
	if err := db.Write.Model(&model.Sandbox{}).Where("id = ?", box.ID).UpdateColumn("error_message", "old ensure failure").Error; err != nil {
		t.Fatal(err)
	}
	old := sandboxEvents(t, s, 0)
	for range 2 {
		if err := store.BackfillSandboxEvents(db.Write); err != nil {
			t.Fatal(err)
		}
	}
	events := sandboxEvents(t, s, cursor)
	if len(events) != 1 || events[0].Type != model.SandboxEventUpdated {
		t.Fatalf("backfill: %+v", events)
	}
	data := eventData(t, events[0])
	if data.ErrorMessage == nil || *data.ErrorMessage != "old ensure failure" || data.DisplayState != "running" {
		t.Fatalf("backfilled %+v", data)
	}
	all := sandboxEvents(t, s, 0)
	for i, event := range old {
		if event.Seq != all[i].Seq || string(event.Data) != string(all[i].Data) {
			t.Fatal("history was rewritten")
		}
	}
}
