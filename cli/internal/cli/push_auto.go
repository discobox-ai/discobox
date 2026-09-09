package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/sandboxpush"
	"github.com/discobox-ai/discobox/internal/hostid"
	"github.com/discobox-ai/x/gitutil"
)

// The automatic push (ADR 0095): `discobox push`, run for an attached client
// rather than by a person, for as long as a terminal attach lasts.
//
// Attaching is the trigger, so this serves both front ends — the launcher's
// workspace through the DataSource seam in tui_push.go, and a raw attach
// through autoPushWhileAttached below.
//
// Everything about what is sent, and what is refused, is sandboxpush's — the
// same code the command runs, with no options, so the two cannot drift. What is
// here is only which sources this machine may push, where they live, and the
// beat.

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

// sourcePush is what one of a discobox's push-delivered sources did on one
// look: what its local branch resolves to now, and whether that moved the
// discobox's origin. A source with nothing to send reports the commit both ends
// already hold and nothing else.
type sourcePush struct {
	Slug   string
	Branch string
	// Commit is the local tip resolved for this source, whether or not it was
	// sent — what a caller records against a failure so the same refused push
	// is not attempted again every beat.
	Commit string
	Pushed bool
	// Err is why this source did not push. Having nothing to send is not one.
	Err error
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

// pushSandboxSources sends this machine's new commits into the origin
// repositories the discobox's push-delivered sources fetch from — the transport
// `discobox push` performs, with no flags. Nothing in the discobox moves: it
// gains origin/<branch>, and whoever is working in it rebases when they choose.
//
// held names, per source slug, a commit whose push has already failed. Such a
// source is resolved but not sent again while it names that same commit, so a
// standing refusal — a stale lease, an unrelated history — costs a ref read
// rather than a rejected transfer on every beat.
//
// The order is deliberate: every source is resolved first, and only if
// something actually moved is the git route opened. Resolving is two ref reads
// in a repository this machine already has, so the ordinary beat — nothing
// committed since the last one — dials nothing at all (ADR 0095 §4).
func (a *App) pushSandboxSources(ctx context.Context, client *apiclientgen.Client, projectID, sandboxID string, held map[string]string) ([]sourcePush, error) {
	targets, err := a.pushTargets(ctx, client, projectID, sandboxID)
	if err != nil {
		return nil, err
	}
	pushes := make([]sourcePush, 0, len(targets.sources))
	var pending []applySourceEntry
	for _, entry := range targets.sources {
		resolved, err := sandboxpush.Resolve(ctx, targets.roots[entry.slug], sandboxID, entry.source, sandboxpush.Options{})
		push := sourcePush{Slug: entry.slug, Branch: resolved.Branch, Commit: resolved.Commit}
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

	gitServerURL, releaseGitServerURL, err := a.gitServerURL(ctx)
	if err != nil {
		return pushes, err
	}
	defer releaseGitServerURL()
	for _, entry := range pending {
		result, err := sandboxpush.Push(ctx, targets.roots[entry.slug], gitServerURL, projectID, sandboxID, a.token, entry.source, sandboxpush.Options{})
		pushes = append(pushes, sourcePush{
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
// life of this invocation.
//
// A source this machine cannot push is left out rather than reported: a
// discobox with a bound or cloned source of its own is not a failure of
// anything, it is simply not one this pushes. The empty answer is held too,
// since what makes it empty does not change either.
func (a *App) pushTargets(ctx context.Context, client *apiclientgen.Client, projectID, sandboxID string) (*pushTargets, error) {
	a.pushMu.Lock()
	cached, ok := a.pushCache[sandboxID]
	a.pushMu.Unlock()
	if ok {
		return cached, nil
	}

	res, err := client.GetSandbox(ctx, apiclientgen.GetSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
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
	// Asked here rather than trusted from a caller: the launcher reads the same
	// answer off the row to know whether to call at all, and a raw attach has
	// no row to read.
	if !pushable(*sandbox, hostID) {
		a.cachePushTargets(sandboxID, targets)
		return targets, nil
	}
	for _, entry := range applySources(sandbox) {
		if err := sandboxpush.CheckPushDelivered(entry.source); err != nil {
			continue
		}
		hostDir, _, err := resolveApplyHostDir(sandbox, hostID, entry, nil)
		if err != nil {
			// Another machine's discobox, or a directory that is no longer
			// there. Both are answered by `discobox push`, which can say so
			// and take a --dir; a push nobody asked for cannot.
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

	a.cachePushTargets(sandboxID, targets)
	return targets, nil
}

func (a *App) cachePushTargets(sandboxID string, targets *pushTargets) {
	a.pushMu.Lock()
	defer a.pushMu.Unlock()
	if a.pushCache == nil {
		a.pushCache = map[string]*pushTargets{}
	}
	a.pushCache[sandboxID] = targets
}

// autoPushEvery is how often an attached client looks for new local commits to
// send. The launcher's window keeps its own copy of this beat, on the listing's
// clock; this is the raw attach's.
const autoPushEvery = 5 * time.Second

// autoPushWhileAttached pushes the discobox's push-delivered sources for as
// long as a terminal attach lasts: once at the start, and again on every beat
// (ADR 0095 §1). Attaching is the trigger, so this runs for `discobox attach
// --raw`, `discobox run --raw` and `discobox admin terminal attach` alike.
//
// It writes nothing while the attach runs. A raw attach is the discobox's
// stream and nothing else — for a pipe, a recording, or a terminal you would
// rather keep as it is — and a line put into it lands in the middle of whatever
// the harness is drawing. The returned stop ends the loop and reports what
// could not be pushed, on a terminal that is the client's again by then; a push
// that worked says nothing, because it is visible in git and nobody asked for
// it (ADR 0095 §5).
func (a *App) autoPushWhileAttached(ctx context.Context, client *apiclientgen.Client, projectID, sandboxID string) (stop func(io.Writer)) {
	pushing, cancel := context.WithCancel(ctx)
	done := make(chan []sourcePush, 1)
	go func() {
		// held is this loop's own record of what has already been refused, so a
		// standing refusal is not retried — and not re-reported — every beat.
		held := map[string]string{}
		var failed []sourcePush
		ticker := time.NewTicker(autoPushEvery)
		defer ticker.Stop()
		for {
			pushes, err := a.pushSandboxSources(pushing, client, projectID, sandboxID, held)
			if err == nil {
				failed = failed[:0]
				for _, push := range pushes {
					switch {
					case push.Err != nil:
						held[push.Slug] = push.Commit
						failed = append(failed, push)
					case push.Pushed:
						delete(held, push.Slug)
					}
				}
			}
			select {
			case <-pushing.Done():
				done <- failed
				return
			case <-ticker.C:
			}
		}
	}()
	return func(stderr io.Writer) {
		cancel()
		for _, push := range <-done {
			// Every failure that is still standing when the attach ends, which
			// is at most one per source: the loop holds each one at the commit
			// it failed on.
			fmt.Fprintf(stderr, "could not push %s into the discobox's origin: %v\n", push.Slug, push.Err)
		}
	}
}
