package vm

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/obot-platform/disco2/internal/id"
	"github.com/obot-platform/disco2/internal/model"
)

const defaultWorkerBootstrapTTL = 30 * time.Minute

// WorkerStore is the persistence surface a VM worker pool needs from the
// control plane. Providers own the scaling policy; the store owns persistence.
type WorkerStore interface {
	ListWorkers(ctx context.Context, projectID, providerID string) ([]model.Worker, error)
	CreateWorkerWithBootstrapToken(ctx context.Context, worker *model.Worker, token *model.WorkerBootstrapToken) error
	ClaimWorker(ctx context.Context, sandbox *model.Sandbox) (*model.Worker, error)
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
func EnsureWorkerPool(ctx context.Context, store WorkerStore, project *model.Project, provider *model.SandboxProviderInstance, cfg WorkerPoolConfig, launch WorkerLauncher) error {
	if store == nil {
		return fmt.Errorf("worker store is required")
	}
	if project == nil {
		return fmt.Errorf("project is required")
	}
	if provider == nil || provider.Disabled {
		return nil
	}
	if launch == nil {
		return fmt.Errorf("worker launcher is required")
	}
	workers, err := store.ListWorkers(ctx, provider.ProjectID, provider.ID)
	if err != nil {
		return err
	}
	additional := DesiredAdditionalWorkers(workers, cfg)
	for i := 0; i < additional; i++ {
		worker, token, err := createWorker(ctx, store, project, provider, len(workers)+1)
		if err != nil {
			return err
		}
		workers = append(workers, *worker)
		if err := launch(ctx, project, provider, worker, token); err != nil {
			return err
		}
	}
	return nil
}

func activeWorker(worker *model.Worker) bool {
	if worker == nil || worker.RevokedAt != nil {
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

func createWorker(ctx context.Context, store WorkerStore, project *model.Project, provider *model.SandboxProviderInstance, ordinal int) (*model.Worker, string, error) {
	workerID, err := id.New()
	if err != nil {
		return nil, "", err
	}
	token, err := randomWorkerToken()
	if err != nil {
		return nil, "", err
	}
	hash := sha256.Sum256([]byte(token))
	worker := &model.Worker{
		ID:                 workerID,
		TenantID:           project.TenantID,
		ProjectID:          project.ID,
		ProviderInstanceID: provider.ID,
		Identity:           fmt.Sprintf("%s/%s/%d", provider.ID, workerID, ordinal),
		ResourceLifecycle:  model.NewResourceLifecycle(model.WorkerCreateOperation, nil),
	}
	bootstrapToken := &model.WorkerBootstrapToken{
		TenantID:  project.TenantID,
		WorkerID:  workerID,
		TokenHash: hash[:],
		ExpiresAt: time.Now().UTC().Add(defaultWorkerBootstrapTTL),
	}
	if err := store.CreateWorkerWithBootstrapToken(ctx, worker, bootstrapToken); err != nil {
		return nil, "", err
	}
	return worker, token, nil
}

func randomWorkerToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
