package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/discobox-ai/discobox/cli/internal/console"
	"github.com/discobox-ai/discobox/pkg/programstatus"
	"github.com/discobox-ai/discobox/termpane"
)

// The workspace screen is the discobox as the server has it, not as this
// window remembers it. Opening it attaches to the primary terminal and to
// every other live TTY session the discobox has; while it is open, a poll of
// the exec listing keeps the tabs following the server, so a shell started
// from another window appears here on its own. Detaching leaves the workspace:
// every stream is closed at once, and every session keeps running.
//
// Which side a session is drawn on is the server's own answer rather than a
// layout this window remembers: a declared service goes at the head of the
// left column and a harness terminal on the left beside the primary, while
// everything else is a shell and goes on the right. See console.Exec.Side.
//
// The poll is a poll rather than a subscription because the control plane has
// no exec event stream yet — exec state lives on the sandbox and is proxied
// through — and it only has to notice *new* sessions: the attach streams
// themselves already deliver every exit. The seam (DataSource.Execs) is a
// snapshot so that a stream, when one exists, replaces the tick loop and
// nothing else.

// workspaceExecsMsg is one answer from the exec listing: the first one opens
// the workspace's terminals, and every one after reconciles the tabs.
type workspaceExecsMsg struct {
	gen   int
	execs []console.Exec
	err   error
}

// workspaceTermMsg carries one connected workspace session back to the model:
// the primary, an existing session's attach, or a terminal or shell the leader
// asked for — which arrives with focus.
type workspaceTermMsg struct {
	gen  int
	exec console.Exec
	term console.Terminal
	err  error
	// asked is what the leader asked for, empty for a session the poll opened
	// on its own. A create that failed carries no session record to say what
	// it would have been, so this is what such a failure is reported as.
	asked console.Interaction
	focus bool
}

// workspaceTickMsg asks for the next poll of the exec and service listings.
type workspaceTickMsg struct{ gen int }

// workspaceServicesMsg is one answer from the service listing.
type workspaceServicesMsg struct {
	gen      int
	services []console.Service
	err      error
}

// serviceTermMsg carries one opened service pane back to the model: the
// service it is for, and the stream it draws — an attach to its running
// process, or the card describing why there is none.
type serviceTermMsg struct {
	gen     int
	service console.Service
	order   int
	term    console.Terminal
	err     error
}

// workspaceForwardMsg carries the port forward the workspace opened with, or
// the reason there is none.
type workspaceForwardMsg struct {
	gen     int
	forward console.Forward
	err     error
}

// workspaceForwardChangedMsg is the forward saying its bindings differ. It
// carries nothing: the header redraws from the forward itself.
type workspaceForwardChangedMsg struct{ gen int }

// workspace is one discobox's workspace as this window holds it: the core's
// (console.Workspace — its columns of panes, what it is ending, its forward,
// its push holds) and what is this renderer's alone, which has to survive with
// it when it is switched away from in the tree (parkWorkspace). Model embeds
// the live one; the parked ones are kept whole beside it (Model.parked).
//
// What is not here is the live screen's alone and goes with the generation
// that ran it — the opens in flight, the polls, the audit.
type workspace struct {
	console.Workspace[*pane]

	// onShells is which of the two columns has the keys.
	onShells bool
	// maximized is whether one column has the whole window instead of the two
	// sharing it. Which column that is follows the focus — onShells — so there
	// is one visible box and it is always the one the keys go to. See
	// toggleMaximized.
	maximized bool
	// toolShown is the tool with the whole window, or nil when the workspace
	// is on screen. A tool's pane lives in the shells column, after the
	// shells, whether or not it is showing: put away, it is a tab there like
	// any other, still attached and still running. See tools.go.
	toolShown *pane
}

// openFromList opens a terminal screen from the list, on the discobox under
// the cursor. It replaces whatever was open: this is a different discobox, or
// the same one started over, and either way what was on screen was about
// something else.
func (m *Model) openFromList(act console.Interaction, sandbox console.Sandbox) tea.Cmd {
	// The workspace on screen is parked rather than closed, so the tree can
	// switch back to it without attaching again. See tree.go.
	m.parkWorkspace()
	if ws := m.parked[m.boxRef(sandbox)]; ws != nil {
		cmd := m.unparkWorkspace(ws)
		if act == console.InteractShell {
			cmd = tea.Batch(cmd, m.newShell())
		}
		return cmd
	}
	m.Box = sandbox
	// The workspace is the screen either way; asking for a shell opens it
	// with a fresh one, focused.
	return m.openWorkspace(sandbox, act == console.InteractShell)
}

// openWorkspace attaches to a discobox's workspace: list what is running,
// then open the primary and a tab for every live session the listing reports.
// The poll starts alongside and follows the server for as long as the screen
// is up.
func (m *Model) openWorkspace(sandbox console.Sandbox, freshShell bool) tea.Cmd {
	m.Box = sandbox
	// Nothing else can still have the window. The prompt can start a discobox
	// and be on the harnesses screen by the time it is created, which is the
	// one way the two ever meet.
	m.harnessesOpen = false
	m.busy = "attach…"
	if m.attach == nil {
		m.showOpening()
	}
	m.connecting = map[string]bool{}
	gen := m.wsGen
	// Nothing is held against a workspace that is only now opening: a kill or
	// a refusal belongs to the session it happened in. Its push loop runs
	// under this generation (push.go).
	m.Open(sandbox, gen)
	if sandbox.AwaitsDelivery {
		return m.deliverSource(gen, sandbox, freshShell)
	}
	return m.attachWorkspace(gen, sandbox, freshShell)
}

// showOpening makes the workspace of Box the screen, tree and all, while
// it is on its way up: the attach waiting, or a create that has the discobox
// still delivering its source (watchCreate). Its frame says what the wait is
// for (viewOpening), and the keys are the leader's until a pane arrives.
func (m *Model) showOpening() {
	m.wsOpening = true
	m.listFront, m.treeExpanded = false, false
	m.closePages()
	m.focus = focusPane
	m.prompt.Blur()
}

// attachWorkspace starts everything an open workspace runs: the listings, the
// attach they lead to, the port forward, and the automatic push.
func (m *Model) attachWorkspace(gen int, sandbox console.Sandbox, freshShell bool) tea.Cmd {
	// The attach waits for the discobox to become attachable, which behind a
	// cold image pull is minutes (ADR 0039). Say what it is waiting for while
	// it does; the watch reports nothing for a discobox that is already up, so
	// attaching to a running one still shows only "attach…" (ADR 0060).
	cmds := []tea.Cmd{m.listExecs(gen), m.listServices(gen), m.workspaceTick(gen), m.startForward(gen), m.watchProvisioning(sandbox), m.autoPush(m.boxRef(sandbox), gen)}
	if freshShell {
		cmds = append(cmds, m.newShell())
	}
	return tea.Batch(cmds...)
}

// sourceDeliveredMsg is the delivery a workspace ran before attaching, done.
type sourceDeliveredMsg struct {
	gen        int
	sandbox    console.Sandbox
	freshShell bool
	err        error
}

// deliverSource hands a discobox the source it is still waiting for, and only
// then attaches (ADR 26-09-24-005). Its create stopped after it parked and before the
// push was reported, so attaching alone would wait on a push nobody is making.
//
// Before the attach, not beside it: the attach wait gives up after a stall
// budget, and nothing it watches moves while this client pushes.
//
// It outlives a workspace left while it runs, on purpose. A push cut off
// part way leaves nothing better than one that finishes, and the discobox is
// still owed it; opening the workspace again joins the delivery still running
// rather than starting a second beside it (deliverBeforeAttach), and takes its
// answer, failure included.
func (m *Model) deliverSource(gen int, sandbox console.Sandbox, freshShell bool) tea.Cmd {
	feed, next := m.narrate()
	ctx, ds := m.ctx, m.ds.ForServer(sandbox.Server)
	deliver := func() tea.Msg {
		defer feed.Close()
		err := ds.DeliverSource(ctx, sandbox.ID, feed.Narration)
		return sourceDeliveredMsg{gen: gen, sandbox: sandbox, freshShell: freshShell, err: err}
	}
	return tea.Batch(deliver, next)
}

// sourceDelivered attaches once the source is in, or closes the workspace the
// way a primary that never came up closes it.
func (m *Model) sourceDelivered(msg sourceDeliveredMsg) tea.Cmd {
	if msg.gen != m.wsGen {
		return nil
	}
	if msg.err != nil {
		m.endNarration()
		m.busy = ""
		m.closeWorkspace()
		if m.attach != nil {
			return m.exit(fmt.Errorf("%s: %w", console.InteractAttach, msg.err))
		}
		m.layout()
		return tea.Batch(m.refresh(), m.report(true, "%s: %v", console.InteractAttach, msg.err))
	}
	return m.attachWorkspace(msg.gen, msg.sandbox, msg.freshShell)
}

// startForward opens the workspace's port forward. It is opened with the
// workspace rather than on a key: the header already lists what the discobox is
// serving, and a port you can see and cannot open is the gap this closes — so
// the reachable form of that list is what the screen shows from the moment it
// is up.
func (m *Model) startForward(gen int) tea.Cmd {
	ctx, ds, id := m.ctx, m.ds.ForServer(m.Box.Server), m.Box.ID
	return func() tea.Msg {
		forward, err := ds.Forward(ctx, id)
		return workspaceForwardMsg{gen: gen, forward: forward, err: err}
	}
}

// workspaceForward takes ownership of the forward, or reports why there is
// none. A workspace that was left while it was opening closes it here: nothing
// else holds it, and the local ports it bound are this window's.
func (m *Model) workspaceForward(msg workspaceForwardMsg) tea.Cmd {
	if msg.gen != m.wsGen {
		if msg.forward != nil {
			_ = msg.forward.Close()
		}
		return nil
	}
	if msg.err != nil {
		// The terminals are the screen; the ports are what rides on its
		// header. Losing them is worth saying once and is not worth closing
		// anything over.
		return status("ports are not being forwarded: %v", msg.err)
	}
	if msg.forward == nil {
		return nil
	}
	m.Forward = msg.forward
	return m.forwardEvents(msg.gen, msg.forward)
}

// forwardEvents waits for the next change to what is bound. Like the panes'
// connection events it re-arms itself, and like them it stops when the window's
// context ends or the channel closes.
func (m *Model) forwardEvents(gen int, forward console.Forward) tea.Cmd {
	ctx := m.ctx
	return listen(func() tea.Msg {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-forward.Events():
			if !ok {
				return nil
			}
			return workspaceForwardChangedMsg{gen: gen}
		}
	})
}

// forwardedPorts is what the header draws its arrows from: the sandbox port a
// local one stands in for. Empty while nothing is forwarded, which is also what
// every screen but the workspace sees.
func (m *Model) forwardedPorts() map[console.PortKey]int {
	if m.Forward == nil {
		return nil
	}
	return console.Forwarded(m.Forward.Bindings())
}

// listServices asks the server what the discobox's repository declares.
//
// It is a second poll beside the exec listing, which the tabs were otherwise
// drawn from alone. That listing only knows about services that are running —
// a service is an exec, and one that never started or that failed at boot has
// no exec to report — so a tab strip drawn from it alone is silent about
// exactly the service you need to hear about.
func (m *Model) listServices(gen int) tea.Cmd {
	ctx, ds, id := m.ctx, m.ds.ForServer(m.Box.Server), m.Box.ID
	return func() tea.Msg {
		services, err := ds.Services(ctx, id)
		return workspaceServicesMsg{gen: gen, services: services, err: err}
	}
}

// listExecs asks the server what is running in the workspace's discobox.
func (m *Model) listExecs(gen int) tea.Cmd {
	ctx, ds, id := m.ctx, m.ds.ForServer(m.Box.Server), m.Box.ID
	return func() tea.Msg {
		execs, err := ds.Execs(ctx, id)
		return workspaceExecsMsg{gen: gen, execs: execs, err: err}
	}
}

// workspaceTick schedules the next poll. The generation is the loop's own
// off-switch: detaching bumps it, and a tick from a workspace that is gone is
// dropped rather than answered.
func (m *Model) workspaceTick(gen int) tea.Cmd {
	return tick(console.WorkspacePollEvery, func(time.Time) tea.Msg {
		return workspaceTickMsg{gen: gen}
	})
}

// workspaceExecs opens what one answer from the exec listing asks the
// workspace to (console.Workspace.Reconcile): the primary on the first answer,
// whatever it says, and every session it does not hold yet — a tool put away,
// after the shells, because attaching to a discobox should show you the
// discobox.
//
// A listing that failed is not reported. The workspace is opened on a sandbox
// that may still be provisioning, where the listing is answered "not yet" and
// only the attach waits for the sandbox to become reachable (ADR 0039) — so
// the listing's error would be noise the user cannot act on, and the poll
// retries it seconds later. The attach is the reporter: it waits, and if it
// fails the workspace closes saying why.
func (m *Model) workspaceExecs(msg workspaceExecsMsg) tea.Cmd {
	if msg.gen != m.wsGen {
		return nil
	}
	execs := msg.execs
	if msg.err != nil {
		execs = nil
	}
	opens := m.Reconcile(execs, func(e console.Exec) bool {
		if e.IsTool() {
			return m.toolOpening[e.Tool]
		}
		return m.connecting[e.ID]
	})

	var cmds []tea.Cmd
	for _, exec := range opens.Tools {
		cmds = append(cmds, m.openToolExec(msg.gen, exec))
	}
	// Sized for the box they will be drawn in, counting the shells and tools
	// about to arrive as well as the ones already here: the full window when
	// there is one box, the halves when there are two. Only the right column
	// decides that — another terminal is a tab in the box the primary already
	// has. See columns.
	term, shells := m.columns(opens.NewShells + m.Shells.Len())
	if opens.Primary {
		m.connecting[console.ExecPrimary] = true
		cmds = append(cmds, m.openExec(msg.gen, console.Exec{ID: console.ExecPrimary, Primary: true}, term))
	}
	for _, exec := range opens.Sessions {
		m.connecting[exec.ID] = true
		width := shells
		if exec.Side() == console.SideTerminals {
			width = term
		}
		cmds = append(cmds, m.openExec(msg.gen, exec, width))
	}
	return tea.Batch(cmds...)
}

// workspaceServices applies one answer from the service listing
// (console.Workspace.ReconcileServices): the tabs whose service stopped on
// purpose, moved on to another run, or was deleted from the repository go,
// and the services with no tab get one. Each service has one writer — this —
// and its pane follows the service rather than any one run of it.
//
// A failed listing is not reported, for the reason the exec listing's is not:
// a workspace can be opened on a sandbox that is still coming up, where this is
// answered "not yet" and the poll retries seconds later.
func (m *Model) workspaceServices(msg workspaceServicesMsg) tea.Cmd {
	if msg.gen != m.wsGen || msg.err != nil {
		return nil
	}
	open, closing := m.ReconcileServices(msg.services, func(id string) bool { return m.connecting[id] })
	for _, p := range closing {
		m.closeTab(p)
	}
	if len(open) == 0 {
		return nil
	}
	cmds := make([]tea.Cmd, 0, len(open))
	for _, o := range open {
		m.connecting[console.ServiceTabID(o.Service.ID)] = true
		cmds = append(cmds, m.openService(msg.gen, o.Service, o.Order))
	}
	return tea.Batch(cmds...)
}

// openService connects one service's pane: its running process when it has
// one, and otherwise the card saying why it does not, with whatever its last
// run printed under it.
func (m *Model) openService(gen int, service console.Service, order int) tea.Cmd {
	term, _ := m.columns(m.Shells.Len())
	cols, rows := m.paneCells(term)
	ctx, ds, id := m.ctx, m.ds.ForServer(m.Box.Server), m.Box.ID
	return func() tea.Msg {
		// The transcript is read for both kinds of pane, and read first.
		//
		// A plain exec has no screen to repaint from, so attaching to a running
		// service starts at "now": without this the pane sits empty until the
		// service next says something, which for a server that has finished
		// booting is a long time. It is read before the attach rather than
		// after, so the seam between the two can only lose a moment of output
		// rather than repeat one — a repeated chunk reads as the service having
		// done something twice, which is worse than a gap.
		logs, err := ds.ServiceLogs(ctx, id, service.ID)
		if err != nil {
			// Not fatal to the pane: a live one still has its stream, and a
			// card without the transcript still says what happened, which is
			// more than an absent tab does.
			logs = nil
		}
		history := tailHistory(logs)
		if service.Live() && service.ExecID != "" {
			stream, err := ds.OpenExec(ctx, id, service.ExecID, cols, rows)
			if err != nil {
				return serviceTermMsg{gen: gen, service: service, order: order, err: err}
			}
			return serviceTermMsg{
				gen: gen, service: service, order: order,
				term: &historyTerminal{history: history, Terminal: stream},
			}
		}
		// A service that is not running has no stream, so the pane is handed
		// the one thing there is to say about it. Its output is part of that:
		// after a crash the output is the reason, and a reason you have to go
		// and ask for is one you find out about later than you needed to.
		return serviceTermMsg{gen: gen, service: service, order: order, term: newTextTerminal(serviceCard(service, history))}
	}
}

// serviceTermOpened starts drawing one service, or drops it and lets the poll
// try again. A service that cannot be opened is not worth reporting on the
// status line: the poll is on a two-second cadence and would say it again and
// again about a sandbox that is simply still coming up.
func (m *Model) serviceTermOpened(msg serviceTermMsg) tea.Cmd {
	if msg.gen != m.wsGen {
		if msg.term != nil {
			_ = msg.term.Close()
		}
		return nil
	}
	tab := console.ServiceTab(msg.service, msg.order)
	delete(m.connecting, tab.ExecID)
	if msg.err != nil || msg.term == nil {
		return nil
	}
	if existing := m.ByExec(tab.ExecID); existing != nil {
		_ = msg.term.Close()
		return nil
	}
	m.nextPaneID++
	// A service is never typed at, so its pane is read-only whether it is
	// drawing a live process or a card.
	p := &pane{
		Tab:     tab,
		id:      m.nextPaneID,
		term:    termpane.New(m.paneOptions(true)...),
		stream:  msg.term,
		sandbox: m.Box,
	}
	// A service never takes the keys on arrival. Nobody asked for it — it
	// appeared because the discobox is running it — and it is read-only, so
	// focus there is focus nowhere. The pane being worked in stays the pane
	// being worked in, by pointer rather than by index: the arrival may have
	// shifted it along the strip.
	working := m.Terminals.Visible()
	m.Terminals.Insert(p, m.column() == &m.Terminals)
	if working != nil {
		if i := m.Terminals.Index(working); i >= 0 {
			m.Terminals.Active = i
		}
	}
	if m.inPanes() {
		// The first pane to arrive may be a service — the primary is still
		// launching its harness, which takes seconds — and a window drawing a
		// pane with the keys still in the prompt takes them nowhere.
		m.focus = focusPane
		m.prompt.Blur()
		m.layout()
	}
	return tea.Batch(
		fromPane(p.id, p.term.Attach(msg.term)),
		fromPane(p.id, m.paneEvents(msg.term)),
	)
}

// openExec connects one workspace terminal. The pane is sized before it is
// opened: the size is what the far end is told, and a terminal that starts at
// the wrong size draws itself wrong before anything can correct it.
func (m *Model) openExec(gen int, exec console.Exec, width int) tea.Cmd {
	cols, rows := m.paneCells(width)
	ctx, ds, id := m.ctx, m.ds.ForServer(m.Box.Server), m.Box.ID
	return func() tea.Msg {
		term, err := ds.OpenExec(ctx, id, exec.ID, cols, rows)
		return workspaceTermMsg{gen: gen, exec: exec, term: term, err: err}
	}
}

// newShell creates a fresh shell in the discobox and opens it as a focused
// tab. The tab is keyed by the exec id the server hands back, which is what
// keeps the poll from opening a second pane onto it when the listing catches
// up.
func (m *Model) newShell() tea.Cmd {
	m.busy = "shell…"
	gen := m.wsGen
	_, shells := m.columns(m.Shells.Len() + 1)
	cols, rows := m.paneCells(shells)
	ctx, ds, id := m.ctx, m.ds.ForServer(m.Box.Server), m.Box.ID
	return func() tea.Msg {
		exec, term, err := ds.NewShell(ctx, id, cols, rows)
		return workspaceTermMsg{gen: gen, exec: exec, term: term, err: err, asked: console.InteractShell, focus: true}
	}
}

// newTerminal opens another of the discobox's own terminals — the same harness
// the primary runs — as a focused tab beside it. It is the left column's
// counterpart of newShell, and like it the tab is keyed by the exec id the
// server hands back, which is what keeps the poll from opening a second pane
// onto the same session.
//
// The left box does not change width for it: another terminal is a tab in the
// box the primary already has, not a third column.
func (m *Model) newTerminal() tea.Cmd {
	m.busy = "terminal…"
	gen := m.wsGen
	term, _ := m.columns(m.Shells.Len())
	cols, rows := m.paneCells(term)
	ctx, ds, id := m.ctx, m.ds.ForServer(m.Box.Server), m.Box.ID
	return func() tea.Msg {
		exec, term, err := ds.NewTerminal(ctx, id, cols, rows)
		return workspaceTermMsg{gen: gen, exec: exec, term: term, err: err, asked: console.InteractTerminal, focus: true}
	}
}

// workspaceTermOpened starts drawing one connected workspace session, or
// reports why there is none. A failed terminal or shell degrades to a report —
// the workspace is still a workspace without it, and the poll retries while
// the listing still says it is live — but a failed primary closes the screen:
// a workspace without its primary terminal is not one.
func (m *Model) workspaceTermOpened(msg workspaceTermMsg) tea.Cmd {
	if msg.gen != m.wsGen {
		// From a workspace that has since been left; its stream must not leak.
		if msg.term != nil {
			_ = msg.term.Close()
		}
		return nil
	}
	// A connected session is the end of the wait this screen narrates: the
	// discobox agent accepts the attach only once the terminal is launched and
	// installed, so there is nothing left to say about getting here.
	m.endNarration()
	m.busy = ""
	delete(m.connecting, msg.exec.ID)
	// What this pane is, in the words the list uses (console.SessionTab): the
	// primary is the attach, another harness session a terminal, and
	// everything else a shell. A create that failed carries no session record,
	// so there what was asked for is what is reported.
	tab := console.SessionTab(msg.exec, msg.asked)
	primary, action := tab.Primary, tab.Action
	if primary {
		m.wsOpening = false
	}

	if msg.err != nil {
		if primary {
			box := m.Box
			m.closeWorkspace()
			if m.attach != nil {
				// The window was opened to be this attach and the attach never
				// came up, so it ends the way the command would have: with the
				// failure, and nothing on screen behind it.
				return m.exit(fmt.Errorf("%s: %w", action, msg.err))
			}
			// The attach error is the transport's account and cannot say the
			// box failed on an image its pool cannot get. A listing read after
			// the failure can, so the offer asks for one of its own.
			m.layout()
			return tea.Batch(m.refresh(), m.checkImageOffer(box), m.report(true, "%s: %v", action, msg.err))
		}
		// Re-fit what did open: a primary sized for a split whose tabs never
		// arrived should take the width back.
		m.layout()
		return m.report(true, "%s: %v", action, msg.err)
	}

	if primary {
		m.recordPaletteVisit(m.Box)
	}
	if !primary {
		if existing := m.ByExec(msg.exec.ID); existing != nil {
			// The poll and a leader key race onto the same session; the tab
			// that arrived first is the tab.
			_ = msg.term.Close()
			if msg.focus {
				m.focusPane(existing)
			}
			return nil
		}
	}

	m.nextPaneID++
	p := &pane{
		Tab:     tab,
		id:      m.nextPaneID,
		stream:  msg.term,
		sandbox: m.Box,
	}
	// A service runs on pipes, so its pane draws and is navigated but is never
	// typed at: there is no stdin at the far end to reach (ADR 0070 §7).
	opts := m.paneOptions(tab.Service != "")
	if primary {
		// The primary is the box's agent, whose status the window speaks
		// for while this box is the one it is on (programstatus.go).
		opts = append(opts,
			termpane.WithOSC(programstatus.Code, p.observeProgramStatus),
			termpane.WithOSC(133, p.observePrompt),
		)
	}
	p.term = termpane.New(opts...)

	col := m.Column(tab.Side())
	at := col.Insert(p, m.column() == col)
	if primary {
		// The workspace lands on the primary: it is the session the screen is
		// a view onto, and the one whose ending ends it. A tab that arrived
		// while it was still launching — a service, a terminal opened
		// elsewhere — must not be left holding the keys. It sets the index
		// only, so a shell explicitly asked for still keeps them.
		col.Active = at
	}
	if msg.focus {
		m.onShells = col == &m.Shells
		col.Active = at
	}
	// Focus moves to the screen only once there is a screen: a tab that lands
	// before the primary keeps emulating off-screen until it does.
	if m.inPanes() {
		m.focus = focusPane
		m.prompt.Blur()
	}
	m.layout()
	return tea.Batch(
		fromPane(p.id, p.term.Attach(msg.term)),
		fromPane(p.id, m.paneEvents(msg.term)),
	)
}

// closeTab takes one pane off the screen: its stream is closed and it leaves
// its column. It is how a held pane is dismissed and how an errored one is
// dropped, and it kills nothing by itself: a session whose tab is closed here
// is still running.
//
// endPane is the one caller that ends a running session, and it does so in two
// parts — this, and the kill it sends after it. Everywhere else, closing a tab
// is closing a view.
func (m *Model) closeTab(p *pane) {
	col, i := m.paneColumn(p)
	if col == nil {
		return
	}
	col.Close(i)
	if m.dialog != nil && m.dialog.over == p {
		// The question was about this tab. See dialog.over.
		m.dialog = nil
	}
	if p == m.toolShown {
		// A tool that had the window gives it back to the workspace.
		m.toolShown = nil
	}
	if m.Shells.Len() == 0 {
		// Nothing left to share the window with, so there is nothing left to
		// maximize over either; the terminals take it back on their own.
		m.onShells = false
		m.maximized = false
	}
	m.layout()
}

// endablePane reports whether a pane's session is this window's to end, and
// when it is not, what to say instead.
//
// The primary is not: it is the session the workspace is a view onto, and
// ending it ends the screen — which is what detaching or quitting is for, and
// neither of those kills anything. A service is not either: it starts and
// stops on the discobox's schedule, and the keys that do that to it are the
// two the pane already offers. What is left is what you opened yourself — a
// shell, or a terminal beside the primary — which is exactly what nothing else
// on this screen can close.
func (m *Model) endablePane(p *pane) (why string, ok bool) {
	switch {
	case p != nil && p.issue != nil:
		// Not a session: ending it is closing the tab.
		return "", true
	case p == nil || p.ExecID == "":
		return "nothing to end here", false
	case p.Primary && !m.detaches():
		return "the primary terminal is the workspace — it keeps running when you switch away or quit", false
	case p.Primary:
		return "the primary terminal is the workspace — " + m.detachHint() + " leaves it, and the session keeps running", false
	case p.Service != "":
		return "a service is stopped rather than ended — " + m.leader() + " t stops it", false
	}
	return "", true
}

// endPane ends one session and takes its tab off the screen: the pane the key
// was typed into or the button was drawn on, and no other. The tabs beside it
// are other sessions, still running, still attached.
//
// It is also what a tool window's [x] does, to the tool with the window. The
// tab goes first and the kill is sent after it: what the press asked for is
// that this session is gone, and a tab that lingered until the server answered
// would leave the one thing the press was about still on screen. A pane whose session is already over has nothing
// to kill and is only dismissed.
func (m *Model) endPane(p *pane) tea.Cmd {
	if why, ok := m.endablePane(p); !ok {
		return status("%s", why)
	}
	if p.Exited {
		return m.dismissPane(p)
	}
	if p.issue != nil {
		m.closeTab(p)
		return nil
	}
	name, execID, gen := p.name(), p.ExecID, m.wsGen
	// The listing is a poll behind the act, so the session it still reports as
	// live is one this window has already closed the tab for. Remembering it
	// is what keeps the next tick from opening the tab straight back up — for
	// the rest of the workspace, since two listings can be in flight at once
	// and the older of them can land last.
	if m.Ending == nil {
		m.Ending = map[string]bool{}
	}
	m.Ending[execID] = true
	m.closeTab(p)
	ctx, ds, box := m.ctx, m.ds.ForServer(m.Box.Server), m.Box.ID
	// The report of the end comes from the same command as the kill, so it is
	// said once the server has done it, and a refusal cannot land ahead of it
	// and be written over.
	return func() tea.Msg {
		if err := ds.EndExec(ctx, box, execID); err != nil {
			return endExecFailedMsg{gen: gen, execID: execID, name: name, err: err}
		}
		return statusMsg{text: name + " ended"}
	}
}

// endExecFailedMsg is a kill the server refused: the pane is already off the
// screen — a tab, or a tool window — and the session it was drawing is still
// running.
type endExecFailedMsg struct {
	gen    int
	execID string
	name   string
	err    error
}

// endExecFailed puts a session the server would not kill back where the poll
// can find it — a shell's tab, or a tool's place in the strip. The view was
// closed on the press, which is right while the kill is in flight and wrong the
// moment it fails: the session is still running, and a workspace that hid it
// would be one where the only way back to it is to detach and attach again.
func (m *Model) endExecFailed(msg endExecFailedMsg) tea.Cmd {
	if msg.gen != m.wsGen {
		return nil
	}
	delete(m.Ending, msg.execID)
	return m.report(true, "could not end %s: %v — it is still running", msg.name, msg.err)
}

// closeWorkspace leaves the workspace: every stream is closed at once — the
// terminal and every tab — and every session keeps running. Bumping
// the generation is what ends the poll and orphans any open still in flight.
func (m *Model) closeWorkspace() {
	if m.paletteReveal != nil && (m.hasWorkspace() || m.wsOpening) && m.paletteReveal.target == m.boxRef(m.Box) {
		m.restorePaletteView()
	}
	m.wsOpening = false
	m.listFront, m.treeExpanded = false, false
	m.endLiveScreen()
	m.Release()
	// The push loop ends with the workspace (pushLoop), and what it was
	// holding against it goes too.
	m.workspace = workspace{Box: m.Box}
	m.leavePanes()
}

// endLiveScreen ends what only the workspace on screen runs, whether it is
// being closed or parked: its generation, which ends its polls and orphans its
// opens, and the audit and wait drawn over it.
func (m *Model) endLiveScreen() {
	m.wsGen++
	m.connecting = nil
	m.toolOpening = nil
	m.closeAudit()
	// The busy line is the screen's — an attach, a delivery, a provisioning
	// watch — unless a create has it: that is the window's, and goes on
	// reporting into whatever workspace is shown next (watchCreate).
	if !m.createNarrating {
		m.endNarration()
		m.busy = ""
	}
	if m.dialog != nil && m.dialog.over != nil {
		// Every pane goes with the workspace, and a question about one of
		// them with it. See dialog.over.
		m.dialog = nil
	}
}

// paneOrdinal is where the focused pane sits in that strip, or -1 when nothing
// on the workspace has the keys.
func (m *Model) paneOrdinal() int {
	if m.column().Visible() == nil {
		return -1
	}
	if m.onShells {
		return m.Terminals.Len() + m.Shells.Active
	}
	return m.Terminals.Active
}

// focusOrdinal puts the keys on the pane wearing that number, clamped to the
// strip's ends.
func (m *Model) focusOrdinal(n int) {
	n = min(max(n, 0), max(m.Terminals.Len()+m.Shells.Len()-1, 0))
	if n < m.Terminals.Len() {
		m.onShells, m.Terminals.Active = false, n
		return
	}
	m.onShells, m.Shells.Active = true, n-m.Terminals.Len()
}

// movePane shifts focus along that strip. It stops at the ends rather than
// wrapping — the screen has a left and a right, and a focus that jumps the
// long way round is one you have to look for.
func (m *Model) movePane(delta int) tea.Cmd {
	at := m.paneOrdinal()
	if at < 0 {
		return nil
	}
	// Off the left edge is the tree.
	if at+delta < 0 && m.treeAvailable() {
		m.treeHidden = false
		m.focusTree()
		return nil
	}
	m.focusOrdinal(at + delta)
	return nil
}

// jumpPane focuses a pane by the number its label wears: 0 is the primary
// terminal, the first tab that is not a service, and the rest are counted
// across the screen from it. A number with no pane under it is answered rather
// than swallowed — a jump that lands nowhere should say so.
func (m *Model) jumpPane(n int) tea.Cmd {
	numbered := m.Numbered()
	if n < 0 || n >= len(numbered) {
		return status("no pane %d", n)
	}
	m.focusPane(numbered[n])
	return nil
}

// jumpService focuses a service by the number its label wears, from one: the
// leader's S chord, so S1 is the first service the repository declares.
//
// It is a chord rather than more digits because the digits are spoken for and
// because a service is a different kind of thing to jump to — read-only, and
// running whether you look at it or not. A number with no service under it is
// answered the same way a pane number with nothing under it is.
func (m *Model) jumpService(n int) tea.Cmd {
	services := m.ServiceTabs()
	if n < 1 || n > len(services) {
		return status("no service %s%d", paneServicesKey, n)
	}
	m.focusPane(services[n-1])
	return nil
}

// tabbedEdge is a column's top border with its tab strip laid into it — a
// titledEdge with several titles: `╭─[ 1 zsh ]─[ 2 claude ]───╮`. The border is
// a line the eye already follows, so the strip costs no row, and both grids
// come out the same height.
//
// The visible tab is lit and never clipped; when the strip outgrows the line,
// a window of tabs around it is shown with an ellipsis at the clipped end.
//
// left is the screen column the box starts at, so a click on a label can be
// routed back to the tab that drew it whichever side it is on, and control is
// the box's already-drawn buttons, which share the line.
func (m *Model) tabbedEdge(col *column, shells bool, left int, control string, edge lipgloss.Style, inner int) string {
	rule := func(n int) string { return strings.Repeat("─", max(n, 0)) }
	// The box's own buttons share the line, at the far end, and there may be
	// more than one of them — which is why what they cost is measured rather
	// than assumed. The strip gets what they leave. See columnControls.
	reserve := 0
	if control != "" {
		reserve = lipgloss.Width(control) + 1
	}
	tail := func(cells int) string {
		out := edge.Render(rule(inner - cells - reserve))
		if control != "" {
			out += control + edge.Render("─")
		}
		return out + edge.Render("╮")
	}
	// The strip is a click target as well as a label row, so where each tab
	// lands is recorded as it is drawn — the drawing loop is the one place
	// that knows. Columns are box-relative: the corner is cell zero.
	side := console.SideTerminals
	if shells {
		side = console.SideShells
	}
	base := m.TabBase(side)
	labels := make([]string, col.Len())
	widths := make([]int, col.Len())
	for i := range col.Tabs {
		labels[i] = tabLabel(col, i, base, m.paneName(col.Tabs[i]))
		widths[i] = len([]rune(labels[i])) + 4 // brackets and their padding
	}

	active := min(max(col.Active, 0), len(labels)-1)
	avail := inner - 2 - reserve // a cell of rule survives at each end
	if widths[active] > avail {
		// Even alone the visible tab does not fit: shorten its label as a last
		// resort, and below any readable size give the row back to the rule.
		keep := avail - 4
		if keep < 1 {
			return edge.Render("╭") + tail(0)
		}
		labels[active] = string([]rune(labels[active])[:keep])
		widths[active] = keep + 4
	}

	// The window grows outward from the visible tab while there is room, a
	// cell of rule between neighbors.
	lo, hi, used := active, active, widths[active]
	for {
		grew := false
		if lo > 0 && used+1+widths[lo-1] <= avail {
			lo--
			used += 1 + widths[lo]
			grew = true
		}
		if hi < len(labels)-1 && used+1+widths[hi+1] <= avail {
			hi++
			used += 1 + widths[hi]
			grew = true
		}
		if !grew {
			break
		}
	}

	var out strings.Builder
	cells := 0
	out.WriteString(edge.Render("╭"))
	if lo > 0 {
		out.WriteString(edge.Render("…"))
	} else {
		out.WriteString(edge.Render("─"))
	}
	cells++
	for i := lo; i <= hi; i++ {
		if i > lo {
			out.WriteString(edge.Render("─"))
			cells++
		}
		st := m.st.dimText
		if i == active {
			st = m.st.headerBar
		}
		out.WriteString(edge.Render("["))
		out.WriteString(st.Render(" " + labels[i] + " "))
		out.WriteString(edge.Render("]"))
		m.tabSpans = append(m.tabSpans, tabSpan{
			shells: shells,
			index:  i,
			start:  left + 1 + cells,
			end:    left + cells + widths[i],
		})
		cells += widths[i]
	}
	if hi < len(labels)-1 {
		out.WriteString(edge.Render("…"))
		cells++
	}
	out.WriteString(tail(cells))
	return out.String()
}
