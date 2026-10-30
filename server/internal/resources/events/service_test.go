package events

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

func newTestService(t *testing.T, buffer int) (*Service, *store.Store) {
	s, st, _ := newTestServiceWithDB(t, buffer)
	return s, st
}

func newTestServiceWithDB(t *testing.T, buffer int) (*Service, *store.Store, *database.DB) {
	t.Helper()
	ctx := context.Background()
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	st := store.New(db.Write, db.Read)
	for _, project := range []string{"project-1", "project-2"} {
		if err := st.UpsertProject(ctx, &model.Project{ID: project, OwnerUserID: "user-1", Name: project}); err != nil {
			t.Fatal(err)
		}
		provider := "prov-" + project
		if err := st.CreateSandboxProviderInstance(ctx, &model.SandboxProviderInstance{ID: provider, ProjectID: project, Type: "docker", Name: provider}); err != nil {
			t.Fatal(err)
		}
		if err := st.CreatePool(ctx, &model.Pool{ID: "pool-" + project, ProjectID: project, PoolManifest: model.PoolManifest{Name: "pool", ProviderInstanceID: provider}}); err != nil {
			t.Fatal(err)
		}
	}
	return &Service{store: st, tailer: newTailer(st, 5*time.Millisecond, buffer)}, st, db
}

// createSandboxes writes one sandbox.created event each.
func createSandboxes(t *testing.T, st *store.Store, project string, names ...string) {
	t.Helper()
	for _, name := range names {
		if err := st.CreateSandbox(context.Background(), &model.Sandbox{
			ID: project + "-" + name, ProjectID: project, PoolID: "pool-" + project, CreatedByUserID: "user-1", Name: name,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func open(t *testing.T, s *Service, start services.ProjectEventStart) services.ProjectEventStream {
	t.Helper()
	stream, err := s.OpenProjectEvents(context.Background(), "project-1", start)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(stream.Close)
	return stream
}

// next reads n events, failing if they do not come.
func next(t *testing.T, stream services.ProjectEventStream, n int) []model.ProjectEvent {
	t.Helper()
	events := make([]model.ProjectEvent, 0, n)
	for range n {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		event, err := stream.Next(ctx)
		cancel()
		if err != nil {
			t.Fatalf("event %d of %d: %v", len(events)+1, n, err)
		}
		events = append(events, event)
	}
	return events
}

// requireQuiet fails if the stream has another event.
func requireQuiet(t *testing.T, stream services.ProjectEventStream) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if event, err := stream.Next(ctx); err == nil {
		t.Fatalf("unexpected event %d %s", event.ID, event.Type)
	}
}

func names(events []model.ProjectEvent) []string {
	out := make([]string, len(events))
	for i, event := range events {
		out[i] = event.ResourceID
	}
	return out
}

func requireNames(t *testing.T, events []model.ProjectEvent, want ...string) {
	t.Helper()
	if got := names(events); !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

// With no Last-Event-ID a stream replays the project's last History events,
// none of another project's, then follows the log live.
func TestStreamReplaysHistoryThenFollows(t *testing.T) {
	s, st := newTestService(t, subscriptionBuffer)
	createSandboxes(t, st, "project-1", "a", "b", "c")
	createSandboxes(t, st, "project-2", "x")

	head, _, err := st.ProjectEventLogBounds(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	stream := open(t, s, services.ProjectEventStart{History: 2})
	if stream.Reset() != "" || stream.Position() != head {
		t.Fatalf("reset = %q, position %d; want none, the head %d", stream.Reset(), stream.Position(), head)
	}
	requireNames(t, next(t, stream, 2), "project-1-b", "project-1-c")

	createSandboxes(t, st, "project-2", "y")
	createSandboxes(t, st, "project-1", "d")
	requireNames(t, next(t, stream, 1), "project-1-d")
	requireQuiet(t, stream)
}

// A Last-Event-ID the log holds resumes after it: everything missed, once, in
// order, then live.
func TestStreamResumesAfterLastEventID(t *testing.T) {
	s, st := newTestService(t, subscriptionBuffer)
	createSandboxes(t, st, "project-1", "a", "b")
	first := open(t, s, services.ProjectEventStart{History: 1})
	seen := next(t, first, 1)[0]
	first.Close()

	createSandboxes(t, st, "project-1", "c", "d")
	stream := open(t, s, services.ProjectEventStart{LastEventID: strconv.FormatInt(seen.ID, 10), History: 50})
	if stream.Reset() != "" || stream.Position() != seen.ID {
		t.Fatalf("reset = %q, position %d; want none, %d", stream.Reset(), stream.Position(), seen.ID)
	}
	requireNames(t, next(t, stream, 2), "project-1-c", "project-1-d")
	createSandboxes(t, st, "project-1", "e")
	requireNames(t, next(t, stream, 1), "project-1-e")
	requireQuiet(t, stream)
}

// A Last-Event-ID the log does not hold resets the stream, which then starts
// as one given none.
func TestStreamResetsOnAnIDTheLogDoesNotHold(t *testing.T) {
	s, st := newTestService(t, subscriptionBuffer)
	createSandboxes(t, st, "project-1", "a", "b")
	head, _, err := st.ProjectEventLogBounds(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"not-a-number", "-1", strconv.FormatInt(head+1, 10)} {
		stream := open(t, s, services.ProjectEventStart{LastEventID: id, History: 1})
		if stream.Reset() != services.ProjectEventResetUnknown {
			t.Fatalf("%s: reset = %q, want unknown", id, stream.Reset())
		}
		requireNames(t, next(t, stream, 1), "project-1-b")
		stream.Close()
	}

	if _, err := st.PruneProjectEvents(context.Background(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	createSandboxes(t, st, "project-1", "c")
	stream := open(t, s, services.ProjectEventStart{LastEventID: "1", History: 5})
	if stream.Reset() != services.ProjectEventResetExpired {
		t.Fatalf("reset = %q, want expired", stream.Reset())
	}
	requireNames(t, next(t, stream, 1), "project-1-c")

	// The last pruned ID is still a place to resume from: nothing after it
	// was removed.
	resumed := open(t, s, services.ProjectEventStart{LastEventID: strconv.FormatInt(head, 10)})
	if resumed.Reset() != "" {
		t.Fatalf("reset = %q, want none", resumed.Reset())
	}
	requireNames(t, next(t, resumed, 1), "project-1-c")
}

// A stream that falls behind is ended rather than thinned, and its client
// loses nothing by resuming from the last event it read.
func TestStreamThatFallsBehindIsEndedAndResumesWhole(t *testing.T) {
	s, st := newTestService(t, 1)
	stream := open(t, s, services.ProjectEventStart{})
	// Wait for the stream to be caught up, so what follows reaches it live.
	requireQuiet(t, stream)

	createSandboxes(t, st, "project-1", "a", "b", "c")
	time.Sleep(100 * time.Millisecond)
	var read []model.ProjectEvent
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		event, err := stream.Next(ctx)
		cancel()
		if errors.Is(err, services.ErrProjectEventStreamBehind) {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		read = append(read, event)
	}
	if len(read) == 3 {
		t.Fatal("a one-event buffer took three events; the test proves nothing")
	}
	stream.Close()

	last := "0"
	if len(read) > 0 {
		last = strconv.FormatInt(read[len(read)-1].ID, 10)
	}
	resumed := open(t, s, services.ProjectEventStart{LastEventID: last})
	requireNames(t, append(read, next(t, resumed, 3-len(read))...), "project-1-a", "project-1-b", "project-1-c")
}

func TestOpenRefusesAProjectThatDoesNotExist(t *testing.T) {
	s, _ := newTestService(t, subscriptionBuffer)
	if _, err := s.OpenProjectEvents(context.Background(), "nope", services.ProjectEventStart{}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("open = %v, want not found", err)
	}
}

// A stream with nothing of its project to pass on stands where the log does,
// so its client's keepalive ID moves past other projects' events and a resume
// from it is never older than retention for a project that was only quiet.
func TestQuietStreamStandsWhereTheLogDoes(t *testing.T) {
	s, st := newTestService(t, subscriptionBuffer)
	stream := open(t, s, services.ProjectEventStart{})
	requireQuiet(t, stream)

	createSandboxes(t, st, "project-2", "x", "y")
	head, _, err := st.ProjectEventLogBounds(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		through, ok := stream.Through()
		if ok && through == head {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("through = %d, %v; want the head %d", through, ok, head)
		}
		time.Sleep(10 * time.Millisecond)
	}

	createSandboxes(t, st, "project-1", "a")
	resumed := open(t, s, services.ProjectEventStart{LastEventID: strconv.FormatInt(head, 10)})
	if resumed.Reset() != "" {
		t.Fatalf("reset = %q, want none", resumed.Reset())
	}
	requireNames(t, next(t, resumed, 1), "project-1-a")
}

// A catch-up that retention overtakes ends rather than skipping what was
// removed, so the client's resume gets the reset that says it lost events.
func TestCatchUpThatRetentionOvertakesEnds(t *testing.T) {
	s, st := newTestService(t, subscriptionBuffer)
	createSandboxes(t, st, "project-1", "a", "b", "c")
	first, err := st.ListProjectEventsAfter(context.Background(), "project-1", 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	stream := open(t, s, services.ProjectEventStart{LastEventID: strconv.FormatInt(first[0].ID, 10)})
	if _, err := st.PruneProjectEvents(context.Background(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if event, err := stream.Next(ctx); !errors.Is(err, services.ErrProjectEventStreamBehind) {
		t.Fatalf("next = %d, %v; want the stream ended", event.ID, err)
	}
	resumed := open(t, s, services.ProjectEventStart{LastEventID: strconv.FormatInt(first[0].ID, 10)})
	if resumed.Reset() != services.ProjectEventResetExpired {
		t.Fatalf("reset = %q, want expired", resumed.Reset())
	}
}
