//go:build !windows

package endpoint

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func repairTree(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", base)
	child := filepath.Join(base, "discobox")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	return base, child
}

func TestRuntimeDirectoryRepair(t *testing.T) {
	for _, modes := range [][2]os.FileMode{{0755, 0755}, {0700, 0777}, {0777, 0700}} {
		t.Run(modes[0].String()+modes[1].String(), func(t *testing.T) {
			base, child := repairTree(t)
			for i, p := range []string{base, child} {
				if err := os.Chmod(p, modes[i]); err != nil {
					t.Fatal(err)
				}
			}
			plan, err := InspectRuntimeDirectories(DefaultEndpoint())
			if err != nil || plan == nil {
				t.Fatalf("plan: %v, %v", plan, err)
			}
			defer plan.Close()
			for i, p := range []string{base, child} {
				st, _ := os.Stat(p)
				if st.Mode().Perm() != modes[i] {
					t.Fatal("inspection mutated permissions")
				}
			}
			if err := protectDefaultSocket(filepath.Join(child, "server.sock")); err == nil {
				t.Fatal("strict validation accepted public mode")
			}
			if err := plan.Repair(); err != nil {
				t.Fatal(err)
			}
			for _, p := range []string{base, child} {
				st, _ := os.Stat(p)
				if st.Mode().Perm() != 0700 {
					t.Fatal("not private")
				}
			}
			if again, err := InspectRuntimeDirectories(DefaultEndpoint()); err != nil || again != nil {
				t.Fatalf("repaired inspection: %v %v", again, err)
			}
		})
	}
}

func TestRuntimeDirectoryRepairRefusesChanges(t *testing.T) {
	for _, change := range []string{"replace", "mode", "symlink", "ancestor"} {
		t.Run(change, func(t *testing.T) {
			base, child := repairTree(t)
			if err := os.Chmod(child, 0755); err != nil {
				t.Fatal(err)
			}
			plan, err := InspectRuntimeDirectories(DefaultEndpoint())
			if err != nil {
				t.Fatal(err)
			}
			defer plan.Close()
			switch change {
			case "replace", "symlink":
				if err := os.Rename(child, child+"-old"); err != nil {
					t.Fatal(err)
				}
				if change == "replace" {
					err = os.Mkdir(child, 0755)
				} else {
					err = os.Symlink(child+"-old", child)
				}
			case "mode":
				err = os.Chmod(child, 0777)
			case "ancestor":
				err = os.Chmod(base, 0777)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := plan.Repair(); err == nil {
				t.Fatal("changed tree repaired")
			}
			st, _ := os.Stat(child)
			if st.Mode().Perm() == 0700 {
				t.Fatal("unexpected mutation")
			}
		})
	}
}

func TestRuntimeDirectoryRepairInspectsWholeTree(t *testing.T) {
	base, child := repairTree(t)
	if err := os.Chmod(base, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(child); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), child); err != nil {
		t.Fatal(err)
	}
	plan, err := InspectRuntimeDirectories(DefaultEndpoint())
	if err == nil || plan != nil {
		t.Fatal("offered repair with symlink child")
	}
	st, _ := os.Stat(base)
	if st.Mode().Perm() != 0755 {
		t.Fatal("mutated base")
	}
}

func TestRuntimeDirectoryRepairConcurrent(t *testing.T) {
	_, child := repairTree(t)
	_ = os.Chmod(child, 0755)
	first, err := InspectRuntimeDirectories(DefaultEndpoint())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := InspectRuntimeDirectories(DefaultEndpoint())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := first.Repair(); err != nil {
		t.Fatal(err)
	}
	if err := second.Repair(); err == nil {
		t.Fatal("stale approval accepted")
	}
}

func TestRuntimeDirectoryRepairPartialRetry(t *testing.T) {
	base, child := repairTree(t)
	_ = os.Chmod(base, 0755)
	_ = os.Chmod(child, 0755)
	plan, err := InspectRuntimeDirectories(DefaultEndpoint())
	if err != nil {
		t.Fatal(err)
	}
	// Fail the second chmod, after revalidation, without changing the paths.
	last := len(plan.dirs) - 1
	_ = unix.Close(plan.dirs[last].fd)
	plan.dirs[last].fd = -1
	if err := plan.Repair(); err == nil {
		t.Fatal("expected partial failure")
	}
	plan.Close()
	st, _ := os.Stat(base)
	if st.Mode().Perm() != 0700 {
		t.Fatal("successful repair rolled back")
	}
	retry, err := InspectRuntimeDirectories(DefaultEndpoint())
	if err != nil {
		t.Fatal(err)
	}
	defer retry.Close()
	if got := retry.Changes(); len(got) != 1 || got[0].Path != child {
		t.Fatalf("retry changes: %v", got)
	}
	if err := retry.Repair(); err != nil {
		t.Fatal(err)
	}
}
