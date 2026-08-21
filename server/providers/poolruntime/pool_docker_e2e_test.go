package poolruntime

import (
	"context"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
	"github.com/discobox-ai/discobox/pool-agent/server"
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
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
	poolID := "pool-e2e-" + uuid.NewString()
	sandboxID := "sandbox-e2e-" + uuid.NewString()
	image := os.Getenv("DISCOBOX_SANDBOX_DOCKER_TEST_IMAGE")
	if image == "" {
		image = "alpine:3.20"
	}

	controlPlaneKey, poolToken := newPoolAgentTestAuth(t, projectID, poolID)
	runtime, err := sandboxruntime.NewDockerSandboxRuntime(sandboxruntime.DockerSandboxRuntimeConfig{
		ProjectID:             projectID,
		PoolID:                poolID,
		ControlPlanePublicKey: controlPlaneKey,
	})
	if err != nil {
		t.Fatalf("new docker sandbox runtime: %v", err)
	}
	t.Cleanup(func() { cleanupPoolSandboxContainers(t, dockerClient, projectID, poolID, sandboxID) })

	router, _ := server.NewRouter(server.Config{
		Identity:              server.Identity{ProjectID: projectID, PoolID: poolID},
		Runtime:               runtime,
		ControlPlanePublicKey: controlPlaneKey,
	})
	poolAgent := httptest.NewServer(router)
	defer poolAgent.Close()

	runtimeProvider := &testRuntimeProvider{baseURL: poolAgent.URL, client: poolAgent.Client(), token: poolToken}
	pool := activePool(poolID)
	pool.ProjectID = projectID
	manager := &fakePoolManager{pool: pool, schedulable: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager)

	runtimeSandbox, state, err := provider.Create(ctx, sandbox.SandboxRef{ProjectID: projectID, SandboxID: sandboxID}, nil, sandbox.CreateOptions{
		PoolID: poolID,
		Image:  sandbox.ImageRef{Name: image},
		Env:    map[string]string{"DISCOBOX_E2E": "true"},
		Source: &model.GitSource{
			Kind: "git",
			Destination: &model.GitSourceDestination{
				WorkingDirectory: e2eString("/tmp"),
			},
		},
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if len(state) == 0 {
		t.Fatal("expected provider state")
	}
	if runtimeSandbox == nil || runtimeSandbox.SandboxID != sandboxID || runtimeSandbox.Status != sandbox.StatusRunning {
		t.Fatalf("runtime sandbox = %#v, want running sandbox %q", runtimeSandbox, sandboxID)
	}
	if runtimeSandbox.Metadata["pool_id"] != poolID {
		t.Fatalf("runtime sandbox worker metadata = %q, want %q", runtimeSandbox.Metadata["pool_id"], poolID)
	}

	containers := listPoolSandboxContainers(t, dockerClient, projectID, poolID, sandboxID)
	if len(containers) != 1 {
		t.Fatalf("sandbox containers = %d, want 1", len(containers))
	}
	created := containers[0]
	if created.ID != runtimeSandbox.ID {
		t.Fatalf("container ID = %q, runtime sandbox ID = %q", created.ID, runtimeSandbox.ID)
	}
	if created.Labels["discobox.sandbox.managed"] != "true" || created.Labels["discobox.project_id"] != projectID || created.Labels["discobox.pool_id"] != poolID || created.Labels["discobox.sandbox_id"] != sandboxID {
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

func listPoolSandboxContainers(t *testing.T, dockerClient *client.Client, projectID, poolID, sandboxID string) []container.Summary {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	filters := client.Filters{}
	filters = filters.Add("label", "discobox.sandbox.managed=true")
	filters = filters.Add("label", "discobox.project_id="+projectID)
	filters = filters.Add("label", "discobox.pool_id="+poolID)
	filters = filters.Add("label", "discobox.sandbox_id="+sandboxID)
	containers, err := dockerClient.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
	if err != nil {
		t.Fatalf("list sandbox containers: %v", err)
	}
	return containers.Items
}

func cleanupPoolSandboxContainers(t *testing.T, dockerClient *client.Client, projectID, poolID, sandboxID string) {
	t.Helper()
	for _, ctr := range listPoolSandboxContainers(t, dockerClient, projectID, poolID, sandboxID) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, err := dockerClient.ContainerRemove(ctx, ctr.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
		cancel()
		if err != nil {
			t.Fatalf("remove sandbox container %s: %v", ctr.ID, err)
		}
	}
}

func e2eString(value string) *string { return &value }

func containsContainerEnv(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
