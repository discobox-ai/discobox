package serverstage

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func stageForRun(t *testing.T, root, version string, files map[string][]byte, command string) Manifest {
	t.Helper()
	served := serveAssets(t, files)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	manifest := served.manifest(version, command, names...)
	if _, err := Stage(context.Background(), manifest, Options{Root: root}); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestInstallPutsTheStagedSetInTheRunDirectory(t *testing.T) {
	root := t.TempDir()
	manifest := stageForRun(t, root, "v1.0.0", map[string][]byte{
		"discobox-server": []byte("server one"),
		"helper.dll":      []byte("helper one"),
	}, "discobox-server")

	command, err := Install(root, manifest)
	if err != nil {
		t.Fatal(err)
	}
	run := RunDir(root, runtime.GOOS, runtime.GOARCH)
	if want := filepath.Join(run, "discobox-server"); command != want {
		t.Fatalf("command %q, want %q", command, want)
	}
	if got := readFile(t, command); got != "server one" {
		t.Fatalf("run directory's server holds %q", got)
	}
	if got := readFile(t, filepath.Join(run, "helper.dll")); got != "helper one" {
		t.Fatalf("run directory's helper holds %q", got)
	}
	// Linked, not copied: the two names are one file.
	staged, _ := os.Stat(filepath.Join(manifest.Dir(root), "discobox-server"))
	linked, _ := os.Stat(command)
	if !os.SameFile(staged, linked) {
		t.Fatal("the run directory holds a copy where a hard link was possible")
	}
	// The versioned directory is still the record.
	if _, ok := Staged(root, manifest); !ok {
		t.Fatal("installing took the versioned set's place")
	}
	if _, err := os.Stat(filepath.Join(run, manifestFileName)); err == nil {
		t.Fatal("the run directory holds a manifest.json, which would make it look like a legacy set")
	}
}

func TestInstallMovesTheRunDirectoryToANewVersion(t *testing.T) {
	root := t.TempDir()
	first := stageForRun(t, root, "v1.0.0", map[string][]byte{
		"discobox-server": []byte("server one"),
		"old-only.dll":    []byte("gone in two"),
	}, "discobox-server")
	if _, err := Install(root, first); err != nil {
		t.Fatal(err)
	}
	second := stageForRun(t, root, "v2.0.0", map[string][]byte{
		"discobox-server": []byte("server two"),
	}, "discobox-server")

	command, err := Install(root, second)
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, command); got != "server two" {
		t.Fatalf("run directory's server holds %q after the upgrade", got)
	}
	if command != filepath.Join(first.RunDir(root), "discobox-server") {
		t.Fatalf("the run path moved with the version: %q", command)
	}
	entries, err := os.ReadDir(second.RunDir(root))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "discobox-server" {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("run directory holds %q after the upgrade, want only the server", names)
	}
	// The older version's set is untouched.
	if got := readFile(t, filepath.Join(first.Dir(root), "discobox-server")); got != "server one" {
		t.Fatalf("the older staged set changed: %q", got)
	}
}

func TestInstallLeavesARunDirectoryThatAlreadyHoldsTheVersion(t *testing.T) {
	root := t.TempDir()
	manifest := stageForRun(t, root, "v1.0.0", map[string][]byte{"discobox-server": []byte("server")}, "discobox-server")
	command, err := Install(root, manifest)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(command)

	// Read-only, as a running executable is on Windows: a second Install that
	// tried to replace it would have to rename over it.
	run := manifest.RunDir(root)
	if err := os.Chmod(run, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(run, 0o700) })
	if runtime.GOOS != "windows" && os.Geteuid() == 0 {
		t.Skip("root ignores the directory's permissions")
	}
	if _, err := Install(root, manifest); err != nil {
		t.Fatalf("installing the version already there changed something: %v", err)
	}
	after, _ := os.Stat(command)
	if !os.SameFile(before, after) {
		t.Fatal("the server was replaced with itself")
	}
}

func TestInstallRelinksAnAssetThatDiffers(t *testing.T) {
	root := t.TempDir()
	manifest := stageForRun(t, root, "v1.0.0", map[string][]byte{"discobox-server": []byte("server")}, "discobox-server")
	command, err := Install(root, manifest)
	if err != nil {
		t.Fatal(err)
	}
	// What a refresh killed between two assets, or a hand edit, leaves: a file
	// at the right name that is not the staged one.
	if err := os.Remove(command); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(command, []byte("not it"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(root, manifest); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, command); got != "server" {
		t.Fatalf("a differing asset was kept: %q", got)
	}
}

func TestInstallRecognisesItsOwnCopy(t *testing.T) {
	root := t.TempDir()
	manifest := stageForRun(t, root, "v1.0.0", map[string][]byte{"discobox-server": []byte("server")}, "discobox-server")
	src := filepath.Join(manifest.Dir(root), "discobox-server")
	run := manifest.RunDir(root)
	if err := os.MkdirAll(run, 0o700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(run, "discobox-server")
	if err := copyAsset(src, copied, info); err != nil {
		t.Fatal(err)
	}
	copiedInfo, _ := os.Stat(copied)
	if !sameAsset(info, copiedInfo) {
		t.Fatal("a copy with its source's size and time is not recognized")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != copiedInfo.Mode().Perm() {
		t.Fatalf("copy has mode %v, source %v", copiedInfo.Mode().Perm(), info.Mode().Perm())
	}
	// A copy whose time differs is not the same.
	if err := os.Chtimes(copied, time.Now(), info.ModTime().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	copiedInfo, _ = os.Stat(copied)
	if sameAsset(info, copiedInfo) {
		t.Fatal("a copy with another modification time counted as the same")
	}
}

func TestInstallClearsLeftovers(t *testing.T) {
	root := t.TempDir()
	manifest := stageForRun(t, root, "v1.0.0", map[string][]byte{"discobox-server": []byte("server")}, "discobox-server")
	run := manifest.RunDir(root)
	if err := os.MkdirAll(run, 0o700); err != nil {
		t.Fatal(err)
	}
	// A killed refresh's temporary, and a file an earlier Install moved aside
	// while a draining server still had it loaded.
	leftovers := []string{
		filepath.Join(run, ".discobox-server"+linkingMarker+"1"),
		filepath.Join(run, ".discobox-server"+asideMarker+"1"),
	}
	for _, leftover := range leftovers {
		if err := os.WriteFile(leftover, []byte("half"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Install(root, manifest); err != nil {
		t.Fatal(err)
	}
	for _, leftover := range leftovers {
		if _, err := os.Stat(leftover); !os.IsNotExist(err) {
			t.Fatalf("%s is still there: %v", leftover, err)
		}
	}
}

func TestInstallRefusesASetThatIsNotStaged(t *testing.T) {
	served := serveAssets(t, map[string][]byte{"discobox-server": []byte("server")})
	manifest := served.manifest("v1.0.0", "discobox-server", "discobox-server")
	_, err := Install(t.TempDir(), manifest)
	if err == nil || !strings.Contains(err.Error(), "not staged") {
		t.Fatalf("Install of an unstaged set: %v", err)
	}
}

func TestStagingLeavesTheRunDirectoryAlone(t *testing.T) {
	root := t.TempDir()
	manifest := stageForRun(t, root, "v1.0.0", map[string][]byte{"discobox-server": []byte("server")}, "discobox-server")
	if _, err := Install(root, manifest); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * abandonedAge)
	run := manifest.RunDir(root)
	if err := os.Chtimes(filepath.Join(run, "discobox-server"), old, old); err != nil {
		t.Fatal(err)
	}
	stageForRun(t, root, "v2.0.0", map[string][]byte{"discobox-server": []byte("two")}, "discobox-server")
	if _, err := os.Stat(filepath.Join(run, "discobox-server")); err != nil {
		t.Fatalf("staging another version took the run directory: %v", err)
	}
}

func TestValidateRefusesAVersionNamedLikeTheRunDirectory(t *testing.T) {
	served := serveAssets(t, map[string][]byte{"discobox-server": []byte("server")})
	for _, version := range []string{"run", "RUN"} {
		if err := served.manifest(version, "discobox-server", "discobox-server").Validate(); err == nil {
			t.Fatalf("version %q validated", version)
		}
	}
}
