package wslc

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/providers/dockerworker"
)

// TestStorageGrowsWhenMaximumIsRaisedE2E checks what the guest's filesystem
// actually offers, since wslc sizes a disk only when it creates it and never
// grows the filesystem on it: raising the maximum must grow an existing pool's
// /var/lib/docker on a boot whose disk no VM holds, must not stop a pool from
// starting when a killed server's VM still holds the disk (growth cannot land
// then), and lowering it must leave the pool's data alone.
//
//	$env:DISCOBOX_WSLC_E2E="1"; go test -run TestStorageGrowsWhenMaximumIsRaisedE2E -v ./providers/wslc/
func TestStorageGrowsWhenMaximumIsRaisedE2E(t *testing.T) {
	if os.Getenv("DISCOBOX_WSLC_E2E") != "1" {
		t.Skip("set DISCOBOX_WSLC_E2E=1 to run the real wslc VM e2e test")
	}

	storageDir := t.TempDir()
	const poolID = "storage-grow-e2e"
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	boot := func(maxStorageMiB int64) *Driver {
		t.Helper()
		driver, err := NewDriver(DriverConfig{StorageDir: storageDir, MaxStorageMiB: maxStorageMiB})
		if err != nil {
			t.Fatalf("NewDriver: %v", err)
		}
		t.Cleanup(func() { _ = driver.Close() })
		if _, err := driver.EnsureVM(ctx, poolID, dockerworker.VMSpec{}); err != nil {
			t.Fatalf("EnsureVM with a %d MiB maximum: %v", maxStorageMiB, err)
		}
		return driver
	}
	filesystemMiB := func(driver *Driver) int {
		t.Helper()
		session, err := driver.session(poolID)
		if err != nil {
			t.Fatalf("session: %v", err)
		}
		var out []byte
		err = runGuestCommand(ctx, session, "df -m --output=size /var/lib/docker | tail -n 1", func(conn net.Conn) error {
			var readErr error
			out, readErr = io.ReadAll(conn)
			return readErr
		})
		if err != nil {
			t.Fatalf("read guest filesystem size: %v", err)
		}
		size, err := strconv.Atoi(strings.TrimSpace(string(out)))
		if err != nil {
			t.Fatalf("guest filesystem size output = %q", out)
		}
		return size
	}
	// ext4 keeps some of a device for itself, so a filesystem that fills a
	// disk reads a little under the disk's size; within 5% is filling it.
	assertFills := func(got int, diskMiB int) {
		t.Helper()
		if got > diskMiB || got < diskMiB*95/100 {
			t.Fatalf("/var/lib/docker is %d MiB, want it to fill the %d MiB disk", got, diskMiB)
		}
	}

	first := boot(4096)
	assertFills(filesystemMiB(first), 4096)

	// A second driver without closing the first is a server started after one
	// that was killed: its VM is still running and holding the disk, so the
	// raise cannot land, but the pool must still start.
	held := boot(8192)
	if err := held.StopVM(ctx, poolID); err != nil {
		t.Fatalf("StopVM: %v", err)
	}

	// StopVM returns before wslc has finished tearing the VM down, and until
	// it has, the disk is attached and cannot grow. Opening the disk without
	// resizing it is what shows it is free; booting before then would test
	// the teardown's timing instead of the growth.
	disk := filepath.Join(storageDir, poolID, storageDiskName)
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, _, err := growStorageDisk(disk, 0)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stopped VM is still holding %s: %v", disk, err)
		}
		time.Sleep(500 * time.Millisecond)
	}

	raised := boot(8192)
	assertFills(filesystemMiB(raised), 8192)
	if err := raised.StopVM(ctx, poolID); err != nil {
		t.Fatalf("StopVM: %v", err)
	}

	lowered := boot(4096)
	assertFills(filesystemMiB(lowered), 8192)
}
