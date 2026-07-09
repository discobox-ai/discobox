package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/obot-platform/discobox/gormdb"
	"github.com/obot-platform/discobox/sandbox-agent/execs"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const DefaultDBPath = "/var/lib/discobox/sandbox-agent.db"

type Store struct {
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

type AgentHookRecord struct {
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
	s := &Store{write: pools.Write, read: pools.Read}
	if err := s.write.WithContext(ctx).AutoMigrate(&AgentState{}, &ResourceSnapshot{}, &AgentHookLog{}, &ExecState{}, &ExecEvent{}, &ExecRecord{}); err != nil {
		return nil, fmt.Errorf("migrate sandbox-agent store: %w", err)
	}
	return s, nil
}

const primaryTerminalLaunchedKey = "primary_terminal_launched"

// PrimaryTerminalLaunched reports whether the sandbox-agent has launched the
// primary terminal in a previous sandbox start. It is used to decide between
// running the agent with the initial prompt (first start) and resuming the
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
		AgentID:   current.Metadata["agentId"],
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

func (s *Store) RecordAgentHook(ctx context.Context, record AgentHookRecord) (AgentHookRecord, error) {
	if s == nil {
		return record, nil
	}
	record.Provider = strings.TrimSpace(record.Provider)
	if record.Provider == "" {
		return AgentHookRecord{}, fmt.Errorf("hook provider is required")
	}
	record.Event = strings.TrimSpace(record.Event)
	if record.Event == "" {
		return AgentHookRecord{}, fmt.Errorf("hook event is required")
	}
	if len(record.Payload) == 0 {
		record.Payload = json.RawMessage(`{}`)
	}
	if !json.Valid(record.Payload) {
		return AgentHookRecord{}, fmt.Errorf("hook payload must be valid JSON")
	}
	id, err := newID()
	if err != nil {
		return AgentHookRecord{}, err
	}
	record.ID = id
	record.TerminalID = strings.TrimSpace(record.TerminalID)
	record.CreatedAt = time.Now().UTC()
	row := AgentHookLog{
		ID:         record.ID,
		TerminalID: record.TerminalID,
		Provider:   record.Provider,
		Event:      record.Event,
		Payload:    append([]byte{}, record.Payload...),
		CreatedAt:  record.CreatedAt,
	}
	return record, s.write.WithContext(ctx).Create(&row).Error
}

func (s *Store) ListAgentHooks(ctx context.Context, terminalID string, limit int) ([]AgentHookRecord, error) {
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
	var rows []AgentHookLog
	if err := query.Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]AgentHookRecord, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		row := rows[i]
		payload := json.RawMessage(append([]byte{}, row.Payload...))
		if len(payload) == 0 || !json.Valid(payload) {
			payload = json.RawMessage(`{}`)
		}
		out = append(out, AgentHookRecord{
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
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return "evt_" + hex.EncodeToString(data[:]), nil
}
