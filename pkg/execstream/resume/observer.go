package resume

import (
	"context"
	"time"
)

// ActionPhase identifies one locally observable step in a resumable action's
// lifecycle. The host acknowledgement is emitted only after the action has
// been applied, so Accepted-to-Acknowledged is the client-observed apply
// round-trip rather than merely a socket-write duration.
type ActionPhase string

const (
	ActionAccepted      ActionPhase = "accepted"
	ActionPhysicalWrite ActionPhase = "physical_write"
	ActionRetransmitted ActionPhase = "retransmitted"
	ActionAcknowledged  ActionPhase = "acknowledged"
)

// ActionEvent is a profiling annotation for one positioned action.
//
// At and Duration use the observing process's monotonic clock. Duration is set
// for physical writes and retransmissions; Accepted-to-Acknowledged can be
// calculated by matching Position. PendingBytes is the retained,
// unacknowledged window after the event's state transition.
type ActionEvent struct {
	At           time.Time
	Phase        ActionPhase
	Position     uint64
	Type         byte
	PayloadBytes int
	PendingBytes int
	Duration     time.Duration
	Err          error
}

// Observer receives resumable-action profiling annotations. Calls are
// synchronous on the stream's read or write goroutine, never while Conn.mu is
// held. Implementations must return promptly and must not call back into the
// observed Conn.
//
// Observers are deliberately opt-in: normal terminal input pays no clock,
// allocation, export, or per-key tracing cost.
type Observer interface {
	ObserveAction(ActionEvent)
}

// ObserverFunc adapts a function to Observer.
type ObserverFunc func(ActionEvent)

// ObserveAction implements Observer.
func (f ObserverFunc) ObserveAction(event ActionEvent) { f(event) }

type observerContextKey struct{}

// WithObserver enables resumable-action annotations for connections created
// from the returned context. This is intended for diagnostics and performance
// harnesses; it does not alter the wire protocol.
func WithObserver(ctx context.Context, observer Observer) context.Context {
	if observer == nil {
		return ctx
	}
	return context.WithValue(ctx, observerContextKey{}, observer)
}

func observerFromContext(ctx context.Context) Observer {
	observer, _ := ctx.Value(observerContextKey{}).(Observer)
	return observer
}
