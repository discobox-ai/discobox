// Package models contains sessions database models.
package models

import (
	"time"

	"github.com/obot-platform/discobox/id"
	"gorm.io/gorm"
)

type Status string

const (
	StatusStarting   Status = "starting"
	StatusRunning    Status = "running"
	StatusTerminated Status = "terminated"
	StatusLost       Status = "lost"
)

type CodingSession struct {
	ID               string     `gorm:"primaryKey" json:"id"`
	AgentID          string     `gorm:"index" json:"agent_id"`
	Command          []byte     `gorm:"type:json" json:"-"`
	Workdir          string     `json:"workdir"`
	Status           string     `gorm:"index" json:"status"`
	PID              int        `json:"pid,omitempty"`
	SupervisorPID    int        `json:"supervisor_pid,omitempty"`
	SupervisorSocket string     `json:"supervisor_socket,omitempty"`
	SupervisorToken  string     `json:"-"`
	RuntimePath      string     `json:"runtime_path,omitempty"`
	ExitCode         *int       `json:"exit_code,omitempty"`
	Error            string     `json:"error,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	ExitedAt         *time.Time `json:"exited_at,omitempty"`
}

func (CodingSession) TableName() string { return "coding_sessions" }

func (s *CodingSession) BeforeCreate(*gorm.DB) error {
	if s.ID == "" {
		generated, err := id.New()
		if err != nil {
			return err
		}
		s.ID = generated
	}
	return nil
}
