package sandbox

import (
	"context"
	"time"
)

// AgentStartKind is what a pool reconcile's provider started: a box no agent
// ran in, or the agent in a box whose old agent was still running.
type AgentStartKind int

const (
	// AgentStartBox is a box the provider made, or started from stopped:
	// no agent ran in it before at, so any report after it is the new
	// agent's (ADR 26-11-15-294 §1).
	AgentStartBox AgentStartKind = iota + 1
	// AgentStartInPlace is a running box whose agent the provider may now
	// restart or hand over from (the apply and the box's units after it). at is just
	// before the first step that can restart it, so the old agent may still
	// report after it; only the new agent's marked first report counts from
	// it (ADR 26-11-15-602 §§1, 3).
	AgentStartInPlace
)

type agentStartReporterKey struct{}

// WithAgentStartReporter has a pool reconcile hear, through report, each time
// its provider makes or starts the pool's box, or is about to restart the
// pool's agent in it, and which of the two (AgentStartKind). The upgrade
// watch counts a ready report after a box start, or a ready first report
// after an in-place one, as the new agent's; the settle, which the pass that
// started the agent often is not, would reject the new agent's first report.
func WithAgentStartReporter(ctx context.Context, report func(at time.Time, kind AgentStartKind)) context.Context {
	return context.WithValue(ctx, agentStartReporterKey{}, report)
}

// ReportAgentStarted tells the reconcile in ctx, if any, that the pool's agent
// was started at at, in the sense kind says.
func ReportAgentStarted(ctx context.Context, at time.Time, kind AgentStartKind) {
	if report, _ := ctx.Value(agentStartReporterKey{}).(func(time.Time, AgentStartKind)); report != nil {
		report(at, kind)
	}
}
