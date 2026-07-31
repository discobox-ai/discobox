package cli

import (
	"context"
	"fmt"
	"strings"

	apimodel "github.com/obot-platform/discobox/api/model"
)

// diffBase is the commit a source's diff is measured from, and why it is that
// commit. The why is reported alongside every diff: a diff is only meaningful
// relative to its base, and the base here is chosen rather than given.
type diffBase struct {
	Commit string
	Origin string
}

// Base origins, in the order diffBaseCommand prefers them.
const (
	diffBaseOverride    = "override"
	diffBaseMergeBase   = "merge-base"
	diffBaseCheckout    = "checkout"
	diffBaseLastApplied = "last-applied"
	diffBaseLocalTree   = "local-tree"
)

// diffBaseSnapshotKeyword is the value --base takes to mean the workspace
// snapshot this sandbox was handed at create, whose ref name only the sandbox
// record knows.
const diffBaseSnapshotKeyword = "snapshot"

// describe says where the base came from, in the terms the user would use.
func (b diffBase) describe(upstreamRef string) string {
	switch b.Origin {
	case diffBaseOverride:
		return "--base"
	case diffBaseMergeBase:
		if upstreamRef == "" {
			return "the merge base with this machine's HEAD, which is where apply would start"
		}
		return "merge base with " + shortUpstreamRef(upstreamRef)
	case diffBaseLastApplied:
		return "the last apply recorded for this source, which is where apply would resume"
	case diffBaseLocalTree:
		return "your local working tree"
	default:
		return "the commit the sandbox cloned"
	}
}

func shortUpstreamRef(ref string) string {
	return strings.TrimPrefix(ref, "refs/remotes/")
}

// resolveDiffBase asks the sandbox which commit this source's work began at.
//
// It is resolved inside the sandbox, from the sandbox's own refs, and never
// from a repository on this machine. `disco apply` does the opposite, and has
// to: it is about to cherry-pick onto a local branch, so its base is only
// meaningful relative to local HEAD, which is why it demands --dir when this
// host has no copy. A diff only describes the sandbox. Resolving it there keeps
// `disco diff` working with no local repository at all — for a source cloned
// from a remote URL, and for a sandbox created on someone else's machine — and
// keeps the answer the same wherever the command is run from.
func (a *App) resolveDiffBase(ctx context.Context, projectID, sandboxID, workdir string, source apimodel.GitSource, override string) (diffBase, string, error) {
	upstreamRef := sourceUpstreamRef(source)
	if override = strings.TrimSpace(override); override != "" {
		// "snapshot" names the one base worth a keyword: the ref is on the
		// sandbox record, not something a user could be expected to type.
		if override == diffBaseSnapshotKeyword {
			ref := sourceSnapshotRef(source)
			if ref == "" {
				return diffBase{}, upstreamRef, fmt.Errorf("this sandbox was not created from a dirty workspace, so it has no snapshot to diff against")
			}
			override = ref
		}
		return diffBase{Commit: override, Origin: diffBaseOverride}, upstreamRef, nil
	}
	checkout := sourceCheckoutCommit(source)
	if checkout == "" {
		return diffBase{}, upstreamRef, fmt.Errorf("the sandbox did not record the commit it cloned, so there is nothing to diff against")
	}
	stdout, stderr, code, err := a.sandboxCommandOutput(ctx, projectID, sandboxID, workdir,
		diffBaseCommand(checkout, upstreamRef))
	if err != nil {
		return diffBase{}, upstreamRef, err
	}
	if code != 0 {
		return diffBase{}, upstreamRef, fmt.Errorf("resolve the commit to diff against: %s", strings.TrimSpace(stderr+stdout))
	}
	commit, origin, ok := strings.Cut(strings.TrimSpace(stdout), "\t")
	if !ok || commit == "" {
		return diffBase{}, upstreamRef, fmt.Errorf("resolve the commit to diff against: unexpected output %q", strings.TrimSpace(stdout))
	}
	return diffBase{Commit: commit, Origin: origin}, upstreamRef, nil
}

// sourceSnapshotRef is the ref holding the uncommitted work the sandbox was
// handed at create, for a source created from a dirty local workspace. Empty
// when the workspace was clean, which is when there is nothing to carry.
func sourceSnapshotRef(source apimodel.GitSource) string {
	workspace, ok := source.Workspace.Get()
	if !ok {
		return ""
	}
	if mode, ok := workspace.Mode.Get(); !ok || string(mode) != "dirty" {
		return ""
	}
	return strings.TrimSpace(workspace.SnapshotRef.Or(""))
}

// sourceUpstreamRef is the remote-tracking ref the sandbox would have fetched
// upstream into, derived from the branch the source was cloned at.
//
// Only a clone-delivered source has one: a push-delivered sandbox's repository
// is `git init` with no remote added at all, so nothing there ever tracks
// upstream and no merge base exists to find. The ref is verified in the sandbox
// rather than assumed, so naming one that does not exist costs nothing.
func sourceUpstreamRef(source apimodel.GitSource) string {
	checkout, ok := source.Checkout.Get()
	if !ok {
		return "refs/remotes/origin/HEAD"
	}
	refName := strings.TrimSpace(checkout.RefName.Or(""))
	if refName == "" || strings.TrimSpace(checkout.RefType.Or("")) != "branch" {
		return "refs/remotes/origin/HEAD"
	}
	return "refs/remotes/origin/" + refName
}

// diffBaseCommand builds the command that picks the base inside the sandbox and
// prints it as "COMMIT<TAB>ORIGIN".
//
// The default is the commit the source was cloned at, so `disco diff` means the
// same thing as `git diff <that commit>` run in the sandbox: everything in the
// sandbox that is not in the committed baseline, whoever put it there. Work
// carried in from a dirty local workspace is part of that — it is in the
// sandbox and not in the base — and hiding it makes the command report nothing
// for a sandbox that visibly holds work.
//
// The merge base with upstream displaces it only when the sandbox actually
// fetched: once it does, the tracking ref moves, and commits the sandbox pulled
// rather than wrote stop counting as its changes. When nothing was pulled the
// merge base is the cloned commit anyway, so this costs nothing in the common
// case and never quietly narrows the diff.
func diffBaseCommand(checkout, upstreamRef string) []string {
	script := `
resolve() {
  git rev-parse --verify --quiet "$1^{commit}" 2>/dev/null
}

checkout=` + shellQuote(checkout) + `
upstream=` + shellQuote(upstreamRef) + `

base=$(resolve "$checkout")
origin=` + diffBaseCheckout + `

head=$(resolve HEAD)
if [ -n "$upstream" ] && [ -n "$head" ]; then
  tip=$(resolve "$upstream")
  if [ -n "$tip" ]; then
    merged=$(git merge-base "$head" "$tip" 2>/dev/null)
    # Only ever forward. An upstream branch that was rewritten leaves a merge
    # base *older* than the cloned commit, and taking it would widen the diff
    # with commits the sandbox never wrote.
    if [ -n "$merged" ] && [ "$merged" != "$base" ] &&
       git merge-base --is-ancestor "$base" "$merged" 2>/dev/null; then
      base=$merged
      origin=` + diffBaseMergeBase + `
    fi
  fi
fi

if [ -z "$base" ]; then
  echo "no commit to diff against: the sandbox does not have the commit it was cloned at" >&2
  exit 1
fi
printf '%s\t%s\n' "$base" "$origin"
`
	return []string{"sh", "-c", script}
}
