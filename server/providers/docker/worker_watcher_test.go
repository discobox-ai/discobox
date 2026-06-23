package docker

import (
	"context"
	"testing"

	"github.com/moby/moby/api/types/events"

	"github.com/obot-platform/discobox/server/internal/model"
)

func TestDockerWorkerWatcherMarksRuntimeLostAndSchedulesProviderReconcile(t *testing.T) {
	manager := &recordingRuntimeLostManager{}
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
	if manager.markProjectID != "project-1" || manager.markProviderID != "provider-1" || manager.markWorkerID != "worker-1" {
		t.Fatalf("mark call = project %q provider %q worker %q", manager.markProjectID, manager.markProviderID, manager.markWorkerID)
	}
	if manager.markMessage != "worker container 1234567890ab die" {
		t.Fatalf("mark message = %q", manager.markMessage)
	}
	if manager.reconcileProjectID != "project-1" || manager.reconcileProviderID != "provider-1" {
		t.Fatalf("reconcile call = project %q provider %q", manager.reconcileProjectID, manager.reconcileProviderID)
	}
}

func TestDockerWorkerWatcherIgnoresNonTerminalContainerEvents(t *testing.T) {
	manager := &recordingRuntimeLostManager{}
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
	if manager.markWorkerID != "" || manager.reconcileProviderID != "" {
		t.Fatalf("unexpected calls: mark worker %q reconcile provider %q", manager.markWorkerID, manager.reconcileProviderID)
	}
}

type recordingRuntimeLostManager struct {
	markProjectID       string
	markProviderID      string
	markWorkerID        string
	markMessage         string
	reconcileProjectID  string
	reconcileProviderID string
}

func (m *recordingRuntimeLostManager) ListWorkers(context.Context, string, string) ([]model.Worker, error) {
	return nil, nil
}

func (m *recordingRuntimeLostManager) GetWorker(context.Context, string) (*model.Worker, error) {
	return nil, nil
}

func (m *recordingRuntimeLostManager) CreateWorker(context.Context, *model.Worker) (*model.Worker, error) {
	return nil, nil
}

func (m *recordingRuntimeLostManager) CreateWorkerBootstrapToken(context.Context, *model.WorkerBootstrapToken) error {
	return nil
}

func (m *recordingRuntimeLostManager) FindSchedulableWorker(context.Context, *model.Sandbox) (*model.Worker, error) {
	return nil, nil
}

func (m *recordingRuntimeLostManager) MarkWorkerRuntimeLost(_ context.Context, projectID, providerID, workerID, message string) (bool, error) {
	m.markProjectID = projectID
	m.markProviderID = providerID
	m.markWorkerID = workerID
	m.markMessage = message
	return true, nil
}

func (m *recordingRuntimeLostManager) ScheduleWorkerProviderReconciliation(_ context.Context, projectID, providerID string) error {
	m.reconcileProjectID = projectID
	m.reconcileProviderID = providerID
	return nil
}
