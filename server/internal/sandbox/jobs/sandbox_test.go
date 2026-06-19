package jobs_test

import (
	"context"
	"errors"
	"testing"

	"github.com/obot-platform/discobox/orchestration"
	"github.com/obot-platform/discobox/server/internal/sandbox/jobs"
)

type fakeSandboxReconciler struct {
	assertErr      error
	reconcileCalls int
}

type fakeWorkerReconciler struct {
	assertErr      error
	reconcileCalls int
}

type fakeProviderReconciler struct {
	reconcileCalls int
}

type fakeWorkerTerminalHandler struct {
	calls      int
	providerID string
	status     orchestration.Status
}

func (r *fakeSandboxReconciler) AssertSandboxGeneration(context.Context, string, string, int64) error {
	return r.assertErr
}

func (r *fakeSandboxReconciler) ReconcileSandboxJob(context.Context, string, string, string, int64) error {
	r.reconcileCalls++
	return nil
}

func (r *fakeWorkerReconciler) AssertWorkerGeneration(context.Context, string, string, string, int64) error {
	return r.assertErr
}

func (r *fakeWorkerReconciler) ReconcileWorkerJob(context.Context, string, string, string, string, int64) error {
	r.reconcileCalls++
	return nil
}

func (r *fakeProviderReconciler) ReconcileProviderJob(context.Context, string, string, string) error {
	r.reconcileCalls++
	return nil
}

func (h *fakeWorkerTerminalHandler) OnWorkerReconcileTerminal(_ context.Context, job *orchestration.Job, payload jobs.WorkerReconcilePayload) error {
	h.calls++
	h.providerID = payload.ProviderID
	h.status = job.Status
	return nil
}

func TestSandboxReconcileExecutorAssertsGenerationBeforeExecute(t *testing.T) {
	reconciler := &fakeSandboxReconciler{assertErr: orchestration.Superseded("sandbox generation changed")}
	executor := jobs.NewSandboxReconcileExecutor(reconciler)
	job, err := orchestration.JobFromPayload(jobs.SandboxReconcilePayload{
		ProjectID:  "project-1",
		SandboxID:  "sandbox-1",
		Generation: 1,
	}, orchestration.QueueConfig{DefaultMaxAttempts: 1})
	if err != nil {
		t.Fatalf("job from payload: %v", err)
	}
	job.ID = "job-1"

	if err := executor.AssertGeneration(context.Background(), job); !errors.Is(err, orchestration.ErrJobCanceled) {
		t.Fatalf("assert generation error = %v, want ErrJobCanceled", err)
	}
	if reconciler.reconcileCalls != 0 {
		t.Fatalf("reconcile calls = %d, want 0", reconciler.reconcileCalls)
	}
}

func TestWorkerReconcileExecutorAssertsGenerationBeforeExecute(t *testing.T) {
	reconciler := &fakeWorkerReconciler{assertErr: orchestration.Superseded("worker generation changed")}
	executor := jobs.NewWorkerReconcileExecutor(reconciler)
	job, err := orchestration.JobFromPayload(jobs.WorkerReconcilePayload{
		ProjectID:  "project-1",
		ProviderID: "provider-1",
		WorkerID:   "worker-1",
		Generation: 1,
	}, orchestration.QueueConfig{DefaultMaxAttempts: 1})
	if err != nil {
		t.Fatalf("job from payload: %v", err)
	}
	job.ID = "job-1"

	if err := executor.AssertGeneration(context.Background(), job); !errors.Is(err, orchestration.ErrJobCanceled) {
		t.Fatalf("assert generation error = %v, want ErrJobCanceled", err)
	}
	if reconciler.reconcileCalls != 0 {
		t.Fatalf("reconcile calls = %d, want 0", reconciler.reconcileCalls)
	}
}

func TestProviderReconcileExecutorExecutesProviderReconciler(t *testing.T) {
	reconciler := &fakeProviderReconciler{}
	executor := jobs.NewProviderReconcileExecutor(reconciler)
	job, err := orchestration.JobFromPayload(jobs.ProviderReconcilePayload{
		ProjectID:  "project-1",
		ProviderID: "provider-1",
	}, orchestration.QueueConfig{DefaultMaxAttempts: 1})
	if err != nil {
		t.Fatalf("job from payload: %v", err)
	}
	job.ID = "job-1"

	if err := executor.Execute(context.Background(), job); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if reconciler.reconcileCalls != 1 {
		t.Fatalf("reconcile calls = %d, want 1", reconciler.reconcileCalls)
	}
}

func TestWorkerReconcileExecutorNotifiesTerminalHandler(t *testing.T) {
	handler := &fakeWorkerTerminalHandler{}
	executor := jobs.NewWorkerReconcileExecutor(&fakeWorkerReconciler{}, handler)
	job, err := orchestration.JobFromPayload(jobs.WorkerReconcilePayload{
		ProjectID:  "project-1",
		ProviderID: "provider-1",
		WorkerID:   "worker-1",
		Generation: 1,
	}, orchestration.QueueConfig{DefaultMaxAttempts: 1})
	if err != nil {
		t.Fatalf("job from payload: %v", err)
	}
	job.ID = "job-1"
	job.Status = orchestration.StatusFailed

	if err := executor.OnTerminal(context.Background(), job); err != nil {
		t.Fatalf("on terminal: %v", err)
	}
	if handler.calls != 1 || handler.providerID != "provider-1" || handler.status != orchestration.StatusFailed {
		t.Fatalf("handler = calls %d provider %q status %q", handler.calls, handler.providerID, handler.status)
	}
}
