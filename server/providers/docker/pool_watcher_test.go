package docker

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"

	"github.com/discobox-ai/discobox/pkg/imagecache"
	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
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
	engine, err := dockerworker.New(dockerworker.Config{SandboxBaseImage: "worker:test"}, driver)
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
	engine, err := dockerworker.New(dockerworker.Config{SandboxBaseImage: "worker:new"}, &LocalDriver{})
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

// A pool container whose row is gone is removed with everything the engine
// made beside it — console, network, and the legacy named volume removing the
// container leaves (#32) — and never its state trees, which a row restored from
// a backup still needs: no container is created to remove them.
func TestDockerWorkerWatcherRemovesAnOrphanPoolsRuntimeButNotItsTrees(t *testing.T) {
	versionPrefix := regexp.MustCompile(`^/v[0-9.]+`)
	var mu sync.Mutex
	var requests []string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := versionPrefix.ReplaceAllString(r.URL.Path, "")
		mu.Lock()
		requests = append(requests, r.Method+" "+path)
		mu.Unlock()
		orphan := `{"Id":"orphan","Config":{"Labels":{"` + dockerworker.LabelPoolID + `":"pool-gone"}}}`
		switch {
		case r.Method == http.MethodGet && path == "/containers/json":
			_, _ = w.Write([]byte(`[` + orphan + `]`))
		case r.Method == http.MethodGet && path == "/containers/orphan/json":
			_, _ = w.Write([]byte(orphan))
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found"}`))
		}
	}))
	t.Cleanup(daemon.Close)
	cli, err := client.New(client.WithHost("tcp://"+strings.TrimPrefix(daemon.URL, "http://")), client.WithAPIVersion("1.51"))
	if err != nil {
		t.Fatalf("new docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	engine, err := dockerworker.New(dockerworker.Config{SandboxBaseImage: "worker:test"}, &LocalDriver{client: cli, agentPort: defaultAgentPort})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	watcher := dockerPoolWatcher{client: cli, engine: engine, manager: &recordingPoolManager{}, projectID: "project-1", providerID: "provider-1"}

	if _, err := watcher.scan(t.Context()); err != nil {
		t.Fatalf("scan: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, want := range []string{
		"DELETE /containers/" + dockerworker.ContainerName("pool-gone"),
		"DELETE /containers/" + dockerworker.ConsoleContainerName("pool-gone"),
		"DELETE /volumes/discobox-pool-pool-gone-docker",
	} {
		found := false
		for _, got := range requests {
			found = found || got == want
		}
		if !found {
			t.Errorf("orphan cleanup never sent %q; requests: %v", want, requests)
		}
	}
	for _, got := range requests {
		if got == "POST /containers/create" {
			t.Errorf("orphan cleanup created a container (its trees must stay); requests: %v", requests)
		}
	}
}

// A pool whose row commits while the scan runs, just before its container
// shows in the Docker list, keeps that container: the scan reads rows after
// listing containers, so the new container is never taken for an orphan
// (#1473).
func TestDockerPoolWatcherKeepsPoolCreatedDuringScan(t *testing.T) {
	versionPrefix := regexp.MustCompile(`^/v[0-9.]+`)
	manager := &poolCreatedDuringScan{}
	var removed atomic.Bool
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := versionPrefix.ReplaceAllString(r.URL.Path, "")
		created := `{"Id":"container-1","Config":{"Labels":{"` + dockerworker.LabelPoolID + `":"pool-1"}}}`
		switch {
		case r.Method == http.MethodGet && path == "/containers/json":
			// The pool row commits immediately before its container becomes visible.
			manager.created.Store(true)
			_, _ = w.Write([]byte(`[` + created + `]`))
		case r.Method == http.MethodGet && path == "/containers/container-1/json":
			_, _ = w.Write([]byte(created))
		case r.Method == http.MethodDelete:
			removed.Store(true)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found"}`))
		}
	}))
	t.Cleanup(daemon.Close)
	cli, err := client.New(client.WithHost("tcp://"+strings.TrimPrefix(daemon.URL, "http://")), client.WithAPIVersion("1.51"))
	if err != nil {
		t.Fatalf("new docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	engine, err := dockerworker.New(dockerworker.Config{SandboxBaseImage: "worker:test"}, &LocalDriver{client: cli, agentPort: defaultAgentPort})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	watcher := dockerPoolWatcher{client: cli, engine: engine, manager: manager, projectID: "project-1", providerID: "provider-1"}

	if _, err := watcher.scan(t.Context()); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if removed.Load() {
		t.Fatal("scan removed container-1 after its pool row was committed")
	}
}

// poolCreatedDuringScan lists no pool rows until its pool's row is created.
type poolCreatedDuringScan struct {
	recordingPoolManager
	created atomic.Bool
}

func (m *poolCreatedDuringScan) ListPoolsForProviderInstance(ctx context.Context, projectID, providerID string) ([]model.Pool, error) {
	if !m.created.Load() {
		return nil, nil
	}
	return m.recordingPoolManager.ListPoolsForProviderInstance(ctx, projectID, providerID)
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

func (m *recordingPoolManager) EnrollPool(context.Context, string, string) error {
	return nil
}

func (m *recordingPoolManager) RecordPoolHost(context.Context, string, model.RawJSON, *model.PoolHostCredential) error {
	return nil
}

func (m *recordingPoolManager) PoolBox(context.Context, imagecache.Platform) (sandbox.PoolBox, error) {
	return sandbox.PoolBox{}, nil
}

func (m *recordingPoolManager) AgentIssuerKey(context.Context) (ed25519.PrivateKey, error) {
	return nil, nil
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

func (m *recordingPoolManager) ReportPoolProvisionProgress(context.Context, string, sandbox.PoolProvisionProgress) error {
	return nil
}

func (m *recordingPoolManager) SchedulePoolRepair(context.Context, string, string) error {
	return nil
}

func (m *recordingPoolManager) ReportPoolBootstrap(context.Context, string, string, int64, model.BootstrapEvidence) (*model.PoolBootstrap, error) {
	return nil, nil
}
