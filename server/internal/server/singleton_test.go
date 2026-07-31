package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAcquireSingletonTakesFreeLock(t *testing.T) {
	dir := t.TempDir()
	release, err := acquireSingleton(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer release()

	data, err := os.ReadFile(filepath.Join(dir, singletonLockName))
	if err != nil {
		t.Fatalf("read lock file: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != strconv.Itoa(os.Getpid()) {
		t.Fatalf("lock file records pid %q, want %d", got, os.Getpid())
	}
}

// TestAcquireSingletonBlocksSecondHolder is the regression this exists for: a
// second server on the same data directory must not start. Before the lock it
// bound the unix socket straight over the incumbent and both kept running.
func TestAcquireSingletonBlocksSecondHolder(t *testing.T) {
	dir := t.TempDir()
	release, err := acquireSingleton(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer release()

	// The lock is per open file description, so a second acquire in this
	// process exercises the same kernel path a second process takes.
	if _, err := tryAcquireSingleton(filepath.Join(dir, singletonLockName)); !errors.Is(err, errLockBusy) {
		t.Fatalf("second acquire err = %v, want errLockBusy", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if _, err := acquireSingleton(ctx, dir, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting acquire err = %v, want DeadlineExceeded", err)
	}
}

// TestAcquireSingletonProceedsAfterRelease covers the handoff: the incumbent
// exiting must let the waiter in, which is what makes an air rebuild recover
// instead of leaving two servers.
func TestAcquireSingletonProceedsAfterRelease(t *testing.T) {
	dir := t.TempDir()
	release, err := acquireSingleton(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	go func() {
		time.Sleep(50 * time.Millisecond)
		release()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	second, err := acquireSingleton(ctx, dir, nil)
	if err != nil {
		t.Fatalf("second acquire after release: %v", err)
	}
	second()
}

// TestAcquireSingletonSeparateDataDirsDoNotConflict pins the chosen boundary:
// the lock is scoped to the data directory, so unrelated servers on their own
// databases are independent.
func TestAcquireSingletonSeparateDataDirsDoNotConflict(t *testing.T) {
	first, err := acquireSingleton(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer first()
	second, err := acquireSingleton(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	defer second()
}

func TestAcquireSingletonRequiresDataDir(t *testing.T) {
	if _, err := acquireSingleton(context.Background(), "  ", nil); err == nil {
		t.Fatal("acquire with empty data dir succeeded, want error")
	}
}

func TestDescribeSingletonHolderFallsBackWithoutPID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, singletonLockName)
	if got := describeSingletonHolder(path); !strings.Contains(got, "another discobox-server") {
		t.Fatalf("missing lock file described as %q", got)
	}
	if err := os.WriteFile(path, []byte("not-a-pid"), 0o600); err != nil {
		t.Fatalf("write lock file: %v", err)
	}
	if got := describeSingletonHolder(path); got != "another discobox-server" {
		t.Fatalf("unparsable pid described as %q", got)
	}
	if err := os.WriteFile(path, []byte("4321\n"), 0o600); err != nil {
		t.Fatalf("write lock file: %v", err)
	}
	if got := describeSingletonHolder(path); !strings.Contains(got, "4321") {
		t.Fatalf("described holder %q, want it to name pid 4321", got)
	}
}
