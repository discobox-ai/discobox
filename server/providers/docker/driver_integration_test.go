package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/google/uuid"
	"github.com/moby/moby/client"

	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/providers/dockerworker"
	workeragent "github.com/obot-platform/discobox/worker-agent"
)

const dockerIntegrationEnv = "DISCOBOX_DOCKER_INTEGRATION"

func TestDockerIntegrationWorkerLifecycle(t *testing.T) {
	if os.Getenv(dockerIntegrationEnv) != "1" {
		t.Skipf("set %s=1 to run Docker integration tests", dockerIntegrationEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	image := os.Getenv("DISCOBOX_DOCKER_TEST_IMAGE")
	if image == "" {
		image = "discobox-docker-sleep:test-" + uuid.NewString()
		buildDockerImage(ctx, t, "testdata/sleep/Dockerfile", image)
	}

	driver, err := NewLocalDriver(ctx, "", defaultAgentPort)
	if err != nil {
		t.Fatalf("new local driver: %v", err)
	}
	engine, err := dockerworker.New(dockerworker.Config{
		ControlPlaneURL: "http://control.example",
		Image:           image,
		Command:         []string{"sleep", "300"},
	}, driver)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	t.Cleanup(func() { _ = engine.Close() })

	project := &model.Project{ID: "project-" + uuid.NewString()}
	provider := &model.SandboxProviderInstance{ID: "provider-" + uuid.NewString(), ProjectID: project.ID}
	worker := &model.Worker{ID: "worker-" + uuid.NewString(), ProjectID: project.ID, ProviderInstanceID: provider.ID}
	bootstrap := workeragent.Bootstrap{ProjectID: project.ID, WorkerID: worker.ID, Token: "token-1", ControlPlaneKey: "key-1"}

	if err := engine.EnsureWorker(ctx, project, provider, worker, bootstrap); err != nil {
		t.Fatalf("ensure worker: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_ = engine.RemoveWorker(cleanupCtx, project, provider, worker)
	})
	state, err := dockerworker.DecodeRuntimeState(worker.RuntimeState)
	if err != nil {
		t.Fatalf("decode runtime state: %v", err)
	}
	if state.ContainerID == "" {
		t.Fatalf("runtime state = %#v, want container ID", state)
	}
	if worker.Phase != model.WorkerPhaseRegistering {
		t.Fatalf("worker phase = %q, want registering", worker.Phase)
	}
	assertContainerRunning(ctx, t, state.ContainerID, true)

	// EnsureWorker is idempotent: the healthy container is kept.
	if err := engine.EnsureWorker(ctx, project, provider, worker, bootstrap); err != nil {
		t.Fatalf("re-ensure worker: %v", err)
	}
	nextState, err := dockerworker.DecodeRuntimeState(worker.RuntimeState)
	if err != nil {
		t.Fatalf("decode runtime state: %v", err)
	}
	if nextState.ContainerID != state.ContainerID {
		t.Fatalf("container replaced on idempotent ensure: %q != %q", nextState.ContainerID, state.ContainerID)
	}

	// Repair replaces the container while preserving the worker identity.
	if err := engine.RepairWorker(ctx, project, provider, worker, bootstrap, "integration test"); err != nil {
		t.Fatalf("repair worker: %v", err)
	}
	repairedState, err := dockerworker.DecodeRuntimeState(worker.RuntimeState)
	if err != nil {
		t.Fatalf("decode repaired runtime state: %v", err)
	}
	if repairedState.ContainerID == state.ContainerID {
		t.Fatalf("repair did not replace container %q", state.ContainerID)
	}
	assertContainerRunning(ctx, t, repairedState.ContainerID, true)

	// The worker-agent client resolves the published loopback endpoint.
	lease, err := engine.AcquireWorkerAgentClient(ctx, worker)
	if err != nil {
		t.Fatalf("acquire worker agent client: %v", err)
	}
	if lease.BaseURL == "" {
		t.Fatalf("worker agent lease has no base URL")
	}
	lease.Release()

	if err := engine.RemoveWorker(ctx, project, provider, worker); err != nil {
		t.Fatalf("remove worker: %v", err)
	}
	if worker.RuntimeState != nil {
		t.Fatalf("runtime state after remove = %s, want nil", worker.RuntimeState)
	}
	assertContainerAbsent(ctx, t, repairedState.ContainerID)
}

func TestDockerIntegrationSystemdWorker(t *testing.T) {
	if os.Getenv(dockerIntegrationEnv) != "1" {
		t.Skipf("set %s=1 to run Docker integration tests", dockerIntegrationEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	image := os.Getenv("DISCOBOX_DOCKER_SYSTEMD_IMAGE")
	if image == "" {
		image = "discobox-docker-systemd:test-" + uuid.NewString()
		buildDockerImage(ctx, t, "testdata/systemd/Dockerfile", image)
	}

	driver, err := NewLocalDriver(ctx, "", defaultAgentPort)
	if err != nil {
		t.Fatalf("new local driver: %v", err)
	}
	engine, err := dockerworker.New(dockerworker.Config{
		ControlPlaneURL: "http://control.example",
		Image:           image,
		Systemd:         true,
		CgroupNSMode:    "host",
		Command:         []string{"/sbin/init"},
	}, driver)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	t.Cleanup(func() { _ = engine.Close() })

	project := &model.Project{ID: "project-" + uuid.NewString()}
	provider := &model.SandboxProviderInstance{ID: "provider-" + uuid.NewString(), ProjectID: project.ID}
	worker := &model.Worker{ID: "worker-" + uuid.NewString(), ProjectID: project.ID, ProviderInstanceID: provider.ID}

	if err := engine.EnsureWorker(ctx, project, provider, worker, workeragent.Bootstrap{ProjectID: project.ID, WorkerID: worker.ID, Token: "token-1"}); err != nil {
		t.Fatalf("ensure systemd worker: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_ = engine.RemoveWorker(cleanupCtx, project, provider, worker)
	})
	state, err := dockerworker.DecodeRuntimeState(worker.RuntimeState)
	if err != nil {
		t.Fatalf("decode runtime state: %v", err)
	}
	assertContainerRunning(ctx, t, state.ContainerID, true)
}

func assertContainerRunning(ctx context.Context, t *testing.T, containerID string, running bool) {
	t.Helper()
	cli, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatalf("new docker client: %v", err)
	}
	defer cli.Close()
	inspect, err := cli.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect container %q: %v", containerID, err)
	}
	if got := inspect.Container.State != nil && inspect.Container.State.Running; got != running {
		t.Fatalf("container %q running = %v, want %v", containerID, got, running)
	}
}

func assertContainerAbsent(ctx context.Context, t *testing.T, containerID string) {
	t.Helper()
	cli, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatalf("new docker client: %v", err)
	}
	defer cli.Close()
	_, err = cli.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err == nil || !cerrdefs.IsNotFound(errors.Unwrap(err)) && !cerrdefs.IsNotFound(err) {
		t.Fatalf("inspect removed container %q err = %v, want not found", containerID, err)
	}
}

func buildDockerImage(ctx context.Context, t *testing.T, dockerfilePath, tag string) {
	t.Helper()
	cli, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatalf("new docker client: %v", err)
	}
	defer cli.Close()
	buildContext, err := dockerBuildContext(dockerfilePath)
	if err != nil {
		t.Fatalf("create docker build context: %v", err)
	}
	resp, err := cli.ImageBuild(ctx, buildContext, client.ImageBuildOptions{Tags: []string{tag}, Dockerfile: "Dockerfile", Remove: true, ForceRemove: true})
	if err != nil {
		t.Fatalf("build image %q: %v", tag, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
}

func dockerBuildContext(dockerfilePath string) (io.Reader, error) {
	data, err := os.ReadFile(filepath.Clean(dockerfilePath))
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	if err := writer.WriteHeader(&tar.Header{Name: "Dockerfile", Mode: 0o644, Size: int64(len(data))}); err != nil {
		_ = writer.Close()
		return nil, err
	}
	if _, err := writer.Write(data); err != nil {
		_ = writer.Close()
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return bytes.NewReader(buf.Bytes()), nil
}
