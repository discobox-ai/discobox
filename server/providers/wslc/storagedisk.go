package wslc

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
)

// storageDiskName is the file wslc keeps a session's persistent
// /var/lib/docker in, under the session's storage path.
const storageDiskName = "storage.vhdx"

// storageGrowth makes a pool's /var/lib/docker as large as the configured
// maximum, before and after its VM boots.
//
// wslc reads the maximum only when it creates the disk, and never grows the
// filesystem on it, so raising the setting would otherwise reach new pools
// only. Neither half is fatal: a pool on a smaller disk than configured still
// works, while one that will not start over a resize does not.
type storageGrowth struct {
	poolID string
	path   string
	size   uint64
}

func newStorageGrowth(poolID, storagePath string, maxStorageMiB int64) *storageGrowth {
	return &storageGrowth{
		poolID: poolID,
		path:   filepath.Join(storagePath, storageDiskName),
		size:   uint64(maxStorageMiB) << 20,
	}
}

// growDisk raises the disk's virtual size before boot. It cannot grow a disk a
// VM still has attached, and two kinds can: one a killed server left running,
// which only creating the new session ends - in development, every restart -
// and one this server closed moments before, since Close returns before wslc
// has finished tearing the VM down (a replacement, a repair, or the driver a
// provider update retired). The failure is only logged, so in practice a
// raised maximum lands on the next server start after a clean shutdown. Like
// WSL's own resize, which refuses an attached disk, it does not wait one out.
func (g storageGrowth) growDisk(ctx context.Context) {
	previous, grown, err := growStorageDisk(g.path, g.size)
	if err != nil {
		slog.WarnContext(ctx, "could not grow wslc pool storage disk; the pool keeps its current size",
			"pool_id", g.poolID, "path", g.path, "error", err)
		return
	}
	if grown {
		slog.InfoContext(ctx, "grew wslc pool storage disk",
			"pool_id", g.poolID, "from_mib", previous>>20, "to_mib", g.size>>20)
	}
}

// growFilesystem grows the guest's filesystem to fill the disk.
func (g storageGrowth) growFilesystem(ctx context.Context, vm guestProcessStarter) {
	if err := growGuestStorageFilesystem(ctx, vm); err != nil {
		slog.WarnContext(ctx, "could not grow the guest's /var/lib/docker filesystem; the pool keeps its current size",
			"pool_id", g.poolID, "error", err)
	}
}

// guestStorageResizeScript grows the filesystem mounted at /var/lib/docker to
// its device's size, and says so on success. resize2fs grows a mounted ext4
// online and is a quick no-op on one that already fills its device, so it runs
// on every boot rather than trying to tell whether the disk was grown.
const guestStorageResizeScript = `dev=$(awk '$2 == "/var/lib/docker" { print $1 }' /proc/mounts); ` +
	`if [ ! -b "$dev" ]; then echo "/var/lib/docker is not on a block device"; exit; fi; ` +
	`out=$(resize2fs "$dev" 2>&1) && echo resized || echo "$out"`

func growGuestStorageFilesystem(ctx context.Context, vm guestProcessStarter) error {
	var out []byte
	err := runGuestCommand(ctx, vm, guestStorageResizeScript, func(conn net.Conn) error {
		var readErr error
		out, readErr = io.ReadAll(conn)
		return readErr
	})
	if err != nil {
		return fmt.Errorf("resize /var/lib/docker in guest: %w", err)
	}
	// The shell's exit status does not cross the stdio relay, so success is the
	// line it prints last and anything else is the reason it stopped.
	if answer := strings.TrimSpace(string(out)); answer != "resized" {
		return fmt.Errorf("resize /var/lib/docker in guest: %s", answer)
	}
	return nil
}
