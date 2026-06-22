package jobs_test

import (
	"context"
	"testing"

	"github.com/obot-platform/discobox/orchestration"
	"github.com/obot-platform/discobox/server/internal/resources/workers"
)

type fakeWorkerTerminalHandler struct {
	calls      int
	providerID string
	status     orchestration.Status
}

func (h *fakeWorkerTerminalHandler) OnWorkerReconcileTerminal(_ context.Context, job *orchestration.Job, payload workers.WorkerReconcilePayload) error {
	h.calls++
	h.providerID = payload.ProviderID
	h.status = job.Status
	return nil
}

func TestWorkerReconcileExecutorNotifiesTerminalHandler(t *testing.T) {
	handler := &fakeWorkerTerminalHandler{}
	executor := workers.NewWorkerReconcileExecutor(nil, workers.WithWorkerReconcileTerminalHandler(handler))
	job, err := orchestration.JobFromPayload(workers.WorkerReconcilePayload{
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
