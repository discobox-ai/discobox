package docker

import (
	"context"
	"testing"
	"time"

	"github.com/moby/moby/api/types/events"

	"github.com/obot-platform/discobox/orchestration"
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

func TestDockerWorkerWatcherSchedulesRunningWorkerWithStaleConfig(t *testing.T) {
	current := &vm.Instance{
		ID:       "container-1",
		Status:   sandbox.StatusRunning,
		Image:    "worker:old",
		Metadata: map[string]string{labelWorkerConfig: "old"},
	}

	if !shouldReconcileWorkerContainer(current, "worker:new", map[string]string{labelWorkerConfig: "new"}) {
		t.Fatalf("shouldReconcileWorkerContainer = false, want true for stale image/config")
	}
	if shouldReconcileWorkerContainer(current, "worker:old", map[string]string{labelWorkerConfig: "old"}) {
		t.Fatalf("shouldReconcileWorkerContainer = true, want false for current image/config")
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

func (m *recordingWorkerReconcileManager) DeleteWorker(context.Context, string) (*model.Worker, error) {
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

func (m *recordingWorkerReconcileManager) GetJob(context.Context, string) (*orchestration.Job, error) {
	return nil, orchestration.ErrJobNotFound
}

func (m *recordingWorkerReconcileManager) CountSandboxesForWorker(context.Context, string) (int64, error) {
	return 0, nil
}

func (m *recordingWorkerReconcileManager) CountSandboxesForWorkers(_ context.Context, workerIDs []string) (map[string]int64, error) {
	return make(map[string]int64, len(workerIDs)), nil
}

func (m *recordingWorkerReconcileManager) GetProject(context.Context, string) (*model.Project, error) {
	return &model.Project{ID: "project-1"}, nil
}

func (m *recordingWorkerReconcileManager) GetSandboxProviderInstance(context.Context, string, string) (*model.SandboxProviderInstance, error) {
	return &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1"}, nil
}

func (m *recordingWorkerReconcileManager) MarkWorkerFailedForJob(context.Context, string, int64, string, string) (bool, error) {
	return false, nil
}

func (m *recordingWorkerReconcileManager) DeleteWorkerForExpiredRegistration(context.Context, string, int64, time.Time, string) (bool, error) {
	return false, nil
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

func (m *recordingWorkerReconcileManager) ScheduleWorkerProviderReconciliationAt(context.Context, string, string, time.Time) error {
	return nil
}
