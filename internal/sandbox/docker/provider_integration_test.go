package docker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/obot-platform/disco2/internal/sandbox"
)

const dockerIntegrationEnv = "DISCO2_DOCKER_INTEGRATION"

func TestDockerProviderIntegrationLifecycle(t *testing.T) {
	provider, image := newIntegrationProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	ref := sandbox.SandboxRef{
		SandboxID: "integration-" + uuid.NewString(),
		ProjectID: "project-" + uuid.NewString(),
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_, _ = provider.Remove(cleanupCtx, ref, nil, sandbox.RemoveVolumes())
		_ = provider.ClearCache(cleanupCtx, ref.ProjectID)
	})

	pullImage(t, ctx, provider, image)
	events, err := provider.Watch(ctx)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("workspace"), 0o600); err != nil {
		t.Fatalf("write workspace file: %v", err)
	}

	created, state, err := provider.Create(ctx, ref, nil, sandbox.CreateOptions{
		Image:         image,
		WorkspacePath: workspace,
		Env: map[string]string{
			"DISCO2_TEST":      "true",
			"DISCO2_TRUST_KEY": "public-key",
		},
		Labels: map[string]string{
			"disco2.integration_test": "true",
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(state) != 0 {
		t.Fatalf("state = %q, want empty", string(state))
	}
	if created.SandboxID != ref.SandboxID || created.Status != sandbox.StatusCreated {
		t.Fatalf("created sandbox = %#v", created)
	}
	volumes, err := provider.projectVolumes(ctx, ref.ProjectID)
	if err != nil {
		t.Fatalf("project volumes: %v", err)
	}
	if !containsString(volumes, dataVolumeName(ref.SandboxID)) || !containsString(volumes, cacheVolumeName(ref.ProjectID)) {
		t.Fatalf("project volumes = %#v", volumes)
	}
	inspect, err := provider.client.ContainerInspect(ctx, containerName(ref.SandboxID), client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect created container: %v", err)
	}
	if !hasMountTarget(inspect.Container.Mounts, provider.dataMountPath) ||
		!hasMountTarget(inspect.Container.Mounts, provider.cacheMountPath) ||
		!hasMountTarget(inspect.Container.Mounts, provider.workspaceMountPath) {
		t.Fatalf("mounts = %#v", inspect.Container.Mounts)
	}

	started, state, err := provider.Start(ctx, ref, state)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if started.Status != sandbox.StatusRunning {
		t.Fatalf("started status = %q, want running", started.Status)
	}
	if got := started.Env["DISCO2_TEST"]; got != "true" {
		t.Fatalf("env DISCO2_TEST = %q, want true", got)
	}
	if got := started.Env["DISCO2_TRUST_KEY"]; got != "public-key" {
		t.Fatalf("env DISCO2_TRUST_KEY = %q, want public-key", got)
	}
	if len(started.Ports) == 0 {
		t.Fatal("expected mapped agent port")
	}
	waitForStateEvent(t, events, ref.SandboxID, sandbox.StatusRunning)

	listed, err := provider.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !containsSandbox(listed, ref.SandboxID) {
		t.Fatalf("list did not include sandbox %q", ref.SandboxID)
	}

	stopped, state, err := provider.Stop(ctx, ref, state, 10*time.Second)
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if stopped.Status != sandbox.StatusStopped {
		t.Fatalf("stopped status = %q, want stopped", stopped.Status)
	}

	state, err = provider.Remove(ctx, ref, state, sandbox.RemoveVolumes())
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if state != nil {
		t.Fatalf("state after remove = %q, want nil", string(state))
	}

	if _, err := provider.Get(ctx, ref, state); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("get after remove error = %v, want ErrNotFound", err)
	}
}

func TestDockerProviderIntegrationClearCache(t *testing.T) {
	provider, image := newIntegrationProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	ref := sandbox.SandboxRef{
		SandboxID: "integration-cache-" + uuid.NewString(),
		ProjectID: "project-" + uuid.NewString(),
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_, _ = provider.Remove(cleanupCtx, ref, nil, sandbox.RemoveVolumes())
		_ = provider.ClearCache(cleanupCtx, ref.ProjectID)
	})

	pullImage(t, ctx, provider, image)
	if _, _, err := provider.Create(ctx, ref, nil, sandbox.CreateOptions{Image: image}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := provider.ClearCache(ctx, ref.ProjectID); err != nil {
		t.Fatalf("clear cache: %v", err)
	}
	if _, err := provider.Get(ctx, ref, nil); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("get after clear cache error = %v, want ErrNotFound", err)
	}
	volumes, err := provider.projectVolumes(ctx, ref.ProjectID)
	if err != nil {
		t.Fatalf("project volumes: %v", err)
	}
	if containsString(volumes, cacheVolumeName(ref.ProjectID)) {
		t.Fatalf("cache volume still exists in %#v", volumes)
	}
}

func TestDockerProviderIntegrationImageManagement(t *testing.T) {
	provider, image := newIntegrationProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	pullImage(t, ctx, provider, image)

	exists, err := provider.ImageExists(ctx, image)
	if err != nil {
		t.Fatalf("image exists: %v", err)
	}
	if !exists {
		t.Fatalf("image %q does not exist after pull", image.Name)
	}

	info, err := provider.GetImage(ctx, image)
	if err != nil {
		t.Fatalf("get image: %v", err)
	}
	if info.Status != sandbox.ImageStatusAvailable || info.ID == "" {
		t.Fatalf("image info = %#v", info)
	}
}

func newIntegrationProvider(t *testing.T) (*Provider, sandbox.ImageRef) {
	t.Helper()
	if os.Getenv(dockerIntegrationEnv) != "1" {
		t.Skipf("set %s=1 to run Docker integration tests", dockerIntegrationEnv)
	}
	imageName := strings.TrimSpace(os.Getenv("DISCO2_DOCKER_TEST_IMAGE"))
	if imageName == "" {
		imageName = "nginx:alpine"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	provider, err := New(ctx, Config{DefaultImage: imageName})
	if err != nil {
		t.Fatalf("new docker provider: %v", err)
	}
	t.Cleanup(func() {
		_ = provider.Close()
	})
	return provider, sandbox.ImageRef{Name: imageName}
}

func pullImage(t *testing.T, ctx context.Context, provider *Provider, image sandbox.ImageRef) {
	t.Helper()
	events, err := provider.PullImage(ctx, image)
	if err != nil {
		t.Fatalf("pull image: %v", err)
	}
	seenEvents := 0
	for event := range events {
		seenEvents++
		if event.Status == sandbox.ImageStatusFailed {
			t.Fatalf("pull image failed: %s", event.Error)
		}
	}
	if seenEvents == 0 {
		t.Fatal("expected image pull events")
	}
}

func containsSandbox(sandboxes []*sandbox.Sandbox, sandboxID string) bool {
	for _, runtimeSandbox := range sandboxes {
		if runtimeSandbox.SandboxID == sandboxID {
			return true
		}
	}
	return false
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func hasMountTarget(mounts []container.MountPoint, target string) bool {
	for _, mountPoint := range mounts {
		if mountPoint.Destination == target {
			return true
		}
	}
	return false
}

func waitForStateEvent(t *testing.T, events <-chan sandbox.StateEvent, sandboxID string, status sandbox.Status) {
	t.Helper()
	timeout := time.After(15 * time.Second)
	for {
		select {
		case event, ok := <-events:
			if !ok {
				t.Fatalf("watch closed before %s event", status)
			}
			if event.SandboxID == sandboxID && event.Status == status {
				return
			}
		case <-timeout:
			t.Fatalf("timed out waiting for %s event", status)
		}
	}
}
