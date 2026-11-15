package sandbox

import (
	"context"
	"time"
)

type agentStartReporterKey struct{}

// WithAgentStartReporter has a pool reconcile hear, through report, each time
// its provider makes or starts the pool's box, or restarts the pool's agent in
// it. at is the moment after which no status report can come from the agent
// that ran there before: none ran in a box made or started, and a restarted
// one has exited. The upgrade watch counts a ready report after it as the new
// agent's (ADR 26-11-15-294 §1); the settle, which the pass that started the
// agent often is not, would reject the new agent's first report.
func WithAgentStartReporter(ctx context.Context, report func(at time.Time)) context.Context {
	return context.WithValue(ctx, agentStartReporterKey{}, report)
}

// ReportAgentStarted tells the reconcile in ctx, if any, that the pool's agent
// was started at at, in the sense of WithAgentStartReporter.
func ReportAgentStarted(ctx context.Context, at time.Time) {
	if report, _ := ctx.Value(agentStartReporterKey{}).(func(time.Time)); report != nil {
		report(at)
	}
}
