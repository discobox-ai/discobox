package boot

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func launcherSource(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"terminal.desktop", "web-browser.desktop"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("[Desktop Entry]\nName="+name+"\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	// Not a launcher, and must not be copied.
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("no\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	return dir
}

func TestSeedDesktopLaunchersPopulatesANewDesktop(t *testing.T) {
	// Both halves of what this asserts are POSIX-only: seedDesktopLaunchers
	// chowns the launchers to the sandbox user, which Windows does not support
	// at all, and the executable bit it checks for does not exist there. The
	// sibling tests below stop before the chown, so they still run everywhere.
	if runtime.GOOS == "windows" {
		t.Skip("chown and the executable bit are POSIX-only")
	}
	home := t.TempDir()
	id := identity{home: home, uid: os.Getuid(), gid: os.Getgid()}
	if err := (&booter{}).seedDesktopLaunchers(id, launcherSource(t)); err != nil {
		t.Fatalf("seedDesktopLaunchers: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(home, "Desktop"))
	if err != nil {
		t.Fatalf("read Desktop: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 launchers, got %d: %v", len(entries), entries)
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatalf("stat %s: %v", entry.Name(), err)
		}
		// Load-bearing: xfdesktop draws a launcher as a launcher only when the
		// file is executable, and shows an unexecutable one as a text file.
		if info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("%s is not executable (%v); xfdesktop will not treat it as a launcher", entry.Name(), info.Mode())
		}
	}
}

// The directory belongs to whoever is using the sandbox. Somebody who deleted
// the browser icon has said something, and a re-seed on the next boot would
// take it back.
func TestSeedDesktopLaunchersLeavesAnExistingDesktopAlone(t *testing.T) {
	home := t.TempDir()
	desktopDir := filepath.Join(home, "Desktop")
	if err := os.MkdirAll(desktopDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	id := identity{home: home, uid: os.Getuid(), gid: os.Getgid()}
	if err := (&booter{}).seedDesktopLaunchers(id, launcherSource(t)); err != nil {
		t.Fatalf("seedDesktopLaunchers: %v", err)
	}
	entries, err := os.ReadDir(desktopDir)
	if err != nil {
		t.Fatalf("read Desktop: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("an existing Desktop was re-seeded: %v", entries)
	}
}

// An image built without the desktop has no launchers to seed, which is not an
// error.
func TestSeedDesktopLaunchersToleratesAMissingSource(t *testing.T) {
	home := t.TempDir()
	id := identity{home: home, uid: os.Getuid(), gid: os.Getgid()}
	if err := (&booter{}).seedDesktopLaunchers(id, filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Fatalf("seedDesktopLaunchers: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "Desktop")); !os.IsNotExist(err) {
		t.Fatalf("a Desktop was created with nothing to put in it")
	}
}
