package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/discobox-ai/discobox/cli/internal/console"
)

// Push, in the window: while a workspace is open on a discobox, the commits
// made here since it was created are sent into the origin it fetches from,
// without anyone asking for it (ADR 26-09-05-008 on automatic push).
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

// autoPushTickMsg asks for the next look, console.PushEvery after the last.
// Like every workspace loop it carries the generation it was scheduled under,
// so leaving the workspace ends it — and the discobox it pushes, because a
// workspace switched away from in the tree is still attached and keeps pushing
// under the generation it was parked with (pushLoop).
type autoPushTickMsg struct {
	box console.BoxRef
	gen int
}

// autoPushedMsg is one look, done: what each of the discobox's push-delivered
// sources resolved to.
type autoPushedMsg struct {
	box    console.BoxRef
	gen    int
	pushes []console.SourcePush
}

// pushLoop is the workspace a push loop belongs to: the live one, or one
// parked in the tree, which is still attached and so still pushes (ADR
// 26-09-05-008 §1), and whether it is a parked one. It is nil once the
// workspace is gone, which ends the loop.
func (m *Model) pushLoop(box console.BoxRef, gen int) (ws *workspace, parked bool) {
	if m.PushHeld != nil && box == m.boxRef(m.Box) && gen == m.PushGen {
		return &m.workspace, false
	}
	if ws := m.parked[box]; ws != nil && gen == ws.PushGen {
		return ws, true
	}
	return nil, false
}

// autoPush runs one look of the automatic push (console.Workspace.PushLook),
// or schedules the next when the box is not this window's to push.
//
// The loop is push → answer → schedule, rather than a free-running tick, so
// there is never a second push in flight over the first: a git transfer can
// outlast the beat, and two pushes racing to move one ref is how a lease starts
// refusing pushes nobody made twice. What a look does and what its answer
// means are the core's; this loop is only the window's clock for it.
func (m *Model) autoPush(id console.BoxRef, gen int) tea.Cmd {
	ws, _ := m.pushLoop(id, gen)
	if ws == nil {
		return nil
	}
	look := ws.PushLook(m.boxByRef(id), m.ds.ForServer(ws.Box.Server))
	if look == nil {
		return m.autoPushTick(id, gen)
	}
	ctx := m.ctx
	return func() tea.Msg {
		return autoPushedMsg{box: id, gen: gen, pushes: look(ctx)}
	}
}

func (m *Model) autoPushTick(id console.BoxRef, gen int) tea.Cmd {
	return tick(console.PushEvery, func(time.Time) tea.Msg {
		return autoPushTickMsg{box: id, gen: gen}
	})
}

// autoPushed reports what moved and schedules the next look. What the answer
// holds against the next look is the core's (console.Workspace.Pushed): a
// refused source is held at the commit it was refused on until a new one. A
// look that failed as a whole arrives empty and reports nothing.
func (m *Model) autoPushed(msg autoPushedMsg) tea.Cmd {
	ws, parked := m.pushLoop(msg.box, msg.gen)
	if ws == nil {
		return nil
	}
	moved, failed := ws.Pushed(msg.pushes)
	cmds := []tea.Cmd{m.autoPushTick(msg.box, msg.gen)}
	// A parked box's report names it, since the box on screen is another.
	who := ""
	if parked {
		who = sandboxLabel(m.boxByRef(msg.box)) + ": "
	}
	// A failure outranks a push: with several sources, the one that did not go
	// is the half of the answer somebody has to act on.
	switch {
	case len(failed) > 0:
		cmds = append(cmds, m.report(true, "%s%s", who, console.PushFailure(failed)))
	case len(moved) > 0:
		cmds = append(cmds, status("%s%s", who, console.PushedText(moved)))
	}
	return tea.Batch(cmds...)
}
