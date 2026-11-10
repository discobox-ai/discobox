package console

import (
	"context"
	"maps"
	"strings"
	"time"
)

// The automatic push (ADR 26-09-05-008): while a workspace is open on a
// discobox, parked or on screen, the commits made on this machine since the
// box was created are sent into the origin it fetches from, without anyone
// asking. Each console runs the loop on its own clock — look, apply its
// answer, wait PushEvery, look again, never two looks in flight — and every
// rule of it is here: whether a box is pushed, what a look sends, what its
// answer holds against the next, and what it says.

// PushEvery is how often an attached workspace, parked or not, looks for new
// local commits to push (ADR 26-09-05-008). It is slower than the exec poll on
// purpose: noticing a commit is not urgent the way noticing a new session is,
// and a look spawns local git rather than making a request. It is its own
// beat, tied to no other.
const PushEvery = 5 * time.Second

// AutoPush is one attached workspace's automatic push: Held is the commit
// each source was refused on, by slug, and that source is not pushed again
// while its branch still names the commit (Pushed). The zero value holds
// nothing.
type AutoPush struct {
	Held map[string]string
}

// Look is one look of the automatic push, for the box as the listing has it
// now, to run off the renderer's loop; the renderer applies its answer with
// Pushed and asks again PushEvery later. It is nil when the box is not one
// this window pushes — another machine's, a clone-delivered source, an
// archived box — and the loop carries on regardless, since the listing that
// answers Pushable is re-read on its own beat and a starting box becomes
// pushable a few seconds in.
//
// The holds are copied as they stand, since the renderer goes on changing
// them while the look runs. A look that fails as a whole — the box could not
// be read, the git route could not be opened — answers nothing: it is ambient
// work nobody asked for, and saying so every beat about a server that cannot
// be reached is a status line people stop reading.
func (a *AutoPush) Look(box Sandbox, ds DataSource) func(context.Context) []SourcePush {
	if !box.Pushable || ds == nil {
		return nil
	}
	held := maps.Clone(a.Held)
	return func(ctx context.Context) []SourcePush {
		pushes, err := ds.PushSources(ctx, box.ID, held)
		if err != nil {
			return nil
		}
		return pushes
	}
}

// Pushed takes one look's answer, and returns what it says: what moved, or —
// outranking it — what was refused, as a warning, since with several sources
// the one that did not go is the half of the answer somebody has to act on.
// A look that moved nothing says nothing.
//
// A refused source is held at the commit it was refused on: a push's
// failures are decisions rather than transients — a lease another machine
// moved, a history nothing could rebase onto — so it is tried again only once
// a new commit is made. A source already in the origin lets its hold go,
// since after a refusal that means somebody answered it themselves.
func (a *AutoPush) Pushed(pushes []SourcePush) Progress {
	var moved, failed []SourcePush
	if a.Held == nil {
		a.Held = map[string]string{}
	}
	for _, push := range pushes {
		switch {
		case push.Err != nil:
			a.Held[push.Slug] = push.Commit
			failed = append(failed, push)
		case push.Pushed:
			delete(a.Held, push.Slug)
			moved = append(moved, push)
		case push.UpToDate:
			delete(a.Held, push.Slug)
		}
	}
	switch {
	case len(failed) > 0:
		return Progress{Text: pushFailure(failed), Warning: true}
	case len(moved) > 0:
		return Progress{Text: pushedText(moved)}
	}
	return Progress{}
}

// PushedElsewhere is what a push's answer says when its box is not the one
// on screen — a parked workspace's: the same, naming the box, since the box
// the person is looking at is another. Nothing said stays nothing.
func PushedElsewhere(box Sandbox, said Progress) Progress {
	if said.Text == "" {
		return said
	}
	said.Text = box.Label() + ": " + said.Text
	return said
}

// pushedText says what the automatic push moved, in the room a status line
// has. One source names where it landed, which is the branch to rebase onto in
// the box; several name themselves.
func pushedText(moved []SourcePush) string {
	if len(moved) == 1 {
		push := moved[0]
		text := "pushed " + ShortCommit(push.Commit)
		if push.Branch != "" {
			remote := push.Remote
			if remote == "" {
				remote = "origin"
			}
			text += " to " + remote + "/" + push.Branch
		}
		return text + " in the discobox"
	}
	slugs := make([]string, 0, len(moved))
	for _, push := range moved {
		slugs = append(slugs, push.Slug)
	}
	return "pushed " + strings.Join(slugs, ", ") + " to the discobox's origin"
}

// pushFailure says why the automatic push refused a source: git's own reason,
// or the push's, both written to be read by whoever has to answer them.
func pushFailure(failed []SourcePush) string {
	if len(failed) == 1 && failed[0].Err != nil {
		return failed[0].Err.Error()
	}
	slugs := make([]string, 0, len(failed))
	for _, push := range failed {
		slugs = append(slugs, push.Slug)
	}
	return "could not push " + strings.Join(slugs, ", ") + " to the discobox's origin; `discobox push` says why"
}
