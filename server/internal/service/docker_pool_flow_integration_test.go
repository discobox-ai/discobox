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

	"github.com/obot-platform/discobox/server/internal/database"
	"github.com/obot-platform/discobox/server/internal/events"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/service"
	services "github.com/obot-platform/discobox/server/internal/services"
	"github.com/obot-platform/discobox/server/internal/store"
)

const dockerIntegrationEnv = "DISCOBOX_DOCKER_INTEGRATION"

func TestDockerProviderPoolCreateFlowE2E(t *testing.T) {
	if os.Getenv(dockerIntegrationEnv) != "1" {
		t.Skipf("set %s=1 to run Docker provider e2e tests", dockerIntegrationEnv)
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

	image := os.Getenv("DISCOBOX_DOCKER_TEST_IMAGE")
	if image == "" {
		image = "discobox-docker-pool-flow:test-" + uuid.NewString()
		buildDockerPoolFlowImage(ctx, t, dockerClient, image)
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
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}

	broker := events.NewBroker()
	appStore := store.New(db.Write, db.Read, store.WithPublisher(broker))
	engine := newTestReconcileEngine(t, db.Write)
	svc := service.New(appStore, engine, service.JobManagerOptions{}, broker)
	project, err := svc.InitializeDefaults(ctx, service.DefaultUserID)
	if err != nil {
		t.Fatalf("initialize defaults: %v", err)
	}
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start service: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
		defer stopCancel()
		if err := engine.Stop(stopCtx); err != nil {
			t.Fatalf("stop reconcile engine: %v", err)
		}
	})

	providerConfig, err := json.Marshal(map[string]any{
		"controlPlaneUrl": "http://127.0.0.1:1",
		"image":           image,
		"systemd":         false,
		"command":         []string{"sleep", "300"},
	})
	if err != nil {
		t.Fatalf("marshal provider config: %v", err)
	}
	provider, err := svc.CreateSandboxProviderInstance(ctx, project.ID, services.CreateSandboxProviderInstanceBody{
		Type:   "docker",
		Name:   "docker e2e",
		Config: providerConfig,
	})
	if err != nil {
		t.Fatalf("create provider instance: %v", err)
	}
	t.Cleanup(func() { cleanupDockerProviderContainers(t, dockerClient, provider.ID) })

	pool, err := svc.CreatePool(ctx, project.ID, services.CreatePoolBody{Name: "docker-e2e", ProviderInstanceId: provider.ID})
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	pool = waitForRegisteringPool(ctx, t, appStore, project.ID, pool.ID)
	if pool.Phase != model.PoolPhaseRegistering {
		t.Fatalf("pool phase = %q, want %q", pool.Phase, model.PoolPhaseRegistering)
	}
	if pool.LastOperationStatus != model.OperationStatusSuccess || pool.ObservedGeneration != pool.Generation {
		t.Fatalf("pool status/generation = %q %d/%d, want success observed", pool.LastOperationStatus, pool.ObservedGeneration, pool.Generation)
	}

	containers := listDockerProviderContainers(t, dockerClient, provider.ID)
	if len(containers) != 1 {
		t.Fatalf("provider containers = %d, want 1", len(containers))
	}
	labels := containers[0].Labels
	if labels["discobox.pool_id"] != pool.ID {
		t.Fatalf("container pool label = %q, want %q", labels["discobox.pool_id"], pool.ID)
	}
	if labels["discobox.provider_instance_id"] != provider.ID {
		t.Fatalf("container provider label = %q, want %q", labels["discobox.provider_instance_id"], provider.ID)
	}
	if labels["discobox.pool_agent"] != "true" {
		t.Fatalf("container pool agent label = %q, want true", labels["discobox.pool_agent"])
	}
	if _, ok := labels["discobox.sandbox_id"]; ok {
		t.Fatalf("container sandbox label present for pool agent: %#v", labels)
	}
	if labels["discobox.vm.managed"] != "true" || labels["discobox.provider_type"] != "docker" {
		t.Fatalf("container labels = %#v, want managed docker", labels)
	}
}

func waitForRegisteringPool(ctx context.Context, t *testing.T, appStore *store.Store, projectID, poolID string) *model.Pool {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last *model.Pool
	for time.Now().Before(deadline) {
		pool, err := appStore.GetPool(ctx, projectID, poolID)
		if err != nil {
			t.Fatalf("get pool: %v", err)
		}
		last = pool
		if pool.Phase == model.PoolPhaseRegistering && pool.LastOperationStatus == model.OperationStatusSuccess {
			return pool
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for registering pool; last pool = %#v", last)
	return nil
}

func buildDockerPoolFlowImage(ctx context.Context, t *testing.T, dockerClient *client.Client, tag string) {
	t.Helper()
	// HEALTHCHECK matters here: without one, docker.Driver's inspectHealthy
	// waits out its full noHealthWaitTimeout grace period (30s) hoping a
	// health status will eventually appear, since it can't distinguish
	// "never will" from "not yet". Real worker-agent/sandbox-agent images
	// always define a HEALTHCHECK; mirror that so this test reflects
	// realistic container readiness timing instead of always paying the
	// worst-case wait.
	dockerfile := []byte("FROM debian:13-slim\nRUN apt-get update \\\n    && apt-get install -y --no-install-recommends ca-certificates \\\n    && apt-get clean \\\n    && rm -rf /var/lib/apt/lists/*\nHEALTHCHECK --interval=1s --timeout=1s --start-period=1s --retries=1 CMD [\"true\"]\nCMD [\"sleep\", \"300\"]\n")
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

func listDockerProviderContainers(t *testing.T, dockerClient *client.Client, providerID string) []container.Summary {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	filterArgs := client.Filters{}
	filterArgs = filterArgs.Add("label", "discobox.provider_instance_id="+providerID)
	filterArgs = filterArgs.Add("label", "discobox.provider_type=docker")
	containers, err := dockerClient.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filterArgs})
	if err != nil {
		t.Fatalf("list provider containers: %v", err)
	}
	return containers.Items
}

func cleanupDockerProviderContainers(t *testing.T, dockerClient *client.Client, providerID string) {
	t.Helper()
	containers := listDockerProviderContainers(t, dockerClient, providerID)
	for _, c := range containers {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, err := dockerClient.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
		cancel()
		if err != nil && !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "No such container") {
			t.Fatalf("remove container %s: %v", c.ID, err)
		}
	}
}
