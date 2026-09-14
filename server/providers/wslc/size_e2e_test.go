package wslc

import (
	"context"
	"io"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/discobox-ai/discobox/server/providers/dockerworker"
	"github.com/discobox-ai/discobox/server/providers/vmsize"
)

// TestPoolVMSizingE2E checks what a guest actually sees, not what was asked
// for: a pool VM with nothing configured has every host CPU and half its
// memory, an EnsureVM with an unchanged size leaves a running VM alone, and a
// changed pool size replaces the VM with one of the new size.
//
//	$env:DISCOBOX_WSLC_E2E="1"; go test -run TestPoolVMSizingE2E -v ./providers/wslc/
func TestPoolVMSizingE2E(t *testing.T) {
	if os.Getenv("DISCOBOX_WSLC_E2E") != "1" {
		t.Skip("set DISCOBOX_WSLC_E2E=1 to run the real wslc VM e2e test")
	}

	driver, err := NewDriver(DriverConfig{StorageDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	t.Cleanup(func() { _ = driver.Close() })

	const poolID = "sizing-e2e"
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	guest := func() (vcpus int, memTotalMiB int) {
		t.Helper()
		session, err := driver.session(poolID)
		if err != nil {
			t.Fatalf("session: %v", err)
		}
		var out []byte
		err = runGuestCommand(ctx, session, "nproc; awk '/MemTotal/ {print int($2/1024)}' /proc/meminfo", func(conn net.Conn) error {
			var readErr error
			out, readErr = io.ReadAll(conn)
			return readErr
		})
		if err != nil {
			t.Fatalf("read guest size: %v", err)
		}
		fields := strings.Fields(string(out))
		if len(fields) != 2 {
			t.Fatalf("guest size output = %q", out)
		}
		vcpus, _ = strconv.Atoi(fields[0])
		memTotalMiB, _ = strconv.Atoi(fields[1])
		return vcpus, memTotalMiB
	}
	// MemTotal is what is left after the guest kernel takes its share, so it
	// reads somewhat under the VM's configured memory; within 10% is the VM
	// having been given the size, and anything further off is a different size.
	assertMemory := func(got, configured int) {
		t.Helper()
		if got > configured || got < configured*9/10 {
			t.Fatalf("guest MemTotal = %d MiB, want just under the %d MiB the VM was given", got, configured)
		}
	}

	// Nothing configured: the host's size.
	if _, err := driver.EnsureVM(ctx, poolID, dockerworker.VMSpec{}); err != nil {
		t.Fatalf("EnsureVM: %v", err)
	}
	host := vmsize.Host()
	vcpus, memory := guest()
	if vcpus != runtime.NumCPU() {
		t.Fatalf("guest has %d vCPUs, want every host CPU (%d)", vcpus, runtime.NumCPU())
	}
	assertMemory(memory, host.MemoryMiB)
	t.Logf("host-sized pool VM: %d vCPUs, MemTotal %d MiB (configured %d MiB)", vcpus, memory, host.MemoryMiB)

	// The same size again is not a reason to touch a running VM.
	first, _ := driver.session(poolID)
	if _, err := driver.EnsureVM(ctx, poolID, dockerworker.VMSpec{}); err != nil {
		t.Fatalf("second EnsureVM: %v", err)
	}
	if again, _ := driver.session(poolID); again != first {
		t.Fatal("EnsureVM replaced a running VM whose size had not changed")
	}

	// A size on the pool: the VM is replaced with one of that size.
	const gib = int64(1024 * 1024 * 1024)
	if _, err := driver.EnsureVM(ctx, poolID, dockerworker.VMSpec{CPUVCPUs: 3, MemoryBytes: 3 * gib}); err != nil {
		t.Fatalf("EnsureVM with a pool size: %v", err)
	}
	if replaced, _ := driver.session(poolID); replaced == first {
		t.Fatal("EnsureVM kept the host-sized VM after the pool asked for 3 vCPUs and 3 GiB")
	}
	vcpus, memory = guest()
	if vcpus != 3 {
		t.Fatalf("resized guest has %d vCPUs, want 3", vcpus)
	}
	assertMemory(memory, 3072)
	t.Logf("pool-sized VM: %d vCPUs, MemTotal %d MiB", vcpus, memory)

	// And it is a working pool VM, not merely one of the right size.
	lease, err := driver.AcquireDockerClient(ctx, poolID)
	if err != nil {
		t.Fatalf("AcquireDockerClient on the resized VM: %v", err)
	}
	defer lease.Release()
	if _, err := lease.Client.Ping(ctx, client.PingOptions{}); err != nil {
		t.Fatalf("Ping on the resized VM: %v", err)
	}
}
