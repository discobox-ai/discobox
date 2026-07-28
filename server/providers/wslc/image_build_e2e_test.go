package wslc

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/obot-platform/discobox/devimage"
	"github.com/obot-platform/discobox/server/providers/dockerworker"
)

// TestDevelopmentImageBuildE2E proves the development image build-mode path end
// to end on Windows: with no Docker daemon on the host, the production
// DevelopmentImageSynchronizer builds the real pool-agent image inside a wslc
// VM through that VM's embedded BuildKit, reached over the wslc bridge.
//
// The VM storage directory is deliberately stable rather than t.TempDir(), so
// the persisted /var/lib/docker keeps the BuildKit layer cache between runs —
// the same property that makes this fast for real development. The first run is
// slow (it pulls the base images and compiles the agent in the guest).
//
//	$env:DISCOBOX_WSLC_E2E="1"; go test -run TestDevelopmentImageBuildE2E -v -timeout 30m ./providers/wslc/
func TestDevelopmentImageBuildE2E(t *testing.T) {
	if os.Getenv("DISCOBOX_WSLC_E2E") != "1" {
		t.Skip("set DISCOBOX_WSLC_E2E=1 to run the real wslc VM e2e test")
	}

	repoRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "pool-agent", "Dockerfile")); err != nil {
		t.Fatalf("repo root %s does not contain pool-agent/Dockerfile: %v", repoRoot, err)
	}

	driver, err := NewDriver(DriverConfig{
		StorageDir:    filepath.Join(os.TempDir(), "discobox-wslc-imagebuild-e2e"),
		CPUCount:      4,
		MemoryMiB:     4096,
		MaxStorageMiB: 32768,
	})
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	t.Cleanup(func() { _ = driver.Close() })

	const poolID = "imagebuild-e2e"
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	if _, err := driver.EnsureVM(ctx, poolID, dockerworker.VMSpec{}); err != nil {
		t.Fatalf("EnsureVM: %v", err)
	}
	lease, err := driver.AcquireDockerClient(ctx, poolID)
	if err != nil {
		t.Fatalf("AcquireDockerClient: %v", err)
	}
	defer lease.Release()

	// A unique reference per run is what build-mode uses for freshness, mirroring
	// the dev-<timestamp> tag the image watcher writes.
	reference := "discobox-pool-agent:e2e-" + time.Now().UTC().Format("20060102150405")
	sync, err := dockerworker.NewDevelopmentImageSynchronizer([]devimage.Image{{
		Reference: reference,
		Build: &devimage.BuildSpec{
			Dockerfile: "pool-agent/Dockerfile",
			Context:    repoRoot,
			Platform:   "linux/amd64",
		},
	}})
	if err != nil {
		t.Fatalf("NewDevelopmentImageSynchronizer: %v", err)
	}

	started := time.Now()
	if err := sync.Ensure(ctx, lease.Client); err != nil {
		t.Fatalf("Ensure (build-mode): %v", err)
	}
	t.Logf("built %s in the guest in %s", reference, time.Since(started))

	inspect, err := lease.Client.ImageInspect(ctx, reference)
	if err != nil {
		t.Fatalf("built image %s is not present on the guest daemon: %v", reference, err)
	}
	if inspect.ID == "" {
		t.Fatalf("built image %s has no ID", reference)
	}

	// A second Ensure must be a no-op: the reference already exists, so nothing
	// rebuilds and — critically — no source Docker daemon is ever opened.
	secondPass := time.Now()
	if err := sync.Ensure(ctx, lease.Client); err != nil {
		t.Fatalf("second Ensure should be a no-op: %v", err)
	}
	if elapsed := time.Since(secondPass); elapsed > time.Minute {
		t.Fatalf("second Ensure took %s; it should have skipped the build", elapsed)
	}

	// The built image must actually run in the guest.
	created, err := lease.Client.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{Image: reference, Entrypoint: []string{"/usr/local/bin/discobox-pool-agent"}, Cmd: []string{"--help"}},
		Name:   "imagebuild-e2e-check",
	})
	if err != nil {
		t.Fatalf("ContainerCreate from built image: %v", err)
	}
	t.Cleanup(func() {
		_, _ = lease.Client.ContainerRemove(context.Background(), created.ID, client.ContainerRemoveOptions{Force: true})
	})
	t.Logf("created container %s from the guest-built image", created.ID[:12])
}
