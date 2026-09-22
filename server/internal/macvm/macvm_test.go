package macvm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenBundleRejectsPathNames(t *testing.T) {
	for _, name := range []string{"", ".", "..", "a/b", `a\b`} {
		if _, err := OpenBundle(t.TempDir(), name); err == nil {
			t.Fatalf("OpenBundle(%q) = nil error, want one", name)
		}
	}
	bundle, err := OpenBundle(t.TempDir(), "poc")
	if err != nil {
		t.Fatalf("OpenBundle: %v", err)
	}
	if filepath.Base(bundle.Dir) != "poc" {
		t.Fatalf("bundle dir = %q, want it to end in poc", bundle.Dir)
	}
}

// A bundle is the four files together, so a run has to be able to tell a
// half-installed one from a complete one rather than failing inside the
// framework.
func TestInstalledNamesTheMissingFile(t *testing.T) {
	bundle := Bundle{Dir: t.TempDir()}
	if err := bundle.Installed(); err == nil {
		t.Fatal("Installed() = nil error on an empty bundle, want one")
	}
	for _, path := range []string{bundle.DiskPath(), bundle.AuxiliaryPath(), bundle.HardwareModelPath()} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	err := bundle.Installed()
	if err == nil {
		t.Fatal("Installed() = nil error without a machine identifier, want one")
	}
	if !strings.Contains(err.Error(), filepath.Base(bundle.MachineIdentifierPath())) {
		t.Fatalf("Installed() = %v, want it to name the missing machine identifier", err)
	}
	if err := os.WriteFile(bundle.MachineIdentifierPath(), []byte("x"), 0o600); err != nil {
		t.Fatalf("write machine identifier: %v", err)
	}
	if err := bundle.Installed(); err != nil {
		t.Fatalf("Installed() = %v, want nil once every file is present", err)
	}
}

func TestMetadataRoundTrip(t *testing.T) {
	bundle := Bundle{Dir: t.TempDir()}
	read, err := bundle.ReadMetadata()
	if err != nil {
		t.Fatalf("ReadMetadata on a bundle without one: %v", err)
	}
	if read != (Metadata{}) {
		t.Fatalf("ReadMetadata = %+v, want the zero value", read)
	}
	want := Metadata{MacOSVersion: "26.6.2", BuildVersion: "25G83", CPUCount: 4, MemoryBytes: 8 << 30, DiskBytes: 96 << 30}
	if err := bundle.writeMetadata(want); err != nil {
		t.Fatalf("writeMetadata: %v", err)
	}
	read, err = bundle.ReadMetadata()
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if read != want {
		t.Fatalf("ReadMetadata = %+v, want %+v", read, want)
	}
}

func TestRunOptionsValidate(t *testing.T) {
	bundle := Bundle{Dir: t.TempDir()}
	for _, path := range []string{bundle.DiskPath(), bundle.AuxiliaryPath(), bundle.HardwareModelPath(), bundle.MachineIdentifierPath()} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	base := RunOptions{Bundle: bundle, DisplayWidth: 1920, DisplayHeight: 1200, DisplayPPI: 80}
	if err := base.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	gui := base
	gui.GUI = true
	if err := gui.Validate(); err == nil {
		t.Fatal("Validate() = nil error for a window with no size, want one")
	}

	relative := base
	relative.Shares = []SharedDirectory{{HostPath: "Users"}}
	if err := relative.Validate(); err == nil {
		t.Fatal("Validate() = nil error for a relative share, want one")
	}

	duplicate := base
	duplicate.Shares = []SharedDirectory{{Tag: "one", HostPath: "/Users"}, {Tag: "one", HostPath: "/tmp"}}
	if err := duplicate.Validate(); err == nil {
		t.Fatal("Validate() = nil error for a tag attached twice, want one")
	}

	// Two automount shares are the same collision: an empty tag is a tag.
	automount := base
	automount.Shares = []SharedDirectory{{HostPath: "/Users"}, {HostPath: "/tmp"}}
	if err := automount.Validate(); err == nil {
		t.Fatal("Validate() = nil error for two automount shares, want one")
	}

	long := base
	long.Shares = []SharedDirectory{{Tag: strings.Repeat("t", maxSharedDirectoryTagLen+1), HostPath: "/Users"}}
	if err := long.Validate(); err == nil {
		t.Fatal("Validate() = nil error for an over-long tag, want one")
	}
}

func TestCloneOptionsValidate(t *testing.T) {
	root := t.TempDir()
	source := Bundle{Dir: filepath.Join(root, "golden")}
	if err := os.MkdirAll(source.Dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, path := range []string{source.DiskPath(), source.AuxiliaryPath(), source.HardwareModelPath(), source.MachineIdentifierPath()} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	dest := Bundle{Dir: filepath.Join(root, "clone")}

	if err := (CloneOptions{Source: source, Dest: dest}).Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := (CloneOptions{Source: source, Dest: source}).Validate(); err == nil {
		t.Fatal("Validate() = nil error cloning onto itself, want one")
	}
	if err := (CloneOptions{Source: Bundle{Dir: filepath.Join(root, "missing")}, Dest: dest}).Validate(); err == nil {
		t.Fatal("Validate() = nil error for an uninstalled source, want one")
	}
	// A snapshot clone without a snapshot would silently become a cold clone
	// carrying the source's identifier, which is the one combination that is
	// never safe.
	if err := (CloneOptions{Source: source, Dest: dest, Snapshot: true}).Validate(); err == nil {
		t.Fatal("Validate() = nil error for a snapshot clone with no saved state, want one")
	}
	if err := os.WriteFile(source.StatePath(), []byte("x"), 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
	if err := (CloneOptions{Source: source, Dest: dest, Snapshot: true}).Validate(); err != nil {
		t.Fatalf("Validate with saved state: %v", err)
	}
	if size, ok := source.HasState(); !ok || size != 1 {
		t.Fatalf("HasState() = %d, %v; want 1, true", size, ok)
	}
}

func TestRestoreNeedsStateAndSize(t *testing.T) {
	bundle := Bundle{Dir: t.TempDir()}
	for _, path := range []string{bundle.DiskPath(), bundle.AuxiliaryPath(), bundle.HardwareModelPath(), bundle.MachineIdentifierPath()} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	base := RunOptions{Bundle: bundle, DisplayWidth: 1920, DisplayHeight: 1200, DisplayPPI: 80, CPUCount: 4, MemoryBytes: 8 << 30}

	restore := base
	restore.Restore = true
	if err := restore.Validate(); err == nil {
		t.Fatal("Validate() = nil error restoring without saved state, want one")
	}
	if err := os.WriteFile(bundle.StatePath(), []byte("x"), 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
	if err := restore.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	// The configuration has to match what was saved, so a restore that does not
	// know the size is refused rather than left to the framework.
	sizeless := restore
	sizeless.CPUCount, sizeless.MemoryBytes = 0, 0
	if err := sizeless.Validate(); err == nil {
		t.Fatal("Validate() = nil error restoring with no recorded size, want one")
	}

	recovery := restore
	recovery.Recovery = true
	if err := recovery.Validate(); err == nil {
		t.Fatal("Validate() = nil error restoring into recovery, want one")
	}
}

func TestProvisioningValidate(t *testing.T) {
	bundle := Bundle{Dir: t.TempDir()}
	for _, path := range []string{bundle.DiskPath(), bundle.AuxiliaryPath(), bundle.HardwareModelPath(), bundle.MachineIdentifierPath(), bundle.StatePath()} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	base := RunOptions{Bundle: bundle, DisplayWidth: 1920, DisplayHeight: 1200, DisplayPPI: 80, CPUCount: 4, MemoryBytes: 8 << 30}

	ok := base
	ok.Provision = &GuestProvisioning{Username: "darren", Password: "secret"}
	if err := ok.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	for name, p := range map[string]GuestProvisioning{
		"no username": {Password: "secret"},
		"no password": {Username: "darren"},
	} {
		bad := base
		bad.Provision = &p
		if err := bad.Validate(); err == nil {
			t.Fatalf("Validate() = nil error with %s, want one", name)
		}
	}
	// Provisioning happens on a first boot; a restore is not a boot.
	restored := ok
	restored.Restore = true
	if err := restored.Validate(); err == nil {
		t.Fatal("Validate() = nil error provisioning a restore, want one")
	}
}

func TestMacOSMajor(t *testing.T) {
	for version, want := range map[string]int{"27.0": 27, "26.6.2": 26, "": 0, "x.1": 0} {
		if got := (Metadata{MacOSVersion: version}).MacOSMajor(); got != want {
			t.Fatalf("MacOSMajor(%q) = %d, want %d", version, got, want)
		}
	}
}
