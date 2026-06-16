package vm

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/obot-platform/discobox/id"
	"github.com/obot-platform/discobox/model"
	"github.com/obot-platform/discobox/orchestration"
)

const defaultWorkerBootstrapTTL = 30 * time.Minute
const defaultWorkerRegistrationTimeout = 2 * time.Minute

var workerRegistrationTimeout = defaultWorkerRegistrationTimeout

// WorkerStore is the persistence surface a VM worker pool needs from the
// control plane. Providers own the scaling policy; the store owns persistence.
type WorkerStore interface {
	ListWorkers(ctx context.Context, projectID, providerID string) ([]model.Worker, error)
	CreateWorker(ctx context.Context, worker *model.Worker) (*model.Worker, error)
	FindSchedulableWorker(ctx context.Context, sandbox *model.Sandbox) (*model.Worker, error)
}

type WorkerBootstrapStore interface {
	CreateWorkerBootstrapToken(ctx context.Context, token *model.WorkerBootstrapToken) error
}

type WorkerLifecycleRepairStore interface {
	GetJob(ctx context.Context, id string) (*orchestration.Job, error)
	MarkWorkerFailedForJob(ctx context.Context, workerID string, generation int64, jobID string, message string) (bool, error)
	MarkWorkerRegistrationExpired(ctx context.Context, workerID string, generation int64, cutoff time.Time, message string) (bool, error)
}

// WorkerLauncher starts the provider-specific runtime node for a persisted worker.
type WorkerLauncher func(ctx context.Context, project *model.Project, provider *model.SandboxProviderInstance, worker *model.Worker, token string) error

// WorkerPoolConfig describes VM worker pool bounds.
type WorkerPoolConfig struct {
	// Min is the minimum number of active worker records/VMs to keep around.
	Min int
	// Max is the maximum number of active worker records/VMs to allow.
	Max int
	// MinHealthy is the minimum number of ready, schedulable, non-degraded workers.
	MinHealthy int
}

// NormalizeWorkerPoolConfig returns a bounded worker-pool config. legacyPoolSize
// preserves the old poolSize behavior as the minimum active pool size while
// allowing one extra replacement node by default when a worker becomes degraded.
func NormalizeWorkerPoolConfig(legacyPoolSize, minWorkers, maxWorkers, minHealthyWorkers int) WorkerPoolConfig {
	if minWorkers <= 0 && maxWorkers <= 0 && minHealthyWorkers <= 0 {
		if legacyPoolSize <= 0 {
			legacyPoolSize = 1
		}
		minWorkers = legacyPoolSize
		minHealthyWorkers = 1
		maxWorkers = legacyPoolSize + minHealthyWorkers
	}
	if minWorkers < 0 {
		minWorkers = 0
	}
	if minHealthyWorkers < 0 {
		minHealthyWorkers = 0
	}
	if minWorkers == 0 && maxWorkers == 0 && minHealthyWorkers == 0 {
		minWorkers = 1
		minHealthyWorkers = 1
	}
	if minHealthyWorkers == 0 && minWorkers > 0 {
		minHealthyWorkers = 1
	}
	if maxWorkers <= 0 {
		maxWorkers = minWorkers + minHealthyWorkers
	}
	if maxWorkers < minWorkers {
		maxWorkers = minWorkers
	}
	if minHealthyWorkers > maxWorkers {
		minHealthyWorkers = maxWorkers
	}
	return WorkerPoolConfig{Min: minWorkers, Max: maxWorkers, MinHealthy: minHealthyWorkers}
}

// DesiredAdditionalWorkers returns how many workers should be launched to bring
// the pool back inside its configured bounds. Degraded workers still count as
// active/schedulable capacity, but not as healthy capacity, so a degraded only
// worker causes a replacement launch when Max allows it.
func DesiredAdditionalWorkers(workers []model.Worker, cfg WorkerPoolConfig) int {
	active := 0
	healthy := 0
	for i := range workers {
		worker := &workers[i]
		if !activeWorker(worker) {
			continue
		}
		active++
		if healthyWorker(worker) {
			healthy++
		}
	}

	target := cfg.Min
	if active > target {
		target = active
	}
	if deficit := cfg.MinHealthy - healthy; deficit > 0 {
		if withHealthyReplacement := active + deficit; withHealthyReplacement > target {
			target = withHealthyReplacement
		}
	}
	if target > cfg.Max {
		target = cfg.Max
	}
	if target <= active {
		return 0
	}
	return target - active
}

// EnsureWorkerPool reconciles a VM provider's warm worker pool.
func EnsureWorkerPool(ctx context.Context, store WorkerStore, project *model.Project, provider *model.SandboxProviderInstance, cfg WorkerPoolConfig) error {
	if store == nil {
		return fmt.Errorf("worker store is required")
	}
	if project == nil {
		return fmt.Errorf("project is required")
	}
	if provider == nil || provider.Disabled {
		return nil
	}
	workers, err := store.ListWorkers(ctx, provider.ProjectID, provider.ID)
	if err != nil {
		return err
	}
	workers, err = repairWorkersWithFailedJobs(ctx, store, workers)
	if err != nil {
		return err
	}
	workers, err = repairExpiredRegisteringWorkers(ctx, store, workers, time.Now().UTC())
	if err != nil {
		return err
	}
	additional := DesiredAdditionalWorkers(workers, cfg)
	for i := 0; i < additional; i++ {
		worker, err := createWorker(ctx, store, project, provider, len(workers)+1)
		if err != nil {
			return err
		}
		workers = append(workers, *worker)
	}
	return nil
}

func repairWorkersWithFailedJobs(ctx context.Context, store WorkerStore, workers []model.Worker) ([]model.Worker, error) {
	repairStore, ok := store.(WorkerLifecycleRepairStore)
	if !ok {
		return workers, nil
	}
	for i := range workers {
		worker := &workers[i]
		if worker.LastJobID == nil || worker.LastOperationStatus == model.OperationStatusFailed || worker.LastOperationStatus == model.OperationStatusSuccess {
			continue
		}
		job, err := repairStore.GetJob(ctx, *worker.LastJobID)
		if err != nil {
			if errors.Is(err, orchestration.ErrJobNotFound) {
				continue
			}
			return nil, err
		}
		if job.Status != orchestration.StatusFailed {
			continue
		}
		message := "worker reconcile job failed"
		if job.Error != nil && *job.Error != "" {
			message = *job.Error
		}
		updated, err := repairStore.MarkWorkerFailedForJob(ctx, worker.ID, worker.Generation, *worker.LastJobID, message)
		if err != nil {
			return nil, err
		}
		if updated {
			worker.FailOperation(message)
		}
	}
	return workers, nil
}

func repairExpiredRegisteringWorkers(ctx context.Context, store WorkerStore, workers []model.Worker, now time.Time) ([]model.Worker, error) {
	repairStore, ok := store.(WorkerLifecycleRepairStore)
	if !ok || workerRegistrationTimeout <= 0 {
		return workers, nil
	}
	cutoff := now.Add(-workerRegistrationTimeout)
	for i := range workers {
		worker := &workers[i]
		if worker.Phase != model.WorkerPhaseRegistering ||
			worker.LastOperationStatus != model.OperationStatusSuccess ||
			worker.RegisteredAt != nil ||
			worker.LastSeenAt != nil ||
			!worker.UpdatedAt.Before(cutoff) {
			continue
		}
		message := "worker did not register before timeout"
		updated, err := repairStore.MarkWorkerRegistrationExpired(ctx, worker.ID, worker.Generation, cutoff, message)
		if err != nil {
			return nil, err
		}
		if updated {
			worker.FailOperation(message)
		}
	}
	return workers, nil
}

func activeWorker(worker *model.Worker) bool {
	if worker == nil || worker.RevokedAt != nil {
		return false
	}
	if worker.Phase == model.WorkerPhaseFailed || worker.LastOperationStatus == model.OperationStatusFailed {
		return false
	}
	switch worker.DesiredState {
	case model.WorkerDesiredStateDeleted, model.WorkerDesiredStateDrained:
		return false
	default:
		return true
	}
}

func healthyWorker(worker *model.Worker) bool {
	return activeWorker(worker) && worker.Ready && worker.Schedulable && !worker.Degraded
}

func createWorker(ctx context.Context, store WorkerStore, project *model.Project, provider *model.SandboxProviderInstance, ordinal int) (*model.Worker, error) {
	workerID, err := id.New()
	if err != nil {
		return nil, err
	}
	worker := &model.Worker{
		ID:                 workerID,
		TenantID:           project.TenantID,
		ProjectID:          project.ID,
		ProviderInstanceID: provider.ID,
		Identity:           fmt.Sprintf("%s/%s/%d", provider.ID, workerID, ordinal),
	}
	return store.CreateWorker(ctx, worker)
}

func CreateWorkerBootstrap(ctx context.Context, store WorkerBootstrapStore, project *model.Project, worker *model.Worker) (string, error) {
	if store == nil {
		return "", fmt.Errorf("worker store is required")
	}
	if project == nil {
		return "", fmt.Errorf("project is required")
	}
	if worker == nil {
		return "", fmt.Errorf("worker is required")
	}
	token, err := randomWorkerToken()
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256([]byte(token))
	bootstrapToken := &model.WorkerBootstrapToken{
		TenantID:  project.TenantID,
		WorkerID:  worker.ID,
		TokenHash: hash[:],
		ExpiresAt: time.Now().UTC().Add(defaultWorkerBootstrapTTL),
	}
	if err := store.CreateWorkerBootstrapToken(ctx, bootstrapToken); err != nil {
		return "", err
	}
	return token, nil
}

func randomWorkerToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
