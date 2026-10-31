package sandboxes

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

func createEventSandbox(t *testing.T, st *store.Store, id string) *model.Sandbox {
	t.Helper()
	ctx := context.Background()
	if err := st.CreateSandboxProviderInstance(ctx, &model.SandboxProviderInstance{ID: "prov-1", ProjectID: "project-1", Type: "docker", Name: "prov-1"}); err != nil && !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("create provider: %v", err)
	}
	if _, err := st.GetPool(ctx, "project-1", "pool-1"); err != nil {
		if err := st.CreatePool(ctx, &model.Pool{ID: "pool-1", ProjectID: "project-1", PoolManifest: model.PoolManifest{Name: "pool-1", ProviderInstanceID: "prov-1"}}); err != nil {
			t.Fatalf("create pool: %v", err)
		}
	}
	sandbox := &model.Sandbox{ID: id, ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: id}
	sandbox.DesiredState = model.DesiredStatePresent
	sandbox.State = model.SandboxStatePending
	if err := st.CreateSandbox(ctx, sandbox); err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
	return sandbox
}

// describe renders what a stream sent as "id type sandbox", with "-" for an
// event with no ID.
func describe(t *testing.T, events []services.SandboxStreamEvent) []string {
	t.Helper()
	out := make([]string, 0, len(events))
	for _, event := range events {
		id := "-"
		if event.ID != nil {
			id = fmt.Sprint(*event.ID)
		}
		var data model.SandboxEventData
		if err := json.Unmarshal(event.Data, &data); err != nil {
			t.Fatalf("decode %s: %v", event.Type, err)
		}
		out = append(out, strings.TrimSpace(id+" "+event.Type+" "+data.SandboxID))
	}
	return out
}

func next(t *testing.T, stream services.SandboxEventStream) []string {
	t.Helper()
	events, err := stream.Next(context.Background())
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	return describe(t, events)
}

func wantEvents(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "; ") != strings.Join(want, "; ") {
		t.Fatalf("events = %q, want %q", got, want)
	}
}

// With no cursor a stream begins with the project's current state, and goes
// on with each change after it.
func TestSandboxEventStreamBeginsWithTheCurrentState(t *testing.T) {
	ctx := context.Background()
	svc, st := newBindingFixture(t)
	a := createEventSandbox(t, st, "sandbox-a")
	createEventSandbox(t, st, "sandbox-b")
	a.SetState(model.SandboxStateReady)
	if err := st.UpdateSandbox(ctx, a); err != nil {
		t.Fatal(err)
	}

	stream, err := svc.OpenSandboxEvents(ctx, "project-1", nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	wantEvents(t, next(t, stream), "2 sandbox.created sandbox-b", "3 sandbox.updated sandbox-a")
	wantEvents(t, next(t, stream))

	if err := st.DeleteSandbox(ctx, "project-1", "sandbox-b"); err != nil {
		t.Fatal(err)
	}
	wantEvents(t, next(t, stream), "4 sandbox.deleted sandbox-b")
}

// A cursor resumes after itself, missing nothing and repeating nothing.
func TestSandboxEventStreamResumesFromItsCursor(t *testing.T) {
	ctx := context.Background()
	svc, st := newBindingFixture(t)
	a := createEventSandbox(t, st, "sandbox-a")
	a.SetState(model.SandboxStateReady)
	if err := st.UpdateSandbox(ctx, a); err != nil {
		t.Fatal(err)
	}
	createEventSandbox(t, st, "sandbox-b")

	cursor := int64(1)
	stream, err := svc.OpenSandboxEvents(ctx, "project-1", &cursor)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	wantEvents(t, next(t, stream), "2 sandbox.updated sandbox-a", "3 sandbox.created sandbox-b")
}

// A cursor from before what retention removed is reset, and given the
// current state; the reset carries no ID, so a client cut off before the
// state arrives is reset again.
func TestSandboxEventStreamResetsAnExpiredCursor(t *testing.T) {
	ctx := context.Background()
	svc, st := newBindingFixture(t)
	a := createEventSandbox(t, st, "sandbox-a")
	createEventSandbox(t, st, "sandbox-b")
	a.SetState(model.SandboxStateReady)
	if err := st.UpdateSandbox(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteSandbox(ctx, "project-1", "sandbox-b"); err != nil {
		t.Fatal(err)
	}
	// Everything a sandbox's latest event is not goes: a's created, b's
	// created and b's deletion.
	if removed, err := st.PruneSandboxEvents(ctx, time.Now().Add(time.Hour)); err != nil || removed != 3 {
		t.Fatalf("prune removed %d, %v", removed, err)
	}

	cursor := int64(1)
	stream, err := svc.OpenSandboxEvents(ctx, "project-1", &cursor)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	wantEvents(t, next(t, stream), "- reset", "3 sandbox.updated sandbox-a")
	// The replayed event is older than the watermark; the stream stands at
	// the head, which is what it gives the client to resume from.
	if stream.Cursor() != 4 {
		t.Fatalf("cursor after the replay = %d, want the head, 4", stream.Cursor())
	}
	wantEvents(t, next(t, stream))

	// Resumed from what the stream gave, nothing was lost and nothing resets.
	cursor = stream.Cursor()
	stream, err = svc.OpenSandboxEvents(ctx, "project-1", &cursor)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	wantEvents(t, next(t, stream))
}

// A quiet project's stream moves with the head while other projects change,
// so retention of their events never leaves its client's cursor behind.
func TestSandboxEventStreamCursorFollowsTheHeadWhileQuiet(t *testing.T) {
	ctx := context.Background()
	svc, st := newBindingFixture(t)
	createEventSandbox(t, st, "sandbox-a")
	if err := st.UpsertProject(ctx, &model.Project{ID: "project-2", OwnerUserID: "user-1", Name: "Other"}); err != nil {
		t.Fatal(err)
	}
	stream, err := svc.OpenSandboxEvents(ctx, "project-2", nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	wantEvents(t, next(t, stream))
	a, err := st.GetSandbox(ctx, "project-1", "sandbox-a")
	if err != nil {
		t.Fatal(err)
	}
	a.SetState(model.SandboxStateReady)
	if err := st.UpdateSandbox(ctx, a); err != nil {
		t.Fatal(err)
	}
	// The first read sees the head move; the next stands on it.
	wantEvents(t, next(t, stream))
	wantEvents(t, next(t, stream))
	if stream.Cursor() != 2 {
		t.Fatalf("cursor = %d, want the head, 2", stream.Cursor())
	}
	if _, err := st.PruneSandboxEvents(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	cursor := stream.Cursor()
	stream, err = svc.OpenSandboxEvents(ctx, "project-2", &cursor)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	wantEvents(t, next(t, stream))
}

// A cursor past every event there is — a database restored from before it —
// cannot be gone on from: new events would reuse its IDs unseen.
func TestSandboxEventStreamResetsACursorPastTheHead(t *testing.T) {
	ctx := context.Background()
	svc, st := newBindingFixture(t)
	createEventSandbox(t, st, "sandbox-a")
	cursor := int64(40)
	stream, err := svc.OpenSandboxEvents(ctx, "project-1", &cursor)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	wantEvents(t, next(t, stream), "- reset", "1 sandbox.created sandbox-a")
}

// A stream reads the watermark after the events, so a prune that commits
// while it tails is seen.
func TestSandboxEventStreamResetsWhenPrunedUnderIt(t *testing.T) {
	ctx := context.Background()
	svc, st := newBindingFixture(t)
	a := createEventSandbox(t, st, "sandbox-a")
	cursor := int64(0)
	stream, err := svc.OpenSandboxEvents(ctx, "project-1", &cursor)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	a.SetState(model.SandboxStateReady)
	if err := st.UpdateSandbox(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PruneSandboxEvents(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	wantEvents(t, next(t, stream), "- reset", "2 sandbox.updated sandbox-a")
}

func TestOpenSandboxEventsRefusesAnUnknownProject(t *testing.T) {
	svc, _ := newBindingFixture(t)
	if _, err := svc.OpenSandboxEvents(context.Background(), "project-nope", nil); err == nil {
		t.Fatal("opened a stream on a project that does not exist")
	}
}

// The sweep removes what is due and arms itself for the next event to come
// due.
func TestSandboxEventPruneReconciler(t *testing.T) {
	ctx := context.Background()
	_, st := newBindingFixture(t)
	a := createEventSandbox(t, st, "sandbox-a")
	a.SetState(model.SandboxStateReady)
	if err := st.UpdateSandbox(ctx, a); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	reconciler := &sandboxEventPruneReconciler{store: st, now: func() time.Time { return now }}
	if ids, err := reconciler.ScanDirty(ctx); err != nil || len(ids) != 0 {
		t.Fatalf("scan before anything is due = %v, %v", ids, err)
	}
	result, err := reconciler.Reconcile(ctx, SandboxEventPruneID)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.RequeueAt.IsZero() || result.RequeueAt.Before(now.Add(23*time.Hour)) {
		t.Fatalf("requeue at %v, want when a's created comes due", result.RequeueAt)
	}

	now = now.Add(25 * time.Hour)
	if ids, err := reconciler.ScanDirty(ctx); err != nil || len(ids) != 1 {
		t.Fatalf("scan once due = %v, %v", ids, err)
	}
	result, err = reconciler.Reconcile(ctx, SandboxEventPruneID)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !result.RequeueAt.IsZero() {
		t.Fatalf("requeue at %v with nothing left to come due", result.RequeueAt)
	}
	position, err := st.GetSandboxEventPosition(ctx)
	if err != nil || position.PrunedThrough != 1 {
		t.Fatalf("pruned through %d, %v; want a's created", position.PrunedThrough, err)
	}
}
