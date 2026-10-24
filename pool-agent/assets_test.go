package poolagent

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The asset socket replaces one an earlier agent left, and only root may
// open it.
func TestListenAssetsReplacesAStaleSocket(t *testing.T) {
	// A short directory: a socket path over ~100 bytes does not bind, and a
	// test's own temporary directory can be longer.
	dir, err := os.MkdirTemp("", "as")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "proxy", "assets.sock")
	first, err := listenAssets(socket)
	if err != nil {
		t.Fatal(err)
	}
	// Left behind, as an agent that died does.
	if l, ok := first.(interface{ SetUnlinkOnClose(bool) }); ok {
		l.SetUnlinkOnClose(false)
	}
	_ = first.Close()
	second, err := listenAssets(socket)
	if err != nil {
		t.Fatalf("listen over a stale socket: %v", err)
	}
	defer second.Close()
	info, err := os.Stat(socket)
	if err != nil {
		t.Fatal(err)
	}
	// Windows keeps no mode bits; the pool agent runs on Linux.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %v, want 0600", info.Mode().Perm())
	}
}
