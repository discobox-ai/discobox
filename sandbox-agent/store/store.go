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
	"github.com/obot-platform/discobox/sandbox-agent/terminal"
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

func Open(ctx context.Context, dsn string) (*Store, error) {
	if strings.TrimSpace(dsn) == "" {
		dsn = DefaultDBPath
	}
	pools, err := gormdb.Open(gormdb.Config{DSN: dsn})
	if err != nil {
		return nil, err
	}
	s := &Store{write: pools.Write, read: pools.Read}
	if err := s.write.WithContext(ctx).AutoMigrate(&TerminalState{}, &TerminalEvent{}, &ResourceSnapshot{}); err != nil {
		return nil, fmt.Errorf("migrate sandbox-agent store: %w", err)
	}
	return s, nil
}

func (s *Store) RecordEvent(ctx context.Context, terminalID, typ, message string, details map[string]any) error {
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
	return s.write.WithContext(ctx).Create(&TerminalEvent{
		ID:         id,
		TerminalID: strings.TrimSpace(terminalID),
		Type:       typ,
		Message:    strings.TrimSpace(message),
		Details:    detailsJSON,
		CreatedAt:  time.Now().UTC(),
	}).Error
}

func (s *Store) ObserveTerminal(ctx context.Context, current terminal.Terminal) error {
	if s == nil || current.ID == "" {
		return nil
	}
	now := time.Now().UTC()
	next := TerminalState{
		TerminalID: current.ID,
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
		var previous TerminalState
		hadPrevious := tx.First(&previous, "terminal_id = ?", current.ID).Error == nil
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "terminal_id"}},
			UpdateAll: true,
		}).Create(&next).Error; err != nil {
			return err
		}
		if !hadPrevious {
			return createEventTx(tx, current.ID, "terminal.observed", "terminal observed", terminalDetails(current))
		}
		if previous.Status != next.Status {
			if err := createEventTx(tx, current.ID, "terminal.status.changed", "terminal status changed", map[string]any{
				"from": previous.Status,
				"to":   next.Status,
			}); err != nil {
				return err
			}
		}
		if previous.ExitedAt == nil && next.ExitedAt != nil {
			return createEventTx(tx, current.ID, "terminal.exited", "terminal exited", terminalDetails(current))
		}
		return nil
	})
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
		query = query.Where("terminal_id = ?", terminalID)
	}
	var rows []TerminalEvent
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
			TerminalID: row.TerminalID,
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

func createEventTx(tx *gorm.DB, terminalID, typ, message string, details map[string]any) error {
	detailsJSON, err := json.Marshal(details)
	if err != nil {
		return err
	}
	id, err := newID()
	if err != nil {
		return err
	}
	return tx.Create(&TerminalEvent{
		ID:         id,
		TerminalID: terminalID,
		Type:       typ,
		Message:    message,
		Details:    detailsJSON,
		CreatedAt:  time.Now().UTC(),
	}).Error
}

func terminalDetails(t terminal.Terminal) map[string]any {
	return map[string]any{
		"status":   t.Status,
		"unit":     t.Unit,
		"pid":      t.PID,
		"exitCode": t.ExitCode,
		"error":    t.Error,
	}
}

func newID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return "evt_" + hex.EncodeToString(data[:]), nil
}
