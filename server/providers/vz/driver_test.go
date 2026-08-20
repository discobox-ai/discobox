package vz

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/obot-platform/discobox/server/providers/guestimage"
	"github.com/obot-platform/discobox/server/providers/vz/internal/vzvm"
)

// Pool IDs become directory names under the state directory, so anything that
// could escape it has to be rejected before it is joined onto a path.
func TestValidatePoolIDRejectsPathEscapes(t *testing.T) {
	for _, poolID := range []string{"", ".", "..", "../escape", "a/b", `a\b`, "-leading", "with space", "..."} {
		if err := validatePoolID(poolID); err == nil {
			t.Errorf("validatePoolID(%q) accepted an unusable pool ID", poolID)
		}
	}
	for _, poolID := range []string{"pool-1", "Pool_1", "p.1", "0"} {
		if err := validatePoolID(poolID); err != nil {
			t.Errorf("validatePoolID(%q) = %v", poolID, err)
		}
	}
}

func newTestDriver(t *testing.T, stateDir string) *Driver {
	t.Helper()
	if err := vzvm.Supported(); err != nil {
		t.Skipf("Virtualization.framework unavailable: %v", err)
	}
	resolver, err := guestimage.New(guestimage.Config{
		OverrideDir: t.TempDir(),
		Artifacts:   []guestimage.Artifact{{Name: kernelArtifact}},
	})
	if err != nil {
		t.Fatalf("guestimage.New: %v", err)
	}
	driver, err := NewDriver(DriverConfig{Guest: resolver, StateDir: stateDir})
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	t.Cleanup(func() { _ = driver.Close() })
	return driver
}

func TestNewDriverRejectsIncompleteConfiguration(t *testing.T) {
	if err := vzvm.Supported(); err != nil {
		t.Skipf("Virtualization.framework unavailable: %v", err)
	}
	resolver, err := guestimage.New(guestimage.Config{
		OverrideDir: t.TempDir(),
		Artifacts:   []guestimage.Artifact{{Name: kernelArtifact}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, cfg := range map[string]DriverConfig{
		"no guest resolver": {StateDir: "/tmp/discobox-vz"},
		"relative state":    {Guest: resolver, StateDir: "pools"},
		"negative sizing":   {Guest: resolver, StateDir: "/tmp/discobox-vz", VCPUs: -1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewDriver(cfg); err == nil {
				t.Fatal("NewDriver accepted the configuration")
			}
		})
	}
}

// The disks are the only thing that survives the server process, so creating
// them must be idempotent: a repair re-runs this against a pool's existing
// state and must not truncate it.
func TestEnsureDisksIsIdempotentAndPreservesContents(t *testing.T) {
	stateDir := t.TempDir()
	driver := newTestDriver(t, stateDir)

	poolState := filepath.Join(stateDir, "pool-1")
	dataDisk, cacheDisk, err := driver.ensureDisks(poolState)
	if err != nil {
		t.Fatalf("ensureDisks: %v", err)
	}
	for _, disk := range []string{dataDisk, cacheDisk} {
		info, err := os.Stat(disk)
		if err != nil {
			t.Fatalf("stat %s: %v", disk, err)
		}
		if info.Size() == 0 {
			t.Fatalf("%s was created empty", disk)
		}
	}

	// Stand in for a pool's data, written into the image rather than replacing
	// it: a second EnsureVM must not lose it.
	marker := []byte("existing pool state")
	file, err := os.OpenFile(dataDisk, os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt(marker, 0); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	if _, _, err := driver.ensureDisks(poolState); err != nil {
		t.Fatalf("second ensureDisks: %v", err)
	}
	contents := make([]byte, len(marker))
	reopened, err := os.Open(dataDisk)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if _, err := reopened.ReadAt(contents, 0); err != nil {
		t.Fatal(err)
	}
	if string(contents) != string(marker) {
		t.Fatal("ensureDisks recreated a disk that already existed")
	}
}

// A pool with no VM is absent, not broken: the engine's reconcile creates one
// rather than trying to repair something that was never started.
func TestInspectVMReportsNotFoundWithoutAVM(t *testing.T) {
	driver := newTestDriver(t, t.TempDir())
	if _, err := driver.InspectVM(t.Context(), "pool-1"); err == nil {
		t.Fatal("InspectVM reported a VM that was never started")
	}
	if _, err := driver.AcquireDockerClient(t.Context(), "pool-1"); err == nil {
		t.Fatal("AcquireDockerClient succeeded without a VM")
	}
	if _, err := driver.AcquirePoolAgentClient(t.Context(), "pool-1"); err == nil {
		t.Fatal("AcquirePoolAgentClient succeeded without a VM")
	}
}

// Stopping a pool that has no VM is how repair and shutdown both start; it must
// be a no-op rather than an error that aborts the caller.
func TestStopVMWithoutAVMSucceeds(t *testing.T) {
	driver := newTestDriver(t, t.TempDir())
	if err := driver.StopVM(t.Context(), "pool-1"); err != nil {
		t.Fatalf("StopVM: %v", err)
	}
}

// DeleteVM is authorized pool deletion: it removes the disks StopVM preserves.
func TestDeleteVMRemovesPoolDisks(t *testing.T) {
	stateDir := t.TempDir()
	driver := newTestDriver(t, stateDir)
	poolState := filepath.Join(stateDir, "pool-1")
	if _, _, err := driver.ensureDisks(poolState); err != nil {
		t.Fatalf("ensureDisks: %v", err)
	}
	if err := driver.DeleteVM(t.Context(), "pool-1"); err != nil {
		t.Fatalf("DeleteVM: %v", err)
	}
	if _, err := os.Stat(poolState); !os.IsNotExist(err) {
		t.Fatalf("pool state survived deletion (stat err = %v)", err)
	}
}

// Disk sizes are ceilings a pool can be given more of, so raising the
// configured size must grow the existing image rather than leave the pool on
// whatever it was created with.
func TestEnsureDisksGrowsButNeverShrinks(t *testing.T) {
	stateDir := t.TempDir()
	driver := newTestDriver(t, stateDir)
	driver.dataDiskGiB = 1
	driver.cacheDiskGiB = 1

	poolState := filepath.Join(stateDir, "pool-1")
	dataDisk, _, err := driver.ensureDisks(poolState)
	if err != nil {
		t.Fatalf("ensureDisks: %v", err)
	}
	created, err := os.Stat(dataDisk)
	if err != nil {
		t.Fatal(err)
	}

	driver.dataDiskGiB = 3
	if _, _, err := driver.ensureDisks(poolState); err != nil {
		t.Fatalf("ensureDisks after raising the size: %v", err)
	}
	grown, err := os.Stat(dataDisk)
	if err != nil {
		t.Fatal(err)
	}
	if grown.Size() <= created.Size() {
		t.Fatalf("data disk = %d bytes, want it grown from %d", grown.Size(), created.Size())
	}

	// Lowering the configured size must not truncate a disk holding pool data.
	driver.dataDiskGiB = 1
	if _, _, err := driver.ensureDisks(poolState); err != nil {
		t.Fatalf("ensureDisks after lowering the size: %v", err)
	}
	kept, err := os.Stat(dataDisk)
	if err != nil {
		t.Fatal(err)
	}
	if kept.Size() != grown.Size() {
		t.Fatalf("data disk = %d bytes, want it left at %d", kept.Size(), grown.Size())
	}
}
