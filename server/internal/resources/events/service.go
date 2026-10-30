// Package events serves a project's event stream (ADR 26-10-30-868): the
// project's retained event log, read back from where a client left off and
// then followed live through this node's tailer.
package events

import (
	"context"
	"strconv"

	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// catchUpPage is how many events one read of the log returns while a stream
// catches up with it.
const catchUpPage = 500

// Service opens project event streams.
type Service struct {
	store  *store.Store
	tailer *tailer
}

// NewService returns the event service. Its tailer polls the log only while
// a stream is open.
func NewService(st *store.Store) *Service {
	return &Service{store: st, tailer: newTailer(st, defaultPollInterval, subscriptionBuffer)}
}

// OpenProjectEvents opens a project's event stream.
//
// The stream subscribes to the tailer before it reads the log, so an event is
// either in what it reads or delivered to it after: one committed before the
// subscription is in the log by the time it is read, because IDs commit in
// order (ADR 26-10-30-868 §5), and one committed after reaches the tailer's
// next poll. What it reads and what it is delivered may overlap; the stream
// passes each ID once.
func (s *Service) OpenProjectEvents(ctx context.Context, projectID string, start services.ProjectEventStart) (services.ProjectEventStream, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apperrors.NotFound(err, "project not found")
	}
	sub, err := s.tailer.subscribe(ctx, projectID)
	if err != nil {
		return nil, err
	}
	stream := &stream{store: s.store, projectID: projectID, sub: sub, tailer: s.tailer}
	if err := stream.position(ctx, start); err != nil {
		stream.Close()
		return nil, err
	}
	return stream, nil
}

// stream is one client's stream: the history it starts with, then the log
// after its cursor, read until it is caught up, then the tailer's delivery.
type stream struct {
	store     *store.Store
	projectID string
	tailer    *tailer
	sub       *subscription

	reset string
	// history is the past events a stream with no usable Last-Event-ID
	// starts with, all at or before cursor.
	history []model.ProjectEvent
	// page is the rest of the last read of the log after cursor.
	page []model.ProjectEvent
	// cursor is the last ID the stream has passed on, or the one it resumes
	// after.
	cursor int64
	// start is where cursor began.
	start int64
	// caughtUp is set once a read of the log after cursor came back short:
	// from then on the tailer's delivery has everything later.
	caughtUp bool
}

// position sets where the stream starts: after a Last-Event-ID the log still
// holds, or else at the head, with the project's last History events before
// it.
func (s *stream) position(ctx context.Context, start services.ProjectEventStart) error {
	head, prunedThrough, err := s.store.ProjectEventLogBounds(ctx)
	if err != nil {
		return err
	}
	if start.LastEventID != "" {
		id, err := strconv.ParseInt(start.LastEventID, 10, 64)
		switch {
		case err != nil || id < 0 || id > head:
			s.reset = services.ProjectEventResetUnknown
		case id < prunedThrough:
			s.reset = services.ProjectEventResetExpired
		default:
			s.cursor, s.start = id, id
			return nil
		}
	}
	if start.History > 0 {
		recent, err := s.store.ListRecentProjectEvents(ctx, s.projectID, head, start.History)
		if err != nil {
			return err
		}
		s.history = recent
	}
	s.cursor, s.start = head, head
	return nil
}

func (s *stream) Reset() string { return s.reset }

func (s *stream) Position() int64 { return s.start }

// Through is where an idle stream stands in the whole log: past every event
// of its project it has passed on, and past every other project's that the
// tailer has, so a quiet project's client does not keep the ID it connected
// with until retention expires it.
func (s *stream) Through() (int64, bool) {
	if len(s.history) > 0 || len(s.page) > 0 || !s.caughtUp {
		return 0, false
	}
	delivered, ok := s.tailer.deliveredThrough(s.sub)
	if !ok {
		return 0, false
	}
	return max(s.cursor, delivered), true
}

func (s *stream) Next(ctx context.Context) (model.ProjectEvent, error) {
	if len(s.history) > 0 {
		event := s.history[0]
		s.history = s.history[1:]
		return event, nil
	}
	for !s.caughtUp || len(s.page) > 0 {
		if len(s.page) == 0 {
			page, err := s.store.ListProjectEventsAfter(ctx, s.projectID, s.cursor, catchUpPage)
			if err != nil {
				return model.ProjectEvent{}, err
			}
			// Retention may have run since the stream was positioned: a long
			// catch-up is a client that was away long, near the 24-hour edge.
			// If it removed anything after the cursor, this page starts past a
			// gap, so the stream ends instead; the client's resume from its
			// last event then gets the reset that says so (ADR 26-10-30-868 §1).
			_, prunedThrough, err := s.store.ProjectEventLogBounds(ctx)
			if err != nil {
				return model.ProjectEvent{}, err
			}
			if prunedThrough > s.cursor {
				return model.ProjectEvent{}, services.ErrProjectEventStreamBehind
			}
			s.page = page
			s.caughtUp = len(page) < catchUpPage
			continue
		}
		event := s.page[0]
		s.page = s.page[1:]
		s.cursor = event.ID
		return event, nil
	}
	for {
		select {
		case <-ctx.Done():
			return model.ProjectEvent{}, ctx.Err()
		case <-s.sub.behind:
			return model.ProjectEvent{}, services.ErrProjectEventStreamBehind
		case event := <-s.sub.events:
			if event.ID > s.cursor {
				s.cursor = event.ID
				return event, nil
			}
		}
	}
}

func (s *stream) Close() { s.tailer.unsubscribe(s.sub) }
