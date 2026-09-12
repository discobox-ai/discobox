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

// The identity is written through a temporary file and renamed, so a process
// that exits mid-write leaves either no identity or a whole one. A truncated
// PEM would be the worst of both: no later command can read it, and none will
// replace it, because a corrupt identity is not an absent one. What is
// observable afterwards is that nothing was left lying beside it.
func TestLoadOrCreateIrohIdentityLeavesNothingPartial(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "iroh")
	path := filepath.Join(dir, "id_ed25519")
	if _, _, err := loadOrCreateIrohIdentity(path); err != nil {
		t.Fatalf("loadOrCreateIrohIdentity() error = %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the identity directory: %v", err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if len(names) != 1 || names[0] != filepath.Base(path) {
		t.Fatalf("identity directory holds %v, want only the identity", names)
	}
	// And what landed is a key rather than the front of one.
	if _, err := readIrohIdentity(path); err != nil {
		t.Fatalf("readIrohIdentity() error = %v", err)
	}
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
	cmd := app.newPeerIDCommand()
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

// `discobox admin peer` is the name; the iroh spellings operators already have
// in their notes keep working, hidden (ADR 0095 §5 on iroh IDs, ADR 0097 §7).
func TestAdminCarriesPeerAndTheOldIrohSpellings(t *testing.T) {
	app := &App{}
	admin := app.newAdminCommand()

	for _, sub := range []string{"id", "ls", "add", "rm"} {
		found, _, err := admin.Find([]string{"peer", sub})
		if err != nil {
			t.Fatalf("find peer %s: %v", sub, err)
		}
		if found.Name() != sub {
			t.Fatalf("peer %s resolved to %q", sub, found.Name())
		}
		// The old group name still reaches the same verbs.
		if found, _, err = admin.Find([]string{"iroh", sub}); err != nil {
			t.Fatalf("find iroh %s: %v", sub, err)
		}
		if found.Name() != sub {
			t.Fatalf("iroh %s resolved to %q", sub, found.Name())
		}
	}

	for _, name := range []string{"iroh", "iroh-id"} {
		alias, _, err := admin.Find([]string{name})
		if err != nil {
			t.Fatalf("find %s: %v", name, err)
		}
		if !alias.Hidden {
			t.Fatalf("%q is listed in help; the old spellings are kept but not advertised", name)
		}
	}
}
