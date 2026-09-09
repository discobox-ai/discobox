package tui

import (
	"maps"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// Push, in the window: while a workspace is open on a discobox, the commits
// made here since it was created are sent into the origin it fetches from,
// without anyone asking for it (ADR 0095).
//
// It is the counterpart of apply.go and deliberately not shaped like it. Apply
// writes the developer's own working tree, so it is offered on a band and run
// on a key. A push writes a bare repository that belongs to one discobox, is
// bound read-only into it, and is read by nothing else — so it cannot interrupt
// anything, and the offer would only ever be accepted.
//
// The push itself is `discobox push` with no flags, through DataSource. Nothing
// here re-decides what it sends or what it refuses: the lease, the
// related-history check and the untouched dirty tree are all ADR 0058 §6, and
// the window never passes --force.

// autoPushEvery is how often an open workspace looks for new local commits to
// send. It is the listing's beat rather than the workspace's own 2s exec poll:
// noticing a commit is not urgent the way noticing a new session is, and this
// one spawns local git rather than making a request.
const autoPushEvery = refreshEvery

// autoPushTickMsg asks for the next look. Like every workspace loop it carries
// the generation it was scheduled under, so leaving the workspace ends it.
type autoPushTickMsg struct{ gen int }

// autoPushedMsg is one look, done: what each of the discobox's push-delivered
// sources resolved to.
type autoPushedMsg struct {
	gen    int
	pushes []SourcePush
}

// autoPush sends whatever this machine has committed since the last look.
//
// The loop is push → answer → schedule, rather than a free-running tick, so
// there is never a second push in flight over the first: a git transfer can
// outlast the beat, and two pushes racing to move one ref is how a lease starts
// refusing pushes nobody made twice.
//
// A discobox that is not this window's to push — someone else's machine, a
// clone-delivered source, an archived box — is skipped rather than ending the
// loop, since the listing that answers Pushable is re-read on its own beat and
// a starting discobox becomes pushable a few seconds in.
func (m *Model) autoPush(gen int) tea.Cmd {
	box := m.currentBox()
	if !box.Pushable {
		return m.autoPushTick(gen)
	}
	ctx, ds, id := m.ctx, m.ds, box.ID
	// Copied, because the window goes on editing it while the push runs.
	held := maps.Clone(m.pushHeld)
	return func() tea.Msg {
		pushes, err := ds.PushSources(ctx, id, held)
		if err != nil {
			// The call failing as a whole is ambient work failing — the
			// discobox could not be read, the git route could not be opened —
			// and is dropped rather than reported. See autoPushed.
			return autoPushedMsg{gen: gen}
		}
		return autoPushedMsg{gen: gen, pushes: pushes}
	}
}

func (m *Model) autoPushTick(gen int) tea.Cmd {
	return tea.Tick(autoPushEvery, func(time.Time) tea.Msg {
		return autoPushTickMsg{gen: gen}
	})
}

// autoPushed reports what moved and schedules the next look.
//
// A source that failed is held at the commit it failed on: the failures a push
// produces are decisions rather than transients — a lease another machine
// moved, a history nothing in the discobox could rebase onto — so retrying one
// every five seconds would put a permanent error on the status line and send
// the same refused pack behind it. The next commit made here is a new tip, and
// a new attempt.
//
// A call that failed as a whole arrives here empty and so reports nothing. It
// is ambient work nobody asked for, on the same footing as the resources
// readout: a window that says something every five seconds about a server it
// cannot reach is a window you stop reading, and the listing beside it is
// already failing visibly.
func (m *Model) autoPushed(msg autoPushedMsg) tea.Cmd {
	if msg.gen != m.wsGen {
		return nil
	}
	var moved []SourcePush
	var failed []SourcePush
	for _, push := range msg.pushes {
		switch {
		case push.Err != nil:
			m.pushHeld[push.Slug] = push.Commit
			failed = append(failed, push)
		case push.Pushed:
			delete(m.pushHeld, push.Slug)
			moved = append(moved, push)
		case push.UpToDate:
			// Already in the origin, which after a refusal means somebody
			// answered it themselves. Nothing to say — but the hold goes, or
			// this window would keep skipping the source until the next
			// commit.
			delete(m.pushHeld, push.Slug)
		}
	}
	cmds := []tea.Cmd{m.autoPushTick(msg.gen)}
	// A failure outranks a push: with several sources, the one that did not go
	// is the half of the answer somebody has to act on.
	switch {
	case len(failed) > 0:
		cmds = append(cmds, m.report(true, "%s", pushFailure(failed)))
	case len(moved) > 0:
		cmds = append(cmds, status("%s", pushedText(moved)))
	}
	return tea.Batch(cmds...)
}

// pushedText says what went, in the room the status line has. One source names
// where it landed, which is the branch to rebase onto inside the discobox;
// several name themselves, since the branches are then not the point.
func pushedText(moved []SourcePush) string {
	if len(moved) == 1 {
		push := moved[0]
		text := "pushed " + shortCommit(push.Commit)
		if push.Branch != "" {
			text += " to origin/" + push.Branch
		}
		return text + " in the discobox"
	}
	slugs := make([]string, 0, len(moved))
	for _, push := range moved {
		slugs = append(slugs, push.Slug)
	}
	return "pushed " + strings.Join(slugs, ", ") + " to the discobox's origin"
}

// pushFailure is why a source did not go. The reason is git's own or the
// push's, and both are written to be read by whoever has to answer them.
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
