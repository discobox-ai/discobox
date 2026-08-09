package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/obot-platform/discobox/gormdb"
	"github.com/obot-platform/discobox/id"
	"github.com/obot-platform/discobox/sandbox-agent/execs"
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
	ID         string          `json:"id,omitempty"`
	TerminalID string          `json:"terminalId,omitempty"`
	Provider   string          `json:"provider"`
	Event      string          `json:"event"`
	Payload    json.RawMessage `json:"payload"`
	CreatedAt  time.Time       `json:"createdAt"`
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
	return s, nil
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
			exec.StartedAt = state.StartedAt
			exec.ExitedAt = state.ExitedAt
		}
		out = append(out, exec)
	}
	return out, nil
}

func (s *Store) ListEvents(ctx context.Context, terminalID string, limit int) ([]Event, error) {
	if s == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 100
	}
	query := s.read.WithContext(ctx).Order("created_at DESC").Limit(limit)
	if strings.TrimSpace(terminalID) != "" {
		query = query.Where("exec_id = ?", terminalID)
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
	record.CreatedAt = time.Now().UTC()
	row := HarnessHookLog{
		ID:         record.ID,
		TerminalID: record.TerminalID,
		Provider:   record.Provider,
		Event:      record.Event,
		Payload:    append([]byte{}, record.Payload...),
		CreatedAt:  record.CreatedAt,
	}
	return record, s.write.WithContext(ctx).Create(&row).Error
}

func (s *Store) ListHarnessHooks(ctx context.Context, terminalID string, limit int) ([]HarnessHookRecord, error) {
	if s == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 100
	}
	query := s.read.WithContext(ctx).Order("created_at DESC").Limit(limit)
	if strings.TrimSpace(terminalID) != "" {
		query = query.Where("terminal_id = ?", strings.TrimSpace(terminalID))
	}
	var rows []HarnessHookLog
	if err := query.Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]HarnessHookRecord, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		row := rows[i]
		payload := json.RawMessage(append([]byte{}, row.Payload...))
		if len(payload) == 0 || !json.Valid(payload) {
			payload = json.RawMessage(`{}`)
		}
		out = append(out, HarnessHookRecord{
			ID:         row.ID,
			TerminalID: row.TerminalID,
			Provider:   row.Provider,
			Event:      row.Event,
			Payload:    payload,
			CreatedAt:  row.CreatedAt,
		})
	}
	return out, nil
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
