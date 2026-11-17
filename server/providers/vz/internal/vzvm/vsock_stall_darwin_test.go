//go:build darwin && cgo

package vzvm

import (
	"context"
	"os"
	"runtime/pprof"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestConnectDuringGuestStallEndsWithContextAndClose dials a guest whose
// machine has stopped answering. A dial must return when its context ends,
// and closing the VM must release every dial still waiting on it, leaving no
// goroutine parked in the binding's connect.
//
// The stall is the VM's Virtualization XPC service under SIGSTOP: the
// framework then never completes a connect. Nothing gentler stands in for it.
// The framework refuses a connect to a paused machine at once, and the guest
// resets one to a port nothing listens on.
//
// It boots a real guest, so it is opt-in and needs a signed test binary: run
// it through `go tool task test:vz-stress`.
func TestConnectDuringGuestStallEndsWithContextAndClose(t *testing.T) {
	guestDir := os.Getenv(stressGuestEnv)
	if guestDir == "" {
		t.Skipf("set %s to a guest image directory", stressGuestEnv)
	}
	before := vzServices(t)
	vm := startStressGuest(t, guestDir, teardownDir(t))
	waitStressDocker(t, vm, 5*time.Minute)
	service := newVZService(t, before)
	// Registered after the guest's own cleanup, so it runs first: Close cannot
	// stop a machine whose service is stopped until the service is gone.
	t.Cleanup(func() { _ = syscall.Kill(service, syscall.SIGKILL) })
	if err := syscall.Kill(service, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}

	const patience = 2 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), patience)
	defer cancel()
	start := time.Now()
	conn, err := vm.Connect(ctx, stressDockerPort)
	if err == nil {
		_ = conn.Close()
		t.Fatal("a dial to a stalled guest succeeded")
	}
	took := time.Since(start)
	if took < patience {
		t.Fatalf("a dial to a stalled guest failed after %s, before its context ended: %v", took.Round(time.Millisecond), err)
	}
	if took > patience+3*time.Second {
		t.Fatalf("a dial outlived its context by %s", took-patience)
	}
	t.Logf("a dial to the stalled guest ended with its context after %s: %v", took.Round(time.Millisecond), err)

	const waiting = 20
	done := make(chan error, waiting)
	for range waiting {
		go func() {
			conn, err := vm.Connect(context.Background(), stressDockerPort)
			if err == nil {
				_ = conn.Close()
			}
			done <- err
		}()
	}
	time.Sleep(time.Second)
	if parked := parkedInConnect(); parked < waiting {
		t.Fatalf("%d dials are waiting on the stalled guest, want %d", parked, waiting)
	}
	// Close releases the dials before it tries to stop the machine, which it
	// cannot do while the service is stopped: let it run on.
	closed := time.Now()
	go func() { _ = vm.Close() }()
	for range waiting {
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("a dial to a closed VM succeeded")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a dial still waits on a closed VM")
		}
	}
	t.Logf("%d waiting dials ended %s after the VM was closed", waiting, time.Since(closed).Round(time.Millisecond))

	// The dials have returned; their goroutines get a moment to unwind.
	deadline := time.Now().Add(5 * time.Second)
	for parkedInConnect() > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines are still parked in the binding's connect after Close", parkedInConnect())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// parkedInConnect counts the goroutines inside the binding's connect.
func parkedInConnect() int {
	var dump strings.Builder
	_ = pprof.Lookup("goroutine").WriteTo(&dump, 2)
	return strings.Count(dump.String(), "(*VirtioSocketDevice).Connect")
}
