package model

import (
	"encoding/json"
	"time"

	"github.com/discobox-ai/discobox/pkg/agentstatus"
)

// A project's events are one retained log, streamed over SSE and resumed by
// Last-Event-ID (ADR 26-10-30-868). Every kind of event shares the log, so
// one ID resumes the whole stream; Type's prefix names the kind.
const (
	ProjectEventSandboxCreated = "sandbox.created"
	ProjectEventSandboxUpdated = "sandbox.updated"
	ProjectEventSandboxDeleted = "sandbox.deleted"
)

// ProjectEvent is one row of the log. Its ID is not generated per row: it is
// the counter's next value, taken under the counter's row lock in the writing
// transaction, so events commit in ID order (ADR 26-10-30-868 §5).
//
// A stream reads a project's events in ID order, after an ID or the last N
// before one, so the project's index is (project_id, id).
type ProjectEvent struct {
	ID         int64     `gorm:"primaryKey;autoIncrement:false;index:idx_project_event_log_project,priority:2" json:"id"`
	ProjectID  string    `gorm:"column:project_id;not null;type:text;index:idx_project_event_log_project,priority:1" json:"projectId"`
	Type       string    `gorm:"column:type;not null;type:text" json:"type"`
	ResourceID string    `gorm:"column:resource_id;not null;type:text" json:"resourceId"`
	Data       RawJSON   `gorm:"column:data;not null;type:text" json:"data"`
	At         time.Time `gorm:"column:at;not null;index" json:"at"`
}

func (ProjectEvent) TableName() string { return "project_event_log" }

// ProjectEventCounterID is the counter's one row.
const ProjectEventCounterID = 1

// ProjectEventCounter is the log's single counter row. Value is the last ID
// given out, which is the log's head; PrunedThrough is the last ID retention
// removed, so a Last-Event-ID below it names an event the log no longer holds.
type ProjectEventCounter struct {
	ID            int64 `gorm:"primaryKey;autoIncrement:false"`
	Value         int64 `gorm:"column:value;not null;default:0"`
	PrunedThrough int64 `gorm:"column:pruned_through;not null;default:0"`
}

func (ProjectEventCounter) TableName() string { return "project_event_counter" }

// SandboxProjection is the state of a sandbox a monitor watches, and the
// whole payload of its events (ADR 26-10-30-868 §3). A sandbox event is
// written exactly when this changes, so adding a field to it is a contract
// change: every change to that field becomes an event.
type SandboxProjection struct {
	SandboxID    string             `json:"sandboxId"`
	ProjectID    string             `json:"projectId"`
	Name         string             `json:"name"`
	DesiredState string             `json:"desiredState"`
	State        string             `json:"state"`
	RuntimeState string             `json:"runtimeState,omitempty"`
	Agent        *SandboxEventAgent `json:"agent"`
	ErrorReason  string             `json:"errorReason,omitempty"`
}

// SandboxEventAgent is what the program in the sandbox's primary
// terminal last reported of itself over OSC 7501 (ADR 26-10-16-424).
type SandboxEventAgent struct {
	State string `json:"state" enum:"idle,working,done,blocked,error"`
	Kind  string `json:"kind,omitempty" enum:"permission,question,auth"`
}

// SandboxEvent is a sandbox event's payload: the projection and when the
// change was recorded.
type SandboxEvent struct {
	SandboxProjection
	At time.Time `json:"at"`
}

// EventProjection is the sandbox's projection as its row stands.
//
// Agent is read only while the sandbox runs. A stopped sandbox keeps the last
// status its agent reported, and nothing rewrites it once the agent is gone,
// so a stopped box would otherwise read as still working.
func (s *Sandbox) EventProjection() SandboxProjection {
	projection := SandboxProjection{
		SandboxID:    s.ID,
		ProjectID:    s.ProjectID,
		Name:         s.Name,
		DesiredState: s.DesiredState,
		State:        s.State,
		RuntimeState: s.RuntimeState,
		ErrorReason:  s.ErrorReason,
	}
	if s.RuntimeState == SandboxRuntimeStateRunning {
		projection.Agent = primaryProgramStatus(s.AgentStatus)
	}
	return projection
}

// EncodedEventProjection is EventProjection as the row stores it, to compare
// with what the last event recorded.
func (s *Sandbox) EncodedEventProjection() (string, error) {
	encoded, err := json.Marshal(s.EventProjection())
	return string(encoded), err
}

// primaryProgramStatus is the primary terminal's program, by the rule the
// client's listings read it with (agentstatus.PrimaryProgram), so an event and
// a listing never disagree about a box.
func primaryProgramStatus(agentStatus RawJSON) *SandboxEventAgent {
	program, ok := agentstatus.PrimaryProgram(json.RawMessage(agentStatus))
	if !ok {
		return nil
	}
	return &SandboxEventAgent{State: string(program.State), Kind: string(program.Kind)}
}
