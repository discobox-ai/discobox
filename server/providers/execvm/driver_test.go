package execvm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/providers/dockerworker"
)

// writeScript materializes a shell script implementing the driver protocol.
func writeScript(t *testing.T, body string) string {
	t.Helper()
	// The exec driver protocol is a program the operator supplies; these tests
	// implement it as a shell script, which needs a /bin/sh to run. Windows has
	// none and cannot execute a .sh, so the script fails before the driver
	// under test is reached. The driver itself is platform-neutral -- it only
	// runs a command -- so this skips where the fixture cannot run, not where
	// the code cannot.
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX host: the driver fixture is a shell script")
	}
	path := filepath.Join(t.TempDir(), "driver.sh")
	script := "#!/bin/sh\nop=\"$1\"\nworker=\"$2\"\n" + body
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return path
}

func newScriptDriver(t *testing.T, body string) *Driver {
	t.Helper()
	driver, err := NewDriver(DriverConfig{Command: []string{writeScript(t, body)}})
	if err != nil {
		t.Fatalf("new driver: %v", err)
	}
	return driver
}

func TestEnsureVMParsesJSONAndPassesEnv(t *testing.T) {
	driver := newScriptDriver(t, `
case "$op" in
ensure-vm)
	if [ "$DISCOBOX_POOL_ID" != "$worker" ]; then
		echo "worker env mismatch" >&2
		exit 1
	fi
	if [ -z "$DISCOBOX_VM_NAME" ] || [ -z "$DISCOBOX_VM_METADATA" ]; then
		echo "missing vm env" >&2
		exit 1
	fi
	printf '{"id":"vm-%s","status":"running","address":"203.0.113.9"}' "$worker"
	;;
*)
	exit 1
	;;
esac
`)
	info, err := driver.EnsureVM(context.Background(), "worker-1", dockerworker.VMSpec{
		Name:     "discobox-vm-worker-1",
		Metadata: map[string]string{"discobox.worker_id": "worker-1"},
	})
	if err != nil {
		t.Fatalf("ensure vm: %v", err)
	}
	if info.ID != "vm-worker-1" || info.Status != sandbox.StatusRunning || info.Address != "203.0.113.9" {
		t.Fatalf("vm info = %#v", info)
	}
}

func TestInspectVMMapsExitCode3ToNotFound(t *testing.T) {
	driver := newScriptDriver(t, `
[ "$op" = "inspect-vm" ] && exit 3
exit 1
`)
	if _, err := driver.InspectVM(context.Background(), "worker-1"); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("inspect err = %v, want ErrNotFound", err)
	}
}

func TestDeleteVMSucceeds(t *testing.T) {
	driver := newScriptDriver(t, `
[ "$op" = "delete-vm" ] && exit 0
exit 1
`)
	if err := driver.DeleteVM(context.Background(), "worker-1"); err != nil {
		t.Fatalf("delete vm: %v", err)
	}
}

func TestStopVMSucceeds(t *testing.T) {
	driver := newScriptDriver(t, `
[ "$op" = "stop-vm" ] && exit 0
exit 1
`)
	if err := driver.StopVM(context.Background(), "worker-1"); err != nil {
		t.Fatalf("stop vm: %v", err)
	}
}

func TestRunSurfacesStderrOnFailure(t *testing.T) {
	driver := newScriptDriver(t, `
echo "droplet quota exceeded" >&2
exit 1
`)
	_, err := driver.EnsureVM(context.Background(), "worker-1", dockerworker.VMSpec{})
	if err == nil || !strings.Contains(err.Error(), "droplet quota exceeded") {
		t.Fatalf("ensure err = %v, want stderr message", err)
	}
}

func TestAcquirePoolAgentClientUsesEndpointLine(t *testing.T) {
	driver := newScriptDriver(t, `
[ "$op" = "harness-endpoint" ] && { echo "http://203.0.113.9:3002"; exit 0; }
exit 1
`)
	lease, err := driver.AcquirePoolAgentClient(context.Background(), "worker-1")
	if err != nil {
		t.Fatalf("acquire worker agent client: %v", err)
	}
	defer lease.Release()
	if lease.BaseURL != "http://203.0.113.9:3002" {
		t.Fatalf("lease base URL = %q", lease.BaseURL)
	}
}

func TestAcquirePoolAgentClientRejectsNonHTTPEndpoint(t *testing.T) {
	driver := newScriptDriver(t, `
echo "203.0.113.9:3002"
`)
	if _, err := driver.AcquirePoolAgentClient(context.Background(), "worker-1"); err == nil || !strings.Contains(err.Error(), "http") {
		t.Fatalf("acquire err = %v, want http endpoint error", err)
	}
}

func TestAcquireDockerClientSupportsDirectEndpoints(t *testing.T) {
	driver := newScriptDriver(t, `
[ "$op" = "docker-endpoint" ] && { echo "tcp://203.0.113.9:2375"; exit 0; }
exit 1
`)
	lease, err := driver.AcquireDockerClient(context.Background(), "worker-1")
	if err != nil {
		t.Fatalf("acquire docker client: %v", err)
	}
	lease.Release()
}

func TestAcquireDockerClientRequiresSSHKeyForSSHEndpoints(t *testing.T) {
	driver := newScriptDriver(t, `
[ "$op" = "docker-endpoint" ] && { echo "ssh://root@203.0.113.9"; exit 0; }
exit 1
`)
	_, err := driver.AcquireDockerClient(context.Background(), "worker-1")
	if err == nil || !strings.Contains(err.Error(), "sshPrivateKey") {
		t.Fatalf("acquire err = %v, want missing ssh key error", err)
	}
}

func TestNewDriverRequiresCommand(t *testing.T) {
	if _, err := NewDriver(DriverConfig{}); err == nil {
		t.Fatal("new driver without command succeeded")
	}
}

func TestValidateRequiresCommandAndControlPlaneURL(t *testing.T) {
	if err := Validate([]byte(`{}`)); err == nil || !strings.Contains(err.Error(), "command") {
		t.Fatalf("validate err = %v, want command required", err)
	}
	if err := Validate([]byte(`{"command":"/usr/local/bin/my-vm-driver"}`)); err == nil || !strings.Contains(err.Error(), "controlPlaneUrl") {
		t.Fatalf("validate err = %v, want controlPlaneUrl required", err)
	}
	if err := Validate([]byte(`{"command":"/usr/local/bin/my-vm-driver","controlPlaneUrl":"https://control.example"}`)); err != nil {
		t.Fatalf("validate err = %v, want valid config", err)
	}
}
