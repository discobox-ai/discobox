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

	"github.com/google/uuid"
	"github.com/moby/moby/client"

	"github.com/obot-platform/discobox/internal/sandbox"
	"github.com/obot-platform/discobox/internal/sandbox/vm"
)

const dockerIntegrationEnv = "DISCOBOX_DOCKER_INTEGRATION"

func TestDockerIntegrationLifecycle(t *testing.T) {
	if os.Getenv(dockerIntegrationEnv) != "1" {
		t.Skipf("set %s=1 to run Docker integration tests", dockerIntegrationEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	image := os.Getenv("DISCOBOX_DOCKER_TEST_IMAGE")
	if image == "" {
		image = "discobox-docker-sleep:test-" + uuid.NewString()
		buildDockerImage(t, ctx, "testdata/sleep/Dockerfile", image)
	}

	driver, err := NewDriver(ctx, Config{Image: image, Command: []string{"sleep", "300"}})
	if err != nil {
		t.Fatalf("new driver: %v", err)
	}
	ref := sandbox.SandboxRef{TenantID: "tenant-" + uuid.NewString(), ProjectID: "project-" + uuid.NewString(), SandboxID: "sandbox-" + uuid.NewString()}
	inst, err := driver.CreateVM(ctx, vm.InstanceSpec{
		Ref:   ref,
		Name:  "integration-" + uuid.NewString(),
		Image: image,
		Boot: vm.BootConfig{Env: map[string]string{
			"DISCOBOX_TENANT_ID": ref.TenantID,
			"DISCOBOX_WORKER_ID": "worker-" + uuid.NewString(),
		}},
		Metadata: map[string]string{"test": "docker"},
	})
	if err != nil {
		t.Fatalf("create vm: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_ = driver.DeleteVM(cleanupCtx, inst.ID, true)
	})
	if inst.Status != sandbox.StatusRunning {
		t.Fatalf("created status = %q", inst.Status)
	}
	if inst.Metadata[labelTenantID] != ref.TenantID {
		t.Fatalf("tenant label = %q", inst.Metadata[labelTenantID])
	}
	if inst.AgentURL == "" {
		t.Fatalf("agent URL was not assigned")
	}
	got, err := driver.InspectVM(ctx, inst.ID)
	if err != nil {
		t.Fatalf("inspect vm: %v", err)
	}
	if got.Status != sandbox.StatusRunning {
		t.Fatalf("inspect status = %q", got.Status)
	}
	stopped, err := driver.StopVM(ctx, inst.ID, 5*time.Second)
	if err != nil {
		t.Fatalf("stop vm: %v", err)
	}
	if stopped.Status == sandbox.StatusRunning {
		t.Fatalf("stopped status = %q", stopped.Status)
	}
	started, err := driver.StartVM(ctx, inst.ID)
	if err != nil {
		t.Fatalf("start vm: %v", err)
	}
	if started.Status != sandbox.StatusRunning {
		t.Fatalf("started status = %q", started.Status)
	}
	if err := driver.DeleteVM(ctx, inst.ID, true); err != nil {
		t.Fatalf("delete vm: %v", err)
	}
	if _, err := driver.InspectVM(ctx, inst.ID); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("inspect after delete err = %v, want ErrNotFound", err)
	}
}

func TestDockerIntegrationSystemdContainer(t *testing.T) {
	if os.Getenv(dockerIntegrationEnv) != "1" {
		t.Skipf("set %s=1 to run Docker integration tests", dockerIntegrationEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	image := os.Getenv("DISCOBOX_DOCKER_SYSTEMD_IMAGE")
	if image == "" {
		image = "discobox-docker-systemd:test-" + uuid.NewString()
		buildDockerImage(t, ctx, "testdata/systemd/Dockerfile", image)
	}

	driver, err := NewDriver(ctx, Config{Image: image, Systemd: true, CgroupNSMode: "host"})
	if err != nil {
		t.Fatalf("new systemd driver: %v", err)
	}
	inst, err := driver.CreateVM(ctx, vm.InstanceSpec{
		Ref:  sandbox.SandboxRef{TenantID: "tenant-" + uuid.NewString(), ProjectID: "project-" + uuid.NewString(), SandboxID: "sandbox-" + uuid.NewString()},
		Name: "systemd-" + uuid.NewString(),
		Boot: vm.BootConfig{Env: map[string]string{"DISCOBOX_WORKER_ID": "worker-" + uuid.NewString()}},
	})
	if err != nil {
		t.Fatalf("create systemd vm: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_ = driver.DeleteVM(cleanupCtx, inst.ID, true)
	})
	if inst.Status != sandbox.StatusRunning {
		t.Fatalf("systemd status = %q", inst.Status)
	}
}

func buildDockerImage(t *testing.T, ctx context.Context, dockerfilePath, tag string) {
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
