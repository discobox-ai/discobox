package poolagent

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/poolauth"
)

// The agent starts on the key its maker had made and enrolled (ADR
// 26-10-24-953 §4), and the same key every start; it never makes one.
func TestLoadIdentityIsTheEnrolledKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity", "agent.key")
	if _, err := LoadIdentity(path); err == nil || !strings.Contains(err.Error(), "enrolls") {
		t.Fatalf("LoadIdentity with no key = %v, want a refusal saying the maker enrolls it", err)
	}
	key, _, err := poolauth.LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	public, err := poolauth.PublicKeyOf(key)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		identity, err := LoadIdentity(path)
		if err != nil {
			t.Fatal(err)
		}
		if identity.PublicKey != public || !identity.PrivateKey.Equal(key) {
			t.Fatalf("identity = %s, want the enrolled key %s", identity.PublicKey, public)
		}
	}
}
