package privatefs

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// A directory and a file written into it are private once restricted, and
// restricting again is what repairs one an older run left behind.
func TestRestrictMakesPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := Restrict(dir); err != nil {
		t.Fatalf("Restrict(dir): %v", err)
	}
	if err := Check(dir); err != nil {
		t.Fatalf("Check(dir): %v", err)
	}

	file := filepath.Join(dir, "key")
	if err := os.WriteFile(file, []byte("key"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := Restrict(file); err != nil {
		t.Fatalf("Restrict(file): %v", err)
	}
	if err := Check(file); err != nil {
		t.Fatalf("Check(file): %v", err)
	}
}

// Check is not a formality: a file others can read fails it.
func TestCheckRejectsAWideMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a mode grants nothing on Windows; the ACL cases are Restrict's")
	}
	file := filepath.Join(t.TempDir(), "wide")
	if err := os.WriteFile(file, []byte("key"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := Check(file); err == nil {
		t.Fatal("Check accepted a 0644 file")
	}
}
