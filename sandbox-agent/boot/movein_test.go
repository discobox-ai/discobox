package boot

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// moveInFixture is a machine's path with content already on its disk, a
// volume directory for it and that volume's layout record.
func moveInFixture(t *testing.T) (target, volDir, record string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("a machine's mounts are made in its Linux guest")
	}
	root := t.TempDir()
	target = filepath.Join(root, "var", "lib", "docker")
	if err := os.MkdirAll(filepath.Join(target, "overlay2"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o711); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "engine-id"), []byte("boxd\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return target, filepath.Join(root, ".discobox", "data", "var", "lib", "docker"), filepath.Join(root, ".discobox", "data", layoutsDir, "%2Fvar%2Flib%2Fdocker")
}

// A machine's existing content at a path is moved into the path's volume,
// which takes the path's own mode, and the volume is recorded bound: the bind
// that follows shows what the box had there rather than hiding it, and
// nothing is overlaid (ADR 26-11-02-023 §2).
func TestAMachinesContentIsMovedIntoItsVolume(t *testing.T) {
	target, volDir, record := moveInFixture(t)
	b := &booter{run: func(name string, args ...string) error {
		t.Fatalf("ran %s %v; one filesystem is a rename", name, args)
		return nil
	}}
	layout, err := b.moveIn(target, volDir, record)
	if err != nil || layout != layoutBind {
		t.Fatalf("moveIn = %q, %v; want a bind", layout, err)
	}
	if data, err := os.ReadFile(filepath.Join(volDir, "engine-id")); err != nil || string(data) != "boxd\n" {
		t.Fatalf("engine-id in the volume = %q, %v", data, err)
	}
	if info, err := os.Stat(filepath.Join(volDir, "overlay2")); err != nil || !info.IsDir() {
		t.Fatalf("overlay2 in the volume: %v", err)
	}
	if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
		t.Fatalf("the path still holds %d entries (%v): they would be hidden under the bind", len(entries), err)
	}
	if info, err := os.Stat(volDir); err != nil || info.Mode().Perm() != 0o711 {
		t.Fatalf("volume root = %v, %v; want the path's 0711", info.Mode(), err)
	}
	if layout, ok, err := readLayout(record); err != nil || !ok || layout != layoutBind {
		t.Fatalf("layout record = %q %v %v, want bind", layout, ok, err)
	}

	// Recorded, it moves nothing again: what the box's services write under
	// the bound path is the volume's, and what is under the mountpoint at the
	// next boot is not the box's to take.
	if err := os.WriteFile(filepath.Join(target, "later"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.moveIn(target, volDir, record); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(volDir, "later")); !os.IsNotExist(err) {
		t.Fatalf("a recorded volume took more of the path: %v", err)
	}
}

// A boot cut short part way through leaves no record, and the next moves the
// rest; a name already in the volume is refused rather than overwritten.
func TestAMachinesMoveInResumesAndOverwritesNothing(t *testing.T) {
	target, volDir, record := moveInFixture(t)
	if err := os.MkdirAll(volDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(target, "overlay2"), filepath.Join(volDir, "overlay2")); err != nil {
		t.Fatal(err)
	}
	b := &booter{}
	if _, err := b.moveIn(target, volDir, record); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(volDir, "engine-id")); err != nil {
		t.Fatalf("the rest was not moved: %v", err)
	}

	target, volDir, record = moveInFixture(t)
	if err := os.MkdirAll(volDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(volDir, "engine-id"), []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := b.moveIn(target, volDir, record)
	if err == nil || !strings.Contains(err.Error(), "is there already") {
		t.Fatalf("move over a name in the volume = %v, want it refused", err)
	}
	if data, _ := os.ReadFile(filepath.Join(volDir, "engine-id")); string(data) != "mine\n" {
		t.Fatalf("the volume's engine-id = %q, overwritten", data)
	}
	if _, ok, _ := readLayout(record); ok {
		t.Fatal("recorded a volume whose move failed")
	}
}

// A layout a data-volume machine recorded before mounts were moves -- an
// overlay over what its root shipped -- is kept, and nothing is moved: its
// upper directory holds the user's writes.
func TestAMachineKeepsALayoutAlreadyRecorded(t *testing.T) {
	target, volDir, record := moveInFixture(t)
	if err := writeLayout(record, layoutOverlay); err != nil {
		t.Fatal(err)
	}
	layout, err := (&booter{}).moveIn(target, volDir, record)
	if err != nil || layout != layoutOverlay {
		t.Fatalf("moveIn = %q, %v; want the recorded overlay", layout, err)
	}
	if _, err := os.Stat(filepath.Join(target, "engine-id")); err != nil {
		t.Fatalf("moved content of a recorded overlay: %v", err)
	}
}

// A cache path inside the home moves with the home, then at its own turn out
// of the home's volume into its cache volume, so nothing is left hidden under
// its bind; and a path that is its volume directory already -- a source below
// the home, reached through the home's bind -- has nothing to move.
func TestACachePathMovesOutOfTheHomesVolume(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a machine's mounts are made in its Linux guest")
	}
	root := t.TempDir()
	home := filepath.Join(root, "home", "dev")
	if err := os.MkdirAll(filepath.Join(home, ".cache", "pip"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "notes.txt"), []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := &booter{}
	homeVol := filepath.Join(root, ".discobox", "data", "home", "dev")
	if _, err := b.moveIn(home, homeVol, filepath.Join(root, "records", "home")); err != nil {
		t.Fatal(err)
	}
	// After the home's bind, ~/.cache is the home volume's own directory.
	cache := filepath.Join(homeVol, ".cache")
	cacheVol := filepath.Join(root, ".discobox", "cache", ".users", "1000", "home", "dev", ".cache")
	if _, err := b.moveIn(cache, cacheVol, filepath.Join(root, "records", "cache")); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(cacheVol, "pip")); err != nil || !info.IsDir() {
		t.Fatalf("pip in the cache volume: %v", err)
	}
	if entries, _ := os.ReadDir(cache); len(entries) != 0 {
		t.Fatalf("~/.cache in the home volume still holds %d entries, hidden under its bind", len(entries))
	}
	if data, err := os.ReadFile(filepath.Join(homeVol, "notes.txt")); err != nil || string(data) != "mine\n" {
		t.Fatalf("notes.txt in the home volume = %q, %v", data, err)
	}

	source := filepath.Join(homeVol, "src")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "README"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	layout, err := b.moveIn(source, source, filepath.Join(root, "records", "source"))
	if err != nil || layout != layoutBind {
		t.Fatalf("moveIn onto itself = %q, %v; want a bind and nothing moved", layout, err)
	}
	if _, err := os.Stat(filepath.Join(source, "README")); err != nil {
		t.Fatalf("README: %v", err)
	}
}

// A source's record sits beside the declared volumes' layouts and apart from
// them: a source's target may be a declared path too.
func TestASourcesRecordIsApartFromAVolumes(t *testing.T) {
	got := sourceRecordPath("/workspace/repo/")
	if got != "/.discobox/data/.layouts/source:%2Fworkspace%2Frepo" {
		t.Fatalf("sourceRecordPath = %q", got)
	}
}

// A copy across filesystems a boot was cut short in is finished by the next:
// cut before its rename, the entry set aside is copied again over what part
// of it was copied; cut after, the whole copy stands and the aside goes.
func TestAMachinesCopyCutShortIsFinished(t *testing.T) {
	b := &booter{run: func(name string, args ...string) error {
		return exec.CommandContext(t.Context(), name, args...).Run()
	}}

	target, volDir, record := moveInFixture(t)
	aside := filepath.Join(target, movingPrefix+"overlay2")
	if err := os.Rename(filepath.Join(target, "overlay2"), aside); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(aside, "layer"), []byte("whole\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	partial := filepath.Join(volDir, movingPrefix+"overlay2")
	if err := os.MkdirAll(partial, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := b.moveIn(target, volDir, record); err != nil {
		t.Fatalf("cut before the rename: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(volDir, "overlay2", "layer")); err != nil || string(data) != "whole\n" {
		t.Fatalf("the finished copy's layer = %q, %v", data, err)
	}
	for _, left := range []string{aside, partial} {
		if _, err := os.Lstat(left); !os.IsNotExist(err) {
			t.Fatalf("%s is left (%v)", left, err)
		}
	}

	target, volDir, record = moveInFixture(t)
	aside = filepath.Join(target, movingPrefix+"engine-id")
	if err := os.Rename(filepath.Join(target, "engine-id"), aside); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(volDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(volDir, "engine-id"), []byte("boxd\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.moveIn(target, volDir, record); err != nil {
		t.Fatalf("cut after the rename: %v", err)
	}
	if _, err := os.Lstat(aside); !os.IsNotExist(err) {
		t.Fatalf("the aside is left (%v)", err)
	}
	if data, err := os.ReadFile(filepath.Join(volDir, "engine-id")); err != nil || string(data) != "boxd\n" {
		t.Fatalf("engine-id = %q, %v", data, err)
	}
}
