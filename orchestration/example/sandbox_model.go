package main

type SandboxID struct {
	ProjectID string
	SandboxID string
}

type SandboxOperation string

const (
	OperationCreate SandboxOperation = "create"
	OperationStart  SandboxOperation = "start"
	OperationStop   SandboxOperation = "stop"
	OperationDelete SandboxOperation = "delete"
)

type SandboxDesiredState string

const (
	DesiredRunning SandboxDesiredState = "running"
	DesiredStopped SandboxDesiredState = "stopped"
	DesiredDeleted SandboxDesiredState = "deleted"
)

type SandboxPhase string

const (
	PhasePending  SandboxPhase = "pending"
	PhaseStarting SandboxPhase = "starting"
	PhaseStopping SandboxPhase = "stopping"
	PhaseRunning  SandboxPhase = "running"
	PhaseStopped  SandboxPhase = "stopped"
	PhaseDeleted  SandboxPhase = "deleted"
)

type Sandbox struct {
	ProjectID          string
	ID                 string
	DesiredState       SandboxDesiredState
	Phase              SandboxPhase
	Generation         int64
	ObservedGeneration int64
	ActiveOperation    SandboxOperation
	LastJobID          *string
}

func (s *Sandbox) IncrementGeneration() {
	s.Generation++
}

func (s *Sandbox) BeginOperation(operation SandboxOperation, _ *string) {
	s.ActiveOperation = operation
	s.ObservedGeneration = 0

	switch operation {
	case OperationCreate:
		s.DesiredState = DesiredRunning
		s.Phase = PhasePending
	case OperationStart:
		s.DesiredState = DesiredRunning
		s.Phase = PhaseStarting
	case OperationStop:
		s.DesiredState = DesiredStopped
		s.Phase = PhaseStopping
	case OperationDelete:
		s.DesiredState = DesiredDeleted
		// A larger application might expose a distinct deleting phase first. This
		// compact example goes straight to the target phase once reconciled.
		s.Phase = PhaseDeleted
	}
}

func (s *Sandbox) SetLastJobID(jobID *string) {
	s.LastJobID = jobID
}
