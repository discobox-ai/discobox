package watcher

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatcherSnapshotDiffCreateModifyDelete(t *testing.T) {
	root := t.TempDir()
	w := newTestWatcher(t, root)

	writeFile(t, filepath.Join(root, "dir", "file.txt"), []byte("hello"))
	batch := waitForBatch(t, w, 2*time.Second)
	assertChange(t, batch, "dir", Created)
	assertChange(t, batch, "dir/file.txt", Created)

	time.Sleep(10 * time.Millisecond)
	writeFile(t, filepath.Join(root, "dir", "file.txt"), []byte("hello, changed"))
	batch = waitForBatch(t, w, 2*time.Second)
	assertChange(t, batch, "dir/file.txt", Modified)

	if err := os.Remove(filepath.Join(root, "dir", "file.txt")); err != nil {
		t.Fatal(err)
	}
	batch = waitForBatch(t, w, 2*time.Second)
	assertChange(t, batch, "dir/file.txt", Deleted)

	if err := w.Close(); err != nil {
		t.Fatalf("close watcher: %v", err)
	}
	assertClosed(t, w.Batches(), "batches")
	assertClosed(t, w.Errors(), "errors")
}

func TestWatcherIgnoresGitDirectory(t *testing.T) {
	root := t.TempDir()
	w := newTestWatcher(t, root)
	defer w.Close()

	writeFile(t, filepath.Join(root, ".git", "config"), []byte("ignored"))
	assertNoBatch(t, w, 250*time.Millisecond)

	writeFile(t, filepath.Join(root, "tracked.txt"), []byte("tracked"))
	batch := waitForBatch(t, w, 2*time.Second)
	assertChange(t, batch, "tracked.txt", Created)
	assertNoChange(t, batch, ".git/config")
}

func TestWatcherIgnoresNodeModulesDirectories(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	w := newTestWatcher(t, root)
	defer w.Close()

	writeFile(t, filepath.Join(root, "node_modules", "pkg", "index.js"), []byte("ignored"))
	writeFile(t, filepath.Join(root, "nested", "node_modules", "pkg", "index.js"), []byte("ignored"))
	assertNoBatch(t, w, 250*time.Millisecond)

	writeFile(t, filepath.Join(root, "src", "app.js"), []byte("tracked"))
	batch := waitForBatch(t, w, 2*time.Second)
	assertChange(t, batch, "src/app.js", Created)
	assertNoChange(t, batch, "node_modules/pkg/index.js")
	assertNoChange(t, batch, "nested/node_modules/pkg/index.js")
}

func TestWatcherDetectsChangesFromInitialSnapshot(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "modified.txt"), []byte("before"))
	writeFile(t, filepath.Join(root, "deleted.txt"), []byte("before"))

	w := newTestWatcher(t, root)
	initial := w.Snapshot()
	if err := w.Close(); err != nil {
		t.Fatalf("close watcher: %v", err)
	}

	time.Sleep(10 * time.Millisecond)
	writeFile(t, filepath.Join(root, "modified.txt"), []byte("after"))
	writeFile(t, filepath.Join(root, "created.txt"), []byte("after"))
	if err := os.Remove(filepath.Join(root, "deleted.txt")); err != nil {
		t.Fatal(err)
	}

	w, err := New(root, Options{Debounce: 25 * time.Millisecond, PeriodicResync: 25 * time.Millisecond, InitialSnapshot: initial})
	if err != nil {
		t.Fatalf("new watcher: %v", err)
	}
	defer w.Close()

	batch := waitForBatch(t, w, 2*time.Second)
	assertChange(t, batch, "created.txt", Created)
	assertChange(t, batch, "deleted.txt", Deleted)
	assertChange(t, batch, "modified.txt", Modified)
	if batch.Snapshot == nil {
		t.Fatal("expected batch snapshot")
	}
	if _, ok := batch.Snapshot["deleted.txt"]; ok {
		t.Fatal("deleted file remained in batch snapshot")
	}
}

func TestDiffSnapshotsOrdersAndClassifiesChanges(t *testing.T) {
	oldSnap := map[string]Entry{
		"a.txt": {Path: "a.txt", Size: 1, ModTime: time.Unix(1, 0)},
		"b.txt": {Path: "b.txt", Size: 1, ModTime: time.Unix(1, 0)},
	}
	newSnap := map[string]Entry{
		"a.txt": {Path: "a.txt", Size: 2, ModTime: time.Unix(2, 0)},
		"c.txt": {Path: "c.txt", Size: 1, ModTime: time.Unix(1, 0)},
	}

	changes := diffSnapshots(oldSnap, newSnap)
	if len(changes) != 3 {
		t.Fatalf("expected 3 changes, got %d: %#v", len(changes), changes)
	}
	want := []Change{{Path: "a.txt", Kind: Modified}, {Path: "b.txt", Kind: Deleted}, {Path: "c.txt", Kind: Created}}
	for i := range want {
		if changes[i].Path != want[i].Path || changes[i].Kind != want[i].Kind {
			t.Fatalf("change %d = (%s, %s), want (%s, %s)", i, changes[i].Path, changes[i].Kind, want[i].Path, want[i].Kind)
		}
	}
}

func TestDiffSnapshotsIgnoresMtimeOnlyRewriteForRegularFiles(t *testing.T) {
	// Regular files (Mode 0 has no type bits, so IsRegular is true).
	oldSnap := map[string]Entry{
		"go.sum": {Path: "go.sum", Size: 10, ModTime: time.Unix(1, 0), Hash: "abc"},
	}
	// Identical content and size, only mtime advanced — an unconditional rewrite
	// by go mod tidy / go work sync. This must NOT register as a change.
	newSnap := map[string]Entry{
		"go.sum": {Path: "go.sum", Size: 10, ModTime: time.Unix(2, 0), Hash: "abc"},
	}
	if changes := diffSnapshots(oldSnap, newSnap); len(changes) != 0 {
		t.Fatalf("expected no changes for identical content, got %#v", changes)
	}

	// An actual content change (different hash) is still reported.
	newSnap["go.sum"] = Entry{Path: "go.sum", Size: 12, ModTime: time.Unix(3, 0), Hash: "def"}
	changes := diffSnapshots(oldSnap, newSnap)
	if len(changes) != 1 || changes[0].Kind != Modified {
		t.Fatalf("expected one Modified change, got %#v", changes)
	}
}

func TestWatcherIgnoresIdenticalContentRewrite(t *testing.T) {
	root := t.TempDir()
	w := newTestWatcher(t, root)
	defer w.Close()

	path := filepath.Join(root, "go.sum")
	writeFile(t, path, []byte("checksum"))
	batch := waitForBatch(t, w, 2*time.Second)
	assertChange(t, batch, "go.sum", Created)

	// Rewrite identical bytes with a fresh mtime, exactly as go work sync does.
	future := time.Now().Add(time.Second)
	if err := os.WriteFile(path, []byte("checksum"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	assertNoBatch(t, w, 500*time.Millisecond)

	// A real content change is still detected.
	writeFile(t, path, []byte("checksum-changed"))
	batch = waitForBatch(t, w, 2*time.Second)
	assertChange(t, batch, "go.sum", Modified)
}

func newTestWatcher(t *testing.T, root string) *Watcher {
	t.Helper()
	w, err := New(root, Options{Debounce: 25 * time.Millisecond})
	if err != nil {
		t.Fatalf("new watcher: %v", err)
	}
	return w
}

func writeFile(t *testing.T, path string, contents []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, contents, 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
}

func waitForBatch(t *testing.T, w *Watcher, timeout time.Duration) Batch {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case batch, ok := <-w.Batches():
			if !ok {
				t.Fatal("batches channel closed")
			}
			if len(batch.Changes) > 0 {
				return batch
			}
		case err, ok := <-w.Errors():
			if ok {
				t.Fatalf("watcher error: %v", err)
			}
		case <-deadline:
			t.Fatal("timed out waiting for batch")
		}
	}
}

func assertNoBatch(t *testing.T, w *Watcher, duration time.Duration) {
	t.Helper()
	select {
	case batch, ok := <-w.Batches():
		if ok {
			t.Fatalf("unexpected batch: %#v", batch)
		}
		t.Fatal("batches channel closed")
	case err, ok := <-w.Errors():
		if ok {
			t.Fatalf("watcher error: %v", err)
		}
		t.Fatal("errors channel closed")
	case <-time.After(duration):
	}
}

func assertChange(t *testing.T, batch Batch, path string, kind ChangeKind) {
	t.Helper()
	for _, change := range batch.Changes {
		if change.Path == path && change.Kind == kind {
			return
		}
	}
	t.Fatalf("missing %s change for %q in %#v", kind, path, batch.Changes)
}

func assertNoChange(t *testing.T, batch Batch, path string) {
	t.Helper()
	for _, change := range batch.Changes {
		if change.Path == path {
			t.Fatalf("unexpected change for %q in %#v", path, batch.Changes)
		}
	}
}

func assertClosed[T any](t *testing.T, ch <-chan T, name string) {
	t.Helper()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatalf("%s channel still open", name)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s channel to close", name)
	}
}
