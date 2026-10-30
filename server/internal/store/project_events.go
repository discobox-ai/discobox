package store

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/server/internal/model"
)

// A project's events are one retained log (ADR 26-10-30-868). A writer that
// moves something an event projects records the change inside its own
// transaction, so an event commits with its change or not at all.
//
// The events are queued, not inserted where the change is made. The counter
// row that numbers them is a lock every event-writing transaction takes, and
// it must be the last lock any transaction takes: one holding the counter and
// waiting on a row that another transaction holds while waiting for the
// counter would deadlock. A writer often runs inside a caller's transaction
// that goes on to take more locks (a dirty mark, a second resource), so the
// queue is appended when the outermost Transaction's callback returns, just
// before it commits.

// eventQueue is the events one outermost transaction appends on commit.
type eventQueue struct {
	events []model.ProjectEvent
}

// writeTx runs fn in the transaction s is bound to, or in a new one.
func (s *Store) writeTx(ctx context.Context, fn func(txStore *Store, tx *gorm.DB) error) error {
	if s.events != nil {
		write, err := s.getWrite(ctx)
		if err != nil {
			return err
		}
		return fn(s, write)
	}
	return s.Transaction(ctx, fn)
}

// sandboxProjectionColumns are what a sandbox's projection is computed from,
// with the projection its last event recorded.
var sandboxProjectionColumns = []string{
	"id", "project_id", "name", "desired_state", "state", "runtime_state",
	"error_reason", "agent_status", "recorded_projection",
}

// recordSandboxChanges queues an event for each sandbox whose projection,
// read back after the caller's write, differs from the one its last event
// recorded, and records the new one. A sandbox.created is queued
// unconditionally. The caller's write has locked each row, so the read sees
// every write committed before it and none that can commit between it and
// this transaction's own commit.
func (s *Store) recordSandboxChanges(ctx context.Context, tx *gorm.DB, eventType string, sandboxIDs ...string) error {
	if s.events == nil {
		return errors.New("sandbox changes are recorded inside a transaction")
	}
	if len(sandboxIDs) == 0 {
		return nil
	}
	var rows []model.Sandbox
	if err := tx.WithContext(ctx).Model(&model.Sandbox{}).
		Select(sandboxProjectionColumns).
		Where("id IN ?", sandboxIDs).
		Order("id").
		Find(&rows).Error; err != nil {
		return err
	}
	for i := range rows {
		row := &rows[i]
		encoded, err := row.EncodedEventProjection()
		if err != nil {
			return err
		}
		if eventType != model.ProjectEventSandboxCreated && encoded == row.RecordedProjection {
			continue
		}
		if err := tx.WithContext(ctx).Model(&model.Sandbox{}).
			Where("id = ?", row.ID).
			UpdateColumn("recorded_projection", encoded).Error; err != nil {
			return err
		}
		if err := s.queueSandboxEvent(eventType, row); err != nil {
			return err
		}
	}
	return nil
}

// queueSandboxEvent queues one event carrying the sandbox's projection as its
// row stands.
func (s *Store) queueSandboxEvent(eventType string, sandbox *model.Sandbox) error {
	at := time.Now().UTC()
	data, err := json.Marshal(model.SandboxEvent{SandboxProjection: sandbox.EventProjection(), At: at})
	if err != nil {
		return err
	}
	s.events.events = append(s.events.events, model.ProjectEvent{
		ProjectID:  sandbox.ProjectID,
		Type:       eventType,
		ResourceID: sandbox.ID,
		Data:       data,
		At:         at,
	})
	return nil
}

// appendProjectEventsTx numbers the queued events and inserts them. Taking
// the IDs increments the counter row, whose lock is then held until commit,
// so a transaction given later IDs cannot commit first (ADR 26-10-30-868 §5).
func appendProjectEventsTx(ctx context.Context, tx *gorm.DB, events []model.ProjectEvent) error {
	if len(events) == 0 {
		return nil
	}
	n := int64(len(events))
	head, err := advanceProjectEventCounterTx(ctx, tx, n)
	if err != nil {
		return err
	}
	for i := range events {
		events[i].ID = head - n + 1 + int64(i)
	}
	return tx.WithContext(ctx).Create(&events).Error
}

// advanceProjectEventCounterTx moves the counter on by n and returns its new
// value. The row is made by the database upgrade, and nothing else makes it
// (database.startProjectEventLog), so a missing row is an error rather than
// a second place that creates it.
func advanceProjectEventCounterTx(ctx context.Context, tx *gorm.DB, n int64) (int64, error) {
	result := tx.WithContext(ctx).Model(&model.ProjectEventCounter{}).
		Where("id = ?", model.ProjectEventCounterID).
		UpdateColumn("value", gorm.Expr("value + ?", n))
	if result.Error != nil {
		return 0, result.Error
	}
	if result.RowsAffected != 1 {
		return 0, errors.New("the project event counter row is missing; the database upgrade makes it")
	}
	var counter model.ProjectEventCounter
	if err := tx.WithContext(ctx).First(&counter, "id = ?", model.ProjectEventCounterID).Error; err != nil {
		return 0, err
	}
	return counter.Value, nil
}

// logRead is the handle the log is read on: the write handle, never a read
// replica. A stream reads the log to catch up and is then delivered what this
// node's tailer reads after it, so both must see one order of commits; two
// replicas lagging by different amounts would let the tailer pass an event
// the stream's catch-up had not yet seen, and resuming would lose it.
func (s *Store) logRead(ctx context.Context) (*gorm.DB, error) {
	return s.getWrite(ctx)
}

// ProjectEventLogBounds is the log's head, the last ID given out, and the
// last ID retention removed. A Last-Event-ID the log holds lies between them,
// inclusive.
func (s *Store) ProjectEventLogBounds(ctx context.Context) (head, prunedThrough int64, err error) {
	read, err := s.logRead(ctx)
	if err != nil {
		return 0, 0, err
	}
	var counter model.ProjectEventCounter
	err = read.First(&counter, "id = ?", model.ProjectEventCounterID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, 0, nil
	}
	return counter.Value, counter.PrunedThrough, err
}

// ListProjectEventsAfter returns up to limit events after afterID, oldest
// first: a project's, or with projectID empty every project's.
func (s *Store) ListProjectEventsAfter(ctx context.Context, projectID string, afterID int64, limit int) ([]model.ProjectEvent, error) {
	read, err := s.logRead(ctx)
	if err != nil {
		return nil, err
	}
	query := read.Where("id > ?", afterID)
	if projectID != "" {
		query = query.Where("project_id = ?", projectID)
	}
	var events []model.ProjectEvent
	err = query.Order("id ASC").Limit(limit).Find(&events).Error
	return events, err
}

// ListRecentProjectEvents returns a project's last limit events at or before
// throughID, oldest first.
func (s *Store) ListRecentProjectEvents(ctx context.Context, projectID string, throughID int64, limit int) ([]model.ProjectEvent, error) {
	read, err := s.logRead(ctx)
	if err != nil {
		return nil, err
	}
	var events []model.ProjectEvent
	if err := read.Where("project_id = ? AND id <= ?", projectID, throughID).
		Order("id DESC").Limit(limit).Find(&events).Error; err != nil {
		return nil, err
	}
	slices.Reverse(events)
	return events, nil
}

// PruneProjectEvents removes every event recorded before cutoff and records
// the last ID it removed, so a Last-Event-ID at or below it is known to be
// gone. It returns how many it removed.
//
// It removes by ID, through the newest event older than the cutoff, so what
// remains is always a suffix of the log and a resume from within it is whole.
func (s *Store) PruneProjectEvents(ctx context.Context, cutoff time.Time) (int64, error) {
	var removed int64
	err := s.Transaction(ctx, func(_ *Store, tx *gorm.DB) error {
		var through *int64
		if err := tx.Model(&model.ProjectEvent{}).
			Where("at < ?", cutoff).
			Select("MAX(id)").
			Scan(&through).Error; err != nil {
			return err
		}
		if through == nil {
			return nil
		}
		result := tx.Where("id <= ?", *through).Delete(&model.ProjectEvent{})
		if result.Error != nil {
			return result.Error
		}
		removed = result.RowsAffected
		return tx.Model(&model.ProjectEventCounter{}).
			Where("id = ? AND pruned_through < ?", model.ProjectEventCounterID, *through).
			UpdateColumn("pruned_through", *through).Error
	})
	return removed, err
}

// OldestProjectEventAt is when the oldest event in the log was recorded, or
// nil when the log is empty.
func (s *Store) OldestProjectEventAt(ctx context.Context) (*time.Time, error) {
	read, err := s.logRead(ctx)
	if err != nil {
		return nil, err
	}
	var event model.ProjectEvent
	err = read.Select("at").Order("id ASC").Limit(1).Take(&event).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &event.At, nil
}
