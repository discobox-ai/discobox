package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/discobox-ai/discobox/pkg/desktopstage"
)

func TestRunPinsTheArchive(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "discobox-desktop-linux-amd64.tar.gz")
	body := []byte("archive bytes")
	if err := os.WriteFile(archive, body, 0o600); err != nil {
		t.Fatal(err)
	}
	encoded, err := run([]string{
		"-version", "v1.2.3", "-os", "linux", "-arch", "amd64",
		"-command", "discobox-desktop",
		"-base-url", "https://mirror.example/v1.2.3/",
		"-base-url", "https://github.com/discobox-ai/discobox/releases/download/v1.2.3",
		"-asset", archive,
	})
	if err != nil {
		t.Fatal(err)
	}
	m, err := desktopstage.DecodeManifest(encoded)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	if m.Asset.SHA256 != hex.EncodeToString(sum[:]) || m.Asset.Size != int64(len(body)) {
		t.Fatalf("pinned %s/%d", m.Asset.SHA256, m.Asset.Size)
	}
	want := []string{
		"https://mirror.example/v1.2.3/discobox-desktop-linux-amd64.tar.gz",
		"https://github.com/discobox-ai/discobox/releases/download/v1.2.3/discobox-desktop-linux-amd64.tar.gz",
	}
	if len(m.Asset.URLs) != 2 || m.Asset.URLs[0] != want[0] || m.Asset.URLs[1] != want[1] {
		t.Fatalf("urls %v, want %v", m.Asset.URLs, want)
	}
}

func TestRunRefusesAnUnstageableManifest(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "a.tar.gz")
	if err := os.WriteFile(archive, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := run([]string{"-version", "v1", "-os", "linux", "-arch", "amd64", "-command", "../x", "-base-url", "https://e.example", "-asset", archive}); err == nil {
		t.Fatal("encoded a manifest whose command escapes the app")
	}
}
