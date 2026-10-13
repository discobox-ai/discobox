package privatefs

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/discobox-ai/discobox/internal/privatefs/privatefstest"
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

// Check is not a formality: a file others can reach fails it, whatever
// "others can reach" means on the platform.
func TestCheckRejectsAWidenedFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "wide")
	if err := os.WriteFile(file, []byte("key"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	privatefstest.Widen(t, file)
	if err := Check(file); err == nil {
		t.Fatal("Check accepted a file others can read")
	}
}

// On Windows Restrict is what takes a widened file back. On Unix the mode is
// the permission and the writer sets it, so there is nothing for Restrict to
// repair.
func TestRestrictRepairsAWidenedFile(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Restrict changes nothing on Unix; the mode a file is written with is its permission")
	}
	file := filepath.Join(t.TempDir(), "wide")
	if err := os.WriteFile(file, []byte("key"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	privatefstest.Widen(t, file)
	if err := Restrict(file); err != nil {
		t.Fatalf("Restrict: %v", err)
	}
	if err := Check(file); err != nil {
		t.Fatal(err)
	}
}

// A file written into a directory others can reach is private all the same,
// holds what was written, and leaves no staging behind.
func TestWriteExclusiveWritesAPrivateFile(t *testing.T) {
	dir := t.TempDir()
	// Widened first, so the file cannot pass by inheriting a private temporary
	// directory, which is what a default Windows profile's is.
	privatefstest.Widen(t, dir)
	path := filepath.Join(dir, "key")
	if err := WriteExclusive(path, []byte("key")); err != nil {
		t.Fatalf("WriteExclusive: %v", err)
	}
	if err := Check(path); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "key" {
		t.Fatalf("read back %q, %v; want %q", got, err, "key")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("directory holds %d entries after the write, want only the file", len(entries))
	}
}

// The file that is already there wins: it is not replaced, and the caller is
// told so in a way it can test for.
func TestWriteExclusiveLeavesAnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	if err := WriteExclusive(path, []byte("first")); err != nil {
		t.Fatalf("first WriteExclusive: %v", err)
	}
	err := WriteExclusive(path, []byte("second"))
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("second WriteExclusive error = %v, want one matching os.ErrExist", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "first" {
		t.Fatalf("read back %q, %v; want %q", got, err, "first")
	}
}
