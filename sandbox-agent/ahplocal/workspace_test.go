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
