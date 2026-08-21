package docker

import (
	"context"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"

	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/providers/dockerworker"
)

// TestStartWorkerWatcherNeverFailsInitOnScanError proves that a failing
// initial drift scan cannot fail provider initialization (and therefore
// cannot crash-loop server startup). The driver points at an unreachable
// Docker endpoint, so the scan, the watch loop, and the image reaper error on
// every call; all three run in the background and their failures are only
// logged.
func TestStartWorkerWatcherNeverFailsInitOnScanError(t *testing.T) {
	cli, err := client.New(client.WithHost("tcp://127.0.0.1:1"))
	if err != nil {
		t.Fatalf("new docker client: %v", err)
	}
	driver := &LocalDriver{client: cli, agentPort: defaultAgentPort}
	t.Cleanup(func() { _ = driver.Close() })
	engine, err := dockerworker.New(dockerworker.Config{Image: "worker:test"}, driver)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}

	if err := startBackgroundWatchers(driver, engine, &recordingPoolManager{}, &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1"}); err != nil {
		t.Fatalf("startBackgroundWatchers returned error: %v", err)
	}
	// Give the background goroutine time to run the doomed scan and enter the
	// watch loop; a propagated failure or panic would surface here.
	time.Sleep(100 * time.Millisecond)
}

func TestDockerWorkerWatcherSchedulesPoolReconcileForTerminalContainerEvent(t *testing.T) {
	manager := &recordingPoolManager{}
	watcher := dockerPoolWatcher{
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
				dockerworker.LabelPoolID:             "pool-1",
				dockerworker.LabelPoolAgent:          "true",
				dockerworker.LabelProviderInstanceID: "provider-1",
			},
		},
	})
	if err != nil {
		t.Fatalf("handle event: %v", err)
	}
	if manager.reconcilePoolID != "pool-1" {
		t.Fatalf("worker reconcile = %q, want worker-1", manager.reconcilePoolID)
	}
}

func TestDockerWorkerWatcherIgnoresNonTerminalContainerEvents(t *testing.T) {
	manager := &recordingPoolManager{}
	watcher := dockerPoolWatcher{
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
				dockerworker.LabelPoolID: "pool-1",
			},
		},
	})
	if err != nil {
		t.Fatalf("handle event: %v", err)
	}
	if manager.reconcilePoolID != "" {
		t.Fatalf("unexpected pool reconcile for %q", manager.reconcilePoolID)
	}
}

func TestDockerWorkerWatcherSchedulesFailedPoolWhenCurrentContainerRuns(t *testing.T) {
	manager := &recordingPoolManager{}
	watcher := dockerPoolWatcher{
		manager:    manager,
		projectID:  "project-1",
		providerID: "provider-1",
	}
	pool := &model.Pool{
		ID:        "pool-1",
		ProjectID: "project-1",
		ResourceLifecycle: model.ResourceLifecycle{
			DesiredState: model.DesiredStatePresent,
			State:        model.PoolStateFailed,
		},
	}
	current := &container.InspectResponse{
		ID:    "container-1",
		State: &container.State{Running: true},
	}

	scheduled, err := watcher.checkPool(context.Background(), pool, current)
	if err != nil {
		t.Fatalf("check pool: %v", err)
	}
	if !scheduled || manager.reconcilePoolID != "pool-1" {
		t.Fatalf("scheduled = %v pool = %q, want pool-1", scheduled, manager.reconcilePoolID)
	}
}

func TestDockerWorkerWatcherSchedulesCreatedFailedPoolWithoutContainer(t *testing.T) {
	manager := &recordingPoolManager{}
	watcher := dockerPoolWatcher{
		manager:    manager,
		projectID:  "project-1",
		providerID: "provider-1",
	}
	registeredAt := time.Now().UTC()
	pool := &model.Pool{
		ID:           "pool-1",
		ProjectID:    "project-1",
		RegisteredAt: &registeredAt,
		ResourceLifecycle: model.ResourceLifecycle{
			DesiredState: model.DesiredStatePresent,
			State:        model.PoolStateOffline,
		},
	}

	// A created pool whose container is gone must still be recreated.
	scheduled, err := watcher.checkPool(context.Background(), pool, nil)
	if err != nil {
		t.Fatalf("check pool: %v", err)
	}
	if !scheduled || manager.reconcilePoolID != "pool-1" {
		t.Fatalf("scheduled = %v pool = %q, want pool-1", scheduled, manager.reconcilePoolID)
	}
}

func TestDockerWorkerWatcherLeavesNeverCreatedFailedPoolWithoutContainer(t *testing.T) {
	manager := &recordingPoolManager{}
	watcher := dockerPoolWatcher{
		manager:    manager,
		projectID:  "project-1",
		providerID: "provider-1",
	}
	pool := &model.Pool{
		ID:        "pool-1",
		ProjectID: "project-1",
		ResourceLifecycle: model.ResourceLifecycle{
			DesiredState: model.DesiredStatePresent,
			State:        model.PoolStateFailed,
		},
	}

	// A pool whose runtime never registered has nothing to recover.
	scheduled, err := watcher.checkPool(context.Background(), pool, nil)
	if err != nil {
		t.Fatalf("check pool: %v", err)
	}
	if scheduled || manager.reconcilePoolID != "" {
		t.Fatalf("scheduled = %v pool = %q, want no reconcile", scheduled, manager.reconcilePoolID)
	}
}

func TestDockerWorkerWatcherSchedulesDeletedPoolWhenContainerRemains(t *testing.T) {
	manager := &recordingPoolManager{}
	watcher := dockerPoolWatcher{
		manager:    manager,
		projectID:  "project-1",
		providerID: "provider-1",
	}
	pool := &model.Pool{
		ID:        "pool-1",
		ProjectID: "project-1",
		ResourceLifecycle: model.ResourceLifecycle{
			DesiredState: model.DesiredStateDeleted,
			State:        model.PoolStateDeleted,
		},
	}
	current := &container.InspectResponse{
		ID:    "container-1",
		State: &container.State{Running: true},
	}

	scheduled, err := watcher.checkPool(context.Background(), pool, current)
	if err != nil {
		t.Fatalf("check pool: %v", err)
	}
	if !scheduled || manager.reconcilePoolID != "pool-1" {
		t.Fatalf("scheduled = %v pool = %q, want pool-1", scheduled, manager.reconcilePoolID)
	}
}

func TestDockerWorkerWatcherSchedulesRunningPoolWithStaleConfig(t *testing.T) {
	engine, err := dockerworker.New(dockerworker.Config{Image: "worker:new"}, &LocalDriver{})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}

	if !engine.ShouldReconcileWorkerContainer("worker:old", map[string]string{dockerworker.LabelPoolConfig: "old"}) {
		t.Fatalf("ShouldReconcileWorkerContainer = false, want true for stale image/config")
	}
	if engine.ShouldReconcileWorkerContainer("worker:new", map[string]string{dockerworker.LabelPoolConfig: engine.ConfigRevision()}) {
		t.Fatalf("ShouldReconcileWorkerContainer = true, want false for current image/config")
	}
}

type recordingPoolManager struct {
	reconcileProjectID string
	reconcilePoolID    string
}

func (m *recordingPoolManager) GetPool(context.Context, string, string) (*model.Pool, error) {
	return &model.Pool{ID: "pool-1", ProjectID: "project-1", PoolManifest: model.PoolManifest{ProviderInstanceID: "provider-1"}}, nil
}

func (m *recordingPoolManager) ListPoolsForProviderInstance(context.Context, string, string) ([]model.Pool, error) {
	return []model.Pool{{ID: "pool-1", ProjectID: "project-1", PoolManifest: model.PoolManifest{ProviderInstanceID: "provider-1"}}}, nil
}

func (m *recordingPoolManager) ListPools(context.Context, string) ([]model.Pool, error) {
	return []model.Pool{{ID: "pool-1", ProjectID: "project-1", PoolManifest: model.PoolManifest{ProviderInstanceID: "provider-1"}}}, nil
}

func (m *recordingPoolManager) SchedulablePoolForSandbox(context.Context, *model.Sandbox) (*model.Pool, error) {
	return nil, nil
}

func (m *recordingPoolManager) GetProject(context.Context, string) (*model.Project, error) {
	return &model.Project{ID: "project-1"}, nil
}

func (m *recordingPoolManager) GetSandboxProviderInstance(context.Context, string, string) (*model.SandboxProviderInstance, error) {
	return &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1"}, nil
}

func (m *recordingPoolManager) CountSandboxesForPool(context.Context, string, string) (int64, error) {
	return 0, nil
}

func (m *recordingPoolManager) CreatePoolBootstrapToken(context.Context, *model.PoolBootstrapToken) error {
	return nil
}

func (m *recordingPoolManager) EnsureAgentTrustKey(context.Context) (string, error) {
	return "control-plane-public-key", nil
}

func (m *recordingPoolManager) CreateAgentToken(context.Context, poolagentauth.TokenClaims) (string, error) {
	return "pool-token", nil
}

func (m *recordingPoolManager) CreateSandboxAgentToken(context.Context, poolagentauth.TokenClaims) (string, error) {
	return "sandbox-agent-token", nil
}

func (m *recordingPoolManager) SchedulePoolReconciliation(_ context.Context, projectID, poolID string) error {
	m.reconcileProjectID = projectID
	m.reconcilePoolID = poolID
	return nil
}

func (m *recordingPoolManager) SchedulePoolRepair(context.Context, string, string) error {
	return nil
}
