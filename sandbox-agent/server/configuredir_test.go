package server

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// requirePermissionBits skips a test that reads a mode back off disk. Windows
// carries no Unix permission bits — it reports 0777 for every directory — so
// nothing there can distinguish the 0700 these tests are about from the 0777
// they are guarding against. The sandbox this directory lives in is Linux.
func requirePermissionBits(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows stats every directory as 0777; the configure dir lives in the Linux sandbox")
	}
}

// The configure directory is the sandbox user's one writable foothold in
// root-owned /run/discobox, so its mode is load-bearing: 0700 is what keeps a
// seeded previous configuration and a collected credential out of reach of
// anything else in the sandbox.
func TestEnsureConfigureDirIsPrivateToItsOwner(t *testing.T) {
	requirePermissionBits(t)
	dir := filepath.Join(t.TempDir(), "configure")
	if err := ensureConfigureDirAt(dir, nil); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("mode = %o, want 700", got)
	}
}

// MkdirAll leaves an existing directory's mode alone, so a rerun over a
// directory something else created has to correct it rather than accept it.
func TestEnsureConfigureDirTightensAnExistingDirectory(t *testing.T) {
	requirePermissionBits(t)
	dir := filepath.Join(t.TempDir(), "configure")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatalf("pre-create: %v", err)
	}
	if err := ensureConfigureDirAt(dir, nil); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("mode = %o, want 700 -- a world-writable configure dir was left as it was found", got)
	}
}
