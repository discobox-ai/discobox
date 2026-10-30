package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/discobox-ai/discobox/pkg/programstatus"
	"github.com/discobox-ai/discobox/server/internal/model"
)

// A sandbox's events are written by the store, in the transaction of the
// write that changed the sandbox (ADR 26-10-29-316 §2). Every writer of a
// column an event reports — create, UpdateSandbox, the pool's state reports,
// the agent status, delete — notes the sandbox on its transaction's
// sandboxEventBatch, and the batch is recorded just before the outermost
// transaction commits.
//
// Recording last is what keeps the counter from deadlocking: its row lock
// (allocateSandboxEventSeqs) is held to commit, and a transaction that took it
// and then waited on a sandbox row could meet one holding that row and
// waiting on the counter. Taken last, the counter is never held while
// anything else is waited for.
//
// The batch is compared with the committed row, not with what the writer
// meant to write, so noting a sandbox that did not change — a heartbeat, a
// state sync's restamp, a write a savepoint rolled back — records nothing.

// sandboxEventBatch is what one transaction changed: the sandboxes it wrote,
// and the last state of those it deleted.
type sandboxEventBatch struct {
	touched []string
	seen    map[string]struct{}
	deleted []model.SandboxEventData
	// backfill records a sandbox's first event as an update: the sandbox
	// was created before there were events.
	backfill bool
}

func (b *sandboxEventBatch) touch(sandboxID string) {
	if b.seen == nil {
		b.seen = map[string]struct{}{}
	}
	if _, ok := b.seen[sandboxID]; ok {
		return
	}
	b.seen[sandboxID] = struct{}{}
	b.touched = append(b.touched, sandboxID)
}

// inSandboxTx runs fn in the transaction the store is bound to, or in a new
// one, so that the sandbox writes it makes are recorded with it.
func (s *Store) inSandboxTx(ctx context.Context, fn func(txStore *Store, tx *gorm.DB) error) error {
	if s.sandboxEvents != nil {
		return fn(s, s.txWrite.WithContext(ctx))
	}
	return s.Transaction(ctx, fn)
}

// sandboxChanged notes a sandbox this transaction wrote.
func (s *Store) sandboxChanged(sandboxID string) {
	s.sandboxEvents.touch(sandboxID)
}

// sandboxDeleted notes a sandbox this transaction is deleting, with the state
// it had: the row will be gone when the batch is recorded.
func (s *Store) sandboxDeleted(sandbox *model.Sandbox) {
	s.sandboxEvents.deleted = append(s.sandboxEvents.deleted, sandboxEventData(sandbox, time.Time{}))
}

// sandboxEventColumns are what an event is projected from.
var sandboxEventColumns = []string{"id", "project_id", "name", "desired_state", "state", "runtime_state", "agent_status", "event_seq"}

type pendingSandboxEvent struct {
	eventType string
	data      model.SandboxEventData
}

// record appends an event for every sandbox whose state differs from its
// latest event, and one for every sandbox deleted.
func (b *sandboxEventBatch) record(tx *gorm.DB) error {
	if len(b.touched) == 0 && len(b.deleted) == 0 {
		return nil
	}
	now := time.Now().UTC()
	var pending []pendingSandboxEvent
	for _, sandboxID := range b.touched {
		// Locked before the counter is, so that the counter is the last lock
		// taken even by a caller that did not write the row itself (the
		// backfill); a row this transaction wrote is already its own.
		var sandbox model.Sandbox
		err := tx.Model(&model.Sandbox{}).Clauses(clause.Locking{Strength: "UPDATE"}).
			Select(sandboxEventColumns).Where("id = ?", sandboxID).Take(&sandbox).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		data := sandboxEventData(&sandbox, now)
		eventType := model.SandboxEventCreated
		if b.backfill {
			eventType = model.SandboxEventUpdated
		}
		if sandbox.EventSeq != 0 {
			same, err := sameAsSandboxEvent(tx, sandbox.EventSeq, data)
			if err != nil {
				return err
			}
			if same {
				continue
			}
			eventType = model.SandboxEventUpdated
		}
		pending = append(pending, pendingSandboxEvent{eventType: eventType, data: data})
	}
	for _, data := range b.deleted {
		// A delete a savepoint rolled back left the row in place.
		var remaining int64
		if err := tx.Model(&model.Sandbox{}).Where("id = ?", data.SandboxID).Count(&remaining).Error; err != nil {
			return err
		}
		if remaining > 0 {
			continue
		}
		data.At = now
		pending = append(pending, pendingSandboxEvent{eventType: model.SandboxEventDeleted, data: data})
	}
	if len(pending) == 0 {
		return nil
	}
	head, err := allocateSandboxEventSeqs(tx, len(pending))
	if err != nil {
		return err
	}
	seq := head - int64(len(pending))
	for _, event := range pending {
		seq++
		if err := insertSandboxEvent(tx, seq, event.eventType, event.data); err != nil {
			return err
		}
		if event.eventType == model.SandboxEventDeleted {
			continue
		}
		if err := tx.Model(&model.Sandbox{}).Where("id = ?", event.data.SandboxID).
			UpdateColumn("event_seq", seq).Error; err != nil {
			return err
		}
	}
	return nil
}

// BackfillSandboxEvents writes a `sandbox.updated` for every sandbox that has
// no event, so that a stream opened with no cursor reports every sandbox from
// the start (ADR 26-10-29-316). database.Migrate runs it on every start; once
// every sandbox has an event it writes nothing.
func BackfillSandboxEvents(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		var sandboxIDs []string
		if err := tx.Model(&model.Sandbox{}).Where("event_seq = 0").
			Order("created_at ASC").Order("id ASC").Pluck("id", &sandboxIDs).Error; err != nil {
			return err
		}
		batch := sandboxEventBatch{backfill: true}
		for _, sandboxID := range sandboxIDs {
			batch.touch(sandboxID)
		}
		return batch.record(tx)
	})
}

func sameAsSandboxEvent(tx *gorm.DB, seq int64, data model.SandboxEventData) (bool, error) {
	var last model.SandboxEvent
	err := tx.Select("data").Where("seq = ?", seq).Take(&last).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// An event this server cannot read is no match: the next one replaces it.
	var previous model.SandboxEventData
	readable := json.Unmarshal(last.Data, &previous) == nil
	return readable && previous.SameState(data), nil
}

func insertSandboxEvent(tx *gorm.DB, seq int64, eventType string, data model.SandboxEventData) error {
	encoded, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return tx.Create(&model.SandboxEvent{
		Seq:       seq,
		ProjectID: data.ProjectID,
		SandboxID: data.SandboxID,
		Type:      eventType,
		Data:      encoded,
		At:        data.At,
	}).Error
}

// allocateSandboxEventSeqs takes n sequence numbers and returns the last.
// The counter's row lock is held until the transaction commits, so a second
// writer takes its numbers only once the first has committed: Seq order is
// commit order on Postgres, where an autoincrement key's is not (ADR
// 26-10-29-316 §3). SQLite has one writer at a time anyway.
func allocateSandboxEventSeqs(tx *gorm.DB, n int) (int64, error) {
	advance := func() (int64, error) {
		result := tx.Model(&model.SandboxEventLog{}).Where("id = ?", model.SandboxEventLogID).
			UpdateColumn("head", gorm.Expr("head + ?", n))
		return result.RowsAffected, result.Error
	}
	advanced, err := advance()
	if err != nil {
		return 0, err
	}
	if advanced == 0 {
		// Migrate writes the row; a database it has not run on yet gets it here.
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).
			Create(&model.SandboxEventLog{ID: model.SandboxEventLogID}).Error; err != nil {
			return 0, err
		}
		if _, err := advance(); err != nil {
			return 0, err
		}
	}
	var log model.SandboxEventLog
	if err := tx.Take(&log, "id = ?", model.SandboxEventLogID).Error; err != nil {
		return 0, err
	}
	return log.Head, nil
}

// sandboxEventData projects a sandbox onto what its events report.
func sandboxEventData(sandbox *model.Sandbox, at time.Time) model.SandboxEventData {
	data := model.SandboxEventData{
		ProjectID:    sandbox.ProjectID,
		SandboxID:    sandbox.ID,
		Name:         sandbox.Name,
		At:           at,
		DesiredState: sandbox.DesiredState,
		State:        sandbox.State,
		RuntimeState: sandbox.RuntimeState,
	}
	switch {
	case sandbox.DesiredState == model.DesiredStateArchived || sandbox.State == model.SandboxStateArchived || sandbox.State == model.SandboxStateDeleted:
		// From the archive on, no pool reports on the sandbox
		// (ApplySandboxStateReports skips it, and puts its state down on the
		// next complete sync), so the runtime state recorded last is from
		// before the archive: it is not reported as now.
		data.RuntimeState = ""
	case sandbox.RuntimeState == model.SandboxRuntimeStateRunning:
		// A stopped sandbox keeps the last status it posted, and says nothing
		// about now.
		data.Program = primaryProgramStatus(sandbox.AgentStatus)
	}
	return data
}

// primaryProgramStatus is the program status of the primary session, by the
// rule `discobox ls` reads it by (programstatus.PrimaryPosted). A kind is
// reported only of a blocked program, as the contract says.
func primaryProgramStatus(status []byte) *model.SandboxEventProgram {
	if len(status) == 0 {
		return nil
	}
	var document struct {
		Sessions json.RawMessage `json:"sessions"`
	}
	if json.Unmarshal(status, &document) != nil || len(document.Sessions) == 0 {
		return nil
	}
	posted, ok := programstatus.PrimaryPosted(document.Sessions)
	if !ok {
		return nil
	}
	program := &model.SandboxEventProgram{State: string(posted.State), App: posted.App}
	if posted.State == programstatus.Blocked {
		program.Kind = string(posted.Kind)
	}
	return program
}

// SandboxEventPosition is where the event stream stands: the last Seq handed
// out, and the highest retention has removed.
type SandboxEventPosition struct {
	Head          int64
	PrunedThrough int64
}

// GetSandboxEventPosition reads the event stream's position as committed.
func (s *Store) GetSandboxEventPosition(ctx context.Context) (SandboxEventPosition, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return SandboxEventPosition{}, err
	}
	var logs []model.SandboxEventLog
	if err := read.Where("id = ?", model.SandboxEventLogID).Limit(1).Find(&logs).Error; err != nil {
		return SandboxEventPosition{}, err
	}
	if len(logs) == 0 {
		return SandboxEventPosition{}, nil
	}
	return SandboxEventPosition{Head: logs[0].Head, PrunedThrough: logs[0].PrunedThrough}, nil
}

// ListSandboxEventsAfter returns up to limit of a project's events after seq,
// in order.
func (s *Store) ListSandboxEventsAfter(ctx context.Context, projectID string, after int64, limit int) ([]model.SandboxEvent, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var events []model.SandboxEvent
	err = read.Where("project_id = ? AND seq > ?", projectID, after).
		Order("seq ASC").Limit(limit).Find(&events).Error
	return events, err
}

// ListLatestSandboxEvents returns a project's current state as events, in
// order: the latest event of every sandbox it holds, and every
// `sandbox.deleted` after deletedAfter (ADR 26-10-29-316 §4).
//
// It is one statement, so one snapshot. A sandbox that changed after the
// caller read deletedAfter is in it either way, by its latest event or by its
// deletion, and anything committed after it has a higher Seq than all of it,
// so a stream that goes on from the last of it, or from deletedAfter if it is
// empty, misses nothing.
func (s *Store) ListLatestSandboxEvents(ctx context.Context, projectID string, deletedAfter int64) ([]model.SandboxEvent, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var events []model.SandboxEvent
	err = read.Where("project_id = ? AND (seq IN (?) OR (type = ? AND seq > ?))", projectID,
		read.Model(&model.Sandbox{}).Select("event_seq").Where("project_id = ?", projectID),
		model.SandboxEventDeleted, deletedAfter).
		Order("seq ASC").Find(&events).Error
	return events, err
}

// PruneSandboxEvents removes every event older than before that is not a
// sandbox's latest, and moves the watermark past the highest it removed
// (ADR 26-10-29-316 §4). It returns how many it removed.
func (s *Store) PruneSandboxEvents(ctx context.Context, before time.Time) (int64, error) {
	var removed int64
	err := s.Transaction(ctx, func(_ *Store, tx *gorm.DB) error {
		prunable := tx.Model(&model.SandboxEvent{}).
			Where("at < ? AND NOT EXISTS (?)", before,
				tx.Model(&model.Sandbox{}).Select("1").Where("sandboxes.event_seq = sandbox_events.seq"))
		var highest *int64
		if err := prunable.Select("MAX(seq)").Scan(&highest).Error; err != nil {
			return err
		}
		if highest == nil {
			return nil
		}
		result := tx.Where("seq <= ? AND at < ? AND NOT EXISTS (?)", *highest, before,
			tx.Model(&model.Sandbox{}).Select("1").Where("sandboxes.event_seq = sandbox_events.seq")).
			Delete(&model.SandboxEvent{})
		if result.Error != nil {
			return result.Error
		}
		// The watermark commits with the delete, so a stream that reads it
		// after reading events cannot have missed one it does not show. Its
		// row is the counter's, so it is written last, as every writer takes
		// it last.
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).
			Create(&model.SandboxEventLog{ID: model.SandboxEventLogID}).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.SandboxEventLog{}).Where("id = ? AND pruned_through < ?", model.SandboxEventLogID, *highest).
			UpdateColumn("pruned_through", *highest).Error; err != nil {
			return err
		}
		removed = result.RowsAffected
		return nil
	})
	return removed, err
}

// OldestPrunableSandboxEvent is when the oldest event retention may remove
// was written, or nil when there is none.
func (s *Store) OldestPrunableSandboxEvent(ctx context.Context) (*time.Time, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var events []model.SandboxEvent
	err = read.Select("at").
		Where("NOT EXISTS (?)", read.Model(&model.Sandbox{}).Select("1").Where("sandboxes.event_seq = sandbox_events.seq")).
		Order("at ASC").Limit(1).Find(&events).Error
	if err != nil || len(events) == 0 {
		return nil, err
	}
	return &events[0].At, nil
}
