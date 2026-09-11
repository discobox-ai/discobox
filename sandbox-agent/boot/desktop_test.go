package boot

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/discobox-ai/discobox/sandbox-agent/desktop"
)

// A new home gets the default scale before anything runs, so the harness's
// login shell -- started at boot, long before the socket-activated viewer --
// sources a GDK_SCALE instead of finding no file.
func TestSeedDesktopScaleWritesTheDefaultForANewHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chown is POSIX-only")
	}
	home := t.TempDir()
	id := identity{home: home, uid: os.Getuid(), gid: os.Getgid()}
	if err := seedDesktopScale(id); err != nil {
		t.Fatalf("seedDesktopScale: %v", err)
	}
	scale, ok := desktop.RememberedScale(filepath.Join(home, desktop.ScaleEnvDir))
	if !ok || scale != desktop.DefaultScale {
		t.Fatalf("RememberedScale = %d, %v; want %d, true", scale, ok, desktop.DefaultScale)
	}
}

// The scale a sandbox settled on survives a reboot: the boot flow rewrites the
// file for this image's variables, but keeps its value.
func TestSeedDesktopScaleKeepsARememberedScale(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chown is POSIX-only")
	}
	home := t.TempDir()
	dir := filepath.Join(home, desktop.ScaleEnvDir)
	if _, err := desktop.WriteScaleEnv(dir, 1); err != nil {
		t.Fatalf("WriteScaleEnv: %v", err)
	}
	id := identity{home: home, uid: os.Getuid(), gid: os.Getgid()}
	if err := seedDesktopScale(id); err != nil {
		t.Fatalf("seedDesktopScale: %v", err)
	}
	if scale, ok := desktop.RememberedScale(dir); !ok || scale != 1 {
		t.Fatalf("RememberedScale = %d, %v; want the remembered 1", scale, ok)
	}
}
