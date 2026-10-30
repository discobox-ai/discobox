package model

import (
	"time"
)

// Sandbox event types (ADR 26-10-29-316 §1). Every event carries the
// sandbox's whole state as a SandboxEventData, so a sandbox's latest event is
// all a client needs to know about it.
const (
	SandboxEventCreated = "sandbox.created"
	SandboxEventUpdated = "sandbox.updated"
	SandboxEventDeleted = "sandbox.deleted"
)

// SandboxEvent is one change of a sandbox's state, written by the store in the
// transaction that changed it (ADR 26-10-29-316 §2).
//
// Seq is the event's ID on the stream. It is taken from SandboxEventLog's
// counter in the writing transaction, never from an autoincrement key, so the
// order of Seq is the order the events committed in (§3): a reader that has
// seen one has seen every event before it.
type SandboxEvent struct {
	Seq       int64     `gorm:"column:seq;primaryKey;autoIncrement:false;index:idx_sandbox_event_project_seq,priority:2"`
	ProjectID string    `gorm:"column:project_id;not null;type:text;index:idx_sandbox_event_project_seq,priority:1"`
	SandboxID string    `gorm:"column:sandbox_id;not null;type:text;index"`
	Type      string    `gorm:"column:type;not null;type:text"`
	Data      RawJSON   `gorm:"column:data;not null;type:text"`
	At        time.Time `gorm:"column:at;not null;index"`
}

func (SandboxEvent) TableName() string { return "sandbox_events" }

// SandboxEventData is an event's data: the state a sandbox's events report,
// and nothing else (ADR 26-10-29-316 §1). Adding a field changes the
// contract, `SandboxEventData` in server.yaml.
type SandboxEventData struct {
	ProjectID    string    `json:"projectId"`
	SandboxID    string    `json:"sandboxId"`
	Name         string    `json:"name"`
	At           time.Time `json:"at"`
	DesiredState string    `json:"desiredState"`
	State        string    `json:"state"`
	RuntimeState string    `json:"runtimeState,omitempty"`
	// Program is the primary session's program status, present only while
	// the sandbox is running.
	Program *SandboxEventProgram `json:"program,omitempty"`
}

// SandboxEventProgram is the part of the primary session's OSC 7501 status an
// event reports: what the program is doing, not what it says about it.
type SandboxEventProgram struct {
	State string `json:"state"`
	Kind  string `json:"kind,omitempty"`
	App   string `json:"app,omitempty"`
}

// SameState reports whether two event data hold the same state, whenever each
// was taken.
func (d SandboxEventData) SameState(other SandboxEventData) bool {
	d.At, other.At = time.Time{}, time.Time{}
	if (d.Program == nil) != (other.Program == nil) {
		return false
	}
	if d.Program != nil && *d.Program != *other.Program {
		return false
	}
	d.Program, other.Program = nil, nil
	return d == other
}

// SandboxEventLogID is the one SandboxEventLog row.
const SandboxEventLogID = 1

// SandboxEventLog is the event stream's one row of bookkeeping: Head is the
// last Seq handed out, and PrunedThrough the highest Seq retention has
// removed (ADR 26-10-29-316 §§3–4). A stream resumed from before
// PrunedThrough may have missed an event, and is told to start again.
type SandboxEventLog struct {
	ID            int   `gorm:"column:id;primaryKey;autoIncrement:false"`
	Head          int64 `gorm:"column:head;not null;default:0"`
	PrunedThrough int64 `gorm:"column:pruned_through;not null;default:0"`
}

func (SandboxEventLog) TableName() string { return "sandbox_event_log" }
