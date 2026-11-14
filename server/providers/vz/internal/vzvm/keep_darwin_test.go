//go:build darwin && cgo

package vzvm

import (
	"os"
	"runtime"
	"testing"
	"time"
	"weak"

	"github.com/Code-Hex/vz/v3"
)

// TestDroppedVMIsHeldUntilItStops drops every reference to a VM whose guest is
// still powering off and collects garbage until the machine is gone. Before
// the machine was kept until it settled, the binding's finalizer deleted the
// state observer's handle while the guest was still stopping, and the stop
// that followed panicked the process with "runtime/cgo: misuse of an invalid
// Handle" (#1114). That is how this test fails: the process dies.
//
// It boots a real guest, so it is opt-in and needs a signed test binary: run
// it through `go tool task test:vz-teardown`.
func TestDroppedVMIsHeldUntilItStops(t *testing.T) {
	guestDir := os.Getenv(stressGuestEnv)
	if guestDir == "" {
		t.Skipf("set %s to a guest image directory", stressGuestEnv)
	}
	dir, err := os.MkdirTemp("/tmp", "vzk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	machine := startAndForget(t, guestDir, dir)

	// settle outlasts the guest's power-off, so a handle deleted early is
	// looked up — and panics — inside the loop rather than after the test.
	const settle, limit = time.Minute, 3 * time.Minute
	start := time.Now()
	for time.Since(start) < settle || machine.Value() != nil {
		if time.Since(start) > limit {
			t.Fatalf("the machine was still held %s after its guest was asked to stop", limit)
		}
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
	t.Logf("machine released, and no state change panicked, %s after the stop request", time.Since(start).Round(time.Second))
}

// startAndForget boots a guest, asks it to power off, and drops the VM before
// it has: what Close did when it could not stop the machine, leaving the
// framework with state changes still to deliver. It returns only a weak
// reference, so nothing in the test keeps the machine alive.
func startAndForget(t *testing.T, guestDir, dir string) weak.Pointer[vz.VirtualMachine] {
	t.Helper()
	vm, err := Start(stressGuestOptions(t, "vz-teardown", guestDir, dir))
	if err != nil {
		t.Fatal(err)
	}
	// The guest must be far enough up to act on a stop request.
	waitStressDocker(t, vm, 5*time.Minute)
	machine := weak.Make(vm.machine)
	if err := vm.RequestStop(); err != nil {
		t.Fatal(err)
	}
	return machine
}
