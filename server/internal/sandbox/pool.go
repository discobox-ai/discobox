package sandbox

import (
	"context"

	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	"github.com/discobox-ai/discobox/server/internal/model"
)

// PoolManager is the control-plane surface pool-backed providers need.
// Providers own runtime mechanics; the manager owns persistence, credentials,
// and lifecycle intent. A pool is its own runtime host (ADR-0006).
type PoolManager interface {
	GetPool(ctx context.Context, projectID, poolID string) (*model.Pool, error)
	ListPoolsForProviderInstance(ctx context.Context, projectID, providerID string) ([]model.Pool, error)
	// ListPools returns every pool in the project, across provider instances.
	// pool-sync needs this wider set: a pool agent reaps by scanning
	// project-scoped host trees, so anything narrower would report another
	// provider instance's live pools as orphans.
	ListPools(ctx context.Context, projectID string) ([]model.Pool, error)
	// SchedulablePoolForSandbox gates placement: the sandbox's pool must be
	// ready, schedulable, and fit the request within its reported capacity.
	SchedulablePoolForSandbox(ctx context.Context, sandbox *model.Sandbox) (*model.Pool, error)
	GetProject(ctx context.Context, projectID string) (*model.Project, error)
	GetSandboxProviderInstance(ctx context.Context, projectID, providerID string) (*model.SandboxProviderInstance, error)
	CountSandboxesForPool(ctx context.Context, projectID, poolID string) (int64, error)
	CreatePoolBootstrapToken(ctx context.Context, token *model.PoolBootstrapToken) error
	EnsureAgentTrustKey(ctx context.Context) (string, error)
	CreateAgentToken(ctx context.Context, claims poolagentauth.TokenClaims) (string, error)
	CreateSandboxAgentToken(ctx context.Context, claims poolagentauth.TokenClaims) (string, error)
	SchedulePoolReconciliation(ctx context.Context, projectID, poolID string) error
	// SchedulePoolRepair re-drives a failed pool as new intent (generation bump
	// plus dirty mark), so schedulers can tell a pending retry from a settled
	// failure.
	SchedulePoolRepair(ctx context.Context, poolID, reason string) error
}
