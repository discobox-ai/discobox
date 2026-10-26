package filelock_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/discobox-ai/discobox/internal/filelock"
)

func TestTryAcquireTakesFreeLockAndRecordsPID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.lock")
	lock, err := filelock.TryAcquire(path)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lock.Release()

	// Windows byte-range locks deny readers where POSIX advisory locks do not,
	// so the recorded pid is only observable off Windows while the lock is held.
	if runtime.GOOS == "windows" {
		t.Skip("a held byte-range lock is unreadable on Windows")
	}
	pid, ok := filelock.HolderPID(path)
	if !ok || pid != os.Getpid() {
		t.Fatalf("HolderPID = %d, %v, want %d, true", pid, ok, os.Getpid())
	}
}

// TestTryAcquireRejectsSecondHolder is what the lock exists for. The lock is per
// open file description, so a second acquire in this process exercises the same
// kernel path a second process takes.
func TestTryAcquireRejectsSecondHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.lock")
	lock, err := filelock.TryAcquire(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer lock.Release()

	if _, err := filelock.TryAcquire(path); !errors.Is(err, filelock.ErrBusy) {
		t.Fatalf("second acquire err = %v, want ErrBusy", err)
	}
}

func TestReleaseLetsTheNextHolderIn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.lock")
	first, err := filelock.TryAcquire(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	second, err := filelock.TryAcquire(path)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatalf("release second: %v", err)
	}
}

func TestHolderPIDWithoutAUsablePID(t *testing.T) {
	dir := t.TempDir()
	if _, ok := filelock.HolderPID(filepath.Join(dir, "missing.lock")); ok {
		t.Fatal("missing lock file reported a holder")
	}
	path := filepath.Join(dir, "test.lock")
	if err := os.WriteFile(path, []byte("not-a-pid"), 0o600); err != nil {
		t.Fatalf("write lock file: %v", err)
	}
	if _, ok := filelock.HolderPID(path); ok {
		t.Fatal("unparsable pid reported a holder")
	}
	if err := os.WriteFile(path, []byte("4321\n"), 0o600); err != nil {
		t.Fatalf("write lock file: %v", err)
	}
	if pid, ok := filelock.HolderPID(path); !ok || pid != 4321 {
		t.Fatalf("HolderPID = %d, %v, want 4321, true", pid, ok)
	}
}

func TestTryAcquireWithoutADirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "test.lock")
	if _, err := filelock.TryAcquire(path); err == nil {
		t.Fatal("acquire in a missing directory succeeded, want error")
	}
}

// A descriptor opened through os.Root can be locked without following its
// pathname again or changing a file's existing contents.
func TestTryAcquireFilePreservesContents(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	f, err := root.OpenFile("lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString("keep"); err != nil {
		t.Fatal(err)
	}
	lock, err := filelock.TryAcquireFile(f)
	if err != nil {
		t.Fatal(err)
	}
	other, err := root.OpenFile("lock", os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = filelock.TryAcquireFile(other); !errors.Is(err, filelock.ErrBusy) {
		t.Fatalf("second lock: %v", err)
	}
	if err = lock.Release(); err != nil {
		t.Fatal(err)
	}
	data, err := root.ReadFile("lock")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "keep" {
		t.Fatal("locking modified contents")
	}
}
