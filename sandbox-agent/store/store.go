package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/sandbox-agent/execs"
	"github.com/discobox-ai/x/gormdb"
	"github.com/discobox-ai/x/id"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const DefaultDBPath = "/var/lib/discobox/sandbox-agent.db"

// defaultLogRetention bounds how long exec transcript chunks are kept. It is
// a fixed constant rather than sandbox-configurable: nothing today needs a
// per-sandbox retention policy, and a long-lived terminal (the primary one
// lives for the sandbox's whole lifetime) would otherwise grow its transcript
// without bound even though individual chunks are compressed.
const defaultLogRetention = 14 * 24 * time.Hour

type Store struct {
	pools *gormdb.Pools
	write *gorm.DB
	read  *gorm.DB
	// hookSignal is closed by the next harness hook recorded, for a caller
	// waiting on one (ADR 0137 §3): the collector's writes are the notification,
	// not a poll of the table.
	hookMu     sync.Mutex
	hookSignal chan struct{}
	// hookClock is the last time handed out on the hook clock: every hook
	// stamp and resume point comes after it, so a point names one instant no
	// hook shares, however coarse the wall clock is.
	hookClock time.Time
}

type Event struct {
	ID         string         `json:"id"`
	TerminalID string         `json:"terminalId,omitempty"`
	Type       string         `json:"type"`
	Message    string         `json:"message,omitempty"`
	Details    map[string]any `json:"details,omitempty"`
	CreatedAt  time.Time      `json:"createdAt"`
}

type ResourceSample struct {
	ID         string          `json:"id,omitempty"`
	TerminalID string          `json:"terminalId"`
	SampledAt  time.Time       `json:"sampledAt"`
	Source     string          `json:"source"`
	Data       json.RawMessage `json:"data"`
}

type HarnessHookRecord struct {
	ID         string `json:"id,omitempty"`
	TerminalID string `json:"terminalId,omitempty"`
	Provider   string `json:"provider"`
	Event      string `json:"event"`
	// CanonicalEvent is Claude Code's name for what Event names, empty when
	// Claude Code has no name for it (ADR 0146 §3). The store derives it when
	// it records the hook; setting it on the way in has no effect.
	CanonicalEvent string          `json:"canonicalEvent,omitempty"`
	Payload        json.RawMessage `json:"payload"`
	CreatedAt      time.Time       `json:"createdAt"`
}

func Open(ctx context.Context, dsn string) (*Store, error) {
	if strings.TrimSpace(dsn) == "" {
		dsn = DefaultDBPath
	}
	pools, err := gormdb.Open(gormdb.Config{DSN: dsn})
	if err != nil {
		return nil, err
	}
	s := &Store{pools: pools, write: pools.Write, read: pools.Read}
	if err := s.write.WithContext(ctx).AutoMigrate(&AgentState{}, &ResourceSnapshot{}, &HarnessHookLog{}, &ExecState{}, &ExecEvent{}, &ExecRecord{}, &ExecLogChunk{}); err != nil {
		return nil, fmt.Errorf("migrate sandbox-agent store: %w", err)
	}
	if err := s.purgeDeletedExecRecords(ctx); err != nil {
		return nil, fmt.Errorf("migrate sandbox-agent store: %w", err)
	}
	if err := s.backfillCanonicalHookEvents(ctx); err != nil {
		return nil, fmt.Errorf("migrate sandbox-agent store: %w", err)
	}
	return s, nil
}

const canonicalHookEventsBackfilledKey = "canonical_hook_events_backfilled"

// backfillCanonicalHookEvents gives a canonical name to hooks recorded before
// the column existed, and to hooks whose name the mapping has learned since,
// so a query by canonical name finds them too (ADR 0146 §7). It is additive:
// no row's own event name is touched, so no stored hook changes meaning.
//
// It is stamped with the mapping's fingerprint rather than a boolean. The scan
// is proportional to the rows with no canonical name, and those are never
// removed — `harness_hook_logs` has no retention, and a hook Claude Code has
// no word for keeps an empty canonical name forever — so an unstamped backfill
// would re-read every one of them on every agent start, for a set that only
// grows. Stamping on the fingerprint runs it once per table, which is exactly
// as often as it can find anything: a new entry changes the fingerprint and
// reaches the old rows, and an unchanged table is skipped in one indexed read.
func (s *Store) backfillCanonicalHookEvents(ctx context.Context) error {
	version := harness.CanonicalHookEventsVersion()
	var stamp AgentState
	err := s.write.WithContext(ctx).Where("key = ?", canonicalHookEventsBackfilledKey).Take(&stamp).Error
	switch {
	case err == nil && stamp.Value == version:
		return nil
	case err != nil && !errors.Is(err, gorm.ErrRecordNotFound):
		return err
	}

	var pairs []struct {
		Provider string
		Event    string
	}
	if err := s.write.WithContext(ctx).Model(&HarnessHookLog{}).
		Select("provider", "event").
		Where("canonical_event = '' OR canonical_event IS NULL").
		Group("provider, event").
		Find(&pairs).Error; err != nil {
		return err
	}
	for _, pair := range pairs {
		canonical := harness.CanonicalHookEvent(pair.Provider, pair.Event)
		if canonical == "" {
			continue
		}
		if err := s.write.WithContext(ctx).Model(&HarnessHookLog{}).
			Where("provider = ? AND event = ? AND (canonical_event = '' OR canonical_event IS NULL)", pair.Provider, pair.Event).
			Update("canonical_event", canonical).Error; err != nil {
			return err
		}
	}
	// Stamped after the fill, so an interrupted run is simply run again.
	return s.write.WithContext(ctx).Save(&AgentState{
		Key: canonicalHookEventsBackfilledKey, Value: version, UpdatedAt: time.Now().UTC(),
	}).Error
}

const deletedExecRecordsPurgedKey = "deleted_exec_records_purged"

// purgeDeletedExecRecords is the upgrade path for DeleteExecRecord. Before it
// existed, deleting an exec left its durable record and observed status
// behind, and every listing surfaced the deleted exec again as lost.
//
// A purge cannot be undone, so it takes a record only when nothing says the
// id lived again after its last deletion, and it asks two writes that fail
// independently. The event log: any event after the deletion counts, except
// the three a store observation emits on its own, because those are exactly
// what re-reading a left-behind record produced, and an attach closing,
// because a terminal deleted with a client attached — how a tab is usually
// closed — records its attach ending after the deletion. An attach opening
// still counts. And the observed status row,
// which is upserted from the live exec: a run started after the deletion is a
// new exec under the same id. It is the start time that says so, not the
// row's created_at, which the upsert never updates — and a left-behind record
// re-read after its deletion carries its old run's start, or none.
//
// It does not repair the other half of the old behavior. An id created again
// under the old code kept the deleted exec's identity — command, workdir and
// metadata — because SaveExecRecord never overwrites. The event log has the
// new command and workdir but not the metadata, and a record whose command
// and harness disagree is worse than one that is consistently stale; such an
// exec takes its new identity the next time it is deleted and created.
//
// It runs once, recorded in agent_state, in one transaction with that marker
// so an interrupted run is simply run again.
func (s *Store) purgeDeletedExecRecords(ctx context.Context) error {
	return s.write.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var done int64
		if err := tx.Model(&AgentState{}).Where("key = ?", deletedExecRecordsPurgedKey).Count(&done).Error; err != nil {
			return err
		}
		if done > 0 {
			return nil
		}
		livedAgain := tx.Model(&ExecEvent{}).Select("1").
			Table("exec_events AS later").
			Where("later.exec_id = deleted.exec_id AND later.created_at > deleted.created_at").
			Where("later.type NOT IN ?", []string{"exec.observed", "exec.status.changed", "exec.exited", "exec.attach.closed"})
		recreated := tx.Model(&ExecState{}).Select("1").
			Table("exec_states AS state").
			Where("state.exec_id = deleted.exec_id AND state.started_at > deleted.created_at")
		deleted := tx.Model(&ExecEvent{}).Select("deleted.exec_id").
			Table("exec_events AS deleted").
			Where("deleted.type = ?", "exec.deleted").
			Where("NOT EXISTS (?)", livedAgain).
			Where("NOT EXISTS (?)", recreated)
		if err := tx.Where("exec_id IN (?)", deleted).Delete(&ExecRecord{}).Error; err != nil {
			return err
		}
		if err := tx.Where("exec_id IN (?)", deleted).Delete(&ExecState{}).Error; err != nil {
			return err
		}
		return tx.Create(&AgentState{Key: deletedExecRecordsPurgedKey, Value: "true", UpdatedAt: time.Now().UTC()}).Error
	})
}

// Close releases the underlying database connections. The long-lived main
// sandbox-agent server process does not need to call this, but the
// short-lived exec-shim process (one per exec, see execs.LogSink) does, to
// release its handle on the shared sqlite file promptly on exit.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	return s.pools.Close()
}

const primaryTerminalLaunchedKey = "primary_terminal_launched"

// PrimaryTerminalLaunched reports whether the sandbox-agent has launched the
// primary terminal in a previous sandbox start. It is used to decide between
// running the harness with the initial prompt (first start) and resuming the
// previous session with the relaunch command (subsequent starts).
func (s *Store) PrimaryTerminalLaunched(ctx context.Context) (bool, error) {
	if s == nil {
		return false, nil
	}
	var count int64
	if err := s.read.WithContext(ctx).Model(&AgentState{}).
		Where("key = ? AND value = ?", primaryTerminalLaunchedKey, "true").
		Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// MarkPrimaryTerminalLaunched durably records that the primary terminal has been
// launched so later starts use the relaunch command.
func (s *Store) MarkPrimaryTerminalLaunched(ctx context.Context) error {
	if s == nil {
		return nil
	}
	return s.write.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		UpdateAll: true,
	}).Create(&AgentState{Key: primaryTerminalLaunchedKey, Value: "true", UpdatedAt: time.Now().UTC()}).Error
}

func (s *Store) RecordExecEvent(ctx context.Context, execID, typ, message string, details map[string]any) error {
	if s == nil {
		return nil
	}
	typ = strings.TrimSpace(typ)
	if typ == "" {
		return fmt.Errorf("event type is required")
	}
	detailsJSON, err := json.Marshal(details)
	if err != nil {
		return err
	}
	id, err := newID()
	if err != nil {
		return err
	}
	return s.write.WithContext(ctx).Create(&ExecEvent{
		ID:        id,
		ExecID:    strings.TrimSpace(execID),
		Type:      typ,
		Message:   strings.TrimSpace(message),
		Details:   detailsJSON,
		CreatedAt: time.Now().UTC(),
	}).Error
}

func (s *Store) ObserveExec(ctx context.Context, current execs.Exec) error {
	if s == nil || current.ID == "" {
		return nil
	}
	now := time.Now().UTC()
	next := ExecState{
		ExecID:     current.ID,
		Unit:       current.Unit,
		Status:     string(current.Status),
		PID:        current.PID,
		ExitCode:   current.ExitCode,
		Error:      current.Error,
		Stopped:    current.Stopped,
		CreatedAt:  current.CreatedAt,
		StartedAt:  current.StartedAt,
		ExitedAt:   current.ExitedAt,
		ObservedAt: now,
		UpdatedAt:  now,
	}
	return s.write.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var previous ExecState
		hadPrevious := tx.First(&previous, "exec_id = ?", current.ID).Error == nil
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "exec_id"}},
			UpdateAll: true,
		}).Create(&next).Error; err != nil {
			return err
		}
		if !hadPrevious {
			return createExecEventTx(tx, current.ID, "exec.observed", "exec observed", execDetails(current))
		}
		if previous.Status != next.Status {
			if err := createExecEventTx(tx, current.ID, "exec.status.changed", "exec status changed", map[string]any{
				"from": previous.Status,
				"to":   next.Status,
			}); err != nil {
				return err
			}
		}
		if previous.ExitedAt == nil && next.ExitedAt != nil {
			return createExecEventTx(tx, current.ID, "exec.exited", "exec exited", execDetails(current))
		}
		return nil
	})
}

// SaveExecRecord durably persists an exec's immutable identity/metadata. It is
// written once at create and never overwritten (OnConflict DoNothing), so the
// status-observe path can't null it out and a shim runtime write that drops the
// metadata field can't lose it.
func (s *Store) SaveExecRecord(ctx context.Context, current execs.Exec) error {
	if s == nil || current.ID == "" {
		return nil
	}
	command, err := json.Marshal(current.Command)
	if err != nil {
		return err
	}
	metadata, err := json.Marshal(current.Metadata)
	if err != nil {
		return err
	}
	record := ExecRecord{
		ExecID:    current.ID,
		HarnessID: current.Metadata["harnessId"],
		Primary:   current.Metadata["primary"] == "true",
		Command:   command,
		Workdir:   current.Workdir,
		TTY:       current.TTY,
		Metadata:  metadata,
		CreatedAt: current.CreatedAt,
	}
	return s.write.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&record).Error
}

// DeleteExecRecord removes an exec's durable record and observed status
// together. Its events stay: they are the exec's history, and the upgrade
// path above reads them.
func (s *Store) DeleteExecRecord(ctx context.Context, execID string) error {
	if s == nil {
		return nil
	}
	execID = strings.TrimSpace(execID)
	if execID == "" {
		return nil
	}
	return s.write.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("exec_id = ?", execID).Delete(&ExecRecord{}).Error; err != nil {
			return err
		}
		return tx.Where("exec_id = ?", execID).Delete(&ExecState{}).Error
	})
}

// LoadExecRecords returns the durable exec identity records joined with their
// latest observed status. It lets the manager restore metadata onto live execs
// (in case a shim write dropped it) and surface execs that outlived their tmpfs
// runtime files across a reboot.
func (s *Store) LoadExecRecords(ctx context.Context) ([]execs.Exec, error) {
	if s == nil {
		return nil, nil
	}
	var records []ExecRecord
	if err := s.read.WithContext(ctx).Find(&records).Error; err != nil {
		return nil, err
	}
	var states []ExecState
	if err := s.read.WithContext(ctx).Find(&states).Error; err != nil {
		return nil, err
	}
	byID := make(map[string]ExecState, len(states))
	for _, state := range states {
		byID[state.ExecID] = state
	}
	out := make([]execs.Exec, 0, len(records))
	for _, record := range records {
		exec := execs.Exec{
			ID:        record.ExecID,
			Workdir:   record.Workdir,
			TTY:       record.TTY,
			CreatedAt: record.CreatedAt,
		}
		_ = json.Unmarshal(record.Command, &exec.Command)
		_ = json.Unmarshal(record.Metadata, &exec.Metadata)
		if state, ok := byID[record.ExecID]; ok {
			exec.Unit = state.Unit
			exec.Status = execs.Status(state.Status)
			exec.PID = state.PID
			exec.ExitCode = state.ExitCode
			exec.Error = state.Error
			exec.Stopped = state.Stopped
			exec.StartedAt = state.StartedAt
			exec.ExitedAt = state.ExitedAt
		}
		out = append(out, exec)
	}
	return out, nil
}

// ExecEventFilter narrows ListEvents. A zero field matches everything.
type ExecEventFilter struct {
	// ID reads the one event it names, for the reason HarnessHookFilter.ID
	// exists.
	ID     string
	ExecID string
	Type   string
	// Since keeps events recorded at or after it, compared in UTC as the
	// events are recorded.
	Since time.Time
	// Ascending returns the earliest Limit matches from Since, oldest first,
	// for a reader following forward. Without it the most recent Limit come
	// back newest first.
	Ascending bool
	Limit     int
}

func (s *Store) ListEvents(ctx context.Context, filter ExecEventFilter) ([]Event, error) {
	if s == nil {
		return nil, nil
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	order := "created_at DESC, id DESC"
	if filter.Ascending {
		order = "created_at ASC, id ASC"
	}
	query := s.read.WithContext(ctx).Order(order).Limit(limit)
	if v := strings.TrimSpace(filter.ID); v != "" {
		query = query.Where("id = ?", v)
	}
	if v := strings.TrimSpace(filter.ExecID); v != "" {
		query = query.Where("exec_id = ?", v)
	}
	if v := strings.TrimSpace(filter.Type); v != "" {
		query = query.Where("type = ?", v)
	}
	if !filter.Since.IsZero() {
		query = query.Where("created_at >= ?", filter.Since.UTC())
	}
	var rows []ExecEvent
	if err := query.Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]Event, 0, len(rows))
	for _, row := range rows {
		var details map[string]any
		if len(row.Details) > 0 {
			_ = json.Unmarshal(row.Details, &details)
		}
		out = append(out, Event{
			ID:         row.ID,
			TerminalID: row.ExecID,
			Type:       row.Type,
			Message:    row.Message,
			Details:    details,
			CreatedAt:  row.CreatedAt,
		})
	}
	return out, nil
}

func (s *Store) RecordResourceSample(ctx context.Context, sample ResourceSample, retentionCount int) (ResourceSample, error) {
	if s == nil {
		return sample, nil
	}
	if strings.TrimSpace(sample.TerminalID) == "" {
		return ResourceSample{}, fmt.Errorf("terminal id is required")
	}
	if strings.TrimSpace(sample.Source) == "" {
		return ResourceSample{}, fmt.Errorf("resource sample source is required")
	}
	if len(sample.Data) == 0 {
		sample.Data = json.RawMessage(`{}`)
	}
	if !json.Valid(sample.Data) {
		return ResourceSample{}, fmt.Errorf("resource sample data must be valid JSON")
	}
	if sample.SampledAt.IsZero() {
		sample.SampledAt = time.Now().UTC()
	}
	id, err := newID()
	if err != nil {
		return ResourceSample{}, err
	}
	sample.ID = id
	row := ResourceSnapshot{
		ID:         id,
		TerminalID: strings.TrimSpace(sample.TerminalID),
		SampledAt:  sample.SampledAt.UTC(),
		Source:     strings.TrimSpace(sample.Source),
		Data:       append([]byte{}, sample.Data...),
		CreatedAt:  time.Now().UTC(),
	}
	if retentionCount <= 0 {
		retentionCount = 300
	}
	return sample, s.write.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		var keep []string
		if err := tx.Model(&ResourceSnapshot{}).
			Where("terminal_id = ?", row.TerminalID).
			Order("sampled_at DESC, created_at DESC").
			Limit(retentionCount).
			Pluck("id", &keep).Error; err != nil {
			return err
		}
		if len(keep) == 0 {
			return nil
		}
		return tx.Where("terminal_id = ? AND id NOT IN ?", row.TerminalID, keep).Delete(&ResourceSnapshot{}).Error
	})
}

func (s *Store) ListResourceSamples(ctx context.Context, terminalID string, limit int) ([]ResourceSample, error) {
	if s == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 100
	}
	var rows []ResourceSnapshot
	if err := s.read.WithContext(ctx).
		Where("terminal_id = ?", strings.TrimSpace(terminalID)).
		Order("sampled_at DESC, created_at DESC").
		Limit(limit).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]ResourceSample, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		row := rows[i]
		out = append(out, ResourceSample{
			ID:         row.ID,
			TerminalID: row.TerminalID,
			SampledAt:  row.SampledAt,
			Source:     row.Source,
			Data:       json.RawMessage(append([]byte{}, row.Data...)),
		})
	}
	return out, nil
}

func (s *Store) RecordHarnessHook(ctx context.Context, record HarnessHookRecord) (HarnessHookRecord, error) {
	if s == nil {
		return record, nil
	}
	record.Provider = strings.TrimSpace(record.Provider)
	if record.Provider == "" {
		return HarnessHookRecord{}, fmt.Errorf("hook provider is required")
	}
	record.Event = strings.TrimSpace(record.Event)
	if record.Event == "" {
		return HarnessHookRecord{}, fmt.Errorf("hook event is required")
	}
	if len(record.Payload) == 0 {
		record.Payload = json.RawMessage(`{}`)
	}
	if !json.Valid(record.Payload) {
		return HarnessHookRecord{}, fmt.Errorf("hook payload must be valid JSON")
	}
	id, err := newID()
	if err != nil {
		return HarnessHookRecord{}, err
	}
	record.ID = id
	record.TerminalID = strings.TrimSpace(record.TerminalID)
	// The canonical name is derived here and nowhere else (ADR 0146 §4): when
	// the row is written, so a wait can match it in SQL, and in one place, so
	// no writer can store a row whose two names disagree. Whatever a caller
	// put in this field is overwritten rather than trusted — it is an answer
	// the store gives, not an input it takes.
	record.CanonicalEvent = harness.CanonicalHookEvent(record.Provider, record.Event)
	// Stamped and written under the lock, so hooks reach the table in the
	// order of their stamps and a wait resuming past one cannot miss an
	// earlier-stamped hook written after it.
	s.hookMu.Lock()
	defer s.hookMu.Unlock()
	record.CreatedAt = s.tickHookClock()
	row := HarnessHookLog{
		ID:             record.ID,
		TerminalID:     record.TerminalID,
		Provider:       record.Provider,
		Event:          record.Event,
		CanonicalEvent: record.CanonicalEvent,
		Payload:        append([]byte{}, record.Payload...),
		CreatedAt:      record.CreatedAt,
	}
	if err := s.write.WithContext(ctx).Create(&row).Error; err != nil {
		return HarnessHookRecord{}, err
	}
	if s.hookSignal != nil {
		close(s.hookSignal)
		s.hookSignal = nil
	}
	return record, nil
}

// HarnessHookResumePoint is a point on the hook clock: every hook recorded
// after the call is stamped after it, so a wait counting from it finds a hook
// however soon that hook follows. Without a store it is the wall clock.
func (s *Store) HarnessHookResumePoint() time.Time {
	if s == nil {
		return time.Now().UTC()
	}
	s.hookMu.Lock()
	defer s.hookMu.Unlock()
	return s.tickHookClock()
}

// tickHookClock advances the hook clock and returns its new time: now, or a
// nanosecond past the last time handed out when the wall clock has not moved
// past it. Call it holding hookMu.
func (s *Store) tickHookClock() time.Time {
	now := time.Now().UTC()
	if !now.After(s.hookClock) {
		now = s.hookClock.Add(time.Nanosecond)
	}
	s.hookClock = now
	return now
}

// HarnessHookSignal returns a channel closed by the next harness hook recorded.
// Take it before reading the records a wait is looking for, so a hook recorded
// between the read and the wait still wakes it.
func (s *Store) HarnessHookSignal() <-chan struct{} {
	if s == nil {
		return nil
	}
	s.hookMu.Lock()
	defer s.hookMu.Unlock()
	if s.hookSignal == nil {
		s.hookSignal = make(chan struct{})
	}
	return s.hookSignal
}

// FirstHarnessHookSince returns the oldest of a terminal's harness hooks that
// was recorded after since and is one of events, or nil when none is.
//
// The events are matched in the query, not after it: a terminal records every
// tool call, and a wait for the one hook that ends a turn must find it behind
// any number of hooks it did not name.
//
// A name matches either the harness's own event or its canonical one
// (ADR 0146 §6), so a wait for Stop ends on every harness that has a turn-end
// hook, and one for an event with no canonical name still ends on the harness
// that emits it.
func (s *Store) FirstHarnessHookSince(ctx context.Context, terminalID string, since time.Time, events []string) (*HarnessHookRecord, error) {
	if s == nil {
		return nil, nil
	}
	// A blank name matches nothing. Without this it matches everything whose
	// canonical name is empty — every Interrupt, every opencode event Claude
	// Code has no word for — because `canonical_event IN ('')` is true for all
	// of them. The API puts no minLength on a wait's event names and pflag
	// reads `--hook Stop,` as two, so a blank reaches here from an ordinary
	// typo.
	named := make([]string, 0, len(events))
	for _, event := range events {
		if event = strings.TrimSpace(event); event != "" {
			named = append(named, event)
		}
	}
	if len(named) == 0 {
		return nil, nil
	}
	events = named
	var rows []HarnessHookLog
	// Hooks are recorded in UTC and SQLite compares times as text carrying
	// their offset, so the bound is compared in UTC.
	if err := s.read.WithContext(ctx).
		Where("terminal_id = ? AND created_at > ? AND (event IN ? OR canonical_event IN ?)",
			strings.TrimSpace(terminalID), since.UTC(), events, events).
		Order("created_at ASC").
		Limit(1).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	record := hookRecord(rows[0])
	return &record, nil
}

// HarnessHookFilter narrows ListHarnessHooks. A zero field matches everything.
type HarnessHookFilter struct {
	// ID reads the one record it names, which is how a caller holding an ID
	// from a listing reads that record in full.
	ID         string
	TerminalID string
	Provider   string
	// Event matches either the harness's own event name or its canonical one
	// (ADR 0146 §6), so a reader need not know which of the two a name is.
	Event string
	// Since keeps hooks recorded at or after it. Hooks are recorded in UTC and
	// SQLite compares times as text carrying their offset, so the bound is
	// compared in UTC.
	Since time.Time
	// Ascending takes the earliest Limit matches from Since, for a reader
	// following the log forward. Without it the most recent Limit are taken.
	// Either way they come back oldest first.
	Ascending bool
	Limit     int
}

func (s *Store) ListHarnessHooks(ctx context.Context, filter HarnessHookFilter) ([]HarnessHookRecord, error) {
	if s == nil {
		return nil, nil
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	order := "created_at DESC, id DESC"
	if filter.Ascending {
		order = "created_at ASC, id ASC"
	}
	query := s.read.WithContext(ctx).Order(order).Limit(limit)
	if v := strings.TrimSpace(filter.ID); v != "" {
		query = query.Where("id = ?", v)
	}
	if v := strings.TrimSpace(filter.TerminalID); v != "" {
		query = query.Where("terminal_id = ?", v)
	}
	if v := strings.TrimSpace(filter.Provider); v != "" {
		query = query.Where("provider = ?", v)
	}
	if v := strings.TrimSpace(filter.Event); v != "" {
		query = query.Where("event = ? OR canonical_event = ?", v, v)
	}
	if !filter.Since.IsZero() {
		query = query.Where("created_at >= ?", filter.Since.UTC())
	}
	var rows []HarnessHookLog
	if err := query.Find(&rows).Error; err != nil {
		return nil, err
	}
	if !filter.Ascending {
		slices.Reverse(rows)
	}
	out := make([]HarnessHookRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, hookRecord(row))
	}
	return out, nil
}

func hookRecord(row HarnessHookLog) HarnessHookRecord {
	payload := json.RawMessage(append([]byte{}, row.Payload...))
	if len(payload) == 0 || !json.Valid(payload) {
		payload = json.RawMessage(`{}`)
	}
	return HarnessHookRecord{
		ID:             row.ID,
		TerminalID:     row.TerminalID,
		Provider:       row.Provider,
		Event:          row.Event,
		CanonicalEvent: row.CanonicalEvent,
		Payload:        payload,
		CreatedAt:      row.CreatedAt,
	}
}

// AppendExecLogChunk durably persists one compressed transcript batch for an
// exec, then prunes chunks for that exec older than defaultLogRetention in
// the same transaction (mirrors RecordResourceSample's insert+prune
// pattern, but time-bounded rather than count-bounded since compressed
// chunk sizes vary with terminal output).
func (s *Store) AppendExecLogChunk(ctx context.Context, execID string, bucketStart time.Time, codec string, data []byte, rawSize int) error {
	if s == nil {
		return nil
	}
	execID = strings.TrimSpace(execID)
	if execID == "" {
		return fmt.Errorf("exec id is required")
	}
	codec = strings.TrimSpace(codec)
	if codec == "" {
		return fmt.Errorf("log chunk codec is required")
	}
	id, err := newID()
	if err != nil {
		return err
	}
	row := ExecLogChunk{
		ID:          id,
		ExecID:      execID,
		BucketStart: bucketStart.UTC(),
		Codec:       codec,
		Data:        append([]byte{}, data...),
		RawSize:     rawSize,
		CreatedAt:   time.Now().UTC(),
	}
	cutoff := time.Now().UTC().Add(-defaultLogRetention)
	return s.write.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		return tx.Where("exec_id = ? AND bucket_start < ?", execID, cutoff).Delete(&ExecLogChunk{}).Error
	})
}

// ListExecLogChunks returns an exec's transcript chunks ordered oldest first,
// as execs.LogChunk so the execs package (which cannot import store, since
// store already imports execs) can decompress and parse them without knowing
// about the sqlite row shape.
func (s *Store) ListExecLogChunks(ctx context.Context, execID string) ([]execs.LogChunk, error) {
	if s == nil {
		return nil, nil
	}
	var rows []ExecLogChunk
	if err := s.read.WithContext(ctx).
		Where("exec_id = ?", strings.TrimSpace(execID)).
		Order("bucket_start ASC, created_at ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]execs.LogChunk, 0, len(rows))
	for _, row := range rows {
		out = append(out, execs.LogChunk{
			BucketStart: row.BucketStart,
			Codec:       row.Codec,
			Data:        append([]byte{}, row.Data...),
			RawSize:     row.RawSize,
		})
	}
	return out, nil
}

// DeleteExecLog hard-deletes every transcript chunk for an exec. Called from
// Manager.Delete so a deleted exec's transcript does not outlive it.
func (s *Store) DeleteExecLog(ctx context.Context, execID string) error {
	if s == nil {
		return nil
	}
	execID = strings.TrimSpace(execID)
	if execID == "" {
		return nil
	}
	return s.write.WithContext(ctx).Where("exec_id = ?", execID).Delete(&ExecLogChunk{}).Error
}

func createExecEventTx(tx *gorm.DB, execID, typ, message string, details map[string]any) error {
	detailsJSON, err := json.Marshal(details)
	if err != nil {
		return err
	}
	id, err := newID()
	if err != nil {
		return err
	}
	return tx.Create(&ExecEvent{
		ID:        id,
		ExecID:    execID,
		Type:      typ,
		Message:   message,
		Details:   detailsJSON,
		CreatedAt: time.Now().UTC(),
	}).Error
}

func execDetails(e execs.Exec) map[string]any {
	return map[string]any{
		"status":   e.Status,
		"unit":     e.Unit,
		"pid":      e.PID,
		"exitCode": e.ExitCode,
		"error":    e.Error,
	}
}

func newID() (string, error) {
	return id.New(id.PrefixEvent)
}
