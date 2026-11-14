package sandbox

import "context"

type boxKeptReporterKey struct{}

// WithBoxKeptReporter has a pool reconcile hear, through report, when its
// provider keeps the box a pool's host last provisioned rather than the one
// this server would place now. A provider does that on purpose — a running
// pool is never taken down over its bundles (ADR 26-10-24-953 §5) — and the
// reconcile still settles cleanly, so this is the only way the server learns
// that the pool still runs its previous agent (ADR 26-11-14-624 §1).
func WithBoxKeptReporter(ctx context.Context, report func(reason error)) context.Context {
	return context.WithValue(ctx, boxKeptReporterKey{}, report)
}

// ReportBoxKept tells the reconcile in ctx, if any, that the pool's host keeps
// its previous box, and why.
func ReportBoxKept(ctx context.Context, reason error) {
	if report, _ := ctx.Value(boxKeptReporterKey{}).(func(error)); report != nil {
		report(reason)
	}
}
