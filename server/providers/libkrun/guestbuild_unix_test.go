//go:build !windows

package libkrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/server/providers/guestimage"
)

// A guest built inside the pool VM is the VM's word, and the VM is the
// boundary: a symlink in what it exported must not have the server write
// through it, nor be published as the image the launcher loads from.
func TestAGuestBuildWithASymlinkIsRefused(t *testing.T) {
	driver := driverWithImage(t)
	victim := filepath.Join(t.TempDir(), "host-file")
	if err := os.WriteFile(victim, []byte("the host's"), 0o600); err != nil {
		t.Fatal(err)
	}
	staging := t.TempDir()
	if err := os.WriteFile(filepath.Join(staging, rootArtifact), []byte("root"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(staging, libraryArtifact)); err != nil {
		t.Fatal(err)
	}

	err := driver.completeGuestBuild(t.Context(), staging)
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("completeGuestBuild() = %v, want a refusal of the symlink", err)
	}
	if data, _ := os.ReadFile(victim); string(data) != "the host's" {
		t.Fatalf("the file the symlink named was written: %q", data)
	}
}

// A clean build is completed with the runtime the current image carries, each
// file created fresh with its mode, so the local directory is a whole image.
func TestAGuestBuildIsCompletedWithTheCurrentRuntime(t *testing.T) {
	driver := driverWithImage(t)
	staging := t.TempDir()
	if err := os.WriteFile(filepath.Join(staging, rootArtifact), []byte("root"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := driver.completeGuestBuild(t.Context(), staging); err != nil {
		t.Fatal(err)
	}
	for _, name := range runtimeArtifacts {
		info, err := os.Lstat(filepath.Join(staging, name))
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("%s after completion: %v, %v", name, info, err)
		}
	}
	if info, _ := os.Stat(filepath.Join(staging, passtArtifact)); info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("passt lost its executable bit: %v", info.Mode())
	}
}

// driverWithImage is a driver whose image resolves to a directory holding
// every artifact, passt executable as it is in the published image.
func driverWithImage(t *testing.T) *Driver {
	t.Helper()
	dir := t.TempDir()
	var artifacts []guestimage.Artifact
	for _, name := range append([]string{rootArtifact}, runtimeArtifacts...) {
		mode := os.FileMode(0o644)
		if name == passtArtifact {
			mode = 0o755
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), mode); err != nil {
			t.Fatal(err)
		}
		artifacts = append(artifacts, guestimage.Artifact{Name: name})
	}
	resolver, err := guestimage.New(guestimage.Config{OverrideDir: dir, Artifacts: artifacts})
	if err != nil {
		t.Fatal(err)
	}
	return &Driver{image: resolver}
}
