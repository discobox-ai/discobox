package docker

import (
	"context"
	"testing"

	"github.com/moby/moby/api/types/events"

	workeragentauth "github.com/obot-platform/discobox/server/internal/auth/workeragent"
	"github.com/obot-platform/discobox/server/internal/model"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/providers/workerpool/vm"
)

func TestDockerWorkerWatcherSchedulesWorkerReconcileForTerminalContainerEvent(t *testing.T) {
	manager := &recordingWorkerReconcileManager{}
	watcher := dockerWorkerWatcher{
		manager:    manager,
		projectID:  "project-1",
		providerID: "provider-1",
	}

	err := watcher.handleEvent(context.Background(), events.Message{
		Type:   events.ContainerEventType,
		Action: events.ActionDie,
		Actor: events.Actor{
			ID: "1234567890abcdef",
			Attributes: map[string]string{
				labelWorkerID:           "worker-1",
				labelWorkerAgent:        "true",
				labelProviderInstanceID: "provider-1",
			},
		},
	})
	if err != nil {
		t.Fatalf("handle event: %v", err)
	}
	if manager.reconcileWorkerID != "worker-1" {
		t.Fatalf("worker reconcile = %q, want worker-1", manager.reconcileWorkerID)
	}
}

func TestDockerWorkerWatcherIgnoresNonTerminalContainerEvents(t *testing.T) {
	manager := &recordingWorkerReconcileManager{}
	watcher := dockerWorkerWatcher{
		manager:    manager,
		projectID:  "project-1",
		providerID: "provider-1",
	}

	err := watcher.handleEvent(context.Background(), events.Message{
		Type:   events.ContainerEventType,
		Action: events.ActionStart,
		Actor: events.Actor{
			ID: "1234567890abcdef",
			Attributes: map[string]string{
				labelWorkerID: "worker-1",
			},
		},
	})
	if err != nil {
		t.Fatalf("handle event: %v", err)
	}
	if manager.reconcileWorkerID != "" || manager.reconcileProviderID != "" {
		t.Fatalf("unexpected calls: worker reconcile %q provider reconcile %q", manager.reconcileWorkerID, manager.reconcileProviderID)
	}
}

func TestDockerWorkerWatcherSchedulesFailedWorkerWhenCurrentContainerRuns(t *testing.T) {
	manager := &recordingWorkerReconcileManager{}
	watcher := dockerWorkerWatcher{
		manager:    manager,
		projectID:  "project-1",
		providerID: "provider-1",
	}
	worker := &model.Worker{
		ID:                 "worker-1",
		ProjectID:          "project-1",
		ProviderInstanceID: "provider-1",
		ResourceLifecycle: model.ResourceLifecycle{
			DesiredState:        model.WorkerDesiredStateActive,
			Phase:               model.WorkerPhaseFailed,
			LastOperationStatus: model.OperationStatusFailed,
		},
	}
	current := &vm.Instance{ID: "container-1", Status: sandbox.StatusRunning}

	scheduled, err := watcher.checkWorker(context.Background(), worker, current)
	if err != nil {
		t.Fatalf("check worker: %v", err)
	}
	if !scheduled || manager.reconcileWorkerID != "worker-1" {
		t.Fatalf("scheduled = %v worker = %q, want worker-1", scheduled, manager.reconcileWorkerID)
	}
}

func TestDockerWorkerWatcherSchedulesDeletedWorkerWhenContainerRemains(t *testing.T) {
	manager := &recordingWorkerReconcileManager{}
	watcher := dockerWorkerWatcher{
		manager:    manager,
		projectID:  "project-1",
		providerID: "provider-1",
	}
	worker := &model.Worker{
		ID:                 "worker-1",
		ProjectID:          "project-1",
		ProviderInstanceID: "provider-1",
		ResourceLifecycle: model.ResourceLifecycle{
			DesiredState:        model.WorkerDesiredStateDeleted,
			Phase:               model.WorkerPhaseDeleted,
			LastOperationStatus: model.OperationStatusSuccess,
		},
	}
	current := &vm.Instance{ID: "container-1", Status: sandbox.StatusRunning}

	scheduled, err := watcher.checkWorker(context.Background(), worker, current)
	if err != nil {
		t.Fatalf("check worker: %v", err)
	}
	if !scheduled || manager.reconcileWorkerID != "worker-1" {
		t.Fatalf("scheduled = %v worker = %q, want worker-1", scheduled, manager.reconcileWorkerID)
	}
}

type recordingWorkerReconcileManager struct {
	reconcileWorkerID   string
	reconcileProjectID  string
	reconcileProviderID string
}

func (m *recordingWorkerReconcileManager) ListWorkers(context.Context, string, string) ([]model.Worker, error) {
	return nil, nil
}

func (m *recordingWorkerReconcileManager) GetWorker(context.Context, string) (*model.Worker, error) {
	return nil, nil
}

func (m *recordingWorkerReconcileManager) CreateWorker(context.Context, *model.Worker) (*model.Worker, error) {
	return nil, nil
}

func (m *recordingWorkerReconcileManager) CreateWorkerBootstrapToken(context.Context, *model.WorkerBootstrapToken) error {
	return nil
}

func (m *recordingWorkerReconcileManager) EnsureWorkerAgentTrustKey(context.Context) (string, error) {
	return "control-plane-public-key", nil
}

func (m *recordingWorkerReconcileManager) CreateWorkerAgentToken(context.Context, workeragentauth.TokenClaims) (string, error) {
	return "worker-token", nil
}

func (m *recordingWorkerReconcileManager) CreateSandboxAgentToken(context.Context, workeragentauth.TokenClaims) (string, error) {
	return "sandbox-agent-token", nil
}

func (m *recordingWorkerReconcileManager) FindSchedulableWorker(context.Context, *model.Sandbox) (*model.Worker, error) {
	return nil, nil
}

func (m *recordingWorkerReconcileManager) ScheduleWorkerReconciliation(_ context.Context, workerID string) error {
	m.reconcileWorkerID = workerID
	return nil
}

func (m *recordingWorkerReconcileManager) ScheduleWorkerProviderReconciliation(_ context.Context, projectID, providerID string) error {
	m.reconcileProjectID = projectID
	m.reconcileProviderID = providerID
	return nil
}
