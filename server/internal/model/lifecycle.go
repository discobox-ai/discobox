package model

import "time"

// Desired state answers existence and nothing else (ADR 0017 §9). Power state —
// whether a sandbox is running right now — is not orchestrated: it is observed
// and reported by the runtime, never requested by the control plane.
//
// Existence is three-valued for a sandbox (ADR 0022 §1): `archived` is not a
// power state but a third answer to what form the resource should exist in —
// as data rather than as a runtime.
const (
	DesiredStatePresent = "present"
	// DesiredStateArchived means: exist as data. No container and no runtime
	// resources, but the durable tree is retained so the sandbox can be
	// reinstantiated by asking for `present` again.
	DesiredStateArchived = "archived"
	DesiredStateDeleted  = "deleted"
)

// SandboxDesiredStates and PoolDesiredStates are the desired-state vocabularies
// of the two orchestrated resources. They were one shared slice until ADR 0022
// §1: only a sandbox can be archived. A pool's data is many sandboxes' data, so
// archiving one is a different decision that nothing converges — putting the
// value in the pool's enum just to keep a single slice would advertise a state
// the pool reconciler cannot reach.
//
// The enum tag on ResourceLifecycle.DesiredState is therefore the union, like
// the State tag, rather than the exact vocabulary of any one resource.
var (
	SandboxDesiredStates = []string{
		DesiredStatePresent,
		DesiredStateArchived,
		DesiredStateDeleted,
	}
	PoolDesiredStates = []string{
		DesiredStatePresent,
		DesiredStateDeleted,
	}
)

// ResourceLifecycle is embedded into orchestrated resources. Two of its fields
// are the orchestration contract — Generation and ObservedGeneration, read by
// the reconcile engine and identical for every resource. The rest belong to the
// resource and are read only by its own reconciler and by the API (ADR 0017 §2).
//
// Embedding keeps DB/API fields flat while sharing the transition helpers. It
// is a convenience, not a contract: a resource owes the orchestrator only the
// two counters.
type ResourceLifecycle struct {
	DesiredState       string    `gorm:"column:desired_state;not null;type:text;index;default:''" json:"desiredState" doc:"Requested existence. Power state is not orchestrated (ADR 0017 §9)." enum:"present,archived,deleted"`
	State              string    `gorm:"column:state;not null;type:text;index;default:''" json:"state" doc:"Existence state, written by the resource's reconciler. A sandbox's power state is a separate field (ADR 0034)." enum:"pending,awaiting_source,registering,ready,active,offline,archived,deleted,failed"`
	Generation         int64     `gorm:"not null;default:0" json:"generation" doc:"Latest spec generation"`
	ObservedGeneration int64     `gorm:"column:observed_generation;not null;default:0" json:"observedGeneration" doc:"Latest generation the reconciler has finished acting on"`
	StateChangedAt     time.Time `gorm:"column:state_changed_at" json:"stateChangedAt,omitempty" doc:"When State last changed to its current value. Anchors how long a resource has been in a state, for timeouts that must not be reset by unrelated reconciles." format:"date-time"`
	ErrorMessage       *string   `gorm:"column:error_message;type:text" json:"errorMessage,omitempty" doc:"Error from the generation currently recorded in ObservedGeneration. Cleared by every accepted intent."`
}

// SetState moves the resource to state, stamping StateChangedAt only on an
// actual change.
//
// The guard is the point: a caller that re-asserts the state it is already in —
// a reconcile that re-parks, re-drives, or simply converges again, or a runtime
// report that repeats what we already knew — must not move the anchor. Timeouts
// derive their deadline from it, so restamping on every write would push the
// deadline out each time anything looked at the resource, and it could never
// expire.
func (l *ResourceLifecycle) SetState(state string) {
	if l.State == state {
		return
	}
	l.State = state
	l.StateChangedAt = time.Now().UTC()
}

// RecordIntent accepts new intent: it sets the desired state and clears the
// error, leaving the generation bump to the caller's transaction.
//
// It deliberately does not touch State. Intent says what should be; State says
// what is, and nothing about accepting intent makes an observation stale.
func (l *ResourceLifecycle) RecordIntent(desiredState string) {
	l.DesiredState = desiredState
	l.ErrorMessage = nil
}

func (l *ResourceLifecycle) IncrementGeneration() {
	l.Generation++
}

// Converged reports whether the reconciler has finished acting on the current
// generation. It is the whole of what the orchestrator knows (ADR 0017 §1).
func (l *ResourceLifecycle) Converged() bool {
	return l.Generation == l.ObservedGeneration
}

// RecordFailure settles one intent as failed: the reconciler has done all it
// can, so it records the resulting state and the reason, and its caller
// advances ObservedGeneration (ADR 0017 §§3–4).
//
// The state is the caller's to choose, because failure is not one thing. A
// sandbox that could not be built is `failed`; a pool whose host stopped
// answering is `offline` and expected back. The single terminal phase this
// replaces is what made those two indistinguishable.
func (l *ResourceLifecycle) RecordFailure(state, message string) {
	l.SetState(state)
	l.ErrorMessage = &message
}

func (l *ResourceLifecycle) SetDefaults(desiredState, state string) {
	if l.DesiredState == "" {
		l.DesiredState = desiredState
	}
	if l.State == "" {
		l.SetState(state)
	}
}
