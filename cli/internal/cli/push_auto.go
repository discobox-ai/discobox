package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
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

// pushable reports that new commits made here are an attached client's to send
// into the discobox's origin: it has a source an automatic push may send, this
// machine is the one it was pushed from, and it is in a state to take another
// push (ADR 0095 §2).
//
// It is two questions, because only one of them keeps its answer: see
// pushableSource and pushableNow.
func pushable(sb apimodel.Sandbox, hostID string) bool {
	return pushableSource(sb, hostID) && pushableNow(sb)
}

// pushableSource is the half of that question a discobox answers the same way
// for its whole life: this machine created it, and at least one of its sources
// is one an automatic push may send.
//
// A machine with no resolvable identity pushes nothing, which is the same
// answer it gives for a discobox created somewhere else: without an identity
// there is no way to claim this one.
func pushableSource(sb apimodel.Sandbox, hostID string) bool {
	if hostID == "" {
		return false
	}
	origin, ok := sb.Origin.Get()
	if !ok || origin.HostId != hostID {
		return false
	}
	for _, entry := range applySources(&sb) {
		if autoPushable(entry.source) {
			return true
		}
	}
	return false
}

// pushableNow is the half that changes while the discobox runs, and so is the
// half nothing may cache.
//
// Awaiting its source is refused rather than merely unsupported. A push to a
// parked discobox is its create's delivery, which starts it
// (deliverAwaitedSource) — a state-machine transition and a decision about a
// create that failed, so it stays something asked for by name.
func pushableNow(sb apimodel.Sandbox) bool {
	if sandboxAwaitingSource(sb) {
		return false
	}
	switch sandboxDisplayState(sb) {
	case "archived", "archiving":
		// No container for the git route to start, so the push would fail in
		// the proxy with nothing useful to say.
		return false
	}
	return true
}

// autoPushable reports whether one source is one an automatic push may send.
//
// It is push-delivered, so there is a mirror to write — and it was checked out
// at a **branch**, which is what makes the thing to send unambiguous. A source
// created from a tag or a bare commit names no branch, and `discobox push`
// falls back to whatever HEAD is now (pushRefs): a rev a person picked the
// moment for, and on a 5s clock whatever branch they have switched to since.
// Sending that into the discobox's origin with nobody present is the one thing
// this must not do, so those sources are left to the command, which is what
// ADR 0058 §6 keeps it for.
func autoPushable(source apimodel.GitSource) bool {
	if sandboxpush.CheckPushDelivered(source) != nil {
		return false
	}
	if source.NoLocalRepository.Or(false) {
		// A directory in no repository at all: the create built a throwaway
		// repository over it and deleted it afterwards (ADR 0045), so there is
		// no history here to send and each run's would be unrelated anyway.
		// The client said so at create, which is why this is decided here
		// rather than discovered every beat by failing to find a repository.
		return false
	}
	checkout, ok := source.Checkout.Get()
	if !ok {
		return false
	}
	return strings.TrimSpace(checkout.RefType.Or("")) == "branch" &&
		strings.TrimSpace(checkout.RefName.Or("")) != ""
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
	// UpToDate reports that the discobox's origin already holds this commit —
	// whether this client put it there or somebody did it by hand. It is what
	// releases a hold that was resolved out of band: `discobox push --force`,
	// the remedy a refusal names, moves the lease rather than the branch, so
	// nothing about the tip a hold remembers would otherwise change.
	UpToDate bool
	// Err is why this source did not push. Having nothing to send is not one.
	Err error
	// sent marks an outcome that came from an attempted transfer rather than
	// from working out whether to make one. A transfer cannot be canceled once
	// started, so its answer is always about the discobox — where an error from
	// the resolving half may only be about the caller giving up.
	sent bool
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
	// Counted for the whole look, not only for the transfer: a caller that
	// waited on the send alone would find the counter at zero while a look was
	// still deciding, leave, and end the transfer that look was one step from
	// starting — the failure App.waitForPushes exists to prevent, in a smaller
	// window. Resolving is fast and cancellable, so waiting for it costs
	// nothing that matters.
	a.pushInFlight.Add(1)
	defer a.pushInFlight.Done()
	targets, err := a.pushTargets(ctx, client, projectID, sandboxID)
	if err != nil {
		return nil, err
	}
	pushes := make([]sourcePush, 0, len(targets.sources))
	var pending []applySourceEntry
	for _, entry := range targets.sources {
		resolved, err := sandboxpush.Resolve(ctx, targets.roots[entry.slug], sandboxID, entry.source, sandboxpush.Options{})
		push := sourcePush{
			Slug:     entry.slug,
			Branch:   resolved.Branch,
			Commit:   resolved.Commit,
			UpToDate: err == nil && resolved.UpToDate(),
		}
		// A source held at exactly what it names now is one whose push already
		// failed and was already reported. It is resolved — that is how a hold
		// is released when the branch moves — and nothing else.
		//
		// Unless the origin turns out to hold it after all, which is what
		// answering the refusal by hand looks like from here: `--force` moves
		// the lease, not the branch, so the tip is the one that failed and only
		// the lease says otherwise. Reported up to date, the hold is released
		// by the caller rather than kept until the next commit.
		if commit, ok := held[entry.slug]; ok && commit == resolved.Commit && !push.UpToDate {
			pushes = append(pushes, push)
			continue
		}
		if err != nil {
			push.Err = err
			pushes = append(pushes, push)
			continue
		}
		if push.UpToDate {
			pushes = append(pushes, push)
			continue
		}
		pending = append(pending, entry)
	}
	if len(pending) == 0 {
		return pushes, nil
	}

	// From here the caller's context no longer applies, deliberately. Killing a
	// `git push` mid-transfer is not a tidy no-op: the receiving end may
	// already have taken the pack, while the lease this client leases against
	// is only written once the push returns (pushTo). A client that detached,
	// or a window that quit, in that window would leave the discobox's origin
	// ahead of its own lease and refuse its next push — manufacturing exactly
	// the refusal ADR 0095 §5 treats as somebody else's doing. Everything above
	// is cancellable, which is where a caller in a hurry gets to stop; a
	// transfer that has started finishes.
	//
	// It still has to end, because a caller waits for it — but not on a clock.
	// The largest transfer this ever makes is the first one after attaching,
	// carrying everything committed since this client last pushed, and that is
	// exactly the one a deadline would cut in half. git is asked to give up on
	// a connection that has stopped moving instead, and not to ask anyone for
	// anything, since the terminal it would ask on is the one the caller is
	// taking back (sandboxgit.PushArgs, sandboxgit.NoPromptEnv).
	sending := context.WithoutCancel(ctx)
	gitServerURL, releaseGitServerURL, err := a.gitServerURL(sending)
	if err != nil {
		return pushes, err
	}
	defer releaseGitServerURL()
	for _, entry := range pending {
		result, err := sandboxpush.Push(sending, targets.roots[entry.slug], gitServerURL, projectID, sandboxID, a.token, entry.source, sandboxpush.Options{})
		pushes = append(pushes, sourcePush{
			Slug:   entry.slug,
			Branch: result.Branch,
			Commit: result.Commit,
			Pushed: err == nil && !result.UpToDate,
			Err:    err,
			sent:   true,
		})
	}
	return pushes, nil
}

// pushTargets is what an automatic push for this discobox sends from, and what
// each source is sent from.
//
// A source this machine cannot push is left out rather than reported: a
// discobox with a bound or cloned source of its own is not a failure of
// anything, it is simply not one this pushes.
//
// **Only an answer that cannot change is held.** A discobox's sources, their
// delivery and the directories they came from are fixed at create, and so is
// "none of them is this machine's to push" — those are worth holding, and
// holding them is what keeps a beat with nothing to send from making a
// request (ADR 0095 §4). Everything else that can say no here says it about
// right now: a discobox still awaiting its source or being archived, a
// directory not mounted yet, a repository nobody has run `git init` in. Those
// are asked again on the next beat, because a client that cached the first no
// would keep it for the whole session — and a discobox that becomes pushable a
// few seconds in is the ordinary case, not the exception.
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
	if !pushableSource(*sandbox, hostID) {
		// Nothing about this discobox will ever make it pushable from here.
		a.cachePushTargets(sandboxID, targets)
		return targets, nil
	}
	if !pushableNow(*sandbox) {
		// Parked or on its way out — true of the moment, so it is answered
		// again on the next beat rather than remembered.
		return targets, nil
	}
	// settled stays true only while every source this discobox has was resolved
	// for a reason that will read the same tomorrow.
	settled := true
	for _, entry := range applySources(sandbox) {
		if !autoPushable(entry.source) {
			// Bound, cloned, or checked out at something that is not a branch:
			// all three are decided at create and none of them changes.
			continue
		}
		hostDir, _, err := resolveApplyHostDir(sandbox, hostID, entry, nil)
		if err != nil {
			// A directory that is not there *right now* — an unmounted drive,
			// a checkout being replaced. `discobox push` answers this one with
			// a --dir; a push nobody asked for waits for it to come back.
			settled = false
			continue
		}
		repoRoot, err := gitutil.Root(ctx, hostDir)
		if err != nil {
			if !errors.Is(err, gitutil.ErrNotARepository) {
				return nil, err
			}
			// Somebody has not run `git init` there yet — the source itself was
			// created from a repository, or autoPushable would have dropped it
			// above. A minute away from being false, so it is not held.
			settled = false
			continue
		}
		targets.sources = append(targets.sources, entry)
		targets.roots[entry.slug] = repoRoot
	}

	if settled {
		a.cachePushTargets(sandboxID, targets)
	}
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
//
// A var so a test can shorten it: what the loop does *between* beats — holding a
// refusal, not re-sending it — cannot be asserted without living through
// several, and five seconds each is not a test anybody will run.
var autoPushEvery = 5 * time.Second

// pushWaitNotice is how long a wait may go unexplained. Below it, saying
// anything would be noise on an exit nobody experienced as a wait.
const pushWaitNotice = 300 * time.Millisecond

// waitForPushes blocks until every look this invocation started has finished,
// so a front end can leave without ending a transfer between receive-pack and
// the lease that guards it (ADR 0095 §6).
//
// The raw attach's stop already waits for its own loop; this is for the
// launcher, whose Bubble Tea program returns on Quit without waiting for the
// commands it has in flight.
//
// A wait long enough to notice says what it is for. The window has taken the
// screen down by the time this runs, so an unexplained pause at the shell
// prompt is exactly the silence ADR 0060 is about.
func (a *App) waitForPushes(stderr io.Writer) {
	waitSaying(stderr, "finishing a push into the discobox's origin…", a.pushInFlight.Wait)
}

// waitSaying runs wait, and puts what it is waiting for on screen if it lasts
// long enough to be worth explaining.
func waitSaying(stderr io.Writer, what string, wait func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		wait()
	}()
	select {
	case <-done:
		return
	case <-time.After(pushWaitNotice):
	}
	status := newStatusLine(stderr)
	status.set(what)
	<-done
	status.clear()
}

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
//
// **A detach never interrupts a push that is already running**, and never
// reports one as failed. pushSandboxSources finishes a transfer it has started
// whatever the caller does, so stop waits for it — a moment, for a transfer
// that is incremental by construction, and at most pushSendCeiling.
//
// What a detach *does* cut short is the half that decides whether to send:
// reading the discobox, resolving a branch tip. An error from there while
// stopping is about the stop, and is dropped. An error from a transfer is
// never about the stop — nothing could have interrupted it — so it is reported
// even when it lands after the detach. That is the case worth getting right:
// commit, detach a second later, and the push that goes out in between is
// exactly the one whose refusal somebody has to act on (ADR 0095 §5).
func (a *App) autoPushWhileAttached(ctx context.Context, client *apiclientgen.Client, projectID, sandboxID string) (stop func(io.Writer)) {
	beating, stopBeating := context.WithCancel(ctx)
	done := make(chan map[string]sourcePush, 1)
	go func() {
		// held is this loop's own record of what has already been refused, so a
		// standing refusal is not retried — and not re-reported — every beat.
		// failed is the same set with the reason kept, for the report.
		//
		// They are maps rather than a list rebuilt each beat, because a held
		// source comes back from the next look with no error at all — that is
		// what holding it means — so a list would forget the refusal one beat
		// after it happened and only ever report a failure that landed in the
		// last five seconds.
		held := map[string]string{}
		failed := map[string]sourcePush{}
		ticker := time.NewTicker(autoPushEvery)
		defer ticker.Stop()
		for {
			pushes, err := a.pushSandboxSources(beating, client, projectID, sandboxID, held)
			if err == nil {
				for _, push := range pushes {
					switch {
					case push.Err != nil:
						if !push.sent && beating.Err() != nil {
							// Cut short by the stop rather than refused.
							continue
						}
						held[push.Slug] = push.Commit
						failed[push.Slug] = push
					case push.Pushed, push.UpToDate:
						// Sent, or already there — including because somebody
						// answered the refusal themselves.
						delete(held, push.Slug)
						delete(failed, push.Slug)
					}
				}
			}
			select {
			case <-beating.Done():
				done <- failed
				return
			case <-ticker.C:
			}
		}
	}()
	return func(stderr io.Writer) {
		stopBeating()
		var standing map[string]sourcePush
		waitSaying(stderr, "finishing a push into the discobox's origin…", func() { standing = <-done })
		slugs := make([]string, 0, len(standing))
		for slug := range standing {
			slugs = append(slugs, slug)
		}
		sort.Strings(slugs)
		for _, slug := range slugs {
			// Every failure still standing when the attach ends — one per
			// source, at the commit it failed on, however many beats ago that
			// was. Ordered, so two sources do not report in a different order
			// each time.
			push := standing[slug]
			fmt.Fprintf(stderr, "could not push %s into the discobox's origin: %v\n", push.Slug, push.Err)
		}
	}
}
