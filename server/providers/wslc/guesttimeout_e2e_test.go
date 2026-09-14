package wslc

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/moby/moby/client"

	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/providers/dockerworker"
	"github.com/discobox-ai/discobox/server/providers/wslc/internal/wslcsession"
	"github.com/discobox-ai/discobox/server/providers/wslc/relay"
)

// TestGuestCommandTimeoutE2E runs the timeout against a real guest, because the
// half of it that matters cannot be faked: a guestConn's deadlines are no-ops,
// so giving up depends entirely on closing the connection unblocking a recv
// that is already parked inside Winsock. If that were not true the call would
// still return - the goroutine behind it would simply be stuck forever, and
// with it the guest process and its COM reference.
//
//	$env:DISCOBOX_WSLC_E2E="1"; go test -run TestGuestCommandTimeoutE2E -v ./providers/wslc/
func TestGuestCommandTimeoutE2E(t *testing.T) {
	if os.Getenv("DISCOBOX_WSLC_E2E") != "1" {
		t.Skip("set DISCOBOX_WSLC_E2E=1 to run the real wslc VM e2e test")
	}

	session, err := wslcsession.NewSession(wslcsession.Options{
		DisplayName: "discobox-guest-timeout-e2e",
		BootTimeout: bootTimeout,
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = session.Close() }()

	// A guest process that starts, says nothing, and does not exit: the read
	// below parks in recv() with nothing to return it.
	finished := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	err = runGuestCommand(ctx, session, "sleep 600", func(conn net.Conn) error {
		_, readErr := io.ReadAll(conn)
		finished <- readErr
		return readErr
	})
	waited := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runGuestCommand = %v after %v, want it to give up on the deadline", err, waited)
	}
	if waited > 30*time.Second {
		t.Errorf("runGuestCommand took %v to give up on a 2s deadline", waited)
	}

	select {
	case <-finished:
		// The parked read came back, which is the whole point: the exchange
		// goroutine is not leaked and the guest process is released.
	case <-time.After(30 * time.Second):
		t.Fatal("the blocked read never returned after the connection was closed; " +
			"the timeout returns control to the caller but leaks the goroutine, the guest process and its COM reference")
	}
}

// TestGuestExecFailureCondemnsTheVME2E removes the relay from a running guest
// and checks that the pool notices. It is the failure mode with no natural
// alarm: the control-plane mux is a process that is already running, so it
// stays healthy while every new dial fails, and a pool that cannot be reported
// unhealthy is never repaired.
//
//	$env:DISCOBOX_WSLC_E2E="1"; go test -run TestGuestExecFailureCondemnsTheVME2E -v ./providers/wslc/
func TestGuestExecFailureCondemnsTheVME2E(t *testing.T) {
	if os.Getenv("DISCOBOX_WSLC_E2E") != "1" {
		t.Skip("set DISCOBOX_WSLC_E2E=1 to run the real wslc VM e2e test")
	}

	// Persistent storage, as every real pool has by default (effectiveStorageDir).
	// It is the part that makes replacement hard: the new VM needs the same
	// storage.vhdx the old one has only just let go of.
	driver, err := NewDriver(DriverConfig{CPUCount: 2, MemoryMiB: 2048, StorageDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	t.Cleanup(func() { _ = driver.Close() })

	const poolID = "condemn-e2e"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if _, err := driver.EnsureVM(ctx, poolID, dockerworker.VMSpec{}); err != nil {
		t.Fatalf("EnsureVM: %v", err)
	}

	// Docker works, and the VM reports running.
	lease, err := driver.AcquireDockerClient(ctx, poolID)
	if err != nil {
		t.Fatalf("AcquireDockerClient: %v", err)
	}
	if _, err := lease.Client.Ping(ctx, client.PingOptions{}); err != nil {
		t.Fatalf("Ping before removing the relay: %v", err)
	}
	lease.Release()

	session, err := driver.session(poolID)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	if err := runGuestCommand(ctx, session, "rm -f "+relay.GuestPath, func(conn net.Conn) error {
		_, copyErr := io.Copy(io.Discard, conn)
		return copyErr
	}); err != nil {
		t.Fatalf("remove the guest relay: %v", err)
	}

	// The mux is a process that is still running, so this is still healthy.
	if info, err := driver.InspectVM(ctx, poolID); err != nil || info.Status != sandbox.StatusRunning {
		t.Fatalf("InspectVM = %+v, %v; want the VM still reported running before a dial is attempted", info, err)
	}

	lease, err = driver.AcquireDockerClient(ctx, poolID)
	if err != nil {
		t.Fatalf("AcquireDockerClient after removal: %v", err)
	}
	defer lease.Release()
	if _, err := lease.Client.Ping(ctx, client.PingOptions{}); err == nil {
		t.Fatal("Ping succeeded with no relay in the guest")
	}

	info, err := driver.InspectVM(ctx, poolID)
	if err != nil {
		t.Fatalf("InspectVM: %v", err)
	}
	if info.Status != sandbox.StatusStopped {
		t.Fatalf("VM reported %q after a failed exec; want %q, which is what makes RepairPool replace it",
			info.Status, sandbox.StatusStopped)
	}

	// An ordinary pool reconcile calls EnsureVM and nothing else, so that is
	// what has to bring the pool back: a condemned VM it reported running would
	// never be replaced.
	if _, err := driver.EnsureVM(ctx, poolID, dockerworker.VMSpec{}); err != nil {
		t.Fatalf("EnsureVM after condemnation: %v", err)
	}
	if info, err := driver.InspectVM(ctx, poolID); err != nil || info.Status != sandbox.StatusRunning {
		t.Fatalf("InspectVM after EnsureVM = %+v, %v; want the replacement VM running", info, err)
	}
	replaced, err := driver.AcquireDockerClient(ctx, poolID)
	if err != nil {
		t.Fatalf("AcquireDockerClient on the replacement VM: %v", err)
	}
	defer replaced.Release()
	if _, err := replaced.Client.Ping(ctx, client.PingOptions{}); err != nil {
		t.Fatalf("Ping on the replacement VM: %v", err)
	}
}
