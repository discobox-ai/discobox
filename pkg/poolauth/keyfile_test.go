package poolauth

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The key is made the first time it is asked for, root's alone, and the same
// key is answered every time after; one that is there and unreadable is
// refused, never replaced.
func TestLoadOrCreateKeyMakesTheKeyOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity", "agent.key")
	if _, err := LoadKey(path); !os.IsNotExist(err) {
		t.Fatalf("LoadKey before = %v, want not there", err)
	}
	made, created, err := LoadOrCreateKey(path)
	if err != nil || !created {
		t.Fatalf("LoadOrCreateKey = %v, %v; want a key made", created, err)
	}
	// Windows does not expose POSIX permission bits.
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("key file = %v, %v; want 0600", info, err)
		}
	}
	again, created, err := LoadOrCreateKey(path)
	if err != nil || created || !again.Equal(made) {
		t.Fatalf("LoadOrCreateKey again = %v, %v; want the same key", created, err)
	}
	first, err := PublicKeyOf(made)
	if err != nil {
		t.Fatal(err)
	}
	if second, _ := PublicKeyOf(again); second != first {
		t.Fatalf("public keys %s and %s", first, second)
	}
	if err := os.WriteFile(path, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrCreateKey(path); err == nil {
		t.Fatal("an unreadable key was replaced")
	}
}
