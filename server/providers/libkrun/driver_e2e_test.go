//go:build linux && amd64

package libkrun

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/client"

	poolagent "github.com/obot-platform/discobox/pool-agent"
	"github.com/obot-platform/discobox/pool-agent/endpoint"
	guestvsock "github.com/obot-platform/discobox/pool-agent/vsock"
	"github.com/obot-platform/discobox/server/internal/model"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/providers/dockerworker"
)

const (
	libkrunE2ERootImageEnv = "DISCOBOX_LIBKRUN_E2E_ROOT_IMAGE"
	libkrunE2EKernelEnv    = "DISCOBOX_LIBKRUN_E2E_KERNEL"
	libkrunE2ELauncherEnv  = "DISCOBOX_LIBKRUN_E2E_LAUNCHER"
	libkrunE2EPoolImageEnv = "DISCOBOX_LIBKRUN_E2E_POOL_IMAGE"
	libkrunE2EDockerEnv    = "DISCOBOX_LIBKRUN_E2E_DOCKER"
)

// TestLibkrunEndToEnd exercises the real KVM, libkrun, passt, VSOCK, storage,
// Docker, and pool-agent path. It is opt-in because it requires /dev/kvm and
// host-built VM and pool-agent images.
func TestLibkrunEndToEnd(t *testing.T) {
	rootImage := requireE2EEnv(t, libkrunE2ERootImageEnv)
	kernelImage := requireE2EEnv(t, libkrunE2EKernelEnv)
	launcher := requireE2EEnv(t, libkrunE2ELauncherEnv)
	poolImage := requireE2EValue(t, libkrunE2EPoolImageEnv)
	dockerCLI := strings.TrimSpace(os.Getenv(libkrunE2EDockerEnv))
	if dockerCLI == "" {
		var err error
		dockerCLI, err = exec.LookPath("docker")
		if err != nil {
			t.Skipf("libkrun E2E requires docker CLI: %v", err)
		}
	}

	testRoot, err := os.MkdirTemp("", "discobox-libkrun-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(testRoot); err != nil {
			t.Logf("remove E2E directory: %v", err)
		}
	})

	controlPlaneSocket := filepath.Join(testRoot, "control.sock")
	var listenConfig net.ListenConfig
	controlPlaneListener, err := listenConfig.Listen(context.Background(), "unix", controlPlaneSocket)
	if err != nil {
		t.Fatal(err)
	}
	var registrations atomic.Int32
	var statusUpdates atomic.Int32
	controlPlaneServer := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodPost && r.URL.Path == "/api/pools/register":
				registrations.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(poolagent.RegisterResponse{}); err != nil {
					t.Errorf("encode pool registration response: %v", err)
				}
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/status"):
				statusUpdates.Add(1)
				w.WriteHeader(http.StatusNoContent)
			default:
				http.Error(w, "unexpected E2E control-plane request", http.StatusNotFound)
			}
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := controlPlaneServer.Serve(controlPlaneListener); err != nil && err != http.ErrServerClosed {
			t.Errorf("control-plane server: %v", err)
		}
	}()
	t.Cleanup(func() {
		_ = controlPlaneServer.Close()
		_ = controlPlaneListener.Close()
	})

	driver, err := NewDriver(DriverConfig{
		RootImage:          rootImage,
		KernelImage:        kernelImage,
		StateDir:           filepath.Join(testRoot, "state"),
		RuntimeDir:         filepath.Join(testRoot, "run"),
		ControlPlaneSocket: controlPlaneSocket,
		LauncherPath:       launcher,
		VCPUs:              2,
		MemoryMiB:          1024,
		DataDiskGiB:        1,
		CacheDiskGiB:       1,
	})
	if err != nil {
		t.Fatal(err)
	}
	const poolID = "pool-e2e"
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := driver.DeleteVM(cleanupCtx, poolID); err != nil {
			t.Logf("delete E2E VM: %v", err)
		}
	})

	hostInterfaces := interfaceNames(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	info, err := driver.EnsureVM(ctx, poolID, dockerworker.VMSpec{Name: "discobox-libkrun-e2e"})
	if err != nil {
		t.Fatalf("boot VM: %v%s", err, launcherDiagnostics(driver, poolID))
	}
	if info.Status != sandbox.StatusRunning {
		t.Fatalf("VM status = %s, want running", info.Status)
	}
	waitForGuestDocker(ctx, t, driver, poolID)
	if got := addedStrings(hostInterfaces, interfaceNames(t)); len(got) != 0 {
		t.Fatalf("VM boot added host interfaces: %v", got)
	}

	dockerSocket := filepath.Join(driver.poolRuntimeDir(poolID), "docker.sock")
	loadImageIntoGuest(ctx, t, dockerCLI, dockerSocket, poolImage)
	loadImageIntoGuest(ctx, t, dockerCLI, dockerSocket, "busybox:1.37.0")
	guestDocker(ctx, t, dockerCLI, dockerSocket, "image", "rm", "busybox:1.37.0")
	guestDocker(ctx, t, dockerCLI, dockerSocket, "pull", "busybox:1.37.0")
	guestDocker(ctx, t, dockerCLI, dockerSocket,
		"run", "--rm", "busybox:1.37.0", "sh", "-ec",
		`nslookup registry-1.docker.io >/dev/null && nc -z -w 10 registry-1.docker.io 443`)
	guestDocker(ctx, t, dockerCLI, dockerSocket,
		"run", "--rm", "--network", "none",
		"--mount", "type=bind,src=/,dst=/host",
		"--mount", "type=bind,src=/var/lib/discobox,dst=/data",
		"--mount", "type=bind,src=/var/lib/discobox/cache,dst=/cache",
		"--mount", "type=bind,src=/var/lib/docker,dst=/docker",
		"busybox:1.37.0", "sh", "-ec", strings.Join([]string{
			`root_device=$(stat -c %d /host)`,
			`data_device=$(stat -c %d /data)`,
			`cache_device=$(stat -c %d /cache)`,
			`docker_device=$(stat -c %d /docker)`,
			`test "$root_device" != "$data_device"`,
			`test "$data_device" != "$cache_device"`,
			`test "$data_device" = "$docker_device"`,
			`printf data-persisted > /data/libkrun-e2e-data`,
			`printf cache-persisted > /cache/libkrun-e2e-cache`,
			`if touch /host/libkrun-e2e-root-write 2>/dev/null; then exit 42; fi`,
		}, "\n"))

	controlPlanePublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapTokenBytes := make([]byte, 32)
	if _, err := rand.Read(bootstrapTokenBytes); err != nil {
		t.Fatal(err)
	}
	bootstrapToken := base64.RawURLEncoding.EncodeToString(bootstrapTokenBytes)
	engine, err := dockerworker.New(dockerworker.Config{
		ControlPlaneURL:    endpoint.VSOCKURL(guestvsock.HostCID, controlPlaneVSOCKPort),
		AgentListenURL:     endpoint.VSOCKListenURL(agentVSOCKPort),
		Image:              poolImage,
		Systemd:            true,
		DockerReadyTimeout: 90 * time.Second,
	}, driver)
	if err != nil {
		t.Fatal(err)
	}
	provider := &model.SandboxProviderInstance{ID: "provider-e2e", ProjectID: "project-e2e", Type: ProviderType}
	pool := &model.Pool{ID: poolID, ProjectID: "project-e2e", ProviderInstanceID: provider.ID}
	mint := func(context.Context) (poolagent.Bootstrap, error) {
		return poolagent.Bootstrap{
			Token:           bootstrapToken,
			ControlPlaneKey: base64.StdEncoding.EncodeToString(controlPlanePublic),
		}, nil
	}
	if err := engine.EnsurePool(ctx, nil, provider, pool, mint); err != nil {
		t.Fatalf("start pool agent: %v%s", err, launcherDiagnostics(driver, poolID))
	}
	waitForPoolAgent(ctx, t, driver, poolID, &registrations, &statusUpdates)
	if output := guestDocker(ctx, t, dockerCLI, dockerSocket,
		"inspect", "--format", "{{json .HostConfig.PortBindings}}", dockerworker.ContainerName(poolID)); strings.TrimSpace(output) != "null" && strings.TrimSpace(output) != "{}" {
		t.Fatalf("pool-agent published ports = %s, want null or empty", strings.TrimSpace(output))
	}

	if err := driver.StopVM(ctx, poolID); err != nil {
		t.Fatalf("gracefully stop VM: %v%s", err, launcherDiagnostics(driver, poolID))
	}
	if info, err := driver.InspectVM(ctx, poolID); err != nil || info.Status != sandbox.StatusStopped {
		t.Fatalf("stopped VM inspect = %#v, %v", info, err)
	}
	if _, err := driver.EnsureVM(ctx, poolID, dockerworker.VMSpec{Name: "discobox-libkrun-e2e"}); err != nil {
		t.Fatalf("restart VM: %v%s", err, launcherDiagnostics(driver, poolID))
	}
	waitForGuestDocker(ctx, t, driver, poolID)
	guestDocker(ctx, t, dockerCLI, dockerSocket,
		"run", "--rm", "--network", "none",
		"--mount", "type=bind,src=/var/lib/discobox,dst=/data",
		"--mount", "type=bind,src=/var/lib/discobox/cache,dst=/cache",
		"busybox:1.37.0", "sh", "-ec",
		`test "$(cat /data/libkrun-e2e-data)" = data-persisted && test "$(cat /cache/libkrun-e2e-cache)" = cache-persisted`)

	if err := engine.EnsurePool(ctx, nil, provider, pool, mint); err != nil {
		t.Fatalf("restore pool agent after VM restart: %v%s", err, launcherDiagnostics(driver, poolID))
	}
	waitForPoolAgent(ctx, t, driver, poolID, &registrations, &statusUpdates)
}

func requireE2EEnv(t *testing.T, name string) string {
	t.Helper()
	value := requireE2EValue(t, name)
	absolute, err := filepath.Abs(value)
	if err != nil {
		t.Fatal(err)
	}
	return absolute
}

func requireE2EValue(t *testing.T, name string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		t.Skipf("set %s to run the libkrun E2E test", name)
	}
	return value
}

func interfaceNames(t *testing.T) []string {
	t.Helper()
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(interfaces))
	for _, iface := range interfaces {
		names = append(names, iface.Name)
	}
	sort.Strings(names)
	return names
}

func addedStrings(before, after []string) []string {
	existing := make(map[string]struct{}, len(before))
	for _, value := range before {
		existing[value] = struct{}{}
	}
	var added []string
	for _, value := range after {
		if _, ok := existing[value]; !ok {
			added = append(added, value)
		}
	}
	return added
}

func waitForGuestDocker(ctx context.Context, t *testing.T, driver *Driver, poolID string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		lease, err := driver.AcquireDockerClient(ctx, poolID)
		if err == nil {
			_, lastErr = lease.Client.Ping(ctx, client.PingOptions{})
			lease.Release()
			if lastErr == nil {
				return
			}
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
	t.Fatalf("guest Docker did not become ready: %v%s", lastErr, launcherDiagnostics(driver, poolID))
}

func waitForPoolAgent(ctx context.Context, t *testing.T, driver *Driver, poolID string, registrations, statusUpdates *atomic.Int32) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		lease, err := driver.AcquirePoolAgentClient(ctx, poolID)
		if err == nil {
			request, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, lease.BaseURL+"/healthz", nil)
			if requestErr != nil {
				lease.Release()
				t.Fatal(requestErr)
			}
			response, requestErr := lease.Client.Do(request)
			if requestErr == nil {
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
				if response.StatusCode == http.StatusOK && registrations.Load() > 0 && statusUpdates.Load() > 0 {
					lease.Release()
					return
				}
				lastErr = fmt.Errorf("health status %s, registrations=%d, status updates=%d", response.Status, registrations.Load(), statusUpdates.Load())
			} else {
				lastErr = requestErr
			}
			lease.Release()
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
	t.Fatalf("pool agent did not become ready over VSOCK: %v%s", lastErr, launcherDiagnostics(driver, poolID))
}

func loadImageIntoGuest(ctx context.Context, t *testing.T, dockerCLI, guestSocket, image string) {
	t.Helper()
	save := exec.CommandContext(ctx, dockerCLI, "image", "save", image)
	stream, err := save.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var saveErr bytes.Buffer
	save.Stderr = &saveErr

	load := exec.CommandContext(ctx, dockerCLI, "--host", "unix://"+guestSocket, "image", "load") //nolint:gosec // The opt-in E2E test resolves the explicitly configured Docker CLI.
	load.Stdin = stream
	var loadOutput bytes.Buffer
	load.Stdout = &loadOutput
	load.Stderr = &loadOutput

	if err := save.Start(); err != nil {
		t.Fatal(err)
	}
	if err := load.Start(); err != nil {
		_ = save.Process.Kill()
		_ = save.Wait()
		t.Fatal(err)
	}
	saveWaitErr := save.Wait()
	loadWaitErr := load.Wait()
	if saveWaitErr != nil || loadWaitErr != nil {
		t.Fatalf("load %s into guest: save=%v (%s), load=%v (%s)", image, saveWaitErr, strings.TrimSpace(saveErr.String()), loadWaitErr, strings.TrimSpace(loadOutput.String()))
	}
	t.Log(strings.TrimSpace(loadOutput.String()))
}

func guestDocker(ctx context.Context, t *testing.T, dockerCLI, guestSocket string, args ...string) string {
	t.Helper()
	commandArgs := append([]string{"--host", "unix://" + guestSocket}, args...)
	output, err := exec.CommandContext(ctx, dockerCLI, commandArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("guest docker %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

func launcherDiagnostics(driver *Driver, poolID string) string {
	runtimeDir := driver.poolRuntimeDir(poolID)
	var output strings.Builder
	for _, name := range []string{"launcher.log", "console.log", "passt.log"} {
		data, err := os.ReadFile(filepath.Join(runtimeDir, name))
		if err == nil && len(data) > 0 {
			fmt.Fprintf(&output, "\n--- %s ---\n%s", name, data)
		}
	}
	return output.String()
}
