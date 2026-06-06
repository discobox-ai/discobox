package model

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
	DesiredState        string  `gorm:"column:desired_state;not null;type:text;index" json:"desiredState" doc:"Requested steady state for reconciliation" enum:"running,stopped,deleted"`
	Phase               string  `gorm:"not null;type:text;index" json:"phase" doc:"Observed lifecycle phase" enum:"pending,provisioning,starting,running,stopping,stopped,deleting,deleted,failed"`
	ActiveOperation     *string `gorm:"column:active_operation;type:text;index" json:"activeOperation,omitempty" doc:"Current queued or running operation" enum:"create,start,stop,restart,delete"`
	LastOperationStatus string  `gorm:"column:last_operation_status;not null;type:text;index" json:"lastOperationStatus" doc:"Status of the most recent operation" enum:"pending,running,success,failed"`
	LastJobID           *string `gorm:"column:last_job_id;type:text;index" json:"lastJobId,omitempty" doc:"Most recent lifecycle job ID" format:"uuid"`
	Generation          int64   `gorm:"not null;default:0" json:"generation" doc:"Latest desired-state generation"`
	ObservedGeneration  int64   `gorm:"column:observed_generation;not null;default:0" json:"observedGeneration" doc:"Latest generation fully observed by reconciliation"`
	StatusMessage       *string `gorm:"column:status_message;type:text" json:"statusMessage,omitempty" doc:"Human-readable status detail"`
	ErrorMessage        *string `gorm:"column:error_message;type:text" json:"errorMessage,omitempty" doc:"Latest error message"`
}

func NewResourceLifecycle(spec OperationSpec, jobID *string) ResourceLifecycle {
	var lifecycle ResourceLifecycle
	lifecycle.BeginOperation(spec, jobID)
	return lifecycle
}

func (l *ResourceLifecycle) BeginOperation(spec OperationSpec, jobID *string) {
	l.DesiredState = spec.DesiredState
	l.Phase = spec.Phase
	l.ActiveOperation = &spec.Operation
	l.LastOperationStatus = OperationStatusPending
	if jobID != nil {
		l.LastJobID = jobID
	}
	l.StatusMessage = nil
	l.ErrorMessage = nil
}

func (l *ResourceLifecycle) IncrementGeneration() {
	l.Generation++
}

func (l *ResourceLifecycle) SetLastJobID(jobID *string) {
	if jobID != nil {
		l.LastJobID = jobID
	}
}

func (l *ResourceLifecycle) MarkOperationRunning(message *string) {
	l.LastOperationStatus = OperationStatusRunning
	l.StatusMessage = message
	l.ErrorMessage = nil
}

func (l *ResourceLifecycle) CompleteOperation(phase string, message *string) {
	l.Phase = phase
	l.ActiveOperation = nil
	l.LastOperationStatus = OperationStatusSuccess
	l.StatusMessage = message
	l.ErrorMessage = nil
}

func (l *ResourceLifecycle) FailOperation(message string) {
	l.Phase = "failed"
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
		l.Phase = phase
	}
	if l.LastOperationStatus == "" {
		l.LastOperationStatus = OperationStatusPending
	}
}
