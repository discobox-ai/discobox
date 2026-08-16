package sandboxcreate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
)

func TestResolveRunSourceCleanLocalBranch(t *testing.T) {
	repo := newRunSourceTestRepo(t)
	git := runSourceTestGit(t, repo)
	baseCommit := strings.TrimSpace(git("rev-parse", "HEAD"))

	source, err := resolveRunSource(context.Background(), repo, runSourceOptions{IncludeDirty: IncludeDirtyAuto})
	if err != nil {
		t.Fatalf("resolveRunSource: %v", err)
	}
	if source.Kind != runSourceKindGit || source.LocalDirectory != repo || source.RepoRoot != repo {
		t.Fatalf("source identity = %#v, want local git repo %s", source, repo)
	}
	if source.Destination.Directory != repo || source.Destination.WorkingDirectory != repo {
		t.Fatalf("destination = %#v, want repo root", source.Destination)
	}
	if source.Checkout.Commit != baseCommit || source.Checkout.RefName != "feature-foo" || source.Checkout.RefType != runSourceRefTypeBranch {
		t.Fatalf("checkout = %#v, want feature branch at %s", source.Checkout, baseCommit)
	}
	if source.Workspace.Mode != runWorkspaceModeClean || source.Workspace.SnapshotRef != "" {
		t.Fatalf("workspace = %#v, want clean without snapshot", source.Workspace)
	}
}

func TestResolveRunSourceDirtyLocalCreatesHiddenSnapshotRef(t *testing.T) {
	repo := newRunSourceTestRepo(t)
	git := runSourceTestGit(t, repo)
	baseCommit := strings.TrimSpace(git("rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "new.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	statusBefore := git("status", "--porcelain=v1", "--untracked-files=all")

	source, err := resolveRunSource(context.Background(), repo, runSourceOptions{IncludeDirty: IncludeDirtyAuto})
	if err != nil {
		t.Fatalf("resolveRunSource: %v", err)
	}
	if source.Checkout.Commit != baseCommit || source.Checkout.RefName != "feature-foo" || source.Checkout.RefType != runSourceRefTypeBranch {
		t.Fatalf("checkout = %#v, want current branch at base commit", source.Checkout)
	}
	if source.Workspace.Mode != runWorkspaceModeDirty || !strings.HasPrefix(source.Workspace.SnapshotRef, runSnapshotRefPrefix) || source.Workspace.BaseCommit != baseCommit {
		t.Fatalf("workspace = %#v, want dirty snapshot based on %s", source.Workspace, baseCommit)
	}
	snapshotCommit := strings.TrimSpace(git("rev-parse", source.Workspace.SnapshotRef+"^{commit}"))
	parentCommit := strings.TrimSpace(git("rev-parse", snapshotCommit+"^"))
	if parentCommit != baseCommit {
		t.Fatalf("snapshot parent = %s, want %s", parentCommit, baseCommit)
	}
	if statusAfter := git("status", "--porcelain=v1", "--untracked-files=all"); statusAfter != statusBefore {
		t.Fatalf("status changed after resolution:\nbefore:\n%s\nafter:\n%s", statusBefore, statusAfter)
	}
}

func TestResolveRunSourceIncludeDirtyNeverKeepsDirtyWorkspaceOutOfTheSandbox(t *testing.T) {
	repo := newRunSourceTestRepo(t)
	git := runSourceTestGit(t, repo)
	baseCommit := strings.TrimSpace(git("rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	source, err := resolveRunSource(context.Background(), repo, runSourceOptions{IncludeDirty: IncludeDirtyNever})
	if err != nil {
		t.Fatalf("resolveRunSource: %v", err)
	}
	if source.Checkout.Commit != baseCommit || source.Checkout.RefName != "feature-foo" {
		t.Fatalf("checkout = %#v, want current branch at base commit", source.Checkout)
	}
	if source.Workspace.Mode != runWorkspaceModeClean || source.Workspace.SnapshotRef != "" {
		t.Fatalf("workspace = %#v, want clean without snapshot", source.Workspace)
	}
	// The dirty answer is "no", so nothing about the repo may change: no
	// snapshot commit means no snapshot ref either.
	if refs := strings.TrimSpace(git("for-each-ref", "--format=%(refname)", runSnapshotRefPrefix)); refs != "" {
		t.Fatalf("snapshot refs = %q, want none", refs)
	}
}

func TestResolveRunSourceIncludeDirtyAutoAsksAndHonorsTheAnswer(t *testing.T) {
	for _, tc := range []struct {
		name     string
		include  bool
		wantMode string
	}{
		{name: "declined", include: false, wantMode: runWorkspaceModeClean},
		{name: "accepted", include: true, wantMode: runWorkspaceModeDirty},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newRunSourceTestRepo(t)
			if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("dirty\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, "new.txt"), []byte("new\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			var asked []DirtyWorkspace
			source, err := resolveRunSource(context.Background(), repo, runSourceOptions{
				IncludeDirty: IncludeDirtyAuto,
				Confirm: func(_ context.Context, workspace DirtyWorkspace) (bool, error) {
					asked = append(asked, workspace)
					return tc.include, nil
				},
			})
			if err != nil {
				t.Fatalf("resolveRunSource: %v", err)
			}
			if len(asked) != 1 {
				t.Fatalf("confirm called %d times, want once", len(asked))
			}
			if asked[0].RepoRoot != repo || len(asked[0].Changes) != 2 {
				t.Fatalf("confirmed workspace = %#v, want %s with both changed paths", asked[0], repo)
			}
			if source.Workspace.Mode != tc.wantMode {
				t.Fatalf("workspace = %#v, want mode %s", source.Workspace, tc.wantMode)
			}
		})
	}
}

func TestResolveRunSourceIncludeDirtyAutoDoesNotAskWhenWorkspaceIsClean(t *testing.T) {
	repo := newRunSourceTestRepo(t)

	source, err := resolveRunSource(context.Background(), repo, runSourceOptions{
		IncludeDirty: IncludeDirtyAuto,
		Confirm: func(context.Context, DirtyWorkspace) (bool, error) {
			t.Fatal("confirm called for a clean workspace")
			return false, nil
		},
	})
	if err != nil {
		t.Fatalf("resolveRunSource: %v", err)
	}
	if source.Workspace.Mode != runWorkspaceModeClean {
		t.Fatalf("workspace = %#v, want clean", source.Workspace)
	}
}

func TestResolveRunSourceIncludeDirtyAlwaysRejectsSourcesWithoutAWorkingTree(t *testing.T) {
	repo := newRunSourceTestRepo(t)

	if _, err := resolveRunSource(context.Background(), repo+"@feature-foo", runSourceOptions{IncludeDirty: IncludeDirtyAlways}); err == nil {
		t.Fatal("explicit ref with --include-dirty=true: want error, got none")
	}
	remoteURL := "file://" + filepath.ToSlash(repo)
	if _, err := resolveRunSource(context.Background(), remoteURL, runSourceOptions{IncludeDirty: IncludeDirtyAlways}); err == nil {
		t.Fatal("remote source with --include-dirty=true: want error, got none")
	}
}

func TestIncludeDirtySetNormalizesAndRejects(t *testing.T) {
	for value, want := range map[string]IncludeDirty{
		"":      IncludeDirtyAuto,
		"AUTO":  IncludeDirtyAuto,
		"true":  IncludeDirtyAlways,
		" yes ": IncludeDirtyAlways,
		"false": IncludeDirtyNever,
		"n":     IncludeDirtyNever,
	} {
		var mode IncludeDirty
		if err := mode.Set(value); err != nil {
			t.Fatalf("Set(%q): %v", value, err)
		}
		if mode != want {
			t.Fatalf("Set(%q) = %q, want %q", value, mode, want)
		}
	}
	var mode IncludeDirty
	if err := mode.Set("maybe"); err == nil {
		t.Fatal("Set(\"maybe\"): want error, got none")
	}
}

func TestResolveRunSourceLocalSubdirectoryUsesRepoRootDestinationAndSubdirWorkingDirectory(t *testing.T) {
	repo := newRunSourceTestRepo(t)
	subdir := filepath.Join(repo, "nested", "work")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(subdir)

	source, err := resolveRunSource(context.Background(), ".", runSourceOptions{IncludeDirty: IncludeDirtyAuto})
	if err != nil {
		t.Fatalf("resolveRunSource: %v", err)
	}
	if source.LocalDirectory != repo || source.RepoRoot != repo {
		t.Fatalf("source identity = %#v, want repo root %s", source, repo)
	}
	if source.Destination.Directory != repo || source.Destination.WorkingDirectory != subdir {
		t.Fatalf("destination = %#v, want directory %s working directory %s", source.Destination, repo, subdir)
	}
}

func TestResolveRunSourceLocalSubdirectoryOutsideCurrentWorkingDirectoryKeepsSubdirWorkingDirectory(t *testing.T) {
	repo := newRunSourceTestRepo(t)
	subdir := filepath.Join(repo, "nested", "work")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())

	source, err := resolveRunSource(context.Background(), subdir, runSourceOptions{IncludeDirty: IncludeDirtyAuto})
	if err != nil {
		t.Fatalf("resolveRunSource: %v", err)
	}
	if source.Destination.Directory != repo || source.Destination.WorkingDirectory != subdir {
		t.Fatalf("destination = %#v, want directory %s working directory %s", source.Destination, repo, subdir)
	}
}

func TestResolveRunSourceLocalRepoRootOutsideCurrentWorkingDirectoryUsesRepoRoot(t *testing.T) {
	repo := newRunSourceTestRepo(t)
	t.Chdir(t.TempDir())

	source, err := resolveRunSource(context.Background(), repo, runSourceOptions{IncludeDirty: IncludeDirtyAuto})
	if err != nil {
		t.Fatalf("resolveRunSource: %v", err)
	}
	if source.Destination.Directory != repo || source.Destination.WorkingDirectory != repo {
		t.Fatalf("destination = %#v, want repo root", source.Destination)
	}
}

func TestResolvedRunSourceConvertsToAPIGitSource(t *testing.T) {
	source := resolvedRunSource{
		Kind:           runSourceKindGit,
		LocalDirectory: "/repo",
		Checkout: resolvedRunSourceCheckout{
			Commit:  "abc123",
			RefName: "feature-foo",
			RefType: runSourceRefTypeBranch,
		},
		Workspace: resolvedRunSourceWorkspace{
			Mode:        runWorkspaceModeDirty,
			SnapshotRef: "refs/discobox/run/snapshot",
			BaseCommit:  "abc123",
		},
		Destination: resolvedRunSourceDestination{
			Directory:        "/workspace/source",
			WorkingDirectory: "/workspace/source",
		},
	}

	apiSource, err := source.apiGitSource()
	if err != nil {
		t.Fatalf("apiGitSource: %v", err)
	}
	if apiSource.Kind != apiclientgen.GitSourceKindGit || apiSource.LocalDirectory.Value != "/repo" {
		t.Fatalf("api source identity = %#v", apiSource)
	}
	checkout, ok := apiSource.Checkout.Get()
	if !ok || checkout.Commit.Value != "abc123" || checkout.RefName.Value != "feature-foo" || checkout.RefType.Value != runSourceRefTypeBranch {
		t.Fatalf("api checkout = %#v, ok=%t", checkout, ok)
	}
	workspace, ok := apiSource.Workspace.Get()
	if !ok || workspace.Mode.Value != apiclientgen.GitSourceWorkspaceModeDirty || workspace.SnapshotRef.Value != "refs/discobox/run/snapshot" || workspace.BaseCommit.Value != "abc123" {
		t.Fatalf("api workspace = %#v, ok=%t", workspace, ok)
	}
	destination, ok := apiSource.Destination.Get()
	if !ok || destination.Directory.Value != "/workspace/source" || destination.WorkingDirectory.Value != "/workspace/source" {
		t.Fatalf("api destination = %#v, ok=%t", destination, ok)
	}
}

func TestResolveRunSourceExplicitHEADIgnoresDirtyWorkspace(t *testing.T) {
	repo := newRunSourceTestRepo(t)
	git := runSourceTestGit(t, repo)
	baseCommit := strings.TrimSpace(git("rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	source, err := resolveRunSource(context.Background(), repo+"@HEAD", runSourceOptions{IncludeDirty: IncludeDirtyAuto})
	if err != nil {
		t.Fatalf("resolveRunSource: %v", err)
	}
	if source.Checkout.Commit != baseCommit || source.Checkout.RefName != "" || source.Checkout.RefType != runSourceRefTypeCommit {
		t.Fatalf("checkout = %#v, want explicit HEAD as commit", source.Checkout)
	}
	if source.Workspace.Mode != runWorkspaceModeClean || source.Workspace.SnapshotRef != "" {
		t.Fatalf("workspace = %#v, want clean explicit checkout", source.Workspace)
	}
}

func TestResolveRunSourceRemoteBranchPinsCommitAndKeepsBranchName(t *testing.T) {
	repo := newRunSourceTestRepo(t)
	git := runSourceTestGit(t, repo)
	commit := strings.TrimSpace(git("rev-parse", "HEAD"))
	remoteURL := "file://" + filepath.ToSlash(repo)

	source, err := resolveRunSource(context.Background(), remoteURL+"@feature-foo", runSourceOptions{IncludeDirty: IncludeDirtyAuto})
	if err != nil {
		t.Fatalf("resolveRunSource: %v", err)
	}
	if source.URL != remoteURL || source.LocalDirectory != "" {
		t.Fatalf("source identity = %#v, want remote URL", source)
	}
	if source.Checkout.Commit != commit || source.Checkout.RefName != "feature-foo" || source.Checkout.RefType != runSourceRefTypeBranch {
		t.Fatalf("checkout = %#v, want remote branch pinned to %s", source.Checkout, commit)
	}
	if source.Workspace.Mode != runWorkspaceModeClean {
		t.Fatalf("workspace = %#v, want clean", source.Workspace)
	}
}

func newRunSourceTestRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	git := runSourceTestGit(t, repo)
	git("init", "-b", "feature-foo")
	git("config", "user.email", "test@example.com")
	git("config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "tracked.txt")
	git("commit", "-m", "base")
	return repo
}

func runSourceTestGit(t *testing.T, dir string) func(...string) string {
	t.Helper()
	return func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(context.Background(), "git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, string(out))
		}
		return string(out)
	}
}

func TestResolveRunSourceDirectoryWithoutRepositoryCarriesEverythingAsASnapshot(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	source, err := resolveRunSource(context.Background(), dir, runSourceOptions{IncludeDirty: IncludeDirtyAuto})
	if err != nil {
		t.Fatalf("resolveRunSource: %v", err)
	}
	local := source.localSource()

	// The directory is what the sandbox mirrors and what apply comes back to;
	// the repository the source was built in is somewhere else entirely.
	if source.LocalDirectory != dir || !source.NoLocalRepository {
		t.Fatalf("source identity = %#v, want %s recorded as a directory with no repository", source, dir)
	}
	if source.RepoRoot == dir || !filepath.IsAbs(source.RepoRoot) {
		t.Fatalf("repo root = %q, want a repository outside %s", source.RepoRoot, dir)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); !os.IsNotExist(err) {
		t.Fatalf("stat %s/.git = %v, want the user's directory left alone", dir, err)
	}
	if source.Destination.Directory != dir || source.Destination.WorkingDirectory != dir {
		t.Fatalf("destination = %#v, want the directory itself", source.Destination)
	}
	if source.Checkout.Commit == "" || source.Checkout.RefName == "" || source.Checkout.RefType != runSourceRefTypeBranch {
		t.Fatalf("checkout = %#v, want a branch at the empty base commit", source.Checkout)
	}
	if source.Workspace.Mode != runWorkspaceModeDirty || !strings.HasPrefix(source.Workspace.SnapshotRef, runSnapshotRefPrefix) || source.Workspace.BaseCommit != source.Checkout.Commit {
		t.Fatalf("workspace = %#v, want a snapshot on the checked-out commit", source.Workspace)
	}

	git := runSourceTestGit(t, source.RepoRoot)
	// The base is a root commit of nothing, and the whole directory is the
	// uncommitted work on top of it.
	if parents := strings.TrimSpace(git("rev-list", "--parents", "-n", "1", source.Checkout.Commit)); parents != source.Checkout.Commit {
		t.Fatalf("base commit parents = %q, want the root commit %s alone", parents, source.Checkout.Commit)
	}
	if branchHead := strings.TrimSpace(git("rev-parse", "refs/heads/"+source.Checkout.RefName)); branchHead != source.Checkout.Commit {
		t.Fatalf("branch %s = %s, want the base commit %s", source.Checkout.RefName, branchHead, source.Checkout.Commit)
	}
	added := strings.Fields(git("diff", "--name-only", source.Checkout.Commit, source.Workspace.SnapshotRef))
	if strings.Join(added, ",") != "a.txt,sub/b.txt" {
		t.Fatalf("snapshot contents = %v, want every file in the directory", added)
	}

	local.Close()
	if _, err := os.Stat(source.RepoRoot); !os.IsNotExist(err) {
		t.Fatalf("stat %s = %v, want the repository removed once the source is delivered", source.RepoRoot, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); err != nil {
		t.Fatalf("the directory did not survive its repository: %v", err)
	}
}

func TestResolveRunSourceEmptyDirectoryWithoutRepositoryStartsFromTheEmptyCommit(t *testing.T) {
	dir := t.TempDir()

	source, err := resolveRunSource(context.Background(), dir, runSourceOptions{IncludeDirty: IncludeDirtyAuto})
	if err != nil {
		t.Fatalf("resolveRunSource: %v", err)
	}
	defer source.localSource().Close()

	// Nothing to carry is not an error: an empty directory is a legitimate
	// place to start, and the sandbox gets the empty commit it checks out.
	if source.Workspace.Mode != runWorkspaceModeClean || source.Workspace.SnapshotRef != "" {
		t.Fatalf("workspace = %#v, want a clean checkout with no snapshot", source.Workspace)
	}
	git := runSourceTestGit(t, source.RepoRoot)
	if files := strings.TrimSpace(git("ls-tree", "-r", "--name-only", source.Checkout.Commit)); files != "" {
		t.Fatalf("base commit contains %q, want an empty tree", files)
	}
}

func TestResolveRunSourceDirectoryWithoutRepositoryRejectsWhatItCannotMean(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// There is no history to name a ref in.
	if _, err := resolveRunSource(context.Background(), dir+"@main", runSourceOptions{IncludeDirty: IncludeDirtyAuto}); err == nil {
		t.Fatal("an explicit ref against a directory with no repository was accepted")
	}
	// Excluding the uncommitted work would leave an empty sandbox, which is
	// never what the flag is asking for.
	if _, err := resolveRunSource(context.Background(), dir, runSourceOptions{IncludeDirty: IncludeDirtyNever}); err == nil {
		t.Fatal("--include-dirty=false against a directory with no repository was accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); !os.IsNotExist(err) {
		t.Fatalf("stat %s/.git = %v, want a rejected source to leave nothing behind", dir, err)
	}
}

func TestResolveRunSourceLocalRepositoryPushesOutOfThatRepository(t *testing.T) {
	repo := newRunSourceTestRepo(t)

	source, err := resolveRunSource(context.Background(), repo, runSourceOptions{IncludeDirty: IncludeDirtyAuto})
	if err != nil {
		t.Fatalf("resolveRunSource: %v", err)
	}
	if source.NoLocalRepository {
		t.Fatal("a real repository was reported as having none")
	}
	local := source.localSource()
	root, err := local.pushRoot()
	if err != nil || root != repo {
		t.Fatalf("pushRoot = %q, %v, want the repository %s", root, err, repo)
	}
	// Closing a real repository releases nothing: it was here before the run
	// and stays after it.
	local.Close()
	if _, err := os.Stat(filepath.Join(repo, ".git")); err != nil {
		t.Fatalf("stat %s/.git = %v, want the user's repository untouched", repo, err)
	}
}
