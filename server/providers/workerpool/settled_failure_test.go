package workerpool

import (
	"errors"
	"testing"
	"time"

	"github.com/obot-platform/discobox/server/internal/model"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
)

func failedWorker(id string, generation, observedGeneration int64, message string) model.Worker {
	return model.Worker{
		ID:                 id,
		ProjectID:          "project-1",
		ProviderInstanceID: "provider-1",
		ResourceLifecycle: model.ResourceLifecycle{
			DesiredState:        model.WorkerDesiredStateActive,
			Phase:               model.WorkerPhaseOffline,
			Generation:          generation,
			ObservedGeneration:  observedGeneration,
			LastOperationStatus: model.OperationStatusFailed,
			ErrorMessage:        &message,
		},
	}
}

// TestSettledWorkerFailureReportsAttemptedFailure pins the fail-fast verdict: a
// worker whose latest generation was attempted and failed cannot be waited into
// health, so scheduling reports its recorded cause instead of blocking.
func TestSettledWorkerFailureReportsAttemptedFailure(t *testing.T) {
	workers := []model.Worker{failedWorker("worker-1", 3, 3, "No such image: discobox-systemd:latest")}

	failed := settledWorkerFailure(workers)
	if failed == nil {
		t.Fatal("settled worker failure = nil, want the failed worker")
	}
	if failed.ID != "worker-1" {
		t.Fatalf("settled worker = %q, want worker-1", failed.ID)
	}
}

// TestSettledWorkerFailureIgnoresPendingRepair is the race this whole mechanism
// exists to close: a repair bumps the generation and marks the worker dirty, but
// the reconciler has not run yet, so the row still records the OLD failure.
// Reading that failure as settled would abandon a worker that is about to be
// retried — and, because the scheduler queues that repair itself moments before
// waiting, would defeat repair almost every time.
func TestSettledWorkerFailureIgnoresPendingRepair(t *testing.T) {
	workers := []model.Worker{failedWorker("worker-1", 4, 3, "No such image: discobox-systemd:latest")}

	if failed := settledWorkerFailure(workers); failed != nil {
		t.Fatalf("settled worker failure = %q, want none while a repair is pending", failed.ID)
	}
}

// TestSettledWorkerFailureIgnoresWorkerStillComingUp keeps the verdict off any
// pool that can still produce capacity: a launching worker, or one that
// reconciled successfully but has not reported ready, is progress.
func TestSettledWorkerFailureIgnoresWorkerStillComingUp(t *testing.T) {
	for name, worker := range map[string]model.Worker{
		"launching": {
			ID: "worker-2",
			ResourceLifecycle: model.ResourceLifecycle{
				DesiredState:        model.WorkerDesiredStateActive,
				Phase:               model.WorkerPhaseLaunching,
				LastOperationStatus: model.OperationStatusRunning,
			},
		},
		"registering": {
			ID: "worker-2",
			ResourceLifecycle: model.ResourceLifecycle{
				DesiredState:        model.WorkerDesiredStateActive,
				Phase:               model.WorkerPhaseRegistering,
				LastOperationStatus: model.OperationStatusSuccess,
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			workers := []model.Worker{
				failedWorker("worker-1", 1, 1, "No such image: discobox-systemd:latest"),
				worker,
			}
			if failed := settledWorkerFailure(workers); failed != nil {
				t.Fatalf("settled worker failure = %q, want none while another worker is coming up", failed.ID)
			}
		})
	}
}

// TestSettledWorkerFailureIgnoresEmptyPool keeps an empty pool waiting: the pool
// may still be creating its first worker.
func TestSettledWorkerFailureIgnoresEmptyPool(t *testing.T) {
	if failed := settledWorkerFailure(nil); failed != nil {
		t.Fatalf("settled worker failure = %q, want none for an empty pool", failed.ID)
	}
}

// TestRepairSkipsWorkerWithRepairAlreadyPending keeps repair idempotent: without
// this, every pool reconcile would bump the generation of a worker that is
// already awaiting one, and the worker would look perpetually in flight.
func TestRepairSkipsWorkerWithRepairAlreadyPending(t *testing.T) {
	registeredAt := time.Now().UTC()
	worker := failedWorker("worker-1", 4, 3, "docker daemon unreachable")
	worker.RegisteredAt = &registeredAt
	manager := &repairingWorkerManager{workers: []model.Worker{worker}, repairUpdated: true}

	if _, err := repairWorkersWithFailedJobs(t.Context(), manager, manager.workers); err != nil {
		t.Fatalf("repair workers: %v", err)
	}

	if len(manager.scheduledRepairs) != 0 {
		t.Fatalf("scheduled repairs = %v, want none while a repair is already pending", manager.scheduledRepairs)
	}
}

// TestWorkerFailureUnwrapsToNoCapacity keeps the failure classifiable as a
// capacity error for callers that branch on it, while carrying the cause.
func TestWorkerFailureUnwrapsToNoCapacity(t *testing.T) {
	err := error(&sandbox.WorkerFailure{WorkerID: "worker-1", Message: "No such image"})

	if !errors.Is(err, sandbox.ErrNoSandboxCapacity) {
		t.Fatalf("errors.Is(%v, ErrNoSandboxCapacity) = false, want true", err)
	}
	if got, want := err.Error(), "worker worker-1 failed: No such image"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}
