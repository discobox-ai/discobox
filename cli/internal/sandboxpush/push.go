// Package sandboxpush implements ADR 0058's `discobox push`: sending a local
// branch's commits into the origin repository a push-delivered source's sandbox
// fetches from, so the sandbox can rebase onto work done since it was created.
//
// It is transport only. No phase, no completion call, nothing about the
// sandbox's state changes: the sandbox rebases when whoever is working in it
// decides to.
package sandboxpush

import (
	"context"
	"errors"
	"fmt"
	"strings"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/sandboxgit"
	"github.com/discobox-ai/x/gitutil"
)

// ErrNotPushDelivered reports a source whose origin is not a pushed repository,
// so there is nothing here to push into. The two ways that happens read
// differently to a user, so the message says which.
var ErrNotPushDelivered = errors.New("source origin is not delivered by push")

// Result is what one source's push did, or would have done. Every field is set
// whether or not the push ran, so a no-op reports as much context as a push.
type Result struct {
	Slug string
	// Branch is the branch in the origin repository the commits land on, which
	// the sandbox sees as origin/<Branch>.
	Branch string
	// LocalRev is the local revision that was resolved, as named.
	LocalRev string
	// Commit is the tip that was pushed, full SHA.
	Commit string
	// Lease is the commit this client last pushed, full SHA, empty when it has
	// never pushed to this source.
	Lease string
	// UpToDate reports that Commit is already what the lease records, so nothing
	// was sent.
	UpToDate bool
	// Forced reports that the push overrode the lease, or ran without one.
	Forced bool
	// DirtyFiles counts uncommitted changes in the local working tree. They are
	// never pushed: this is what the caller warns about.
	DirtyFiles int
}

// Pending is what a push would send for one source, worked out without sending
// anything: the revision to push and the branch it lands on, what that revision
// names right now, and what this client last put in the origin.
type Pending struct {
	Slug string
	// Branch is the branch in the origin repository the commits would land on.
	Branch string
	// LocalRev is the local revision to push, as named.
	LocalRev string
	// Commit is what LocalRev resolves to now, full SHA.
	Commit string
	// Lease is the commit this client last pushed, full SHA, and LeaseRef is
	// where that is recorded. HasLease is false when this client has never
	// pushed to this source, which is not the same as having pushed nothing:
	// there is then no lease to hold and git's own fast-forward rule stands.
	Lease    string
	LeaseRef string
	HasLease bool
}

// UpToDate reports that the origin already holds this commit, by this client's
// own record of what it put there — so there is nothing to send.
func (p Pending) UpToDate() bool { return p.HasLease && p.Lease == p.Commit }

// Resolve answers "is there anything here to push", locally.
//
// It is two ref reads in a repository this machine already has open: the branch
// tip and the lease. Nothing is dialed and nothing is transferred, which is
// what makes it the whole cost of an automatic push on the ordinary tick where
// nothing has been committed since the last one (ADR 0095 §4 on automatic
// push).
//
// It is also the head of the push itself, so what counts as "new commits" is
// decided in one place rather than once for the asking and once for the doing.
func Resolve(ctx context.Context, repoRoot, sandboxID string, source apimodel.GitSource, opts Options) (Pending, error) {
	slug := strings.TrimSpace(source.Slug.Or(""))
	if slug == "" {
		return Pending{}, fmt.Errorf("source has no slug to address its origin repository")
	}
	pending := Pending{Slug: slug}
	if err := CheckPushDelivered(source); err != nil {
		return pending, err
	}
	pending.LocalRev, pending.Branch = pushRefs(source, opts.Branch)
	commit, err := gitutil.ResolveCommit(ctx, repoRoot, pending.LocalRev)
	if err != nil {
		return pending, err
	}
	pending.Commit = commit
	pending.LeaseRef = sandboxgit.OriginLeaseRef(sandboxID, slug, pending.Branch)
	pending.Lease, pending.HasLease = resolveRef(ctx, repoRoot, pending.LeaseRef)
	return pending, nil
}

// Options names what to push and how hard to insist.
type Options struct {
	// Branch is the local branch to push, landing in the origin repository under
	// the same name. Empty takes the branch the source names.
	Branch string
	// Force pushes regardless of the lease, and regardless of whether the local
	// history is related to what the sandbox holds.
	Force bool
}

// Push sends one source's commits into its origin repository.
//
// The push is a lease, not a force and not a plain fast-forward (ADR 0058 §6):
// a local rebase or amend is the normal way a client's branch moves, so
// non-fast-forward has to be possible, while a stale second machine silently
// rewinding a sandbox's origin does not. The lease is this client's record of
// what it last pushed; with no record there is nothing to lease against, so the
// push is left to git's own fast-forward rule and only --force can rewind.
func Push(ctx context.Context, repoRoot, serverURL, projectID, sandboxID, token string, source apimodel.GitSource, opts Options) (Result, error) {
	originURL, err := sandboxgit.OriginURL(serverURL, projectID, sandboxID, source)
	if err != nil {
		return Result{Slug: source.Slug.Or("")}, err
	}
	return pushTo(ctx, repoRoot, originURL, token, sandboxID, source, opts)
}

// pushTo is the push itself, against an already-resolved remote: the control
// plane's proxy to the origin repository in normal use, and the repository's own
// path where one is reachable directly. Everything that decides what to send, and
// what to refuse, lives here; Push only resolves where to send it.
func pushTo(ctx context.Context, repoRoot, originURL, token, sandboxID string, source apimodel.GitSource, opts Options) (Result, error) {
	pending, err := Resolve(ctx, repoRoot, sandboxID, source, opts)
	// Reported whether or not the resolve got that far: a source with no slug
	// fills in nothing, and every field it did answer is context for the error.
	result := Result{
		Slug:     pending.Slug,
		Branch:   pending.Branch,
		LocalRev: pending.LocalRev,
		Commit:   pending.Commit,
		Lease:    pending.Lease,
	}
	if err != nil {
		return result, err
	}
	if pending.UpToDate() {
		result.UpToDate = true
		return result, nil
	}
	commit, branch, lease, hasLease := pending.Commit, pending.Branch, pending.Lease, pending.HasLease
	if !opts.Force {
		if err := checkRelatedHistory(ctx, repoRoot, source, commit); err != nil {
			return result, err
		}
	}
	if changes, err := gitutil.StatusChanges(ctx, repoRoot); err == nil {
		result.DirtyFiles = len(changes)
	}

	args := []string{"push"}
	switch {
	case opts.Force:
		args = append(args, "--force")
		result.Forced = true
	case hasLease:
		args = append(args, "--force-with-lease=refs/heads/"+branch+":"+lease)
		// Anything else has no lease to hold — this client has never pushed here
		// — so git's own fast-forward rule stands. It is the right floor: a
		// fast-forward cannot lose a commit, and a rewind needs --force.
	}
	args = append(args, originURL, commit+":refs/heads/"+branch)
	if _, err := gitutil.Output(ctx, repoRoot, nil, sandboxgit.NoPromptEnv, sandboxgit.PushArgs(token, args)...); err != nil {
		return result, pushError(err, hasLease, opts.Force)
	}
	if err := gitutil.UpdateRef(ctx, repoRoot, pending.LeaseRef, commit); err != nil {
		return result, err
	}
	return result, nil
}

// CheckPushDelivered rejects a source whose origin the sandbox already reaches
// on its own. Neither case is a failure of the user's: the answer is that there
// is nothing to push, and why. It is exported because a caller pushing every
// source needs to tell "nothing to send here" from a real failure before it
// starts resolving directories.
func CheckPushDelivered(source apimodel.GitSource) error {
	if delivery, ok := source.Delivery.Get(); ok && delivery == apiclientgen.GitSourceDeliveryPush {
		return nil
	}
	if strings.TrimSpace(source.LocalDirectory.Or("")) == "" {
		return fmt.Errorf("%w: the discobox clones it from a remote, which it can fetch from directly", ErrNotPushDelivered)
	}
	return fmt.Errorf("%w: the discobox reads your directory live, so `git fetch origin` inside it already sees new commits", ErrNotPushDelivered)
}

// pushRefs is the local revision to push and the origin branch it lands on.
//
// A source checked out at a branch pushes that branch, by name, to the same name
// — the ref the sandbox tracks as origin/<branch>. A source checked out at a bare
// commit or tag names no branch, so its origin's HEAD is the conventional push
// branch and the local side is whatever HEAD is now.
func pushRefs(source apimodel.GitSource, override string) (localRev, branch string) {
	if override = strings.TrimSpace(override); override != "" {
		return override, override
	}
	if checkout, ok := source.Checkout.Get(); ok {
		if strings.TrimSpace(checkout.RefType.Or("")) == "branch" {
			if name := strings.TrimSpace(checkout.RefName.Or("")); name != "" {
				return name, name
			}
		}
	}
	return "HEAD", sandboxgit.SourcePushBranch
}

// checkRelatedHistory refuses to push a history that has nothing in common with
// what the sandbox holds, because nothing in the sandbox could rebase onto it.
//
// The test is a shared merge base, deliberately not "the sandbox's base commit is
// an ancestor of what I am pushing": a local rebase rewrites that commit away
// routinely, and refusing then would refuse the ordinary case. A base commit this
// repository no longer has is skipped rather than failed — unknown is not the
// same as unrelated (ADR 0058 §6).
func checkRelatedHistory(ctx context.Context, repoRoot string, source apimodel.GitSource, commit string) error {
	checkout, ok := source.Checkout.Get()
	if !ok {
		return nil
	}
	base := strings.TrimSpace(checkout.Commit.Or(""))
	if base == "" {
		return nil
	}
	if _, err := gitutil.Output(ctx, repoRoot, nil, nil, "rev-parse", "--verify", "--quiet", base+"^{commit}"); err != nil {
		return nil //nolint:nilerr // A base commit this repository does not have says nothing about relatedness.
	}
	if _, err := gitutil.Output(ctx, repoRoot, nil, nil, "merge-base", base, commit); err != nil {
		return fmt.Errorf("%s shares no history with the commit the discobox was created from (%s), so nothing in the discobox could rebase onto it; pass --force to push it anyway",
			commit[:min(len(commit), 12)], base[:min(len(base), 12)])
	}
	return nil
}

// pushError explains a rejected push in terms of what the caller can do about
// it, since git's own wording describes a workflow this one does not have —
// there is no remote configured to pull from, and the sandbox's origin is not a
// branch anyone else is committing to.
func pushError(err error, hasLease, forced bool) error {
	message := err.Error()
	switch {
	case forced:
		return fmt.Errorf("push to the discobox's origin: %w", err)
	case hasLease && strings.Contains(message, "stale info"):
		return fmt.Errorf("the discobox's origin has moved since your last push — another machine pushed to it, so rewinding it now would lose those commits; pass --force to push anyway: %w", err)
	case strings.Contains(message, "non-fast-forward") || strings.Contains(message, "fetch first"):
		return fmt.Errorf("the discobox's origin holds commits this push would drop, and this machine has no record of putting them there; pass --force to push anyway: %w", err)
	default:
		return fmt.Errorf("push to the discobox's origin: %w", err)
	}
}

// resolveRef reads a ref that is allowed not to exist.
func resolveRef(ctx context.Context, repoRoot, ref string) (string, bool) {
	out, err := gitutil.Output(ctx, repoRoot, nil, nil, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return "", false
	}
	commit := strings.TrimSpace(out)
	return commit, commit != ""
}
