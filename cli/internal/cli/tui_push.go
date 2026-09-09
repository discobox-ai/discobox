package cli

import (
	"context"
	"errors"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/sandboxpush"
	"github.com/discobox-ai/discobox/cli/internal/tui"
	"github.com/discobox-ai/discobox/internal/hostid"
	"github.com/discobox-ai/x/gitutil"
)

// The launcher's automatic push (ADR 0095): the window's end of `discobox
// push`, run for it rather than by it while a workspace is open on a discobox.
//
// Everything about what is sent, and what is refused, is sandboxpush's — the
// same code the command runs, with no options, so the two cannot drift. What is
// here is only which sources this machine may push and where they live.

// pushable reports that new commits made here are this window's to send into
// the discobox's origin: it has a source delivered by pushing it, this machine
// is the one it was pushed from, and it is in a state to take another push
// (ADR 0095 §2).
//
// A machine with no resolvable identity pushes nothing, which is the same
// answer it gives for a discobox created somewhere else: without an identity
// there is no way to claim this one.
//
// Awaiting its source is refused rather than merely unsupported. A push to a
// parked discobox is its create's delivery, which starts it (deliverAwaitedSource)
// — a state-machine transition and a decision about a create that failed, so it
// stays something asked for by name.
func pushable(sb apimodel.Sandbox, hostID string) bool {
	if hostID == "" {
		return false
	}
	origin, ok := sb.Origin.Get()
	if !ok || origin.HostId != hostID {
		return false
	}
	if sandboxAwaitingSource(sb) {
		return false
	}
	switch sandboxDisplayState(sb) {
	case "archived", "archiving":
		// No container for the git route to start, so the push would fail in
		// the proxy with nothing useful to say.
		return false
	}
	for _, entry := range applySources(&sb) {
		if sandboxpush.CheckPushDelivered(entry.source) == nil {
			return true
		}
	}
	return false
}

// pushTargets is what an automatic push for one discobox resolves to on this
// machine: its push-delivered sources, and the repository root each one is
// pushed from.
//
// None of it can change while the discobox exists — a source's delivery, slug
// and local directory are all fixed at create — which is what makes it worth
// holding rather than asking for on every beat.
type pushTargets struct {
	sources []applySourceEntry
	// roots is the local repository each source pushes from, keyed by slug.
	roots map[string]string
}

// PushSources sends this machine's new commits into the origin repositories the
// discobox's push-delivered sources fetch from.
//
// The order is deliberate: every source is resolved first, and only if
// something actually moved is the git route opened. Resolving is two ref reads
// in a repository this machine already has, so the ordinary beat — nothing
// committed since the last one — dials nothing at all (ADR 0095 §4).
func (d *apiDataSource) PushSources(ctx context.Context, sandboxID string, held map[string]string) ([]tui.SourcePush, error) {
	targets, err := d.pushTargets(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	pushes := make([]tui.SourcePush, 0, len(targets.sources))
	var pending []applySourceEntry
	for _, entry := range targets.sources {
		resolved, err := sandboxpush.Resolve(ctx, targets.roots[entry.slug], sandboxID, entry.source, sandboxpush.Options{})
		push := tui.SourcePush{Slug: entry.slug, Branch: resolved.Branch, Commit: resolved.Commit}
		// A source held at exactly what it names now is one whose push already
		// failed and was already reported. It is resolved — that is how the
		// hold is released when the branch moves — and nothing else.
		if commit, ok := held[entry.slug]; ok && commit == resolved.Commit {
			pushes = append(pushes, push)
			continue
		}
		if err != nil {
			push.Err = err
			pushes = append(pushes, push)
			continue
		}
		if resolved.UpToDate() {
			pushes = append(pushes, push)
			continue
		}
		pending = append(pending, entry)
	}
	if len(pending) == 0 {
		return pushes, nil
	}

	gitServerURL, releaseGitServerURL, err := d.app.gitServerURL(ctx)
	if err != nil {
		return pushes, err
	}
	defer releaseGitServerURL()
	for _, entry := range pending {
		result, err := sandboxpush.Push(ctx, targets.roots[entry.slug], gitServerURL, d.projectID, sandboxID, d.app.token, entry.source, sandboxpush.Options{})
		pushes = append(pushes, tui.SourcePush{
			Slug:   entry.slug,
			Branch: result.Branch,
			Commit: result.Commit,
			Pushed: err == nil && !result.UpToDate,
			Err:    err,
		})
	}
	return pushes, nil
}

// pushTargets resolves the discobox's sources once and holds the answer for the
// life of the window.
//
// A source this machine cannot push is left out rather than reported: the
// window asks this of a discobox whose row says it is pushable, and a second
// source of its own that is bound or cloned is not a failure of anything. The
// empty answer is held too, since what makes it empty does not change either.
func (d *apiDataSource) pushTargets(ctx context.Context, sandboxID string) (*pushTargets, error) {
	d.pushMu.Lock()
	cached, ok := d.pushCache[sandboxID]
	d.pushMu.Unlock()
	if ok {
		return cached, nil
	}

	res, err := d.client.GetSandbox(ctx, apiclientgen.GetSandboxParams{ProjectId: d.projectID, SandboxId: sandboxID})
	if err != nil {
		return nil, err
	}
	sandbox, err := expectResponse[apimodel.Sandbox](res)
	if err != nil {
		return nil, err
	}
	hostID, err := hostid.Get()
	if err != nil {
		return nil, err
	}
	targets := &pushTargets{roots: map[string]string{}}
	for _, entry := range applySources(sandbox) {
		if err := sandboxpush.CheckPushDelivered(entry.source); err != nil {
			continue
		}
		hostDir, _, err := resolveApplyHostDir(sandbox, hostID, entry, nil)
		if err != nil {
			// Another machine's discobox, or a directory that is no longer
			// there. Both are answered by `discobox push`, which can say so
			// and take a --dir; a window pushing on its own cannot.
			continue
		}
		repoRoot, err := gitutil.Root(ctx, hostDir)
		if err != nil {
			if !errors.Is(err, gitutil.ErrNotARepository) {
				return nil, err
			}
			// The source was delivered from a repository built over the
			// directory for one run and thrown away with it (ADR 0045), so
			// there is no history here to push and each run's would be
			// unrelated anyway.
			continue
		}
		targets.sources = append(targets.sources, entry)
		targets.roots[entry.slug] = repoRoot
	}

	d.pushMu.Lock()
	if d.pushCache == nil {
		d.pushCache = map[string]*pushTargets{}
	}
	d.pushCache[sandboxID] = targets
	d.pushMu.Unlock()
	return targets, nil
}
