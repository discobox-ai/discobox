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

	source, err := resolveRunSource(context.Background(), repo)
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

	source, err := resolveRunSource(context.Background(), repo)
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

func TestResolveRunSourceLocalSubdirectoryUsesRepoRootDestinationAndSubdirWorkingDirectory(t *testing.T) {
	repo := newRunSourceTestRepo(t)
	subdir := filepath.Join(repo, "nested", "work")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(subdir)

	source, err := resolveRunSource(context.Background(), ".")
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

	source, err := resolveRunSource(context.Background(), subdir)
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

	source, err := resolveRunSource(context.Background(), repo)
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

	source, err := resolveRunSource(context.Background(), repo+"@HEAD")
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

	source, err := resolveRunSource(context.Background(), remoteURL+"@feature-foo")
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
