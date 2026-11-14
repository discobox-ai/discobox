package dockerworker

import (
	"bytes"
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"

	"github.com/discobox-ai/discobox/server/internal/model"
)

// A pool's container boots the sandbox base image with the host's Docker
// socket bound in over a tmpfs /run; systemd in it must still come up with no
// failed unit, and the host's daemon must still answer through that socket. It
// boots a privileged container from the engine's own spec on the local
// daemon, so it runs only when pointed at an image there.
func TestPoolContainerBootsWithNoFailedUnitsE2E(t *testing.T) {
	image := os.Getenv("DISCOBOX_TEST_POOL_BOOT_IMAGE")
	if image == "" {
		t.Skip("set DISCOBOX_TEST_POOL_BOOT_IMAGE to a sandbox base image on the local Docker daemon")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	cli, err := NewDockerClientForHost("unix://" + dockerSocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	pool := &model.Pool{ID: "pool-boot-test-" + strconv.FormatInt(time.Now().UnixNano(), 36)}
	engine := newTestEngine(t, Config{SandboxBaseImage: image, HostStateRoot: t.TempDir()})
	config, hostConfig, _, err := engine.poolContainerSpec(pool, image, nil)
	if err != nil {
		t.Fatal(err)
	}
	created, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{Config: config, HostConfig: hostConfig})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup := context.WithoutCancel(ctx)
		_, _ = cli.ContainerRemove(cleanup, created.ID, client.ContainerRemoveOptions{Force: true})
		_, _ = cli.VolumeRemove(cleanup, poolScopedVolumeName(pool.ID, "docker"), client.VolumeRemoveOptions{Force: true})
	}()
	if _, err := cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatal(err)
	}

	exec := func(args ...string) (string, int) {
		t.Helper()
		ex, err := cli.ExecCreate(ctx, created.ID, client.ExecCreateOptions{Cmd: args, AttachStdout: true, AttachStderr: true})
		if err != nil {
			t.Fatal(err)
		}
		att, err := cli.ExecAttach(ctx, ex.ID, client.ExecAttachOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer att.Close()
		var out bytes.Buffer
		_, _ = stdcopy.StdCopy(&out, &out, att.Reader)
		inspect, err := cli.ExecInspect(ctx, ex.ID, client.ExecInspectOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(out.String()), inspect.ExitCode
	}
	state := ""
	for ctx.Err() == nil {
		state, _ = exec("systemctl", "is-system-running")
		if state == "running" || state == "degraded" {
			break
		}
		time.Sleep(time.Second)
	}
	if state != "running" {
		failed, _ := exec("systemctl", "--failed", "--no-legend", "--plain")
		t.Fatalf("pool container system state = %q, want running; failed units:\n%s", state, failed)
	}
	if out, code := exec("docker", "version"); code != 0 {
		t.Fatalf("docker version in pool container exited %d, want the host daemon to answer:\n%s", code, out)
	}
}
