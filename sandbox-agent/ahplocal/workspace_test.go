package ahplocal

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"

	"github.com/discobox-ai/discobox/pkg/ahphost"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestWritesPlaceDataAsTheModeSays(t *testing.T) {
	ws := Workspace{}
	path := filepath.Join(t.TempDir(), "f")
	if err := ws.WriteFile(path, ahphost.FileWrite{Data: []byte("hello\n"), Mode: ahptypes.ResourceWriteModeTruncate}); err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteFile(path, ahphost.FileWrite{Data: []byte("world\n"), Mode: ahptypes.ResourceWriteModeAppend}); err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteFile(path, ahphost.FileWrite{Data: []byte(">"), Mode: ahptypes.ResourceWriteModeInsert}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); got != ">hello\nworld\n" {
		t.Fatalf("file = %q", got)
	}
	if err := ws.WriteFile(path, ahphost.FileWrite{Data: []byte("x"), CreateOnly: true, Mode: ahptypes.ResourceWriteModeTruncate}); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("create-only over a file: %v", err)
	}
	if err := ws.WriteFile(path, ahphost.FileWrite{Data: []byte("x"), IfMatch: "0-0", Mode: ahptypes.ResourceWriteModeTruncate}); !errors.Is(err, ahphost.ErrChanged) {
		t.Fatalf("a write against a stale version: %v", err)
	}
	info, err := ws.Stat(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteFile(path, ahphost.FileWrite{Data: []byte("new"), IfMatch: info.Etag, Mode: ahptypes.ResourceWriteModeTruncate}); err != nil {
		t.Fatalf("a write against the current version: %v", err)
	}
}

// A copy or move never destroys what it was asked to carry: not onto
// itself, not into itself, and not by clearing the destination before it
// has the source in hand.
func TestTransfersNeverLoseTheirSource(t *testing.T) {
	ws := Workspace{}
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	write(t, filepath.Join(src, "a.txt"), "a")
	for name, err := range map[string]error{
		"move onto itself": ws.Move(src, src, true),
		"copy onto itself": ws.Copy(src, src, true),
		"copy into itself": ws.Copy(src, filepath.Join(src, "sub"), true),
		"move into itself": ws.Move(src, filepath.Join(src, "sub"), true),
	} {
		if !errors.Is(err, fs.ErrInvalid) {
			t.Fatalf("%s: %v, want ErrInvalid", name, err)
		}
	}
	if got := read(t, filepath.Join(src, "a.txt")); got != "a" {
		t.Fatalf("the source changed: %q", got)
	}

	dst := filepath.Join(dir, "dst")
	write(t, filepath.Join(dst, "old.txt"), "old")
	if err := ws.Copy(src, dst, false); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("copy over a folder without replace: %v", err)
	}
	if err := ws.Copy(src, dst, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "old.txt")); !errors.Is(err, fs.ErrNotExist) || read(t, filepath.Join(dst, "a.txt")) != "a" {
		t.Fatalf("copy did not replace the folder: %v", err)
	}
	moved := filepath.Join(dir, "moved")
	if err := ws.Move(src, moved, true); err != nil {
		t.Fatal(err)
	}
	if err := ws.Move(moved, dst, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(moved); !errors.Is(err, fs.ErrNotExist) || read(t, filepath.Join(dst, "a.txt")) != "a" {
		t.Fatalf("move over a folder: %v", err)
	}
	if err := ws.Move(filepath.Join(dir, "missing"), dst, true); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("moving nothing: %v", err)
	}
	if read(t, filepath.Join(dst, "a.txt")) != "a" {
		t.Fatal("a failed move cleared its destination")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".ahp-") {
			t.Fatalf("left behind %s", entry.Name())
		}
	}
}

func TestAWatchReportsChangesItIsNotToldToSkip(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Nothing runs this host on a Windows sandbox: its local exec socket
		// serves no one there (localexec).
		t.Skip("the in-box AHP host does not run on Windows")
	}
	dir := t.TempDir()
	write(t, filepath.Join(dir, "node_modules", "x"), "")
	got := make(chan []ahptypes.ResourceChange, 16)
	stop, err := Workspace{}.Watch(dir, true, []string{"**/node_modules"}, nil, func(changes []ahptypes.ResourceChange) { got <- changes })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "node_modules", "ignored"), "")
	time.Sleep(50 * time.Millisecond) // the new directory is watched as it appears
	write(t, filepath.Join(dir, "sub", "f"), "x")
	want := "file://" + filepath.Join(dir, "sub", "f")
	deadline := time.After(5 * time.Second)
	for {
		select {
		case changes := <-got:
			for _, change := range changes {
				if strings.Contains(change.Uri, "node_modules") {
					t.Fatalf("an excluded change was reported: %+v", change)
				}
				if change.Uri == want {
					return
				}
			}
		case <-deadline:
			t.Fatal("no change reported for the new file")
		}
	}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", //nolint:gosec // G204: the test's own git commands.
		append([]string{"-C", dir, "-c", "user.email=t@example.com", "-c", "user.name=t", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestChangesAreTheWorkingTreesAgainstItsBase(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	write(t, filepath.Join(dir, "kept.txt"), "one\n")
	write(t, filepath.Join(dir, "edited.txt"), "one\ntwo\n")
	write(t, filepath.Join(dir, "removed.txt"), "gone\n")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-q", "-m", "base")
	base := gitIn(t, dir, "rev-parse", "HEAD")
	write(t, filepath.Join(dir, "committed.txt"), "new\n")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-q", "-m", "work")
	write(t, filepath.Join(dir, "edited.txt"), "one\nTWO\nthree\n")
	write(t, filepath.Join(dir, "sub", "untracked.txt"), "a\nb\n")
	if err := os.Remove(filepath.Join(dir, "removed.txt")); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	ws := Workspace{}
	byName := func(changes []ahphost.Changed) map[string]ahphost.Changed {
		out := map[string]ahphost.Changed{}
		for _, change := range changes {
			out[filepath.Base(change.Path)] = change
		}
		return out
	}
	// Asked from a folder inside the tree, as a discobox started there is.
	changes, err := ws.Changes(ctx, filepath.Join(dir, "sub"), "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	uncommitted := byName(changes)
	edited := uncommitted["edited.txt"]
	if len(uncommitted) != 3 || !edited.Before || !edited.After || *edited.Added != 2 || *edited.Removed != 1 {
		t.Fatalf("uncommitted = %+v", uncommitted)
	}
	if uncommitted["removed.txt"].After || uncommitted["untracked.txt"].Before || *uncommitted["untracked.txt"].Added != 2 {
		t.Fatalf("removed or untracked = %+v %+v", uncommitted["removed.txt"], uncommitted["untracked.txt"])
	}
	changes, err = ws.Changes(ctx, dir, base)
	if err != nil {
		t.Fatal(err)
	}
	if branch := byName(changes); len(branch) != 4 || !branch["committed.txt"].After || branch["committed.txt"].Before {
		t.Fatalf("branch = %+v", branch)
	}

	before, err := ws.ReadRevision(ctx, edited.Path, "HEAD")
	if err != nil || string(before) != "one\ntwo\n" {
		t.Fatalf("before = %q, %v", before, err)
	}
	if _, err := ws.ReadRevision(ctx, filepath.Join(dir, "sub", "untracked.txt"), "HEAD"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a file the revision lacks: %v", err)
	}
	if _, err := ws.ReadRevision(ctx, edited.Path, "--output=/tmp/x"); !errors.Is(err, fs.ErrInvalid) {
		t.Fatalf("a revision that is an option: %v", err)
	}
}
