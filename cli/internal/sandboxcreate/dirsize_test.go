package sandboxcreate

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestMeasureDirectoryCountsTheFilesThatWouldBeCopied(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "b.txt"), []byte("01234"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A symlink is carried as the link, so its target is not counted — least of
	// all a target outside the directory, which is not being copied at all.
	outside := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(outside, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Skipf("this filesystem has no symlinks: %v", err)
	}

	total := waitForTotal(t, MeasureDirectory(context.Background(), dir))
	if total.Bytes != 15 || total.Files != 2 {
		t.Fatalf("total = %+v, want 15 bytes in 2 files", total)
	}
}

// An unreadable subtree is skipped rather than failing the walk: a total short
// by one directory still answers whether the rest is worth copying.
func TestMeasureDirectorySkipsWhatItCannotRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Chmod on Windows moves the read-only attribute and nothing else, so
		// the directory this wants closed stays readable and gets counted.
		t.Skip("directory permissions")
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads every directory, so there is nothing to skip")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	closed := filepath.Join(dir, "closed")
	if err := os.MkdirAll(closed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(closed, "b.txt"), []byte("01234"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(closed, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(closed, 0o755) })

	total := waitForTotal(t, MeasureDirectory(context.Background(), dir))
	if total.Bytes != 10 || total.Files != 1 {
		t.Fatalf("total = %+v, want the readable file alone", total)
	}
}

// A stopped walk never reports its total as final: it stopped short of the
// directory, so what it counted is not what copying would cost, and a frontend
// that showed it as the answer would be showing a wrong one.
func TestMeasureDirectoryStopIsNotAFinalTotal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	walk := MeasureDirectory(ctx, dir)
	// Stopping twice, and stopping a walk that is already over, are both things
	// a frontend does on its way out of a question.
	walk.Stop()
	walk.Stop()
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if walk.Total().Done {
			t.Fatal("a stopped walk reported its total as final")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitForTotal(t *testing.T, walk *DirectoryWalk) DirectoryTotal {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if total := walk.Total(); total.Done {
			return total
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the walk never finished")
	return DirectoryTotal{}
}
