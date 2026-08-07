package sshd

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
)

const testAuthorizedKeyLine = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBboEyGDIiA0m5NEPRKXBTvzqSFCosRkVUUxfoM6RB6i user@laptop"

func TestLoadAuthorizedKeysMissingFileIsEmpty(t *testing.T) {
	keys, err := LoadAuthorizedKeys(t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("keys = %v, want empty", keys)
	}
}

func TestLoadAuthorizedKeysParsesLines(t *testing.T) {
	dir := t.TempDir()
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(testAuthorizedKeyLine))
	if err != nil {
		t.Fatalf("fixture key: %v", err)
	}
	content := "# a comment\n\n" +
		testAuthorizedKeyLine + "\n" +
		"not a valid line at all\n" +
		"command=\"/bin/true\" " + testAuthorizedKeyLine + "\n"
	if err := os.WriteFile(filepath.Join(dir, authorizedKeysFileName), []byte(content), 0o600); err != nil {
		t.Fatalf("write authorized_keys: %v", err)
	}

	keys, err := LoadAuthorizedKeys(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Both the plain line and the options-prefixed line resolve to the same
	// fingerprint, so the malformed line is silently skipped and the map has
	// exactly one entry.
	if len(keys) != 1 {
		t.Fatalf("keys = %v, want exactly 1", keys)
	}
	if _, ok := keys[ssh.FingerprintSHA256(pub)]; !ok {
		t.Fatalf("expected fingerprint of the fixture key to be present")
	}
}

func TestLoadAuthorizedKeysReloadsEveryCall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, authorizedKeysFileName)

	keys, err := LoadAuthorizedKeys(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("expected no keys before the file exists")
	}

	if err := os.WriteFile(path, []byte(testAuthorizedKeyLine+"\n"), 0o600); err != nil {
		t.Fatalf("write authorized_keys: %v", err)
	}
	keys, err = LoadAuthorizedKeys(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("expected the newly written key to be picked up without a restart")
	}
}
