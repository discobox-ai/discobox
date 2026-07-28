package relay

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The relay must either be a real Linux binary or fail loudly. A silently
// truncated or placeholder artifact would be mounted into a guest and fail there,
// far from the cause.
func TestExtractProducesTheEmbeddedBinaryOrFailsClearly(t *testing.T) {
	dir := t.TempDir()
	path, err := Extract(dir)
	if !Available() || errors.Is(err, ErrNotBuilt) {
		if err == nil {
			t.Fatal("Extract succeeded even though no relay is embedded")
		}
		t.Skipf("relay artifact not built into this binary (run `task build:cp-relay`): %v", err)
	}
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat extracted relay: %v", err)
	}
	if info.Size() < minimumSize {
		t.Fatalf("extracted relay is %d bytes, want a real binary", info.Size())
	}
	if filepath.Base(path) != BinaryName {
		t.Fatalf("extracted name = %q, want %q", filepath.Base(path), BinaryName)
	}

	// An ELF magic number confirms this is the cross-compiled Linux binary and
	// not, say, a host executable or a text placeholder.
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open relay: %v", err)
	}
	defer func() { _ = file.Close() }()
	magic := make([]byte, 4)
	if _, err := file.Read(magic); err != nil {
		t.Fatalf("read magic: %v", err)
	}
	if string(magic) != "\x7fELF" {
		t.Fatalf("relay magic = %q, want an ELF binary", magic)
	}
}

// Extraction runs on every pool start, so a second call must be a cheap no-op
// rather than rewriting a file another pool may be mounting.
func TestExtractIsIdempotent(t *testing.T) {
	if !Available() {
		t.Skip("relay artifact not built into this binary")
	}
	dir := t.TempDir()
	first, err := Extract(dir)
	if err != nil {
		t.Skipf("relay not built: %v", err)
	}
	firstInfo, err := os.Stat(first)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	second, err := Extract(dir)
	if err != nil {
		t.Fatalf("second Extract: %v", err)
	}
	if second != first {
		t.Fatalf("second path = %q, want %q", second, first)
	}
	secondInfo, err := os.Stat(second)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !secondInfo.ModTime().Equal(firstInfo.ModTime()) {
		t.Fatal("Extract rewrote an identical relay; it should leave it in place")
	}

	// A stale or corrupted file must be replaced rather than trusted.
	if err := os.WriteFile(first, []byte("corrupted"), 0o600); err != nil {
		t.Fatalf("corrupt relay: %v", err)
	}
	if _, err := Extract(dir); err != nil {
		t.Fatalf("Extract over corrupted file: %v", err)
	}
	info, err := os.Stat(first)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() < minimumSize {
		t.Fatal("Extract left a corrupted relay in place")
	}
}

func TestDigestIsStable(t *testing.T) {
	first, second := Digest(), Digest()
	if first == "" {
		t.Fatal("Digest returned an empty identifier")
	}
	if first != second {
		t.Fatalf("Digest is not stable: %q then %q", first, second)
	}
}
