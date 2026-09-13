package wslc

import (
	"context"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/providers/dockerworker"
)

// TestGuestHostIPCContainerStartsE2E checks that a wslc guest can start a
// container in the host's IPC namespace, which is what the pool console asks
// for on every backend. The stock guest image has no /dev/shm, and Docker
// refuses --ipc=host without one: "/dev/shm is not mounted, but must be for
// --ipc=host". Both the precondition Docker checks and the start itself are
// asserted, so a failure says which one broke.
//
//	$env:DISCOBOX_WSLC_E2E="1"; go test -run TestGuestHostIPCContainerStartsE2E -v ./providers/wslc/
func TestGuestHostIPCContainerStartsE2E(t *testing.T) {
	if os.Getenv("DISCOBOX_WSLC_E2E") != "1" {
		t.Skip("set DISCOBOX_WSLC_E2E=1 to run the real wslc VM e2e test")
	}

	driver, err := NewDriver(DriverConfig{CPUCount: 2, MemoryMiB: 2048})
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	t.Cleanup(func() { _ = driver.Close() })

	const poolID = "host-ipc-e2e"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if _, err := driver.EnsureVM(ctx, poolID, dockerworker.VMSpec{}); err != nil {
		t.Fatalf("EnsureVM: %v", err)
	}
	session, err := driver.session(poolID)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	guest := func(script string) string {
		t.Helper()
		var out []byte
		err := runGuestCommand(ctx, session, script, func(conn net.Conn) error {
			var readErr error
			out, readErr = io.ReadAll(conn)
			return readErr
		})
		if err != nil {
			t.Fatalf("%s: %v", script, err)
		}
		return strings.TrimSpace(string(out))
	}

	if mounts := guest("grep ' /dev/shm ' /proc/mounts || true"); !strings.Contains(mounts, "tmpfs") {
		t.Fatalf("/dev/shm is not a tmpfs mount in the guest after EnsureVM (mount table line: %q)", mounts)
	}

	// busybox is pulled rather than assumed: this VM's storage is ephemeral, so
	// it starts with no images at all.
	answer := guest("docker run --rm --ipc=host busybox echo host-ipc-ok")
	if !strings.Contains(answer, "host-ipc-ok") {
		t.Fatalf("a --ipc=host container did not run in the guest; docker said:\n%s", answer)
	}
}
