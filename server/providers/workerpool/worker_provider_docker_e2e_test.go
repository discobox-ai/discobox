package workerpool

import (
	"context"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/obot-platform/discobox/server/internal/model"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/providers/workerpool/vm"
	"github.com/obot-platform/discobox/worker-agent/sandboxruntime"
	"github.com/obot-platform/discobox/worker-agent/server"
)

const dockerIntegrationEnv = "DISCOBOX_DOCKER_INTEGRATION"

func TestWorkerProviderCreateCreatesDockerContainerE2E(t *testing.T) {
	if os.Getenv(dockerIntegrationEnv) != "1" {
		t.Skipf("set %s=1 to run Docker sandbox e2e tests", dockerIntegrationEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	dockerClient, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatalf("new docker client: %v", err)
	}
	t.Cleanup(func() { _ = dockerClient.Close() })
	if _, err := dockerClient.Ping(ctx, client.PingOptions{}); err != nil {
		t.Fatalf("ping docker: %v", err)
	}

	projectID := "project-e2e-" + uuid.NewString()
	workerID := "worker-e2e-" + uuid.NewString()
	sandboxID := "sandbox-e2e-" + uuid.NewString()
	providerID := "provider-e2e-" + uuid.NewString()
	image := os.Getenv("DISCOBOX_SANDBOX_DOCKER_TEST_IMAGE")
	if image == "" {
		image = "alpine:3.20"
	}

	runtime, err := sandboxruntime.NewDockerSandboxRuntime(projectID, workerID)
	if err != nil {
		t.Fatalf("new docker sandbox runtime: %v", err)
	}
	t.Cleanup(func() { cleanupWorkerProviderSandboxContainers(t, dockerClient, projectID, workerID, sandboxID) })

	router, _ := server.NewRouter(server.Config{
		Identity:   server.Identity{ProjectID: projectID, WorkerID: workerID},
		Runtime:    runtime,
		AuthTokens: []string{"worker-token"},
	})
	workerAgent := httptest.NewServer(router)
	defer workerAgent.Close()

	driver := &workerHTTPOnlyDriver{baseURL: workerAgent.URL, client: workerAgent.Client(), authToken: "worker-token"}
	baseProvider, err := vm.New(vm.Config{Driver: driver})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	workerManager := &recordingWorkerManager{worker: &model.Worker{ID: workerID, ProjectID: projectID, ProviderInstanceID: providerID, Ready: true, Schedulable: true}}
	provider := NewWorkerPoolProvider(baseProvider, WorkerPoolConfig{}, workerManager, false)

	runtimeSandbox, state, err := provider.Create(ctx, sandbox.SandboxRef{ProjectID: projectID, SandboxID: sandboxID}, nil, sandbox.CreateOptions{
		ProviderInstanceID: providerID,
		Image:              sandbox.ImageRef{Name: image},
		Env:                map[string]string{"DISCOBOX_E2E": "true"},
		WorkingDirectory:   "/tmp",
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if driver.workerID != workerID {
		t.Fatalf("worker HTTP lease workerID = %q, want %q", driver.workerID, workerID)
	}
	if len(state) == 0 {
		t.Fatal("expected provider state")
	}
	if runtimeSandbox == nil || runtimeSandbox.SandboxID != sandboxID || runtimeSandbox.Status != sandbox.StatusRunning {
		t.Fatalf("runtime sandbox = %#v, want running sandbox %q", runtimeSandbox, sandboxID)
	}
	if runtimeSandbox.Metadata["worker_id"] != workerID {
		t.Fatalf("runtime sandbox worker metadata = %q, want %q", runtimeSandbox.Metadata["worker_id"], workerID)
	}

	containers := listWorkerProviderSandboxContainers(t, dockerClient, projectID, workerID, sandboxID)
	if len(containers) != 1 {
		t.Fatalf("sandbox containers = %d, want 1", len(containers))
	}
	created := containers[0]
	if created.ID != runtimeSandbox.ID {
		t.Fatalf("container ID = %q, runtime sandbox ID = %q", created.ID, runtimeSandbox.ID)
	}
	if created.Labels["discobox.sandbox.managed"] != "true" || created.Labels["discobox.project_id"] != projectID || created.Labels["discobox.worker_id"] != workerID || created.Labels["discobox.sandbox_id"] != sandboxID {
		t.Fatalf("container labels = %#v", created.Labels)
	}

	inspect, err := dockerClient.ContainerInspect(ctx, created.ID, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect sandbox container: %v", err)
	}
	if inspect.Container.State == nil || !inspect.Container.State.Running {
		t.Fatalf("container state = %#v, want running", inspect.Container.State)
	}
	if inspect.Container.Config.Image != image {
		t.Fatalf("container image = %q, want %q", inspect.Container.Config.Image, image)
	}
	if inspect.Container.Config.WorkingDir != "/tmp" {
		t.Fatalf("working dir = %q, want /tmp", inspect.Container.Config.WorkingDir)
	}
	if !containsContainerEnv(inspect.Container.Config.Env, "DISCOBOX_E2E=true") {
		t.Fatalf("container env = %#v, missing DISCOBOX_E2E=true", inspect.Container.Config.Env)
	}
}

func listWorkerProviderSandboxContainers(t *testing.T, dockerClient *client.Client, projectID, workerID, sandboxID string) []container.Summary {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	filters := client.Filters{}
	filters = filters.Add("label", "discobox.sandbox.managed=true")
	filters = filters.Add("label", "discobox.project_id="+projectID)
	filters = filters.Add("label", "discobox.worker_id="+workerID)
	filters = filters.Add("label", "discobox.sandbox_id="+sandboxID)
	containers, err := dockerClient.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
	if err != nil {
		t.Fatalf("list sandbox containers: %v", err)
	}
	return containers.Items
}

func cleanupWorkerProviderSandboxContainers(t *testing.T, dockerClient *client.Client, projectID, workerID, sandboxID string) {
	t.Helper()
	for _, ctr := range listWorkerProviderSandboxContainers(t, dockerClient, projectID, workerID, sandboxID) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, err := dockerClient.ContainerRemove(ctx, ctr.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
		cancel()
		if err != nil {
			t.Fatalf("remove sandbox container %s: %v", ctr.ID, err)
		}
	}
}

func containsContainerEnv(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
