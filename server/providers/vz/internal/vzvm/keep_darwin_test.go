//go:build darwin && cgo

package vzvm

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"weak"

	"github.com/Code-Hex/vz/v3"
)

// These boot a real guest, so they are opt-in and need a signed test binary:
// run them through `go tool task test:vz-teardown`.

// TestDroppedVMIsHeldUntilItStops drops every reference to a VM whose guest is
// still powering off and collects garbage until the machine is gone: it must
// be released once the guest has stopped, and no state change may panic.
//
// The guest is stopped the way the driver stops it, through its lifecycle
// service over VSOCK. The pool guest has no power button (no gpio_keys, and
// systemd-logind is masked), so a RequestStop alone never stops it.
func TestDroppedVMIsHeldUntilItStops(t *testing.T) {
	guestDir := teardownGuest(t)
	dir := teardownDir(t)

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

// TestDroppedVMWithAStopInFlightIsHeldUntilItFails is #1114's crash: a VM
// whose service is unresponsive, with a stop in flight, is dropped, and the
// framework later moves it to Error (state 3). The framework's queued stop
// retains the native machine while nothing in Go does, so before machines
// were kept until they settled, the binding's finalizer deleted the state
// observer's handle and the Error that followed panicked the process with
// "runtime/cgo: misuse of an invalid Handle" in changeStateOnObserver. Before
// that fix this test fails at once, the machine released within milliseconds
// of the drop; without that check, the SIGKILL below panics the process with
// #1114's trace.
//
// The service is the VM's Virtualization XPC process: SIGSTOP makes it
// unresponsive, as a hung guest's was, and SIGKILL makes the framework report
// the machine's Error.
func TestDroppedVMWithAStopInFlightIsHeldUntilItFails(t *testing.T) {
	guestDir := teardownGuest(t)
	dir := teardownDir(t)

	before := vzServices(t)
	vm, err := Start(stressGuestOptions(t, "vz-teardown-error", guestDir, dir))
	if err != nil {
		t.Fatal(err)
	}
	waitStressDocker(t, vm, 5*time.Minute)
	service := newVZService(t, before)
	t.Cleanup(func() { _ = syscall.Kill(service, syscall.SIGKILL) })

	if err := syscall.Kill(service, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	machine := weak.Make(vm.machine)
	// The goroutine stays blocked on the stop's completion without keeping
	// the wrapper reachable.
	go func(m *vz.VirtualMachine) { _ = m.Stop() }(vm.machine)
	time.Sleep(500 * time.Millisecond)
	vm = nil

	// A dropped wrapper is finalized at the first collection after the drop.
	const held = 5 * time.Second
	start := time.Now()
	for time.Since(start) < held {
		if machine.Value() == nil {
			t.Fatalf("the machine was released %s after it was dropped, with its stop still in flight", time.Since(start).Round(time.Millisecond))
		}
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}

	if err := syscall.Kill(service, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	killed := time.Now()
	const limit = 30 * time.Second
	for machine.Value() != nil {
		if time.Since(killed) > limit {
			t.Fatalf("the machine was still held %s after its service died", limit)
		}
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
	// Let any late state change arrive while the test still collects.
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
	t.Logf("machine released %s after its service died, and no state change panicked", time.Since(killed).Round(time.Millisecond))
}

func teardownGuest(t *testing.T) string {
	t.Helper()
	guestDir := os.Getenv(stressGuestEnv)
	if guestDir == "" {
		t.Skipf("set %s to a guest image directory", stressGuestEnv)
	}
	return guestDir
}

func teardownDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "vzk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
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
	requestStressShutdown(t, vm)
	return machine
}

// requestStressShutdown asks the guest's lifecycle service to power off, as
// the vz driver's StopVM does first.
func requestStressShutdown(t *testing.T, vm *VM) {
	t.Helper()
	transport := &http.Transport{DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
		return vm.Connect(stressLifecyclePort)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second}
	response, err := client.Post("http://lifecycle.local/shutdown", "", nil)
	if err != nil {
		t.Fatalf("ask the guest to power off: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("ask the guest to power off: %s", response.Status)
	}
}

// vzServices lists the Virtualization XPC processes running now.
func vzServices(t *testing.T) map[int]bool {
	t.Helper()
	out, _ := exec.Command("/usr/bin/pgrep", "-f", "com.apple.Virtualization.VirtualMachine").Output()
	pids := map[int]bool{}
	for _, field := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(field); err == nil {
			pids[pid] = true
		}
	}
	return pids
}

// newVZService is the one Virtualization XPC process that was not running
// before: the service of the VM the test started.
func newVZService(t *testing.T, before map[int]bool) int {
	t.Helper()
	var found []int
	for pid := range vzServices(t) {
		if !before[pid] {
			found = append(found, pid)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want the one Virtualization service the VM started, found %v", found)
	}
	return found[0]
}
