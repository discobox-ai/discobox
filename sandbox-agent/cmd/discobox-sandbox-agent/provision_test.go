package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/poolauth"
)

// identity makes the box's key the first time it is asked, and answers its
// public half, the same one every time; the private half is the box's file
// alone (ADR 26-10-24-953 §4).
func TestIdentityAnswersTheSameKeyMadeOnDemand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity", "agent.key")
	var first, second bytes.Buffer
	if code := provisionIdentity(path, &first); code != 0 {
		t.Fatalf("identity exited %d", code)
	}
	if code := provisionIdentity(path, &second); code != 0 {
		t.Fatalf("identity again exited %d", code)
	}
	if first.String() != second.String() || strings.TrimSpace(first.String()) == "" {
		t.Fatalf("identity answered %q then %q", first.String(), second.String())
	}
	key, err := poolauth.LoadKey(path)
	if err != nil {
		t.Fatal(err)
	}
	public, _ := poolauth.PublicKeyOf(key)
	if strings.TrimSpace(first.String()) != public {
		t.Fatalf("identity answered %q, not the key's public half %q", first.String(), public)
	}
	if strings.Contains(first.String(), string(key)) {
		t.Fatal("identity printed the private key")
	}
	if err := os.WriteFile(path, []byte("damaged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := provisionIdentity(path, &bytes.Buffer{}); code == 0 {
		t.Fatal("identity replaced a damaged key")
	}
}
