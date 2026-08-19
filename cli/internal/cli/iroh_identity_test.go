package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// The identity is what an operator enrolls, so regenerating it would silently
// revoke this machine's access.
func TestLoadOrCreateIrohIdentityIsStable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "iroh", "id_ed25519")

	first, created, err := loadOrCreateIrohIdentity(path)
	if err != nil {
		t.Fatalf("loadOrCreateIrohIdentity() error = %v", err)
	}
	if !created {
		t.Fatal("created = false on first call, want true")
	}
	if first.IsZero() {
		t.Fatal("generated endpoint ID is zero")
	}

	second, created, err := loadOrCreateIrohIdentity(path)
	if err != nil {
		t.Fatalf("loadOrCreateIrohIdentity() second call error = %v", err)
	}
	if created {
		t.Fatal("created = true on second call; the enrolled ID would change")
	}
	if second != first {
		t.Fatalf("endpoint ID changed: %s then %s", first, second)
	}
}

func TestLoadOrCreateIrohIdentityWritesPrivateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "iroh", "id_ed25519")
	if _, _, err := loadOrCreateIrohIdentity(path); err != nil {
		t.Fatalf("loadOrCreateIrohIdentity() error = %v", err)
	}
	// A private key, so whatever "only this user" means here: a mode on Unix,
	// an access list on Windows, which the mode would not have set.
	assertPrivateToUser(t, path)
	assertPrivateToUser(t, filepath.Dir(path))
}

// A broken identity must be reported, not silently replaced: overwriting it
// would change the ID an operator already enrolled.
func TestLoadOrCreateIrohIdentityRejectsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(path, []byte("not a key"), 0o600); err != nil {
		t.Fatalf("write identity: %v", err)
	}
	if _, _, err := loadOrCreateIrohIdentity(path); err == nil {
		t.Fatal("loadOrCreateIrohIdentity() succeeded on a corrupt identity, want error")
	}
}

func TestIrohIDCommandPrintsTheEndpointID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "id_ed25519")
	app := &App{}
	cmd := app.newIrohIDCommand()
	cmd.SetArgs([]string{"--identity-file", path})

	var out, errOut testBuffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	id, err := readIrohIdentity(path)
	if err != nil {
		t.Fatalf("readIrohIdentity() error = %v", err)
	}
	if got, want := out.String(), id.String()+"\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	// Minting a credential is announced, and on stderr so that stdout stays
	// exactly the ID for anything piping it.
	if errOut.String() == "" {
		t.Fatal("generating an identity printed nothing to stderr")
	}
}

type testBuffer struct {
	data []byte
}

func (b *testBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	return len(p), nil
}

func (b *testBuffer) String() string { return string(b.data) }
