package docker

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/google/uuid"
	"github.com/moby/moby/client"

	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/providers/dockerworker"
)

// TestDockerIntegrationPoolConsole covers the console end to end on a real
// daemon: it is created in the host's namespaces, it runs a shell an operator
// can type into, it survives a detach, and pool teardown removes it.
//
// It deliberately does not create a pool runtime first. A console must open on
// a host whose pool never came up, which is the case an operator opens one for.
func TestDockerIntegrationPoolConsole(t *testing.T) {
	if os.Getenv(dockerIntegrationEnv) != "1" {
		t.Skipf("set %s=1 to run Docker integration tests", dockerIntegrationEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	image := os.Getenv("DISCOBOX_CONSOLE_TEST_IMAGE")
	if image == "" {
		image = "debian:13-slim"
	}
	driver, err := NewLocalDriver(ctx, "", defaultAgentPort)
	if err != nil {
		t.Fatalf("new local driver: %v", err)
	}
	t.Cleanup(func() { _ = driver.Close() })
	engine, err := dockerworker.New(dockerworker.Config{ControlPlaneURL: "http://control.example", Image: image}, driver)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	t.Cleanup(func() { _ = engine.Close() })

	project := &model.Project{ID: "project-" + uuid.NewString()}
	provider := &model.SandboxProviderInstance{ID: "provider-" + uuid.NewString(), ProjectID: project.ID}
	pool := &model.Pool{ID: "pool-" + uuid.NewString(), ProjectID: project.ID, PoolManifest: model.PoolManifest{Name: "pool", ProviderInstanceID: provider.ID}}

	dockerClient, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatalf("new docker client: %v", err)
	}
	t.Cleanup(func() { _ = dockerClient.Close() })
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_, _ = dockerClient.ContainerRemove(cleanupCtx, dockerworker.ConsoleContainerName(pool.ID), client.ContainerRemoveOptions{Force: true})
	})

	console, err := engine.OpenConsole(ctx, provider, pool, sandbox.ConsoleOptions{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatalf("open console: %v", err)
	}

	inspect, err := dockerClient.ContainerInspect(ctx, dockerworker.ConsoleContainerName(pool.ID), client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect console: %v", err)
	}
	if inspect.Container.HostConfig == nil || inspect.Container.HostConfig.PidMode != "host" || !inspect.Container.HostConfig.Privileged {
		t.Fatalf("console host config = %#v", inspect.Container.HostConfig)
	}
	firstID := inspect.Container.ID

	if _, err := console.Write([]byte("echo discobox-console-$((6*7))\n")); err != nil {
		t.Fatalf("write to console: %v", err)
	}
	// The shell echoes the typed line back before running it, so the arithmetic
	// result is what proves a shell actually ran rather than a line bouncing off
	// the TTY.
	if !readConsoleUntil(t, console, "discobox-console-42") {
		t.Fatal("console never produced the shell's output")
	}
	if err := console.Close(); err != nil {
		t.Fatalf("close console: %v", err)
	}

	// Detaching must leave the console running: a capture or trace started in it
	// outlives the operator's terminal, and the next console is the same shell.
	reattached, err := engine.OpenConsole(ctx, provider, pool, sandbox.ConsoleOptions{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatalf("reopen console: %v", err)
	}
	defer reattached.Close()
	inspect, err = dockerClient.ContainerInspect(ctx, dockerworker.ConsoleContainerName(pool.ID), client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect reattached console: %v", err)
	}
	if inspect.Container.ID != firstID {
		t.Fatalf("reattach created a new console container %q, want %q", inspect.Container.ID, firstID)
	}
	if inspect.Container.State == nil || !inspect.Container.State.Running {
		t.Fatal("console container stopped when the first session detached")
	}
	_ = reattached.Close()

	if err := engine.RemovePool(ctx, project, provider, pool); err != nil {
		t.Fatalf("remove pool: %v", err)
	}
	if _, err := dockerClient.ContainerInspect(ctx, dockerworker.ConsoleContainerName(pool.ID), client.ContainerInspectOptions{}); !cerrdefs.IsNotFound(err) {
		t.Fatalf("console container survived pool teardown: %v", err)
	}
}

// readConsoleUntil reads the console's TTY until want appears or the read
// blocks past the deadline.
func readConsoleUntil(t *testing.T, console sandbox.PTY, want string) bool {
	t.Helper()
	seen := make(chan string, 1)
	go func() {
		var buffer strings.Builder
		chunk := make([]byte, 4096)
		for {
			n, err := console.Read(chunk)
			if n > 0 {
				buffer.Write(chunk[:n])
				if strings.Contains(buffer.String(), want) {
					seen <- buffer.String()
					return
				}
			}
			if err != nil {
				seen <- buffer.String()
				return
			}
		}
	}()
	select {
	case output := <-seen:
		return strings.Contains(output, want)
	case <-time.After(30 * time.Second):
		return false
	}
}
