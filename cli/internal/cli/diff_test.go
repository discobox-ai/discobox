package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
)

func TestSourceCheckoutCommit(t *testing.T) {
	var missing apimodel.GitSource
	if got := sourceCheckoutCommit(missing); got != "" {
		t.Fatalf("source without a checkout: got %q, want empty", got)
	}

	checkout := apimodel.GitSourceCheckout{}
	checkout.SetCommit(apiclientgen.NewOptString("  4f3a1c2b8d90aaaabbbbccccddddeeeeffff0000  "))
	var source apimodel.GitSource
	source.SetCheckout(apiclientgen.NewOptGitSourceCheckout(checkout))
	if got, want := sourceCheckoutCommit(source), "4f3a1c2b8d90aaaabbbbccccddddeeeeffff0000"; got != want {
		t.Fatalf("checkout commit: got %q, want %q", got, want)
	}
}

func TestDiffOptionsGitArgs(t *testing.T) {
	opts := diffOptions{stat: true, ignoreAllSpace: true, findRenames: true, unified: 8, diffFilter: "AM"}
	got := strings.Join(opts.gitArgs(true), " ")
	want := "--stat --ignore-all-space --find-renames --unified=8 --diff-filter=AM"
	if got != want {
		t.Fatalf("git args: got %q, want %q", got, want)
	}
	unset := (diffOptions{unified: 3}).gitArgs(false)
	if len(unset) != 0 {
		t.Fatalf("untouched options should add no arguments, got %v", unset)
	}
	if !(diffOptions{nameStatus: true}).rawGitOutput() {
		t.Fatal("--name-status must select git's own output")
	}
	if (diffOptions{ignoreAllSpace: true}).rawGitOutput() {
		t.Fatal("-w changes the comparison, not the output format")
	}
}

func TestShellQuote(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"plain", "'plain'"},
		{"with space", "'with space'"},
		{"it's", `'it'\''s'`},
		{"; rm -rf /", "'; rm -rf /'"},
		{"$(whoami)", "'$(whoami)'"},
	} {
		if got := shellQuote(tc.in); got != tc.want {
			t.Fatalf("shellQuote(%q): got %s, want %s", tc.in, got, tc.want)
		}
	}
}

// TestSandboxDiffCommandOutput runs the script the sandbox would run, against
// a real repository. Its whole job is what `git diff BASE` alone cannot do:
// account for files git has never been told about, without writing to the
// repository an agent is working in.
func TestSandboxDiffCommandOutput(t *testing.T) {
	repo := newGitRepo(t)
	repo.write("doomed.txt", "temporary\n")
	base := repo.commit("tracked.txt", "one\n", "init")

	// One committed change, one unstaged edit, one file git has never seen, and
	// one deletion: all four are the sandbox's work, and all four must show.
	repo.commit("tracked.txt", "one\ntwo\n", "second")
	repo.write("tracked.txt", "one\ntwo\nthree\n")
	if err := os.Remove(filepath.Join(repo.dir, "doomed.txt")); err != nil {
		t.Fatal(err)
	}
	repo.write("untracked.txt", "new\n")
	repo.write("sub/it's a dir/new.txt", "nested\n")

	// The repository's own index must come out of this untouched: an agent is
	// working in it, and a diff that stages its files would be felt.
	indexBefore := repo.git("status", "--porcelain")

	full, err := repo.run(sandboxDiffCommand(base, nil, nil))
	if err != nil {
		t.Fatalf("diff script: %v", err)
	}
	for _, want := range []string{"+two", "+three", "b/untracked.txt", "+new", "+nested", "a/doomed.txt"} {
		if !strings.Contains(full, want) {
			t.Fatalf("diff missing %q:\n%s", want, full)
		}
	}
	if after := repo.git("status", "--porcelain"); after != indexBefore {
		t.Fatalf("the diff changed the repository's index:\nbefore:\n%s\nafter:\n%s", indexBefore, after)
	}

	// A pathspec narrows the diff, and survives being a directory name full of
	// shell syntax.
	narrowed, err := repo.run(sandboxDiffCommand(base, nil, []string{"sub/it's a dir"}))
	if err != nil {
		t.Fatalf("diff script: %v", err)
	}
	if !strings.Contains(narrowed, "+nested") {
		t.Fatalf("pathspec dropped the file it names:\n%s", narrowed)
	}
	for _, unwanted := range []string{"+three", "untracked.txt"} {
		if strings.Contains(narrowed, unwanted) {
			t.Fatalf("pathspec did not exclude %q:\n%s", unwanted, narrowed)
		}
	}

	// The pathspec has to narrow the "git add" as well as the diff, or a diff of
	// one directory still pays to hash every untracked file in the tree. What
	// the pathspec excludes must never reach the object database at all.
	excluded := repo.git("hash-object", "untracked.txt")
	if err := repo.gitErr("cat-file", "-e", excluded); err != nil {
		t.Fatalf("an unnarrowed diff should have hashed untracked.txt: %v", err)
	}
	fresh := newGitRepo(t)
	fresh.commit("tracked.txt", "one\n", "init")
	fresh.write("wanted/new.txt", "wanted\n")
	fresh.write("store/huge.txt", "not wanted\n")
	unwanted := fresh.git("hash-object", "store/huge.txt")
	if _, err := fresh.run(sandboxDiffCommand(fresh.git("rev-parse", "HEAD"), nil, []string{"wanted"})); err != nil {
		t.Fatalf("diff script: %v", err)
	}
	if err := fresh.gitErr("cat-file", "-e", unwanted); err == nil {
		t.Fatal("a pathspec-narrowed diff hashed a file outside the pathspec")
	}

	// Comparing trees rather than a tree against the working copy is what makes
	// this one diff, so git's own formats summarize it once.
	stat, err := repo.run(sandboxDiffCommand(base, diffOptions{stat: true}.gitArgs(false), nil))
	if err != nil {
		t.Fatalf("diff script: %v", err)
	}
	if got := strings.Count(stat, "files changed"); got != 1 {
		t.Fatalf("--stat produced %d summaries, want 1:\n%s", got, stat)
	}
}

// TestSandboxDiffCommandAgainstSnapshotBase is the base that made the tree
// comparison necessary: against a base that already contains the untracked
// files, comparing to the working copy would report them as deleted, because
// the index is what git consults for what exists.
func TestSandboxDiffCommandAgainstSnapshotBase(t *testing.T) {
	repo := newGitRepo(t)
	repo.commit("tracked.txt", "one\n", "init")

	// A snapshot commit holding the work handed to the sandbox, including a file
	// that is untracked in the sandbox's own working copy.
	repo.git("checkout", "-q", "-b", "snap")
	snapshot := repo.commit("carried.txt", "handed over\n", "workspace snapshot")
	repo.git("checkout", "-q", "main")
	repo.write("carried.txt", "handed over\n")

	out, err := repo.run(sandboxDiffCommand(snapshot, nil, nil))
	if err != nil {
		t.Fatalf("diff script: %v", err)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("a sandbox that changed nothing should produce no diff:\n%s", out)
	}
}

// TestSandboxDiffCommandWithoutAnIndex covers a repository that has no index
// file to seed the scratch one from. mktemp leaves an empty file behind and git
// rejects an empty index rather than initializing it, so the diff has to hand
// git a name with no file at it.
func TestSandboxDiffCommandWithoutAnIndex(t *testing.T) {
	repo := newGitRepo(t)
	base := repo.commit("tracked.txt", "one\n", "init")
	repo.write("tracked.txt", "one\ntwo\n")
	repo.write("untracked.txt", "new\n")
	if err := os.Remove(filepath.Join(repo.dir, ".git", "index")); err != nil {
		t.Fatal(err)
	}

	out, err := repo.run(sandboxDiffCommand(base, nil, nil))
	if err != nil {
		t.Fatalf("diff script: %v", err)
	}
	for _, want := range []string{"+two", "b/untracked.txt"} {
		if !strings.Contains(out, want) {
			t.Fatalf("diff missing %q:\n%s", want, out)
		}
	}
}
