package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// TestGeneratedIdentityIsReadableByOpenSSH is the check a Go-only test cannot
// make: x/crypto parsing its own output proves nothing about the binary that
// will actually read this file. ed25519 in PKCS#8 PEM, for instance, round
// trips fine in Go and is not loadable by every ssh in the field.
func TestGeneratedIdentityIsReadableByOpenSSH(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not installed")
	}
	path := filepath.Join(t.TempDir(), "id_ed25519")
	line, created, err := loadOrCreateSSHIdentity(path)
	if err != nil {
		t.Fatalf("create identity: %v", err)
	}
	if !created {
		t.Fatal("expected a freshly generated identity")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat identity: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("identity permissions = %o, want 600 (ssh refuses a group/world-readable key)", perm)
	}

	// -y derives the public key from the private one, which requires actually
	// parsing it.
	out, err := exec.Command("ssh-keygen", "-y", "-f", path).Output()
	if err != nil {
		t.Fatalf("ssh-keygen could not read the generated key: %v", err)
	}
	derived := strings.Fields(strings.TrimSpace(string(out)))
	emitted := strings.Fields(line)
	if len(derived) < 2 || len(emitted) < 2 || derived[0] != emitted[0] || derived[1] != emitted[1] {
		t.Fatalf("ssh-keygen derived %q, which does not match the enrolled line %q", out, line)
	}
}

// TestLoadOrCreateSSHIdentityReusesAnExistingKey: regenerating would silently
// invalidate the key the project already has enrolled.
func TestLoadOrCreateSSHIdentityReusesAnExistingKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "id_ed25519")
	first, created, err := loadOrCreateSSHIdentity(path)
	if err != nil || !created {
		t.Fatalf("create identity: %v (created=%v)", err, created)
	}
	second, created, err := loadOrCreateSSHIdentity(path)
	if err != nil {
		t.Fatalf("reload identity: %v", err)
	}
	if created {
		t.Fatal("second call regenerated the key")
	}
	if first != second {
		t.Fatalf("public key changed across calls:\n%s\n%s", first, second)
	}
}

// TestLoadOrCreateSSHIdentityIgnoresAStalePubFile pins that the public key is
// derived from the private key rather than read from the adjacent .pub, which
// nothing keeps in sync.
func TestLoadOrCreateSSHIdentityIgnoresAStalePubFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "id_ed25519")
	want, _, err := loadOrCreateSSHIdentity(path)
	if err != nil {
		t.Fatalf("create identity: %v", err)
	}
	if err := os.WriteFile(path+".pub", []byte("ssh-ed25519 AAAAsomethingelse== stale\n"), 0o644); err != nil {
		t.Fatalf("write stale pub: %v", err)
	}
	got, _, err := loadOrCreateSSHIdentity(path)
	if err != nil {
		t.Fatalf("reload identity: %v", err)
	}
	if got != want {
		t.Fatalf("public key came from the stale .pub file: got %q, want %q", got, want)
	}
}

func mustFingerprint(t *testing.T, publicKeyLine string) string {
	t.Helper()
	parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(publicKeyLine))
	if err != nil {
		t.Fatalf("parse public key line %q: %v", publicKeyLine, err)
	}
	return ssh.FingerprintSHA256(parsed)
}
