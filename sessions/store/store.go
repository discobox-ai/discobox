package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/obot-platform/discobox/gormdb"
	sessions "github.com/obot-platform/discobox/sessions"
	"github.com/obot-platform/discobox/sessions/models"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type Store struct {
	write *gorm.DB
	read  *gorm.DB
	pools *gormdb.Pools
}

type Options struct {
	Path   string
	DSN    string
	Driver gormdb.Driver
	Logger logger.Interface
}

type RuntimeExit struct {
	SessionID string     `json:"sessionId"`
	PID       int        `json:"pid,omitempty"`
	ExitCode  *int       `json:"exitCode,omitempty"`
	Error     string     `json:"error,omitempty"`
	ExitedAt  *time.Time `json:"exitedAt,omitempty"`
}

func Open(ctx context.Context, opts Options) (*Store, error) {
	dsn := opts.DSN
	if dsn == "" {
		if opts.Path == "" {
			return nil, fmt.Errorf("store path or dsn is required")
		}
		dsn = opts.Path
	}
	pools, err := gormdb.Open(gormdb.Config{Driver: opts.Driver, DSN: dsn, Logger: opts.Logger})
	if err != nil {
		return nil, fmt.Errorf("open sessions store db: %w", err)
	}
	s := &Store{write: pools.Write, read: pools.Read, pools: pools}
	if err := s.Migrate(ctx); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Migrate(ctx context.Context) error {
	return s.write.WithContext(ctx).AutoMigrate(&models.CodingSession{})
}

func (s *Store) Close() error {
	if s.pools == nil {
		return nil
	}
	return s.pools.Close()
}

func (s *Store) CreateSession(ctx context.Context, row *models.CodingSession) error {
	return s.write.WithContext(ctx).Create(row).Error
}

func (s *Store) UpdateSession(ctx context.Context, row *models.CodingSession) error {
	return s.write.WithContext(ctx).Save(row).Error
}

func (s *Store) GetSession(ctx context.Context, id string) (*models.CodingSession, error) {
	var row models.CodingSession
	if err := s.read.WithContext(ctx).First(&row, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &row, nil
}

func (s *Store) ListSessions(ctx context.Context) ([]models.CodingSession, error) {
	var rows []models.CodingSession
	if err := s.read.WithContext(ctx).Order("created_at asc, id asc").Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (s *Store) MarkLost(ctx context.Context, row *models.CodingSession, reason string) error {
	if row.Status == string(models.StatusTerminated) || row.Status == string(models.StatusLost) {
		return nil
	}
	row.Status = string(models.StatusLost)
	row.Error = reason
	now := time.Now().UTC()
	row.ExitedAt = &now
	return s.UpdateSession(ctx, row)
}

func RowToSession(row models.CodingSession) sessions.Session {
	var command []string
	_ = json.Unmarshal(row.Command, &command)
	return sessions.Session{
		ID:        row.ID,
		AgentID:   row.AgentID,
		Command:   command,
		Workdir:   row.Workdir,
		PID:       row.PID,
		Running:   row.Status == string(models.StatusRunning) || row.Status == string(models.StatusStarting),
		ExitCode:  row.ExitCode,
		Error:     row.Error,
		CreatedAt: row.CreatedAt,
		ExitedAt:  row.ExitedAt,
	}
}

func EncodeCommand(command []string) []byte {
	data, err := json.Marshal(command)
	if err != nil {
		return nil
	}
	return data
}

func ReadRuntimeExit(path string) (RuntimeExit, bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return RuntimeExit{}, false, nil
	}
	if err != nil {
		return RuntimeExit{}, false, err
	}
	var out RuntimeExit
	if err := json.Unmarshal(data, &out); err != nil {
		return RuntimeExit{}, false, err
	}
	return out, true, nil
}

func ApplyRuntimeExit(row *models.CodingSession, exit RuntimeExit) {
	row.Status = string(models.StatusTerminated)
	row.ExitCode = exit.ExitCode
	row.Error = exit.Error
	row.ExitedAt = exit.ExitedAt
	if exit.PID != 0 {
		row.PID = exit.PID
	}
}
