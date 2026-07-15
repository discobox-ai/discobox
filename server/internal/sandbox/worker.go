package sandbox

import (
	"context"
	"time"

	workeragentauth "github.com/obot-platform/discobox/server/internal/auth/workeragent"
	"github.com/obot-platform/discobox/server/internal/model"
)

// WorkerManager is the control-plane surface worker-backed providers need.
// Providers own runtime mechanics and scaling policy; the manager owns
// persistence, credentials, and lifecycle intent.
type WorkerManager interface {
	ListWorkers(ctx context.Context, projectID, providerID string) ([]model.Worker, error)
	GetWorker(ctx context.Context, workerID string) (*model.Worker, error)
	CreateWorker(ctx context.Context, worker *model.Worker) (*model.Worker, error)
	DeleteWorker(ctx context.Context, workerID string) (*model.Worker, error)
	CreateWorkerBootstrapToken(ctx context.Context, token *model.WorkerBootstrapToken) error
	EnsureWorkerAgentTrustKey(ctx context.Context) (string, error)
	CreateWorkerAgentToken(ctx context.Context, claims workeragentauth.TokenClaims) (string, error)
	CreateSandboxAgentToken(ctx context.Context, claims workeragentauth.TokenClaims) (string, error)
	FindSchedulableWorker(ctx context.Context, sandbox *model.Sandbox) (*model.Worker, error)
	GetProject(ctx context.Context, projectID string) (*model.Project, error)
	GetSandboxProviderInstance(ctx context.Context, projectID, providerID string) (*model.SandboxProviderInstance, error)
	CountSandboxesForWorker(ctx context.Context, workerID string) (int64, error)
	CountSandboxesForWorkers(ctx context.Context, workerIDs []string) (map[string]int64, error)
	DeleteWorkerForExpiredRegistration(ctx context.Context, workerID string, generation int64, cutoff time.Time, message string) (bool, error)
	ScheduleWorkerReconciliation(ctx context.Context, workerID string) error
	ScheduleWorkerRepair(ctx context.Context, workerID, reason string) error
	ScheduleWorkerProviderReconciliation(ctx context.Context, projectID, providerID string) error
	ScheduleWorkerProviderReconciliationAt(ctx context.Context, projectID, providerID string, scheduledAt time.Time) error
}
