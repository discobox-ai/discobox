package sandboxes

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// sandboxEventRetention is how long an event that is not its sandbox's latest
// is kept (ADR 26-10-29-316 §4).
const sandboxEventRetention = 24 * time.Hour

// sandboxEventPageSize bounds one read of the events after a cursor.
const sandboxEventPageSize = 500

// sandboxEventStream is services.SandboxEventStream over the store.
type sandboxEventStream struct {
	store     *store.Store
	projectID string
	// replay is set while the next read is the project's current state; reset
	// while that replay must be preceded by a reset.
	replay, reset bool
	cursor        int64
	// head is the stream's head as last read, which was before the next read
	// of the events: when that read comes back short, every event of the
	// project up to it has been seen.
	head int64
}

// OpenSandboxEvents starts a stream of a project's sandbox events after
// lastEventID, or with the project's current state when there is none.
func (s *Service) OpenSandboxEvents(ctx context.Context, projectID string, lastEventID *int64) (services.SandboxEventStream, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apperrors.NotFound(err, "project not found")
	}
	stream := &sandboxEventStream{store: s.store, projectID: projectID}
	if lastEventID == nil {
		stream.replay = true
		return stream, nil
	}
	stream.cursor = *lastEventID
	position, err := s.store.GetSandboxEventPosition(ctx)
	if err != nil {
		return nil, err
	}
	stream.head = position.Head
	if lost(stream.cursor, position) {
		stream.replay, stream.reset = true, true
	}
	return stream, nil
}

// lost reports whether a stream cannot go on from cursor: retention has
// removed events after it, or it is past every event there is, which a
// database restored from before it would make it.
func lost(cursor int64, position store.SandboxEventPosition) bool {
	return cursor < position.PrunedThrough || cursor > position.Head
}

func (st *sandboxEventStream) Cursor() int64 { return st.cursor }

// Next returns the events the client has not had, which may be none.
func (st *sandboxEventStream) Next(ctx context.Context) ([]services.SandboxStreamEvent, error) {
	if st.replay {
		return st.replayState(ctx)
	}
	events, err := st.store.ListSandboxEventsAfter(ctx, st.projectID, st.cursor, sandboxEventPageSize)
	if err != nil {
		return nil, err
	}
	// Read after the events: a prune that removed one after the cursor has
	// committed by now if the read above missed it, and it moved this.
	position, err := st.store.GetSandboxEventPosition(ctx)
	if err != nil {
		return nil, err
	}
	if lost(st.cursor, position) {
		st.replay, st.reset = true, true
		return st.replayState(ctx)
	}
	out := make([]services.SandboxStreamEvent, 0, len(events))
	for _, event := range events {
		out = append(out, streamEvent(event))
		st.cursor = event.Seq
	}
	// A short read saw every event of the project up to the head read before
	// it, so the stream stands there, whatever other projects did meanwhile.
	if len(events) < sandboxEventPageSize {
		st.cursor = max(st.cursor, st.head)
	}
	st.head = position.Head
	return out, nil
}

// replayState sends the project's current state, preceded by a reset when
// the cursor was lost, and goes on from the head read before it.
func (st *sandboxEventStream) replayState(ctx context.Context) ([]services.SandboxStreamEvent, error) {
	position, err := st.store.GetSandboxEventPosition(ctx)
	if err != nil {
		return nil, err
	}
	events, err := st.store.ListLatestSandboxEvents(ctx, st.projectID, position.Head)
	if err != nil {
		return nil, err
	}
	out := make([]services.SandboxStreamEvent, 0, len(events)+1)
	if st.reset {
		out = append(out, services.SandboxStreamEvent{Type: services.SandboxEventReset, Data: json.RawMessage(`{}`)})
	}
	st.cursor, st.head = position.Head, position.Head
	for _, event := range events {
		out = append(out, streamEvent(event))
		st.cursor = max(st.cursor, event.Seq)
	}
	st.replay, st.reset = false, false
	return out, nil
}

func streamEvent(event model.SandboxEvent) services.SandboxStreamEvent {
	seq := event.Seq
	return services.SandboxStreamEvent{ID: &seq, Type: event.Type, Data: event.Data}
}

// SandboxEventPruneResourceType is the reconcile resource that applies the
// events' retention: one row, armed by the sweep itself for the next event
// due and found by its scan when that mark is lost.
const SandboxEventPruneResourceType = "sandboxEventPrune"

// SandboxEventPruneID is the sweep's one ID.
const SandboxEventPruneID = "all"

// sandboxEventPruneReconciler removes the events retention no longer keeps.
type sandboxEventPruneReconciler struct {
	store *store.Store
	now   func() time.Time
}

func (r *sandboxEventPruneReconciler) Reconcile(ctx context.Context, _ string) (reconcile.Result, error) {
	removed, err := r.store.PruneSandboxEvents(ctx, r.now().Add(-sandboxEventRetention))
	if err != nil {
		return reconcile.Result{}, err
	}
	if removed > 0 {
		slog.DebugContext(ctx, "sandbox events pruned", "removed", removed)
	}
	oldest, err := r.store.OldestPrunableSandboxEvent(ctx)
	if err != nil || oldest == nil {
		return reconcile.Result{}, err
	}
	return reconcile.RequeueAt(oldest.Add(sandboxEventRetention)), nil
}

// ScanDirty finds the sweep when an event is due and nothing armed it: the
// first after a start, or one whose mark was lost.
func (r *sandboxEventPruneReconciler) ScanDirty(ctx context.Context) ([]string, error) {
	oldest, err := r.store.OldestPrunableSandboxEvent(ctx)
	if err != nil {
		return nil, err
	}
	if oldest == nil || oldest.After(r.now().Add(-sandboxEventRetention)) {
		return nil, nil
	}
	return []string{SandboxEventPruneID}, nil
}
