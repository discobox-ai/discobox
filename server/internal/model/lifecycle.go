package model

import "time"

const (
	OperationStatusPending = "pending"
	OperationStatusRunning = "running"
	OperationStatusSuccess = "success"
	OperationStatusFailed  = "failed"
)

// OperationSpec describes the resource state update made when an operation is
// accepted and its durable job is queued.
type OperationSpec struct {
	Operation    string
	DesiredState string
	Phase        string
}

// ResourceLifecycle is embedded into resources that are reconciled by queued
// operations. Embedding keeps DB/API fields flat while sharing transition code.
type ResourceLifecycle struct {
	DesiredState        string    `gorm:"column:desired_state;not null;type:text;index" json:"desiredState" doc:"Requested steady state for reconciliation" enum:"running,stopped,deleted"`
	Phase               string    `gorm:"not null;type:text;index" json:"phase" doc:"Observed lifecycle phase" enum:"pending,provisioning,starting,running,stopping,stopped,deleting,deleted,failed"`
	ActiveOperation     *string   `gorm:"column:active_operation;type:text;index" json:"activeOperation,omitempty" doc:"Current queued or running operation" enum:"create,start,stop,restart,delete"`
	LastOperationStatus string    `gorm:"column:last_operation_status;not null;type:text;index" json:"lastOperationStatus" doc:"Status of the most recent operation" enum:"pending,running,success,failed"`
	Generation          int64     `gorm:"not null;default:0" json:"generation" doc:"Latest desired-state generation"`
	ObservedGeneration  int64     `gorm:"column:observed_generation;not null;default:0" json:"observedGeneration" doc:"Latest generation fully observed by reconciliation"`
	StatusMessage       *string   `gorm:"column:status_message;type:text" json:"statusMessage,omitempty" doc:"Human-readable status detail"`
	PhaseChangedAt      time.Time `gorm:"column:phase_changed_at" json:"phaseChangedAt,omitempty" doc:"When Phase last changed to its current value. Anchors how long a resource has been in a phase, for timeouts that must not be reset by unrelated reconciles." format:"date-time"`
	ErrorMessage        *string   `gorm:"column:error_message;type:text" json:"errorMessage,omitempty" doc:"Latest error message"`
}

func NewResourceLifecycle(spec OperationSpec) ResourceLifecycle {
	var lifecycle ResourceLifecycle
	lifecycle.BeginOperation(spec)
	return lifecycle
}

// SetPhase moves the resource to phase, stamping PhaseChangedAt only on an
// actual change.
//
// The guard is the point: a caller that re-asserts the phase it is already in —
// a reconcile that re-parks, re-drives, or simply converges again — must not
// move the anchor. Timeouts derive their deadline from it, so restamping on
// every write would push the deadline out each time anything looked at the
// resource, and it could never expire.
func (l *ResourceLifecycle) SetPhase(phase string) {
	if l.Phase == phase {
		return
	}
	l.Phase = phase
	l.PhaseChangedAt = time.Now().UTC()
}

func (l *ResourceLifecycle) BeginOperation(spec OperationSpec) {
	l.DesiredState = spec.DesiredState
	l.SetPhase(spec.Phase)
	l.ActiveOperation = &spec.Operation
	l.LastOperationStatus = OperationStatusPending
	l.StatusMessage = nil
	l.ErrorMessage = nil
}

func (l *ResourceLifecycle) IncrementGeneration() {
	l.Generation++
}

func (l *ResourceLifecycle) MarkOperationRunning(message *string) {
	l.LastOperationStatus = OperationStatusRunning
	l.StatusMessage = message
	l.ErrorMessage = nil
}

func (l *ResourceLifecycle) CompleteOperation(phase string, message *string) {
	l.SetPhase(phase)
	l.ActiveOperation = nil
	l.LastOperationStatus = OperationStatusSuccess
	l.StatusMessage = message
	l.ErrorMessage = nil
}

func (l *ResourceLifecycle) FailOperation(message string) {
	l.SetPhase("failed")
	l.ActiveOperation = nil
	l.LastOperationStatus = OperationStatusFailed
	l.StatusMessage = nil
	l.ErrorMessage = &message
}

// FailOperationRetryable records an operation failure without moving the
// resource to the terminal "failed" phase. It is used for stateful resources
// that must keep being reconciled toward health after a non-create operation
// fails: the caller supplies a non-terminal phase (e.g. offline) so downstream
// reconcilers continue to re-drive the resource rather than abandon it.
func (l *ResourceLifecycle) FailOperationRetryable(phase string, message string) {
	l.SetPhase(phase)
	l.ActiveOperation = nil
	l.LastOperationStatus = OperationStatusFailed
	l.StatusMessage = nil
	l.ErrorMessage = &message
}

func (l *ResourceLifecycle) SetDefaults(desiredState, phase string) {
	if l.DesiredState == "" {
		l.DesiredState = desiredState
	}
	if l.Phase == "" {
		l.SetPhase(phase)
	}
	if l.LastOperationStatus == "" {
		l.LastOperationStatus = OperationStatusPending
	}
}
