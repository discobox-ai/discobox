package sandboxcreate

import (
	"context"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/cli/internal/gitunborn"
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
	if want := wantRunDestination(t, repo, repo); source.Destination != want {
		t.Fatalf("destination = %#v, want %#v", source.Destination, want)
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
	if want := wantRunDestination(t, repo, subdir); source.Destination != want {
		t.Fatalf("destination = %#v, want %#v", source.Destination, want)
	}
}

func TestResolveRunSourceLocalSubdirectoryOutsideCurrentWorkingDirectoryKeepsSubdirWorkingDirectory(t *testing.T) {
	repo := newRunSourceTestRepo(t)
	subdir := filepath.Join(repo, "nested", "work")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(testWorkspace(t))

	source, err := resolveRunSource(context.Background(), subdir, runSourceOptions{IncludeDirty: IncludeDirtyAuto})
	if err != nil {
		t.Fatalf("resolveRunSource: %v", err)
	}
	if want := wantRunDestination(t, repo, subdir); source.Destination != want {
		t.Fatalf("destination = %#v, want %#v", source.Destination, want)
	}
}

func TestResolveRunSourceLocalRepoRootOutsideCurrentWorkingDirectoryUsesRepoRoot(t *testing.T) {
	repo := newRunSourceTestRepo(t)
	t.Chdir(testWorkspace(t))

	source, err := resolveRunSource(context.Background(), repo, runSourceOptions{IncludeDirty: IncludeDirtyAuto})
	if err != nil {
		t.Fatalf("resolveRunSource: %v", err)
	}
	if want := wantRunDestination(t, repo, repo); source.Destination != want {
		t.Fatalf("destination = %#v, want %#v", source.Destination, want)
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
	repo := testWorkspace(t)
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
	dir := testWorkspace(t)
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
	local := testLocalSources(source)

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
	if want := wantRunDestination(t, dir, dir); source.Destination != want {
		t.Fatalf("destination = %#v, want %#v", source.Destination, want)
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
	dir := testWorkspace(t)

	source, err := resolveRunSource(context.Background(), dir, runSourceOptions{IncludeDirty: IncludeDirtyAuto})
	if err != nil {
		t.Fatalf("resolveRunSource: %v", err)
	}
	defer testLocalSources(source).Close()

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

func TestResolveRunSourceDirectoryWithoutRepositoryRejectsARefItCannotHave(t *testing.T) {
	dir := testWorkspace(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// There is no history to name a ref in.
	if _, err := resolveRunSource(context.Background(), dir+"@main", runSourceOptions{IncludeDirty: IncludeDirtyAuto}); err == nil {
		t.Fatal("an explicit ref against a directory with no repository was accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); !os.IsNotExist(err) {
		t.Fatalf("stat %s/.git = %v, want a rejected source to leave nothing behind", dir, err)
	}
}

// Declining the copy is an answer, not a cancel: it resolves to no source at
// all — not a repository of nothing at the directory's path — and the directory
// stays here untouched (ADR 0077 §1).
func TestResolveRunSourceDirectoryWithoutRepositoryNotCopiedResolvesToNoSource(t *testing.T) {
	dir := testWorkspace(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	asked := 0
	decline := func(_ context.Context, directory DirectoryCopy) (bool, error) {
		asked++
		if directory.Dir != dir {
			t.Fatalf("asked about %q, want the source directory %s", directory.Dir, dir)
		}
		if directory.Size == nil {
			t.Fatal("asked with nothing measuring the directory")
		}
		return false, nil
	}

	source, err := resolveRunSource(context.Background(), dir, runSourceOptions{IncludeDirty: IncludeDirtyAuto, ConfirmCopy: decline})
	if err != nil {
		t.Fatalf("resolveRunSource: %v", err)
	}
	defer testLocalSources(source).Close()
	if asked != 1 {
		t.Fatalf("asked %d times, want exactly one question", asked)
	}
	if source.resolved() {
		t.Fatalf("source = %#v, want no source at all", source)
	}
	// Declining costs nothing: the repository the copy would have been indexed
	// into is never built, which is the whole reason the question comes first.
	if source.RepoRoot != "" || source.cleanup != nil {
		t.Fatalf("source = %#v, want nothing built over the directory", source)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); !os.IsNotExist(err) {
		t.Fatalf("stat %s/.git = %v, want the user's directory left alone", dir, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); err != nil {
		t.Fatalf("the directory did not survive being left out: %v", err)
	}
}

// --include-dirty=false is the same answer given ahead of time, and answering
// it that way asks nobody anything.
func TestResolveRunSourceDirectoryWithoutRepositoryIncludeDirtyNeverResolvesToNoSource(t *testing.T) {
	dir := testWorkspace(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	confirm := func(context.Context, DirectoryCopy) (bool, error) {
		t.Fatal("--include-dirty=false asked a question it had already answered")
		return false, nil
	}

	source, err := resolveRunSource(context.Background(), dir, runSourceOptions{IncludeDirty: IncludeDirtyNever, ConfirmCopy: confirm})
	if err != nil {
		t.Fatalf("resolveRunSource: %v", err)
	}
	defer testLocalSources(source).Close()
	if source.resolved() {
		t.Fatalf("source = %#v, want no source at all", source)
	}
}

// Accepting is the behavior the directory source has always had, and the
// question is not asked at all when there is no answer to it: nobody to ask,
// an answer already given, or nothing in the directory to copy.
func TestResolveRunSourceDirectoryWithoutRepositoryCopiesWhenItIsMeantTo(t *testing.T) {
	newDir := func(t *testing.T) string {
		t.Helper()
		dir := testWorkspace(t)
		if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	refuse := func(context.Context, DirectoryCopy) (bool, error) {
		t.Helper()
		t.Error("a question was asked that had already been answered")
		return false, nil
	}
	accept := func(context.Context, DirectoryCopy) (bool, error) { return true, nil }

	for _, tc := range []struct {
		name     string
		dir      func(*testing.T) string
		opts     runSourceOptions
		snapshot bool
	}{
		{name: "answered yes", dir: newDir, opts: runSourceOptions{IncludeDirty: IncludeDirtyAuto, ConfirmCopy: accept}, snapshot: true},
		{name: "include-dirty=true", dir: newDir, opts: runSourceOptions{IncludeDirty: IncludeDirtyAlways, ConfirmCopy: refuse}, snapshot: true},
		{name: "nobody to ask", dir: newDir, opts: runSourceOptions{IncludeDirty: IncludeDirtyAuto}, snapshot: true},
		{name: "empty directory", dir: testWorkspace, opts: runSourceOptions{IncludeDirty: IncludeDirtyAuto, ConfirmCopy: refuse}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source, err := resolveRunSource(context.Background(), tc.dir(t), tc.opts)
			if err != nil {
				t.Fatalf("resolveRunSource: %v", err)
			}
			defer testLocalSources(source).Close()
			if snapshot := source.Workspace.Mode == runWorkspaceModeDirty; snapshot != tc.snapshot {
				t.Fatalf("workspace = %#v, want snapshot = %v", source.Workspace, tc.snapshot)
			}
		})
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
	local := testLocalSources(source)
	root, err := local.pushRoot("")
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

// testLocalSources is the single-source LocalSources a create builds for a
// sandbox with no source code references.
func testLocalSources(source resolvedRunSource) *LocalSources {
	local := &LocalSources{}
	local.add("", source)
	return local
}

// wantRunDestination is where a local repo root and working directory land
// inside the sandbox on the platform the test is running on.
//
// On a POSIX host the destination mirrors the host path, because a POSIX path
// is already usable inside the sandbox. A Windows path is not -- the sandbox
// runs Linux -- so it is mirrored under the /mnt name WSL gives that same path.
// The invariant both share, and what these tests are really about, is that the
// working directory sits at the same relative position under the source root.
func wantRunDestination(t *testing.T, repoRoot, workingDirectory string) resolvedRunSourceDestination {
	t.Helper()
	if runtime.GOOS != "windows" {
		return resolvedRunSourceDestination{Directory: repoRoot, WorkingDirectory: workingDirectory}
	}
	return resolvedRunSourceDestination{
		Directory:        wantWSLPath(t, repoRoot),
		WorkingDirectory: wantWSLPath(t, workingDirectory),
	}
}

// wantSandboxPath is the path a host directory has inside the sandbox on the
// platform the test is running on.
func wantSandboxPath(t *testing.T, hostPath string) string {
	t.Helper()
	if runtime.GOOS != "windows" {
		return hostPath
	}
	return wantWSLPath(t, hostPath)
}

// wantWSLPath spells a Windows path the way WSL mounts it, written out here
// rather than taken from the code under test.
func wantWSLPath(t *testing.T, hostPath string) string {
	t.Helper()
	volume := filepath.VolumeName(hostPath)
	if len(volume) != 2 || volume[1] != ':' {
		t.Fatalf("test path %s has no drive letter to mount", hostPath)
	}
	return path.Clean("/mnt/" + strings.ToLower(volume[:1]) + filepath.ToSlash(hostPath[len(volume):]))
}

func TestWSLPathLowercasesTheDriveAndKeepsTheRestOfTheCase(t *testing.T) {
	for _, tt := range []struct {
		hostPath string
		want     string
		ok       bool
	}{
		{hostPath: `E:\src\disco2`, want: "/mnt/e/src/disco2", ok: true},
		{hostPath: `C:\Users\darre`, want: "/mnt/c/Users/darre", ok: true},
		{hostPath: "C:/Users/darre", want: "/mnt/c/Users/darre", ok: true},
		{hostPath: `c:\`, want: "/mnt/c", ok: true},
		// A path on a share, or already inside a distro, has no /mnt name.
		{hostPath: `\\wsl$\Ubuntu\home\darre`},
		{hostPath: `\\server\share\src`},
		{hostPath: "/home/darre"},
		{hostPath: "relative/path"},
	} {
		got, ok := wslPath(tt.hostPath)
		if ok != tt.ok || got != tt.want {
			t.Errorf("wslPath(%q) = %q, %t, want %q, %t", tt.hostPath, got, ok, tt.want, tt.ok)
		}
	}
}

func TestResolveRunSourceUnbornRepositoryCarriesTheWorkingTreeAsASnapshot(t *testing.T) {
	repo := newUnbornRunSourceTestRepo(t)
	git := runSourceTestGit(t, repo)
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "sub", "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Staged for a first commit the user has not made yet: the real index must
	// come back exactly as it was.
	git("add", "a.txt")
	statusBefore := git("status", "--porcelain=v1", "--untracked-files=all")

	source, err := resolveRunSource(context.Background(), repo, runSourceOptions{IncludeDirty: IncludeDirtyAuto})
	if err != nil {
		t.Fatalf("resolveRunSource: %v", err)
	}

	// The repository is the user's own, so it is what the sandbox mirrors and
	// what a later push delivers out of — no repository was built anywhere else.
	if source.LocalDirectory != repo || source.RepoRoot != repo || source.NoLocalRepository {
		t.Fatalf("source identity = %#v, want the repository at %s itself", source, repo)
	}
	if !source.NoLocalCommits {
		t.Fatalf("source = %#v, want a repository reported as having no commits", source)
	}
	if want := wantRunDestination(t, repo, repo); source.Destination != want {
		t.Fatalf("destination = %#v, want %#v", source.Destination, want)
	}
	if source.Checkout.RefName != "main" || source.Checkout.RefType != runSourceRefTypeBranch || source.Checkout.Commit == "" {
		t.Fatalf("checkout = %#v, want the unborn branch at the empty base commit", source.Checkout)
	}
	if source.Workspace.Mode != runWorkspaceModeDirty || !strings.HasPrefix(source.Workspace.SnapshotRef, runSnapshotRefPrefix) || source.Workspace.BaseCommit != source.Checkout.Commit {
		t.Fatalf("workspace = %#v, want a snapshot on the checked-out commit", source.Workspace)
	}

	if parents := strings.TrimSpace(git("rev-list", "--parents", "-n", "1", source.Checkout.Commit)); parents != source.Checkout.Commit {
		t.Fatalf("base commit parents = %q, want the root commit %s alone", parents, source.Checkout.Commit)
	}
	if files := strings.TrimSpace(git("ls-tree", "-r", "--name-only", source.Checkout.Commit)); files != "" {
		t.Fatalf("base commit contains %q, want an empty tree", files)
	}
	added := strings.Fields(git("diff", "--name-only", source.Checkout.Commit, source.Workspace.SnapshotRef))
	if strings.Join(added, ",") != "a.txt,sub/b.txt" {
		t.Fatalf("snapshot contents = %v, want every file in the working tree", added)
	}

	// HEAD is still unborn and the branch still does not exist: the user's
	// first commit is theirs to write.
	if !gitunborn.HeadIsUnborn(context.Background(), repo) {
		t.Fatalf("HEAD is born after resolving, want %s left with no commits", repo)
	}
	if statusAfter := git("status", "--porcelain=v1", "--untracked-files=all"); statusAfter != statusBefore {
		t.Fatalf("status = %q, want the index and working tree untouched (%q)", statusAfter, statusBefore)
	}
}

func TestResolveRunSourceEmptyUnbornRepositoryStartsFromTheEmptyCommit(t *testing.T) {
	repo := newUnbornRunSourceTestRepo(t)

	source, err := resolveRunSource(context.Background(), repo, runSourceOptions{IncludeDirty: IncludeDirtyAuto})
	if err != nil {
		t.Fatalf("resolveRunSource: %v", err)
	}

	// `git init` in an empty directory is how a project that does not exist yet
	// gets started: the sandbox comes up on the empty base at that path.
	if source.Workspace.Mode != runWorkspaceModeClean || source.Workspace.SnapshotRef != "" {
		t.Fatalf("workspace = %#v, want a clean checkout with no snapshot", source.Workspace)
	}
	if !source.NoLocalCommits || source.Checkout.Commit == "" {
		t.Fatalf("source = %#v, want the empty base commit reported as having no local commits", source)
	}
	// Nothing else in the repository points at the base commit, so it has to be
	// a ref or a later delivery is racing git's own pruning.
	git := runSourceTestGit(t, repo)
	refs := strings.TrimSpace(git("for-each-ref", "--format=%(objectname)", runSnapshotRefPrefix))
	if !strings.Contains(refs, source.Checkout.Commit) {
		t.Fatalf("refs under %s = %q, want the base commit %s held by one", runSnapshotRefPrefix, refs, source.Checkout.Commit)
	}
}

func TestResolveRunSourceUnbornRepositoryWithoutDirtyStartsFromTheEmptyCommit(t *testing.T) {
	repo := newUnbornRunSourceTestRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	source, err := resolveRunSource(context.Background(), repo, runSourceOptions{IncludeDirty: IncludeDirtyNever})
	if err != nil {
		t.Fatalf("resolveRunSource: %v", err)
	}

	// The flag answers ahead of time, and its answer keeps the repository's own
	// path rather than dropping the source: a repository is a project.
	if source.Kind != runSourceKindGit || source.LocalDirectory != repo {
		t.Fatalf("source = %#v, want the repository kept at %s", source, repo)
	}
	if source.Workspace.Mode != runWorkspaceModeClean || source.Workspace.SnapshotRef != "" {
		t.Fatalf("workspace = %#v, want the empty base with none of the working tree", source.Workspace)
	}
}

func TestResolveRunSourceUnbornRepositoryRefusesAnExplicitRef(t *testing.T) {
	repo := newUnbornRunSourceTestRepo(t)

	_, err := resolveRunSource(context.Background(), repo+"@main", runSourceOptions{IncludeDirty: IncludeDirtyAuto})
	if err == nil {
		t.Fatal("resolveRunSource accepted a ref in a repository with no commits")
	}
	// git's own "Needed a single revision" names neither the repository nor
	// what is actually wrong with the request.
	if !strings.Contains(err.Error(), "no commits yet") || !strings.Contains(err.Error(), repo) {
		t.Fatalf("error = %v, want it to name %s and its missing history", err, repo)
	}
}

// newUnbornRunSourceTestRepo is what `git init` leaves behind: a repository
// whose HEAD names a branch that has no commits.
func newUnbornRunSourceTestRepo(t *testing.T) string {
	t.Helper()
	repo := testWorkspace(t)
	git := runSourceTestGit(t, repo)
	git("init", "-b", "main")
	git("config", "user.email", "test@example.com")
	git("config", "user.name", "Test User")
	return repo
}
