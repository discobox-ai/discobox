package workerpool

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/obot-platform/discobox/id"
	"github.com/obot-platform/discobox/orchestration"
	"github.com/obot-platform/discobox/server/internal/model"
)

const defaultWorkerBootstrapTTL = 30 * time.Minute
const defaultWorkerRegistrationTimeout = 2 * time.Minute

var workerRegistrationTimeout = defaultWorkerRegistrationTimeout

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

// desiredAdditionalWorkers returns how many workers should be launched to bring
// the pool back inside its configured bounds. Degraded workers still count as
// active/schedulable capacity, but not as healthy capacity, so a degraded only
// worker causes a replacement launch when Max allows it.
func desiredAdditionalWorkers(workers []model.Worker, cfg WorkerPoolConfig) int {
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

// ensureWorkerPool reconciles a VM provider's worker pool.
func ensureWorkerPool(ctx context.Context, manager WorkerManager, project *model.Project, provider *model.SandboxProviderInstance, cfg WorkerPoolConfig) error {
	if manager == nil {
		return fmt.Errorf("worker manager is required")
	}
	if project == nil {
		return fmt.Errorf("project is required")
	}
	if provider == nil || provider.Disabled {
		return nil
	}
	workers, err := manager.ListWorkers(ctx, provider.ProjectID, provider.ID)
	if err != nil {
		return err
	}
	workers, err = repairWorkersWithFailedJobs(ctx, manager, workers)
	if err != nil {
		return err
	}
	workers, err = repairExpiredRegisteringWorkers(ctx, manager, workers, time.Now().UTC())
	if err != nil {
		return err
	}
	workers, err = deleteExcessWorkers(ctx, manager, workers, cfg)
	if err != nil {
		return err
	}
	additional := desiredAdditionalWorkers(workers, cfg)
	for i := 0; i < additional; i++ {
		worker, err := createWorker(ctx, manager, project, provider, len(workers)+1)
		if err != nil {
			return err
		}
		workers = append(workers, *worker)
	}
	return nil
}

func deleteExcessWorkers(ctx context.Context, manager WorkerManager, workers []model.Worker, cfg WorkerPoolConfig) ([]model.Worker, error) {
	if cfg.Max <= 0 {
		return workers, nil
	}
	candidates := activeWorkersByRetentionPriority(workers)
	excess := len(candidates) - cfg.Max
	if excess <= 0 {
		return workers, nil
	}
	candidateIDs := make([]string, len(candidates))
	for i, worker := range candidates {
		candidateIDs[i] = worker.ID
	}
	assignedCounts, err := manager.CountSandboxesForWorkers(ctx, candidateIDs)
	if err != nil {
		return nil, err
	}
	deleteIDs := make(map[string]struct{}, excess)
	for i := 0; i < len(candidates) && len(deleteIDs) < excess; i++ {
		worker := candidates[len(candidates)-1-i]
		if assignedCounts[worker.ID] > 0 {
			continue
		}
		if _, err := manager.DeleteWorker(ctx, worker.ID); err != nil {
			return nil, err
		}
		deleteIDs[worker.ID] = struct{}{}
	}
	for i := range workers {
		if _, ok := deleteIDs[workers[i].ID]; !ok {
			continue
		}
		workers[i].IncrementGeneration()
		workers[i].BeginOperation(model.WorkerDeleteOperation, nil)
	}
	return workers, nil
}

func activeWorkersByRetentionPriority(workers []model.Worker) []model.Worker {
	active := make([]model.Worker, 0, len(workers))
	for i := range workers {
		if activeWorker(&workers[i]) {
			active = append(active, workers[i])
		}
	}
	sort.SliceStable(active, func(i, j int) bool {
		return retainWorkerBefore(active[i], active[j])
	})
	return active
}

func retainWorkerBefore(left, right model.Worker) bool {
	leftHealthy := healthyWorker(&left)
	rightHealthy := healthyWorker(&right)
	if leftHealthy != rightHealthy {
		return leftHealthy
	}
	if left.Ready != right.Ready {
		return left.Ready
	}
	if left.Schedulable != right.Schedulable {
		return left.Schedulable
	}
	if left.Degraded != right.Degraded {
		return !left.Degraded
	}
	leftSuccess := left.LastOperationStatus == model.OperationStatusSuccess
	rightSuccess := right.LastOperationStatus == model.OperationStatusSuccess
	if leftSuccess != rightSuccess {
		return leftSuccess
	}
	if left.LastSeenAt != nil && right.LastSeenAt != nil && !left.LastSeenAt.Equal(*right.LastSeenAt) {
		return left.LastSeenAt.After(*right.LastSeenAt)
	}
	if left.LastSeenAt != nil && right.LastSeenAt == nil {
		return true
	}
	if left.LastSeenAt == nil && right.LastSeenAt != nil {
		return false
	}
	if !left.CreatedAt.Equal(right.CreatedAt) {
		return left.CreatedAt.After(right.CreatedAt)
	}
	return left.ID < right.ID
}

// repairWorkersWithFailedJobs keeps active workers progressing after a
// reconcile job ends. A worker that never completed its initial create fails
// terminally when that create dies: there is no runtime to recover. An
// already-created worker is stateful, so a failed or lost reconcile re-enqueues
// a fresh reconcile instead of latching the worker to a terminal failure.
func repairWorkersWithFailedJobs(ctx context.Context, manager WorkerManager, workers []model.Worker) ([]model.Worker, error) {
	for i := range workers {
		worker := &workers[i]
		if worker.RevokedAt != nil || worker.DesiredState != model.WorkerDesiredStateActive {
			continue
		}

		switch worker.LastOperationStatus {
		case model.OperationStatusPending, model.OperationStatusRunning:
			// The record shows an operation in progress. Detect a job that died
			// without writing its result back and reconcile the row.
			if worker.LastJobID == nil {
				continue
			}
			job, err := manager.GetJob(ctx, *worker.LastJobID)
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
			if worker.EverCreated() {
				// Stateful worker: recover rather than abandon the runtime.
				if err := manager.ScheduleWorkerReconciliation(ctx, worker.ID); err != nil {
					return nil, err
				}
				continue
			}
			updated, err := manager.MarkWorkerFailedForJob(ctx, worker.ID, worker.Generation, *worker.LastJobID, message)
			if err != nil {
				return nil, err
			}
			if updated {
				worker.Phase = model.WorkerPhaseFailed
				worker.ActiveOperation = nil
				worker.LastOperationStatus = model.OperationStatusFailed
				worker.StatusMessage = nil
				worker.ErrorMessage = &message
			}
		case model.OperationStatusFailed:
			// A created worker that failed a prior reconcile must keep being
			// driven back to health. Re-enqueue only when no reconcile is
			// already queued or running, so retries back off at the pool cadence.
			if !worker.EverCreated() {
				continue
			}
			queued, err := workerReconcileInFlight(ctx, manager, worker)
			if err != nil {
				return nil, err
			}
			if queued {
				continue
			}
			if err := manager.ScheduleWorkerReconciliation(ctx, worker.ID); err != nil {
				return nil, err
			}
		}
	}
	return workers, nil
}

// workerReconcileInFlight reports whether the worker's most recent reconcile
// job is still queued or running, so callers avoid re-enqueuing duplicate work.
func workerReconcileInFlight(ctx context.Context, manager WorkerManager, worker *model.Worker) (bool, error) {
	if worker.LastJobID == nil {
		return false, nil
	}
	job, err := manager.GetJob(ctx, *worker.LastJobID)
	if err != nil {
		if errors.Is(err, orchestration.ErrJobNotFound) {
			return false, nil
		}
		return false, err
	}
	switch job.Status {
	case orchestration.StatusPending, orchestration.StatusBackoff, orchestration.StatusRunning:
		return true, nil
	default:
		return false, nil
	}
}

func repairExpiredRegisteringWorkers(ctx context.Context, manager WorkerManager, workers []model.Worker, now time.Time) ([]model.Worker, error) {
	if workerRegistrationTimeout <= 0 {
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
		updated, err := manager.DeleteWorkerForExpiredRegistration(ctx, worker.ID, worker.Generation, cutoff, message)
		if err != nil {
			return nil, err
		}
		if updated {
			worker.IncrementGeneration()
			worker.BeginOperation(model.WorkerDeleteOperation, nil)
			worker.StatusMessage = &message
		}
	}
	return workers, nil
}

func activeWorker(worker *model.Worker) bool {
	if worker == nil || worker.RevokedAt != nil {
		return false
	}
	switch worker.DesiredState {
	case model.WorkerDesiredStateDeleted, model.WorkerDesiredStateDrained:
		return false
	}
	// A failed reconcile is terminal only for a worker that never completed its
	// initial create. An already-created worker is stateful and stays active
	// while it is reconciled back to health; healthyWorker still excludes it
	// from schedulable capacity via Ready/Schedulable until it recovers.
	if (worker.Phase == model.WorkerPhaseFailed || worker.LastOperationStatus == model.OperationStatusFailed) && !worker.EverCreated() {
		return false
	}
	return true
}

func healthyWorker(worker *model.Worker) bool {
	return activeWorker(worker) && worker.Ready && worker.Schedulable && !worker.Degraded
}

func createWorker(ctx context.Context, manager WorkerManager, project *model.Project, provider *model.SandboxProviderInstance, ordinal int) (*model.Worker, error) {
	workerID, err := id.New()
	if err != nil {
		return nil, err
	}
	worker := &model.Worker{
		ID:                 workerID,
		ProjectID:          project.ID,
		ProviderInstanceID: provider.ID,
		Identity:           fmt.Sprintf("%s/%s/%d", provider.ID, workerID, ordinal),
	}
	return manager.CreateWorker(ctx, worker)
}

func createWorkerBootstrap(ctx context.Context, manager WorkerManager, project *model.Project, worker *model.Worker) (string, error) {
	if manager == nil {
		return "", fmt.Errorf("worker manager is required")
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
		WorkerID:  worker.ID,
		TokenHash: hash[:],
		ExpiresAt: time.Now().UTC().Add(defaultWorkerBootstrapTTL),
	}
	if err := manager.CreateWorkerBootstrapToken(ctx, bootstrapToken); err != nil {
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
