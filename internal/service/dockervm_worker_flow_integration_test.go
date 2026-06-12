package service_test

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/obot-platform/discobox/internal/api"
	"github.com/obot-platform/discobox/internal/database"
	"github.com/obot-platform/discobox/internal/events"
	"github.com/obot-platform/discobox/internal/model"
	"github.com/obot-platform/discobox/internal/sandbox/jobs"
	"github.com/obot-platform/discobox/internal/service"
	"github.com/obot-platform/discobox/internal/store"
	"github.com/obot-platform/discobox/orchestration"
)

const dockerVMIntegrationEnv = "DISCOBOX_DOCKER_VM_INTEGRATION"

func TestDockerVMProviderWorkerCreateFlowE2E(t *testing.T) {
	if os.Getenv(dockerVMIntegrationEnv) != "1" {
		t.Skipf("set %s=1 to run Docker VM provider e2e tests", dockerVMIntegrationEnv)
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

	image := os.Getenv("DISCOBOX_DOCKER_VM_TEST_IMAGE")
	if image == "" {
		image = "discobox-dockervm-worker-flow:test-" + uuid.NewString()
		buildDockerVMWorkerFlowImage(t, ctx, dockerClient, image)
	}

	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	if err := db.MigrateTenant(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}

	broker := events.NewBroker()
	appStore := store.New(database.StaticResolver{DB: db}, store.WithPublisher(broker), store.WithDefaultTenantID(service.DefaultTenantID))
	queueConfig := orchestration.QueueConfig{DefaultMaxAttempts: 1}
	dispatcher := orchestration.NewDispatcher(appStore, orchestration.DispatcherConfig{
		SingleNode:         true,
		PollInterval:       10 * time.Millisecond,
		JobTimeout:         30 * time.Second,
		StaleJobTimeout:    time.Minute,
		ImmediateExecution: true,
		DefaultConcurrency: 2,
	})
	svc := service.New(appStore, queueConfig, func(context.Context) { dispatcher.NotifyNewJob() }, broker)
	if err := dispatcher.Register(jobs.NewWorkerReconcileExecutor(svc.NewWorkerReconciler())); err != nil {
		t.Fatalf("register worker executor: %v", err)
	}
	if err := svc.InitializeDefaults(ctx, service.DefaultTenantID, service.DefaultUserID); err != nil {
		t.Fatalf("initialize defaults: %v", err)
	}
	if err := dispatcher.Start(ctx); err != nil {
		t.Fatalf("start dispatcher: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
		defer stopCancel()
		if err := dispatcher.DrainAndStop(stopCtx); err != nil {
			t.Fatalf("stop dispatcher: %v", err)
		}
	})

	providerConfig, err := json.Marshal(map[string]any{
		"controlPlaneUrl":   "http://127.0.0.1:1",
		"image":             image,
		"systemd":           false,
		"command":           []string{"sleep", "300"},
		"minWorkers":        1,
		"maxWorkers":        1,
		"minHealthyWorkers": 1,
	})
	if err != nil {
		t.Fatalf("marshal provider config: %v", err)
	}
	provider, err := svc.CreateSandboxProviderInstance(ctx, service.DefaultProjectID, api.CreateSandboxProviderInstanceBody{
		Type:   "dockervm",
		Name:   "docker e2e",
		Config: providerConfig,
	})
	if err != nil {
		t.Fatalf("create provider instance: %v", err)
	}
	t.Cleanup(func() { cleanupDockerVMProviderContainers(t, dockerClient, provider.ID) })

	worker := waitForProviderWorker(t, ctx, appStore, provider.ID)
	if worker.LastJobID == nil {
		t.Fatal("worker last job ID is nil")
	}
	if worker.Phase != model.WorkerPhaseRegistering {
		t.Fatalf("worker phase = %q, want %q", worker.Phase, model.WorkerPhaseRegistering)
	}
	if worker.LastOperationStatus != model.OperationStatusSuccess || worker.ObservedGeneration != worker.Generation {
		t.Fatalf("worker status/generation = %q %d/%d, want success observed", worker.LastOperationStatus, worker.ObservedGeneration, worker.Generation)
	}

	containers := listDockerVMProviderContainers(t, dockerClient, provider.ID)
	if len(containers) != 1 {
		t.Fatalf("provider containers = %d, want 1", len(containers))
	}
	labels := containers[0].Labels
	if labels["discobox.worker_id"] != worker.ID {
		t.Fatalf("container worker label = %q, want %q", labels["discobox.worker_id"], worker.ID)
	}
	if labels["discobox.provider_instance_id"] != provider.ID {
		t.Fatalf("container provider label = %q, want %q", labels["discobox.provider_instance_id"], provider.ID)
	}
	if labels["discobox.vm.managed"] != "true" || labels["discobox.provider_type"] != "dockervm" {
		t.Fatalf("container labels = %#v, want managed dockervm", labels)
	}
}

func waitForProviderWorker(t *testing.T, ctx context.Context, appStore *store.Store, providerID string) model.Worker {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last []model.Worker
	for time.Now().Before(deadline) {
		workers, err := appStore.ListWorkers(ctx, service.DefaultProjectID, providerID)
		if err != nil {
			t.Fatalf("list workers: %v", err)
		}
		last = workers
		for _, worker := range workers {
			if worker.Phase == model.WorkerPhaseRegistering && worker.LastOperationStatus == model.OperationStatusSuccess {
				return worker
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for registering worker; last workers = %#v", last)
	return model.Worker{}
}

func buildDockerVMWorkerFlowImage(t *testing.T, ctx context.Context, dockerClient *client.Client, tag string) {
	t.Helper()
	dockerfile := []byte("FROM debian:13-slim\nRUN apt-get update \\\n    && apt-get install -y --no-install-recommends ca-certificates \\\n    && apt-get clean \\\n    && rm -rf /var/lib/apt/lists/*\nCMD [\"sleep\", \"300\"]\n")
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	if err := writer.WriteHeader(&tar.Header{Name: "Dockerfile", Mode: 0o644, Size: int64(len(dockerfile))}); err != nil {
		t.Fatalf("write dockerfile header: %v", err)
	}
	if _, err := writer.Write(dockerfile); err != nil {
		t.Fatalf("write dockerfile: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close docker context: %v", err)
	}
	resp, err := dockerClient.ImageBuild(ctx, &buf, client.ImageBuildOptions{Tags: []string{tag}, Dockerfile: "Dockerfile", Remove: true, ForceRemove: true})
	if err != nil {
		t.Fatalf("build image %q: %v", tag, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
}

func listDockerVMProviderContainers(t *testing.T, dockerClient *client.Client, providerID string) []container.Summary {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	filterArgs := client.Filters{}
	filterArgs = filterArgs.Add("label", "discobox.provider_instance_id="+providerID)
	filterArgs = filterArgs.Add("label", "discobox.provider_type=dockervm")
	containers, err := dockerClient.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filterArgs})
	if err != nil {
		t.Fatalf("list provider containers: %v", err)
	}
	return containers.Items
}

func cleanupDockerVMProviderContainers(t *testing.T, dockerClient *client.Client, providerID string) {
	t.Helper()
	containers := listDockerVMProviderContainers(t, dockerClient, providerID)
	for _, c := range containers {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, err := dockerClient.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
		cancel()
		if err != nil && !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "No such container") {
			t.Fatalf("remove container %s: %v", c.ID, err)
		}
	}
}
