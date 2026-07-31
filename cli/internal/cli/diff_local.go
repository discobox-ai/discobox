package cli

import (
	"context"
	"fmt"
	"strings"

	apimodel "github.com/obot-platform/discobox/api/model"
	"github.com/obot-platform/discobox/cli/internal/gitapply"
	"github.com/obot-platform/discobox/cli/internal/sandboxapply"
	"github.com/obot-platform/discobox/internal/gitutil"
	"github.com/obot-platform/discobox/internal/hostid"
)

// diffBaseLocalKeyword is the value --base takes to mean this machine's working
// tree, uncommitted changes and all.
const diffBaseLocalKeyword = "local"

// sandboxDiffRef is where a sandbox's working state is parked as a commit so it
// can be fetched, mirroring apply's refs/discobox/apply/ convention.
func sandboxDiffRef(sandboxID, slug string) string {
	return "refs/discobox/diff/" + sandboxID + "/" + slug
}

// localDiff is a comparison whose two sides start out on different machines.
//
// Two working trees cannot be diffed where they sit, and only committed objects
// can travel, so the sandbox's working state is written as a tree, wrapped in a
// commit under a discobox ref, and fetched through the same git proxy apply
// uses. Both sides are then present here and the comparison is ordinary.
//
// Neither index is touched on either end: both sides build into a scratch index
// and leave the repository's own alone.
//
// These are the modes that need a local repository, which is exactly the
// precondition ADR 0018 keeps out of the default. They are opt-in for that
// reason, and say so when the local directory cannot be found.
type localDiff struct {
	repoRoot string
	// commit is the sandbox's working state, on this machine.
	commit string
}

func (a *App) fetchSandboxWorkingState(ctx context.Context, projectID, sandboxID string, sandbox *apimodel.Sandbox, entry applySourceEntry, dirOverrides map[string]string, flagName string) (localDiff, error) {
	host, err := hostid.Get()
	if err != nil {
		return localDiff{}, err
	}
	hostDir, _, err := resolveApplyHostDir(sandbox, host, entry, dirOverrides)
	if err != nil {
		return localDiff{}, fmt.Errorf("%s compares against a repository on this machine: %w", flagName, err)
	}
	repoRoot, err := gitutil.Root(ctx, hostDir)
	if err != nil {
		return localDiff{}, fmt.Errorf("%s is not a Git repository: %w", hostDir, err)
	}

	gitServerURL, release, err := a.gitServerURL(ctx)
	if err != nil {
		return localDiff{}, err
	}
	defer release()

	ref := sandboxDiffRef(sandboxID, entry.slug)
	stdout, stderr, code, err := a.sandboxCommandOutput(ctx, projectID, sandboxID, sourceWorkdir(entry.source),
		sandboxWorkingCommitCommand(ref))
	if err != nil {
		return localDiff{}, err
	}
	if code != 0 {
		return localDiff{}, fmt.Errorf("record the sandbox's working state: %s", strings.TrimSpace(stderr+stdout))
	}
	commit := strings.TrimSpace(stdout)
	if commit == "" {
		return localDiff{}, fmt.Errorf("record the sandbox's working state: no commit was reported")
	}

	// The commit's parent is the sandbox's HEAD, so the fetch carries only what
	// this machine is missing rather than a whole tree's worth of objects.
	if err := sandboxapply.Fetch(ctx, repoRoot, gitServerURL, projectID, sandboxID, a.token, entry.source, "+"+ref+":"+ref); err != nil {
		return localDiff{}, err
	}
	return localDiff{repoRoot: repoRoot, commit: commit}, nil
}

// localWorkingTree is this machine's working state as a tree object, built the
// same way the sandbox's is and leaving the local index equally untouched.
func localWorkingTree(ctx context.Context, repoRoot string) (string, func(), error) {
	workspace, cleanup, err := gitutil.CurrentWorkspaceTree(ctx, repoRoot)
	if err != nil {
		return "", func() {}, fmt.Errorf("record this machine's working state: %w", err)
	}
	return workspace.Tree, cleanup, nil
}

// applyPreviewBase is the commit `disco apply` would take its range from, and
// therefore the only left-hand side that answers "what would applying this
// land here?".
//
// It mirrors apply exactly (ADR 0014 §2): the last recorded apply for this
// source when there is one, since apply would not re-send what already landed,
// and otherwise the merge base with local HEAD. Both are resolved here, in the
// local repository, because that is where apply resolves them and where the
// commits would go — the opposite of the default base, which is the sandbox's
// business alone.
func applyPreviewBase(ctx context.Context, repoRoot string, sandbox *apimodel.Sandbox, slug, tip string) (diffBase, error) {
	if applied := lastAppliedCommit(sandbox, slug); applied != "" {
		return diffBase{Commit: applied, Origin: diffBaseLastApplied}, nil
	}
	base, err := gitapply.MergeBase(ctx, repoRoot, tip)
	if err != nil {
		return diffBase{}, err
	}
	return diffBase{Commit: base, Origin: diffBaseMergeBase}, nil
}

// diffTrees compares two commit-ish objects that are both present locally.
func diffTrees(ctx context.Context, repoRoot, left, right string, gitArgs, pathspecs []string) (string, error) {
	args := append([]string{"--no-pager", "diff"}, gitArgs...)
	args = append(args, left, right, "--")
	args = append(args, pathspecs...)
	return gitutil.Output(ctx, repoRoot, nil, nil, args...)
}

// sandboxWorkingCommitCommand builds the sandbox's working state into a commit
// parked at ref, and prints the commit.
//
// A commit rather than a bare tree because only refs are advertised over the
// git protocol, and only what a ref reaches can be fetched. HEAD is its parent
// so the two sides share history and the fetch stays incremental. The identity
// is set inline: a sandbox need not have user.email configured, and
// commit-tree refuses without one.
func sandboxWorkingCommitCommand(ref string) []string {
	script := sandboxWorkingTreeScript + `
parent=$(git rev-parse --verify --quiet HEAD 2>/dev/null)
if [ -n "$parent" ]; then
  set -- -p "$parent"
else
  set --
fi
commit=$(GIT_AUTHOR_NAME=discobox GIT_AUTHOR_EMAIL=discobox@localhost \
  GIT_COMMITTER_NAME=discobox GIT_COMMITTER_EMAIL=discobox@localhost \
  git commit-tree "$tree" "$@" -m 'disco diff: sandbox working state') || exit $?
git update-ref ` + shellQuote(ref) + ` "$commit" || exit $?
printf '%s\n' "$commit"
`
	return []string{"sh", "-c", script}
}

// localSideDiff produces the patch for the two modes that compare against
// something on this machine, and reports the base it used.
//
// Both fetch the sandbox's working state; they differ only in the left-hand
// side, which is the whole point of keeping them here rather than in apply.
// Every other diff flag — the git ones, the pathspecs, the rendered view — is
// unaffected by that choice and keeps working.
func (a *App) localSideDiff(ctx context.Context, projectID, sandboxID string, sandbox *apimodel.Sandbox, entry applySourceEntry, dirOverrides map[string]string, opts diffOptions, gitArgs, pathspecs []string) (string, diffBase, error) {
	flagName := "--base local"
	if opts.applyPreview {
		flagName = "--apply-preview"
	}
	local, err := a.fetchSandboxWorkingState(ctx, projectID, sandboxID, sandbox, entry, dirOverrides, flagName)
	if err != nil {
		return "", diffBase{}, err
	}

	var base diffBase
	if opts.applyPreview {
		if base, err = applyPreviewBase(ctx, local.repoRoot, sandbox, entry.slug, local.commit); err != nil {
			return "", diffBase{}, err
		}
	} else {
		tree, cleanup, err := localWorkingTree(ctx, local.repoRoot)
		if err != nil {
			return "", diffBase{}, err
		}
		defer cleanup()
		base = diffBase{Commit: tree, Origin: diffBaseLocalTree}
	}

	patch, err := diffTrees(ctx, local.repoRoot, base.Commit, local.commit, gitArgs, pathspecs)
	if err != nil {
		return "", diffBase{}, err
	}
	return patch, base, nil
}
