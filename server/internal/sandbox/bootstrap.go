package sandbox

import (
	"context"

	"github.com/discobox-ai/discobox/server/internal/model"
)

type bootstrapReporterKey struct{}
type bootstrapReporter func(context.Context, model.BootstrapEvidence) (*model.PoolBootstrap, error)

// WithBootstrapExecutor binds all reports to the authoritative attempt read by
// the reconciler. This works before an agent or heartbeat exists.
func WithBootstrapExecutor(ctx context.Context, manager PoolManager, pool *model.Pool) context.Context {
	b := pool.BootstrapFacts()
	if b == nil || pool.RegisteredAt != nil || b.Generation != pool.Generation {
		return ctx
	}
	return context.WithValue(ctx, bootstrapReporterKey{}, bootstrapReporter(func(ctx context.Context, e model.BootstrapEvidence) (*model.PoolBootstrap, error) {
		return manager.ReportPoolBootstrap(ctx, pool.ID, b.Attempt, b.Generation, e)
	}))
}

func ReportBootstrap(ctx context.Context, e model.BootstrapEvidence) (*model.PoolBootstrap, error) {
	reporter, _ := ctx.Value(bootstrapReporterKey{}).(bootstrapReporter)
	if reporter == nil {
		return nil, nil
	}
	return reporter(ctx, e)
}
