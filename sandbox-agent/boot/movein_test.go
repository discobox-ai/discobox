package boot

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
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
	// It runs the real copy, GNU cp's --no-target-directory, which a Linux
	// machine's guest has and macOS's cp does not; nothing moves volumes in
	// on any other platform.
	if runtime.GOOS != "linux" {
		t.Skip("the move-in copy is a Linux guest's GNU cp")
	}
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

// adoptionFixture is a data volume an agent wired for sbx_1 at an earlier
// start, with the data root and its record files at a test's own directory.
func adoptionFixture(t *testing.T, wiredFor string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("a machine's mounts are made in its Linux guest")
	}
	dir := t.TempDir()
	savedData, savedCache, savedWired, savedVolume := dataMountPath, cacheMountPath, machineDataWiredFile, machineDataVolume
	t.Cleanup(func() {
		dataMountPath, cacheMountPath, machineDataWiredFile, machineDataVolume = savedData, savedCache, savedWired, savedVolume
	})
	dataMountPath, cacheMountPath = filepath.Join(dir, "data"), filepath.Join(dir, "cache")
	machineDataWiredFile = filepath.Join(dataMountPath, ".discobox-wired")
	machineDataVolume = func() bool { return true }
	if err := os.MkdirAll(dataMountPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if wiredFor != "" {
		if err := os.WriteFile(machineDataWiredFile, []byte(wiredFor+" 0123\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// A data volume an older agent wired bound its cache paths and its sources
// without recording a layout. Before anything is moved, each whose volume
// directory is there is recorded bound, once, so moveIn moves nothing into
// the live directory -- not even a name the disk under the old bind also
// has, which moving would refuse and so power the machine off at every boot.
// A path whose directory is not there was never mounted, and is left to
// moveIn; and the volume is marked adopted, so this never runs again.
func TestAnOlderAgentsUnrecordedPathsAreAdoptedOnce(t *testing.T) {
	adoptionFixture(t, "sbx_1")
	root := t.TempDir()
	cache := harness.ResolvedVolume{Path: filepath.Join(root, "home", "dev", ".cache"), Kind: harness.VolumeCache}
	newCache := harness.ResolvedVolume{Path: filepath.Join(root, "home", "dev", ".npm"), Kind: harness.VolumeCache}
	repo := filepath.Join(root, "workspace", "repo")
	for _, dir := range []string{volumeDir(cache, 1000), machineSourceDir(repo)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(volumeDir(cache, 1000), "pip"), []byte("live\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cache.Path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache.Path, "pip"), []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	sources := []sandboxconfig.Source{{Slug: "primary", Target: repo}}
	if err := adoptLegacyLayouts("sbx_1", []harness.ResolvedVolume{cache, newCache}, sources, 1000); err != nil {
		t.Fatal(err)
	}
	for _, record := range []string{layoutRecordPath(cache), sourceRecordPath(repo)} {
		if layout, ok, err := readLayout(record); err != nil || !ok || layout != layoutBind {
			t.Fatalf("%s = %q %v %v, want bind", record, layout, ok, err)
		}
	}
	if _, ok, _ := readLayout(layoutRecordPath(newCache)); ok {
		t.Fatal("a newly declared path was adopted; it was never mounted")
	}
	if !fileExists(adoptedPath()) {
		t.Fatal("the volume was not marked adopted")
	}

	b := &booter{}
	if layout, err := b.moveIn(cache.Path, volumeDir(cache, 1000), layoutRecordPath(cache)); err != nil || layout != layoutBind {
		t.Fatalf("moveIn = %q, %v; want the adopted bind", layout, err)
	}
	if data, err := os.ReadFile(filepath.Join(volumeDir(cache, 1000), "pip")); err != nil || string(data) != "live\n" {
		t.Fatalf("the live cache = %q, %v; want it kept", data, err)
	}
}

// A volume this agent wired from the start is marked adopted by its first
// boot's wiring, so a path declared on it later -- a new pin's -- has no
// record and no directory and is moved in like any first mount, rather than
// bound over what the machine has there.
func TestAPathNewlyDeclaredOnAWiredVolumeIsMovedIn(t *testing.T) {
	adoptionFixture(t, "sbx_1")
	savedWired := machineWiredPath
	t.Cleanup(func() { machineWiredPath = savedWired })
	machineWiredPath = filepath.Join(t.TempDir(), "run", "machine-wired")
	if err := markMachineWired("sbx_1", wireMachine); err != nil {
		t.Fatal(err)
	}
	target, _, _ := moveInFixture(t)
	v := harness.ResolvedVolume{Path: target, Kind: harness.VolumeCache}
	if err := adoptLegacyLayouts("sbx_1", []harness.ResolvedVolume{v}, nil, 1000); err != nil {
		t.Fatal(err)
	}
	b := &booter{run: func(name string, args ...string) error {
		return exec.CommandContext(t.Context(), name, args...).Run()
	}}
	if layout, err := b.moveIn(target, volumeDir(v, 1000), layoutRecordPath(v)); err != nil || layout != layoutBind {
		t.Fatalf("moveIn = %q, %v", layout, err)
	}
	if data, err := os.ReadFile(filepath.Join(volumeDir(v, 1000), "engine-id")); err != nil || string(data) != "boxd\n" {
		t.Fatalf("engine-id in the volume = %q, %v; want the path's content moved in", data, err)
	}
}

// The agent's start, which records no layout for a cache path or a source,
// does not mark a volume adopted: a boot's wiring after it still adopts what
// it bound.
func TestTheAgentsStartDoesNotMarkAVolumeAdopted(t *testing.T) {
	adoptionFixture(t, "")
	savedWired := machineWiredPath
	t.Cleanup(func() { machineWiredPath = savedWired })
	machineWiredPath = filepath.Join(t.TempDir(), "run", "machine-wired")
	if err := markMachineWired("sbx_1", wireMachineStarted); err != nil {
		t.Fatal(err)
	}
	if fileExists(adoptedPath()) {
		t.Fatal("the agent's start marked the volume adopted")
	}
}

// Only a line naming this sandbox says its data volume was wired before.
func TestDataWiredForNamesThisSandbox(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data-wired")
	if dataWiredFor(path, "sbx_1") {
		t.Fatal("no line read as wired")
	}
	if err := os.WriteFile(path, []byte("sbx_1 0123abcd\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !dataWiredFor(path, "sbx_1") || dataWiredFor(path, "sbx_2") || dataWiredFor(path, "") {
		t.Fatal("dataWiredFor read the line wrong")
	}
}
