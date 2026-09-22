package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/gitapply"
	"github.com/discobox-ai/discobox/cli/internal/gitunborn"
)

const thisHost = "hst_thismachine0001"

func sandboxWithOrigin(t *testing.T, hostID, hostname, localDir string) (*apimodel.Sandbox, applySourceEntry) {
	t.Helper()
	sandbox := &apimodel.Sandbox{}
	origin := apiclientgen.Origin{HostId: hostID}
	if hostname != "" {
		origin.Hostname = apiclientgen.NewOptString(hostname)
	}
	sandbox.Origin = apiclientgen.NewOptOrigin(origin)
	source := apimodel.GitSource{Slug: apiclientgen.NewOptString("primary")}
	if localDir != "" {
		source.LocalDirectory = apiclientgen.NewOptString(localDir)
	}
	return sandbox, applySourceEntry{slug: "primary", source: source}
}

// The sandbox was created here, from a directory that is still there: the one
// case that needs no --dir.
func TestResolveHostDirUsesRecordedDirectoryOnTheSameMachine(t *testing.T) {
	dir := t.TempDir()
	sandbox, entry := sandboxWithOrigin(t, thisHost, "laptop", dir)

	got, origin, err := resolveApplyHostDir(sandbox, thisHost, entry, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != dir {
		t.Fatalf("dir = %q, want %q", got, dir)
	}
	if origin != hostDirFromSandboxOrigin {
		t.Fatalf("origin = %q, want %q", origin, hostDirFromSandboxOrigin)
	}
}

// Applying a sandbox created elsewhere would cherry-pick onto whatever
// repository happens to sit at the recorded path here, which is not the same
// repository. It has to be named explicitly instead.
func TestResolveHostDirRefusesADifferentMachine(t *testing.T) {
	dir := t.TempDir()
	sandbox, entry := sandboxWithOrigin(t, "hst_othermachine0002", "build-box", dir)

	_, _, err := resolveApplyHostDir(sandbox, thisHost, entry, nil)
	if err == nil {
		t.Fatal("a sandbox from another machine resolved a directory anyway")
	}
	for _, want := range []string{"different machine", "hst_othermachine0002", `"build-box"`, thisHost, "--dir primary=PATH"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

func TestResolveHostDirRefusesASandboxWithNoOrigin(t *testing.T) {
	_, entry := sandboxWithOrigin(t, thisHost, "", t.TempDir())

	_, _, err := resolveApplyHostDir(&apimodel.Sandbox{}, thisHost, entry, nil)
	if err == nil {
		t.Fatal("a sandbox with no origin resolved a directory anyway")
	}
	if !strings.Contains(err.Error(), "no recorded origin") {
		t.Fatalf("error = %q", err)
	}
}

// A source cloned from a remote never had a local checkout here to go back to.
func TestResolveHostDirRefusesASourceWithNoLocalDirectory(t *testing.T) {
	sandbox, entry := sandboxWithOrigin(t, thisHost, "", "")

	_, _, err := resolveApplyHostDir(sandbox, thisHost, entry, nil)
	if err == nil {
		t.Fatal("a remote-cloned source resolved a directory anyway")
	}
	if !strings.Contains(err.Error(), "cloned from a remote") {
		t.Fatalf("error = %q", err)
	}
}

// Right machine, but the checkout has since been moved or deleted. Silently
// failing later inside git would blame the wrong thing.
func TestResolveHostDirRefusesAMissingDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone")
	sandbox, entry := sandboxWithOrigin(t, thisHost, "", missing)

	_, _, err := resolveApplyHostDir(sandbox, thisHost, entry, nil)
	if err == nil {
		t.Fatal("a missing directory resolved anyway")
	}
	if !strings.Contains(err.Error(), missing) || !strings.Contains(err.Error(), "--dir primary=PATH") {
		t.Fatalf("error = %q", err)
	}
}

// --dir is the escape hatch for every case above, so it deliberately skips the
// identity check — and the report says so rather than implying the sandbox
// vouched for the directory.
func TestResolveHostDirOverrideSkipsTheIdentityCheck(t *testing.T) {
	sandbox, entry := sandboxWithOrigin(t, "hst_othermachine0002", "", "/gone/on/this/machine")

	got, origin, err := resolveApplyHostDir(sandbox, thisHost, entry, map[string]string{"primary": "/home/ada/src/web"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "/home/ada/src/web" {
		t.Fatalf("dir = %q", got)
	}
	if origin != hostDirFromOverride {
		t.Fatalf("origin = %q, want %q", origin, hostDirFromOverride)
	}

	explanation := formatHostDirOrigin(applySourceReport{Slug: "primary", HostPath: got, HostPathOrigin: origin})
	if !strings.Contains(explanation, "--dir primary=/home/ada/src/web") || !strings.Contains(explanation, "not consulted") {
		t.Fatalf("override explained as %q", explanation)
	}
}

func TestFormatHostDirOriginNamesTheSandboxOrigin(t *testing.T) {
	got := formatHostDirOrigin(applySourceReport{HostPathOrigin: hostDirFromSandboxOrigin})
	if !strings.Contains(got, "created on this machine") {
		t.Fatalf("sandbox-origin explained as %q", got)
	}
}

// createdFromTree decides what a first apply into a repository with no commits
// is allowed to overwrite, so getting it wrong either refuses every apply or
// silently replaces work the user did after the discobox was created.
func TestCreatedFromTreeIsTheSnapshotTheDiscoboxCarried(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The empty base and snapshot as create writes them, with the branch left
	// unborn.
	emptyTree := git("hash-object", "-t", "tree", "-w", "--stdin")
	base := git("commit-tree", emptyTree, "-m", "discobox new empty base")
	snapshotTree, cleanup, err := gitunborn.WorkspaceTree(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	git("update-ref", "refs/discobox/run/snap", git("commit-tree", snapshotTree, "-p", base, "-m", "snapshot"))

	app := &App{}
	t.Run("a carried workspace is the snapshot's tree", func(t *testing.T) {
		source := apimodel.GitSource{Workspace: apiclientgen.NewOptGitSourceWorkspace(apimodel.GitSourceWorkspace{
			SnapshotRef: apiclientgen.NewOptString("refs/discobox/run/snap"),
		})}
		got, carried, err := app.createdFromTree(ctx, repo, "", "", "", source)
		if err != nil {
			t.Fatalf("createdFromTree: %v", err)
		}
		if got != snapshotTree || !carried {
			t.Fatalf("tree = %s (carried %v), want the snapshot's tree %s carried", got, carried, snapshotTree)
		}
	})

	// A discobox created from an empty repository, or one told not to carry the
	// working tree, was given nothing — so nothing is what has to still be here.
	t.Run("nothing carried is the empty tree", func(t *testing.T) {
		got, carried, err := app.createdFromTree(ctx, repo, "", "", "", apimodel.GitSource{})
		if err != nil {
			t.Fatalf("createdFromTree: %v", err)
		}
		if got != emptyTree || carried {
			t.Fatalf("tree = %s (carried %v), want the empty tree %s not carried", got, carried, emptyTree)
		}
		if got == snapshotTree {
			t.Fatal("a discobox that carried nothing must not be compared against a snapshot")
		}
	})
}

// A source with no local directory here — a remote clone, most often — is only
// a problem when the discobox has committed something to it. The commit it is
// measured against is the last apply's when there has been one.
func TestUnplacedSourceBasePrefersTheLastApply(t *testing.T) {
	sandbox, entry := sandboxWithOrigin(t, thisHost, "laptop", "")
	entry.source.Checkout = apiclientgen.NewOptGitSourceCheckout(apiclientgen.GitSourceCheckout{
		Commit: apiclientgen.NewOptString("1111111111111111111111111111111111111111"),
	})
	sandbox.Runtime.AppliedCommits = apiclientgen.NewOptNilAppliedSourceCommitArray([]apimodel.AppliedSourceCommit{
		{Slug: "other", Commit: "3333333333333333333333333333333333333333", AppliedAt: time.Now().Add(-time.Hour)},
		{Slug: entry.slug, Commit: "2222222222222222222222222222222222222222", AppliedAt: time.Now()},
	})

	base, origin := unplacedSourceBase(sandbox, entry)
	if base != "2222222222222222222222222222222222222222" || origin != baseOriginLastApplied {
		t.Fatalf("base = %q (%s), want the last applied commit", base, origin)
	}
}

// With no apply on record, the commit the source was created at is what the
// discobox has to have moved past for anything to be stranded.
func TestUnplacedSourceBaseFallsBackToTheCheckoutCommit(t *testing.T) {
	sandbox, entry := sandboxWithOrigin(t, thisHost, "laptop", "")
	entry.source.Checkout = apiclientgen.NewOptGitSourceCheckout(apiclientgen.GitSourceCheckout{
		Commit: apiclientgen.NewOptString("1111111111111111111111111111111111111111"),
	})

	base, origin := unplacedSourceBase(sandbox, entry)
	if base != "1111111111111111111111111111111111111111" || origin != baseOriginSourceCheckout {
		t.Fatalf("base = %q (%s), want the source's checkout commit", base, origin)
	}
}

// Nothing recorded means nothing to compare a tip against, so the source keeps
// the directory error it already has rather than guessing it is up to date.
func TestUnplacedSourceBaseIsEmptyWhenNothingRecordsWhereItStarted(t *testing.T) {
	sandbox, entry := sandboxWithOrigin(t, thisHost, "laptop", "")

	if base, origin := unplacedSourceBase(sandbox, entry); base != "" || origin != "" {
		t.Fatalf("base = %q (%s), want neither", base, origin)
	}
}

// The base a source with no local directory is measured against is explained in
// the report like every other one, since it is not a merge base and a reader
// should not have to assume it is.
func TestBaseOriginExplainsTheSourceCheckout(t *testing.T) {
	explained := formatBaseOrigin(baseOriginSourceCheckout)
	if explained == string(baseOriginSourceCheckout) || !strings.Contains(explained, "created at") {
		t.Fatalf("formatBaseOrigin(source-checkout) = %q, want it spelled out", explained)
	}
}

// A discobox created from an empty directory with no repository in it (ADR
// 0045) brings its work home by making that repository (ADR 0139): unborn, on
// the branch the discobox worked on, so the landing is the same first apply a
// `git init` and nothing since gets (ADR 0084), and the discobox's empty base
// never enters the history.
func TestFirstApplyIntoADirectoryWithNoRepositoryMakesOne(t *testing.T) {
	ctx := context.Background()
	// gitapply's cherry-pick runs git with this process's environment, so the
	// identity has to be here, not only on the test's own git commands.
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")
	gitIn := func(dir string) func(args ...string) string {
		return func(args ...string) string {
			t.Helper()
			cmd := exec.CommandContext(ctx, "git", args...)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
			}
			return strings.TrimSpace(string(out))
		}
	}

	// The discobox's side: the empty base create made, and one commit on it.
	sandboxRepo := t.TempDir()
	sandboxGit := gitIn(sandboxRepo)
	sandboxGit("init", "--initial-branch=trunk")
	emptyTree := sandboxGit("hash-object", "-t", "tree", "-w", "--stdin")
	base := sandboxGit("commit-tree", emptyTree, "-m", "discobox new empty base")
	sandboxGit("update-ref", "refs/heads/trunk", base)
	sandboxGit("reset", "--hard")
	if err := os.WriteFile(filepath.Join(sandboxRepo, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sandboxGit("add", "main.go")
	sandboxGit("commit", "-m", "start the project")
	tip := sandboxGit("rev-parse", "HEAD")

	local := t.TempDir()
	source := apimodel.GitSource{
		NoLocalRepository: apiclientgen.NewOptBool(true),
		Checkout: apiclientgen.NewOptGitSourceCheckout(apiclientgen.GitSourceCheckout{
			Commit:  apiclientgen.NewOptString(base),
			RefName: apiclientgen.NewOptString("trunk"),
			RefType: apiclientgen.NewOptString("branch"),
		}),
	}
	repoRoot, err := initApplyRepository(ctx, local, source)
	if err != nil {
		t.Fatalf("initApplyRepository: %v", err)
	}
	if !gitunborn.HeadIsUnborn(ctx, repoRoot) {
		t.Fatal("the repository made to apply into has a commit already; its first commit has to be the discobox's")
	}
	localGit := gitIn(repoRoot)
	if branch := localGit("symbolic-ref", "--short", "HEAD"); branch != "trunk" {
		t.Fatalf("HEAD names %q, want the branch the discobox was created on", branch)
	}

	localGit("fetch", sandboxRepo, "+refs/heads/trunk:refs/discobox/sandbox")
	wantTree, carried, err := (&App{}).createdFromTree(ctx, repoRoot, "", "", "", source)
	if err != nil || carried {
		t.Fatalf("createdFromTree = %s (carried %v, err %v), want the empty tree an empty directory was", wantTree, carried, err)
	}
	result, err := gitapply.AttemptRoot(ctx, repoRoot, base, tip, wantTree)
	if err != nil || !result.Landed {
		t.Fatalf("AttemptRoot = %+v, %v; want it landed", result, err)
	}
	if roots := localGit("rev-list", "--max-parents=0", "HEAD"); roots != localGit("rev-parse", "HEAD") {
		t.Fatalf("root commits %q, want the discobox's first commit as the only one", roots)
	}
	if got, err := os.ReadFile(filepath.Join(local, "main.go")); err != nil || string(got) != "package main\n" {
		t.Fatalf("main.go = %q, %v; want the discobox's file checked out", got, err)
	}
	if kept, err := removeApplyRepositoryUnlessBorn(ctx, repoRoot); err != nil || !kept {
		t.Fatalf("removeApplyRepositoryUnlessBorn after landing = %v, %v; want the repository kept", kept, err)
	}
	if _, err := os.Stat(filepath.Join(local, ".git")); err != nil {
		t.Fatalf("the repository holding the landed commits is gone: %v", err)
	}
}

// Nothing landed, the repository apply made goes again and the directory is
// as it was found. But a branch that holds commits keeps it, even with the
// checkout never done — an apply interrupted between AttemptRoot's update-ref
// and its reset has still put the commits there.
func TestTheRepositoryAnApplyMadeIsRemovedUnlessItsBranchHoldsCommits(t *testing.T) {
	ctx := context.Background()
	source := apimodel.GitSource{NoLocalRepository: apiclientgen.NewOptBool(true)}

	refused := t.TempDir()
	root, err := initApplyRepository(ctx, refused, source)
	if err != nil {
		t.Fatalf("initApplyRepository: %v", err)
	}
	if kept, err := removeApplyRepositoryUnlessBorn(ctx, root); err != nil || kept {
		t.Fatalf("removeApplyRepositoryUnlessBorn on an unborn repository = %v, %v; want it removed", kept, err)
	}
	if _, err := os.Lstat(filepath.Join(refused, ".git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the .git apply made is still there: %v", err)
	}

	interrupted := t.TempDir()
	root, err = initApplyRepository(ctx, interrupted, source)
	if err != nil {
		t.Fatalf("initApplyRepository: %v", err)
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	emptyTree := git("hash-object", "-t", "tree", "-w", "--stdin")
	commit := git("commit-tree", emptyTree, "-m", "landed")
	git("update-ref", "refs/heads/"+git("symbolic-ref", "--short", "HEAD"), commit)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if kept, err := removeApplyRepositoryUnlessBorn(canceled, root); err != nil || !kept {
		t.Fatalf("removeApplyRepositoryUnlessBorn with commits on the branch = %v, %v; want it kept", kept, err)
	}
}

// Git calls a directory whose .git it cannot read "not a git repository", and
// `git init` would reuse that .git. Apply must not make a repository there,
// since it would then remove the user's history along with it.
func TestApplyMakesNoRepositoryOverAnExistingGitDirectory(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	objects := filepath.Join(dir, ".git", "objects")
	if err := os.MkdirAll(objects, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := initApplyRepository(ctx, dir, apimodel.GitSource{NoLocalRepository: apiclientgen.NewOptBool(true)}); err == nil {
		t.Fatal("initApplyRepository made a repository over an existing .git")
	}
	if _, err := os.Stat(objects); err != nil {
		t.Fatalf("the existing .git was touched: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "HEAD")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("git init ran over the existing .git: %v", err)
	}
}

// Refused, the directory has no repository to commit anything in, so the way
// out makes one first — and the look around does not ask git about a
// repository that is not there.
func TestLocalChangesNextStepsMakeTheRepositoryAnApplyTookAway(t *testing.T) {
	steps := localChangesNextSteps("sbx_1", "primary", "/work/new", "", true, true)
	if got := steps[0].Commands[0]; got != "git -C /work/new init" {
		t.Fatalf("first command = %q, want the repository made before anything is committed", got)
	}
	if got := steps[1].Commands[0]; strings.HasPrefix(got, "git ") {
		t.Fatalf("look-around command = %q, want one that works with no repository", got)
	}
	if got := blockedLocalChanges("/work/new", true, true); !strings.Contains(got, "not a Git repository") {
		t.Fatalf("refusal = %q, want it to say there is no repository", got)
	}
}
