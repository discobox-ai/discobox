package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/discobox-ai/discobox/cli/internal/keys"
	"github.com/discobox-ai/discobox/termpane"
)

// A pane is a discobox's terminal drawn in the window rather than by handing
// the real terminal over.
//
// The screen is the discobox's workspace: its primary terminal on the left,
// and every other live session as a tab on the right — one visible at a time.
// The workspace mirrors the server rather than remembering what was opened
// here: attaching draws every session the discobox has, and a session started
// from anywhere appears as a tab on its own. See workspace.go.
//
// Over them, a command that runs and finishes — apply — takes the
// whole screen for as long as it runs. The terminals underneath are untouched:
// still connected, still running, still where they were when it comes back.
//
// Every command the list offers is reachable from here behind the leader, on
// the key it has there, and acts on the one discobox the screen is showing.

const (
	paneMouseKey = "m"
	paneLeftKey  = "h"
	paneRightKey = "l"
	// paneDetachAlt is the detach key behind the leader, for a pane whose
	// application needs Ctrl-C more than the window does. It is the d screen,
	// tmux, and a plain `discobox attach` all detach on. The leader also carries
	// the list's own keys, but none of them is d anymore — it was diff, until
	// diff left the CLI.
	paneDetachAlt = "d"
	// paneTerminalKey opens another of the discobox's own terminals, beside
	// the primary on the left. It is c because that is what screen and tmux
	// create a window on, and this leader is screen's; t, the obvious letter,
	// is stop — a command the list already has on it, and the workspace keeps
	// the list's key map.
	paneTerminalKey = "c"
	// paneZoomKey maximizes the focused column over the other and restores it,
	// the same toggle the boxes' own [+]/[-] button is. It is z because that is
	// what tmux zooms a pane on, and because a workspace key nothing but a
	// mouse can reach is one half the users cannot reach at all.
	paneZoomKey = "z"
	// paneQuitKey quits the whole window from behind the leader. Ctrl-C quits
	// everywhere else but belongs to the application inside a pane, so the
	// leader carries the exit here — beside d, which only detaches.
	paneQuitKey = "q"
	// paneInterruptKey is the application's everywhere, and never the window's.
	// The one exception is a pane whose command has finished, where there is
	// nothing left to interrupt and it means done like the rest of them.
	paneInterruptKey = keys.Interrupt
	// paneServicesKey leads everything to do with the discobox's services. It
	// is a chord rather than a key: S then a digit jumps to the service
	// wearing that number, S1 being the first one the repository declares, and
	// S0 opens the menu — everything declared under `.discobox/services`,
	// including what is not running, and the three verbs.
	//
	// The services have a numbering of their own because the digits are the
	// terminals' and the shells', and because a service comes and goes on the
	// discobox's schedule rather than yours: sharing one set of numbers would
	// mean a service starting renumbers the shell you were reaching for. The
	// menu is on the chord's zero for the same reason a service tab cannot
	// replace it — the services with no tab are exactly the ones a key has to
	// reach, and naming one is not something the pane in front of you can do.
	//
	// S rather than s, which is a shell; the workspace carries the list's key
	// map whole, and none of the verbs on it (t, T) could be reused here
	// without meaning the discobox on one pane and a service on another.
	paneServicesKey = "S"
	// paneServicesMenuKey is the digit that opens the services menu behind the
	// S chord. Zero because the services themselves count from one, so the
	// menu is the one number left over — and because it is where a hand
	// already reaching for S1 can find it.
	paneServicesMenuKey = "0"
)

// pane is one terminal in the window.
type pane struct {
	// id addresses this pane in the messages its own commands produce. Without
	// it a pane closing would be reported as "a pane closed", and with several
	// of them the window would have to guess which — and a guess that lands on
	// the wrong one closes a session nobody asked to end.
	id int

	term    *termpane.Model
	stream  Terminal
	action  Interaction
	sandbox Sandbox
	status  string

	// execID keys the pane to the server session it is drawing, which is what
	// lets the workspace's poll tell a session it already shows from one it
	// has to open. Empty for the overlay, which is no session at all.
	execID string
	// title is what to call the tab until the application names itself.
	title string
	// created is when the session was created, which is the tab order.
	created time.Time

	// primary marks the discobox's primary terminal, which is the one pane on
	// the screen whose ending ends the workspace. It is the first tab of the
	// terminals column and always wears 0.
	primary bool

	// service is the declared service this pane is drawing, empty for every
	// other pane. It is what the lifecycle keys act on, and its presence is
	// what makes the pane read-only: there is nothing at the far end reading
	// stdin.
	service string
	// serviceName is the service's display name, kept beside its id so a verb
	// run from this pane can report on it without going back to the listing.
	serviceName string
	// serviceOrder is where the service sits in the repository's declaration
	// order, which is what orders the service tabs. It is kept on the pane
	// because insert compares an arriving session against the ones already in
	// the strip, and a pane described without it would sort as though it were
	// declared first.
	serviceOrder int
	// unread marks a service whose output has moved on since it was last on
	// screen. A service is the one pane you deliberately look away from — that
	// is what running it in the background means — so its tab is where it has
	// to say that something happened. seenOutput is the reading it was last
	// seen at.
	unread     bool
	seenOutput uint64
	// serviceRun identifies the run this pane was opened on, so the poll can
	// tell a pane that is still looking at what the server reports from one
	// whose service has since restarted, stopped or been fixed. See
	// Service.runKey.
	serviceRun string

	// tool is the tool this pane is running, and empty for every pane that is
	// not one. A tool pane is not in the strip and wears no number: it is a
	// window over the workspace, put away rather than moved between. See
	// tools.go.
	tool string

	// exited is set when what was running in the pane finished and the pane
	// was kept anyway, so its last screen can be read.
	exited bool
	// failed is set with it when what finished did so badly. It is what the
	// banner and the hints are colored by: the same screen either way, and a
	// word that is not the same word.
	failed bool

	// notice is guidance drawn in the window header, outside this pane.
	notice string
	// configure identifies a configure command whose completion advances the
	// harness workflow rather than leaving a finished report open.
	configure *configurePane
}

type configurePane struct {
	harness    Harness
	andDefault *Harness
	resume     *RunRequest
}

// name is what to call a pane: what the application inside it says it is,
// else what was asked for when it was opened.
func (p *pane) name() string {
	// A configure command may set an application title identical to a normal
	// session. Keep the pane's purpose visible throughout this special flow.
	if p.configure != nil {
		return p.title
	}
	if title := strings.TrimSpace(p.term.Title()); title != "" {
		return title
	}
	if p.title != "" {
		return p.title
	}
	return string(p.action)
}

// detachHint is how to get out of the workspace, as the key lists spell it.
//
// It is the same in every pane, and Ctrl-C is not it. A harness attach used to
// take Ctrl-C as "back out of this", which is a fine reading of the key right
// up until it is wrong: someone who types it to stop an agent and gets a
// detached session instead has not stopped anything, and has no way to tell
// from the screen they are looking at. The key belongs to whatever is running,
// in a shell and in a harness alike, and the window asks for the leader.
func (m *Model) detachHint() string { return m.leader() + " " + paneDetachAlt }

// leader is the pane's prefix key, as a Bubble Tea key name. It is the same key
// a plain `discobox attach` detaches behind; see [keys].
func (m *Model) leader() string {
	if m.leaderKey == "" {
		return keys.DefaultLeader
	}
	return m.leaderKey
}

// paneMouseHint is the mouse toggle, as the key lists spell it.
func (m *Model) paneMouseHint() string { return m.leader() + " " + paneMouseKey }

// toggleMouseMsg is the leader plus m: take the mouse from the box, or give
// it back.
//
// While nothing in the box has asked for the mouse there is nothing to take —
// drag selects and copies on its own. The toggle is for the programs that did
// ask, vim and htop and their kind, when you would rather copy a stack trace
// than click on it. Taken, the box sees no mouse at all.
type toggleMouseMsg struct{}

// paneActionMsg is the leader plus one of the list's own action keys: the same
// command, on the discobox the screen is showing.
type paneActionMsg struct{ key string }

// movePaneMsg is the leader plus h or l: move focus that way, along the strip
// the screen is — the terminals, then the shells.
type movePaneMsg struct{ delta int }

// jumpPaneMsg is the leader plus a digit: focus the pane wearing that number —
// 0 the primary, and the rest counted across the screen from it.
type jumpPaneMsg struct{ n int }

// newTerminalMsg is the leader plus c: another of the discobox's terminals,
// opened beside the primary and focused.
type newTerminalMsg struct{}

// openServicesMsg is the leader plus S0: open the discobox's declared services.
type openServicesMsg struct{}

// jumpServiceMsg is the leader plus S and a digit: focus the service pane
// wearing that number, counted from one in the order the repository declares
// them.
type jumpServiceMsg struct{ n int }

// paneServiceVerbs are the list's own lifecycle keys, re-read as the focused
// service's rather than the discobox's. They are the list's keys and not new
// ones so that stopping the thing in front of you is the key you already know.
var paneServiceVerbs = map[string]ServiceVerb{
	"t": ServiceStop,
	"T": ServiceStart,
}

// zoomPaneMsg is the leader plus z: give the focused column the whole window,
// or give the window back to the split.
type zoomPaneMsg struct{}

// quitPaneMsg is the leader plus q: quit the whole window, every session left
// running. It is the same exit Ctrl-C is everywhere else, spelled behind the
// leader because inside a pane Ctrl-C belongs to the application.
type quitPaneMsg struct{}

// paneOpenedMsg carries a connected overlay terminal back to the model, or the
// reason there is none. The workspace's own terminals arrive as
// workspaceTermMsg instead; see workspace.go.
//
// A failure comes back the same way a success does rather than as a bare
// status: the open left the window busy, and only the handler that was waiting
// for the answer knows it has stopped waiting.
type paneOpenedMsg struct {
	action  Interaction
	sandbox Sandbox
	term    Terminal
	err     error
}

type configurePaneOpenedMsg struct {
	harness    Harness
	andDefault *Harness
	resume     *RunRequest
	term       Terminal
	err        error
}

// paneEventMsg is a connection state change under an open pane.
type paneEventMsg struct{ event TerminalEvent }

// paneMsg is anything a pane's own commands produced, addressed to the pane it
// came from. See pane.id.
type paneMsg struct {
	id  int
	msg tea.Msg
}

// fromPane tags a pane's commands so what comes back says where it came from.
// A batch is tagged through to its parts, since each half arrives separately.
func fromPane(id int, cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	return func() tea.Msg {
		msg := cmd()
		if msg == nil {
			return nil
		}
		if batch, ok := msg.(tea.BatchMsg); ok {
			tagged := make([]tea.Cmd, 0, len(batch))
			for _, sub := range batch {
				tagged = append(tagged, fromPane(id, sub))
			}
			return tea.BatchMsg(tagged)
		}
		return paneMsg{id: id, msg: msg}
	}
}

// paneByID finds the pane a message came from: the overlay, the terminal, or
// one of the shell tabs.
func (m *Model) paneByID(id int) *pane {
	if m.overlay != nil && m.overlay.id == id {
		return m.overlay
	}
	for _, p := range m.allPanes() {
		if p.id == id {
			return p
		}
	}
	return nil
}

// allPanes is every pane this window is drawing or holding attached: the strip
// the workspace is, and the tools beside it, showing or put away. It is what
// anything addressing a pane by identity walks — never what counts positions,
// which is panes().
func (m *Model) allPanes() []*pane {
	out := append([]*pane{}, m.terminals.panes...)
	out = append(out, m.shells.panes...)
	return append(out, m.tools.panes...)
}

// inPanes reports whether the window is showing terminals rather than the list.
func (m *Model) inPanes() bool {
	return m.terminals.len() > 0 || m.overlay != nil || m.toolOpen
}

// screenPane is the pane with the whole window, or nil when the workspace
// itself is on screen: the command running over it, or the tool showing over
// it. The two differ in what leaving them means — a command is over when it is
// over, a tool is put away — and in nothing else the screen has to know.
func (m *Model) screenPane() *pane {
	if m.overlay != nil {
		return m.overlay
	}
	return m.showingTool()
}

// hasScreen reports whether a pane is the one with the whole window.
func (m *Model) hasScreen(p *pane) bool { return p != nil && p == m.screenPane() }

// focusedPane is the pane every key goes to: the overlay while one is up, since
// it has the screen, and otherwise the terminal or the visible shell tab.
func (m *Model) focusedPane() *pane {
	if p := m.screenPane(); p != nil {
		return p
	}
	return m.column().visible()
}

// column is the side of the workspace the keys are in: the shells while the
// focus is on them, the terminals otherwise.
func (m *Model) column() *column {
	if m.onShells {
		return &m.shells
	}
	return &m.terminals
}

// paneColumn is the column a pane is in and where it sits in it, or nil and -1
// for one that is in neither — the overlay, or a pane already closed.
func (m *Model) paneColumn(p *pane) (*column, int) {
	if i := m.terminals.index(p); i >= 0 {
		return &m.terminals, i
	}
	if i := m.shells.index(p); i >= 0 {
		return &m.shells, i
	}
	return nil, -1
}

// primary is the discobox's primary terminal, or nil before it has arrived. It
// is the head of the terminals in the left column — every other session is
// created later, and the primary is attached under a virtual id with no
// creation time at all, so it sorts first whatever order the attaches land in
// — but not the head of the column, which the services take. See execBefore.
func (m *Model) primary() *pane {
	for _, p := range m.terminals.panes {
		if p.primary {
			return p
		}
	}
	return nil
}

// currentBox is the discobox the workspace is showing, as the list last saw
// it. The workspace was opened on a snapshot, and what a command may do to it
// — a diffstat that has since arrived, a state that has since changed — moves
// on without it.
//
// It is what the keys dispatch against and what the header reads, so the two
// answer from the same listing: a screen that offers "apply" is a screen whose
// header already said there was something to apply.
func (m *Model) currentBox() Sandbox {
	for _, s := range m.list.all {
		if s.ID == m.paneBox.ID {
			return s
		}
	}
	return m.paneBox
}

// openOverlay runs a command in a screen of its own, over whatever it was
// started from: the workspace when one is open, and the list when the command
// is all that is on screen.
//
// It has the screen because it is the thing you asked for and it is over when
// it is over — a report you opened to read is not something to read in half a
// window beside a harness scrolling past. What is underneath keeps running,
// unresized and unredrawn, and is exactly where it was when the command exits.
func (m *Model) openOverlay(act Interaction, sandbox Sandbox) tea.Cmd {
	if m.overlay != nil {
		return status("%s is still up — close it first", m.overlay.action)
	}
	// The discobox the screen is about, which the banner reads and the leader
	// keys dispatch against. Over a workspace it is the one already open, which
	// is where this box came from; from the list it is the row the cursor is
	// on.
	m.paneBox = sandbox
	// A terminal wants the whole screen, so opening one opens the window out
	// even when nothing has asked for the list yet. The overlay is sized
	// before it is opened: the size is what the far end is told, and a
	// terminal that starts at the wrong size draws itself wrong before
	// anything can correct it.
	m.expanded = true
	m.busy = string(act) + "…"
	cols, rows := m.paneCells(m.width)
	ctx, ds, id := m.ctx, m.ds, sandbox.ID
	return func() tea.Msg {
		term, err := ds.Open(ctx, act, id, cols, rows)
		return paneOpenedMsg{action: act, sandbox: sandbox, term: term, err: err}
	}
}

// paneOpened starts drawing a connected overlay terminal, or reports why there
// is none.
func (m *Model) paneOpened(msg paneOpenedMsg) tea.Cmd {
	m.busy = ""
	if msg.err != nil {
		// Nothing was opened, so the screen is the workspace it already was —
		// which is where the report has to be legible. See statusLine.
		return m.report(true, "%s: %v", msg.action, msg.err)
	}
	m.nextPaneID++
	p := &pane{
		id:      m.nextPaneID,
		term:    termpane.New(m.paneOptions(paneOverlay, false)...),
		stream:  msg.term,
		action:  msg.action,
		sandbox: msg.sandbox,
	}
	m.overlay = p
	m.focus = focusPane
	m.prompt.Blur()
	m.layout()
	return tea.Batch(
		fromPane(p.id, p.term.Attach(msg.term)),
		fromPane(p.id, m.paneEvents(msg.term)),
	)
}

func (m *Model) configurePaneOpened(msg configurePaneOpenedMsg) tea.Cmd {
	m.busy = ""
	if msg.err != nil {
		return m.report(true, "configure %s: %v", msg.harness.displayName(), msg.err)
	}
	m.nextPaneID++
	p := &pane{
		id: m.nextPaneID, term: termpane.New(m.paneOptions(paneOverlay, false)...),
		stream: msg.term, action: Interaction("configure"),
		title:     "Harness configuration · " + msg.harness.displayName(),
		notice:    msg.harness.ConfigReminder,
		configure: &configurePane{harness: msg.harness, andDefault: msg.andDefault, resume: msg.resume},
	}
	m.overlay = p
	m.focus = focusPane
	m.prompt.Blur()
	m.layout()
	return tea.Batch(fromPane(p.id, p.term.Attach(msg.term)), fromPane(p.id, m.paneEvents(msg.term)))
}

// paneKind is what a pane is, as far as the keys it keeps are concerned.
type paneKind int

const (
	// paneWorkspace is a terminal or a shell in the strip.
	paneWorkspace paneKind = iota
	// paneOverlay is a command of this CLI's own, running to completion over
	// whatever it was started from.
	paneOverlay
	// paneTool is a tool session with the window; see tools.go.
	paneTool
)

// paneOptions are the keys a pane keeps for the window rather than passing on.
//
// A workspace terminal carries the whole key map, because every one of them is
// about the discobox on screen and the screen is where you are. The overlay
// carries only its way out and the mouse: it is one command running to
// completion, and a key that opened something else over it would be a key that
// lost it. A tool carries those plus its own two — the picker, so one tool can
// be swapped for another without going back first, and the one that ends it,
// which nothing else on the screen can do.
//
// readOnly is the other axis: a pane onto something with no input side — a
// service — is drawn and navigated but never typed at, whichever kind it is.
func (m *Model) paneOptions(kind paneKind, readOnly bool) []termpane.Option {
	opts := []termpane.Option{
		// No bare detach key: nothing the window reserves stands between a
		// program and its own interrupt. The way out is behind the leader, and
		// it is the same one in every pane. See detachHint.
		termpane.WithPrefix(m.leader(), ""),
		termpane.WithPrefixBinding(paneMouseKey, toggleMouseMsg{}),
		termpane.WithPrefixBinding(paneDetachAlt, termpane.DetachMsg{}),
		termpane.WithPrefixBinding(paneQuitKey, quitPaneMsg{}),
		// A URL printed in here was printed on the other side of the forward,
		// where the port it names is the sandbox's own. See forwardedURL.
		termpane.WithLinkRewrite(m.forwardedURL),
	}
	if readOnly {
		opts = append(opts, termpane.WithReadOnly())
	}
	switch kind {
	case paneOverlay:
		return opts
	case paneTool:
		return append(opts,
			termpane.WithPrefixBinding(toolsKey, openToolsMsg{}),
			termpane.WithPrefixBinding(toolCloseKey, closeToolMsg{}),
		)
	}
	opts = append(opts,
		// Moving between the terminal and the tabs is something you do in
		// runs, so it holds the leader open while Ctrl is down: leader, then
		// Ctrl-← Ctrl-← walks across without pressing the leader again. The
		// arrows and the letters are the same binding, so neither has to be
		// learned.
		termpane.WithRepeatingPrefixBinding(paneLeftKey, movePaneMsg{delta: -1}),
		termpane.WithRepeatingPrefixBinding(paneRightKey, movePaneMsg{delta: 1}),
		termpane.WithRepeatingPrefixBinding("left", movePaneMsg{delta: -1}),
		termpane.WithRepeatingPrefixBinding("right", movePaneMsg{delta: 1}),
		termpane.WithPrefixBinding(paneZoomKey, zoomPaneMsg{}),
		termpane.WithPrefixBinding(paneTerminalKey, newTerminalMsg{}),
	)
	// The digits jump straight to a pane by the number its label wears, the
	// way tmux selects windows: 0 is the terminal, 1 through 9 the tabs.
	for n := 0; n <= 9; n++ {
		opts = append(opts, termpane.WithPrefixBinding(strconv.Itoa(n), jumpPaneMsg{n: n}))
	}
	// The tools picker, which is where vscode now lives: it used to be bound
	// here on its own key, and a second way to open one of three tools is one
	// key to remember for no more reach. See tools.go.
	opts = append(opts, termpane.WithPrefixBinding(toolsKey, openToolsMsg{}))
	// The services have the same alphabet one keystroke further in: S1 through
	// S9 are their own tabs, and S0 is the menu that reaches the ones with no
	// tab at all. See paneServicesKey.
	services := map[string]tea.Msg{paneServicesMenuKey: openServicesMsg{}}
	for n := 1; n <= 9; n++ {
		services[strconv.Itoa(n)] = jumpServiceMsg{n: n}
	}
	opts = append(opts, termpane.WithPrefixChord(paneServicesKey, services))
	// The credential request the header is telling you about. It is bound here
	// rather than only on the list because the workspace is where the banner
	// says there is one, and an affordance that names a key the pane swallows
	// is not one.
	opts = append(opts, termpane.WithPrefixBinding(credentialsLeaderKey, openCredentialsMsg{}))
	// Every command the list offers, on the key it has there. One key map for
	// the two screens is the point: the workspace is a discobox with the
	// cursor on it, and what you can do to it does not change with where you
	// are looking at it from.
	for key := range interactions {
		opts = append(opts, termpane.WithPrefixBinding(key, paneActionMsg{key: key}))
	}
	for key := range verbs {
		opts = append(opts, termpane.WithPrefixBinding(key, paneActionMsg{key: key}))
	}
	return opts
}

// paneEvents waits for the next connection state change. A reconnect never
// appears in the output — the stream simply carries on — so it is reported here
// or not at all.
func (m *Model) paneEvents(term Terminal) tea.Cmd {
	ctx := m.ctx
	return func() tea.Msg {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-term.Events():
			if !ok {
				return nil
			}
			return paneEventMsg{event: event}
		}
	}
}

// closeOverlay ends the command that had the screen and gives it back to what
// was under it: the workspace, which has been running the whole time, or the
// list it was opened from.
func (m *Model) closeOverlay() {
	if m.overlay == nil {
		return
	}
	_ = m.overlay.term.Close()
	m.overlay = nil
	if m.terminals.len() > 0 {
		m.layout()
		return
	}
	// It was opened from the list rather than over anything, so there is
	// nothing to come back to.
	m.leavePanes()
}

// leavePanes returns focus to where the screen was opened from.
func (m *Model) leavePanes() {
	if m.focus != focusPane {
		return
	}
	// Back to the list, which is where the screen was opened from, with the
	// cursor still on the discobox it was opened on. Landing in the prompt would
	// mean two presses to act on the one you were just looking at.
	m.prompt.Blur()
	m.focus = focusList
	if len(m.list.rows()) == 0 {
		m.backToPrompt()
	}
}

// updatePane routes a key or a mouse event to the pane with focus. Everything a
// pane's own commands produced comes back addressed, and goes through
// updatePaneMsg instead.
func (m *Model) updatePane(msg tea.Msg) tea.Cmd {
	p := m.focusedPane()
	if p == nil {
		return nil
	}
	if tagged, ok := msg.(paneMsg); ok {
		return m.updatePaneMsg(tagged)
	}
	// The mouse routes by position rather than focus — a pointer names the
	// pane it is over — and works on finished panes too, where the last
	// screen still selects and scrolls.
	if mouse, ok := msg.(tea.MouseMsg); ok {
		return m.routeMouse(mouse)
	}
	// A chrome selection takes the copy chords the way a pane's own would;
	// with none showing, every key is the pane's as usual.
	if key, ok := msg.(tea.KeyPressMsg); ok {
		if cmd, taken := m.copyChord(key); taken {
			return cmd
		}
	}
	// A pane whose command has finished is a screen to read, not a terminal to
	// type at. Its keys are the reader's: the arrows walk back through output
	// longer than the pane, and the ones that mean "done" take it away.
	if p.exited {
		if key, ok := msg.(tea.KeyPressMsg); ok {
			return m.readFinished(p, key)
		}
		return nil
	}
	term, cmd := p.term.Update(msg)
	p.term = term
	return fromPane(p.id, cmd)
}

// updatePaneMsg handles what one pane's commands produced, against that pane.
//
// Which pane it came from is the whole point: a pane that has just been closed
// still has a read in flight, and its parting message must not be taken for the
// survivor's.
func (m *Model) updatePaneMsg(tagged paneMsg) tea.Cmd {
	p := m.paneByID(tagged.id)
	if p == nil {
		// Its pane is already gone; nothing left to tell.
		return nil
	}
	switch msg := tagged.msg.(type) {
	case toggleMouseMsg:
		m.mouseSeized = !m.mouseSeized
		if m.mouseSeized {
			return status("mouse taken from the box — drag selects; %s gives it back", m.paneMouseHint())
		}
		return status("mouse handed back to the box")

	case termpane.CopyMsg:
		return m.copyText(msg.Text)

	case paneActionMsg:
		// On a service, stop and start are the service's. The pane you are
		// looking at is what a verb applies to — the same rule that makes
		// these keys mean the discobox on screen rather than the row the list
		// left its cursor on — and a service is the one kind of pane that has
		// a lifecycle of its own to apply them to.
		//
		// Only those two are re-scoped. Upgrade, archive and the rest have no
		// meaning for a service, so they keep meaning the discobox, and
		// restart — which the list has no verb for — stays on the services
		// menu rather than taking a key that means something else everywhere.
		if p.service != "" {
			if verb, ok := paneServiceVerbs[msg.key]; ok {
				return m.runServiceVerb(verb, Service{ID: p.service, Name: p.serviceName})
			}
		}
		// The list's dispatcher, on the one discobox this screen is showing:
		// the same enabled checks, the same confirmations, the same reports.
		return m.actOn(msg.key, []Sandbox{m.currentBox()})

	case movePaneMsg:
		if m.hasScreen(p) {
			return nil
		}
		// A run of these is one chord: the pane it fired in is left armed while
		// Ctrl is down, and moving focus has to carry that to the pane it moved
		// to, or the sequence would end on its own first step.
		armed := p.term.PrefixArmed()
		p.term.SetPrefixArmed(false)
		cmd := m.movePane(msg.delta)
		if armed {
			if next := m.focusedPane(); next != nil {
				next.term.SetPrefixArmed(true)
			}
		}
		return cmd

	case jumpPaneMsg:
		if m.hasScreen(p) {
			return nil
		}
		return m.jumpPane(msg.n)

	case jumpServiceMsg:
		if p == m.overlay {
			return nil
		}
		return m.jumpService(msg.n)

	case newTerminalMsg:
		if m.hasScreen(p) {
			return nil
		}
		return m.newTerminal()

	case openServicesMsg:
		if p == m.overlay {
			return nil
		}
		return m.openServices()

	case zoomPaneMsg:
		if m.hasScreen(p) || m.shells.len() == 0 {
			// Nothing beside it to maximize over: the box already has the
			// window, and saying so beats a key that looks broken.
			return status("nothing to maximize — the box has the window")
		}
		m.toggleMaximized(m.onShells)
		return nil

	case quitPaneMsg:
		// The same exit Ctrl-C is outside a pane: the window goes, and every
		// session keeps running without it.
		return m.closeWindow()

	case openToolsMsg:
		return m.openTools()

	case openCredentialsMsg:
		return m.openCredentialDialog(m.paneBox.ID)

	case closeToolMsg:
		return m.closeTool()

	case termpane.DetachMsg:
		if p.tool != "" && m.hasScreen(p) {
			// A tool is put away rather than closed: its session keeps running
			// and its stream stays attached, so choosing it again shows where
			// it has got to. Ending it is the other key. See tools.go.
			return m.minimizeTool()
		}
		if p == m.overlay {
			// The overlay is this CLI's own command, so leaving it closes it.
			m.closeOverlay()
			m.layout()
			return tea.Batch(m.refresh(), status("%s closed", p.action))
		}
		// Detaching leaves every session running: they are the discobox's
		// terminals, and closing the window onto them is not the same as
		// ending them.
		m.closeWorkspace()
		if m.attach != nil {
			// The window is the attach, so detaching from it leaves the
			// window: there is no list behind it to come back to, and landing
			// on one would be a screen nobody asked for. See WithAttach.
			return m.exit(nil)
		}
		return tea.Batch(m.refresh(), status("detached — the discobox is still running"))

	case termpane.ClosedMsg:
		return m.paneClosed(p, msg)

	case paneEventMsg:
		switch msg.event.State {
		case TerminalReconnecting:
			p.status = "reconnecting…"
		case TerminalReconnected:
			p.status = ""
		}
		return fromPane(p.id, m.paneEvents(p.stream))
	}

	term, cmd := p.term.Update(tagged.msg)
	p.term = term
	m.noteOutput(p)
	return fromPane(p.id, cmd)
}

// noteOutput records whether a pane has drawn something its reader has not
// seen. Only a service is marked: every other pane is one you are working in
// or have deliberately left, and a shell running a build would wear the mark
// permanently while telling you nothing you did not already know.
//
// A pane on screen is a pane being read, so its output counts as seen the
// moment it arrives.
func (m *Model) noteOutput(p *pane) {
	if p.service == "" {
		return
	}
	seq := p.term.OutputSeq()
	if seq == p.seenOutput {
		return
	}
	if m.isOnScreen(p) {
		p.seenOutput, p.unread = seq, false
		return
	}
	p.unread = true
}

// markSeen clears the mark on whatever is on screen, which is what looking at
// a pane means. It runs after any message that could have changed which pane
// that is, rather than at each of the several places one can.
func (m *Model) markSeen() {
	for _, p := range m.onScreen() {
		if p == nil || p.service == "" {
			continue
		}
		p.seenOutput, p.unread = p.term.OutputSeq(), false
	}
}

// isOnScreen reports whether a pane is one of the ones currently drawn.
func (m *Model) isOnScreen(p *pane) bool {
	for _, on := range m.onScreen() {
		if on == p {
			return true
		}
	}
	return false
}

// paneClosed handles a session or command ending on its own.
//
// Where the pane sits decides what happens: the primary terminal ending ends
// the workspace, since a workspace is a view onto that session; a tool ending
// takes its window with it, since the window was the program; a shell tab and
// a finished command hold their last screen to be read — an apply with little
// to say is over in a moment, and a pane that vanished with it would be a
// screen you never got to read. Only an error closes one of those unread, and
// is reported instead.
func (m *Model) paneClosed(p *pane, msg termpane.ClosedMsg) tea.Cmd {
	action := p.action
	switch {
	case p.configure != nil:
		flow := p.configure
		m.closeOverlay()
		m.layout()
		err := msg.Err
		if err == nil {
			if code, done := exitCode(p.stream); done && code != 0 {
				err = fmt.Errorf("configure command exited with code %d", code)
			}
		}
		name := flow.harness.displayName()
		if err != nil {
			return func() tea.Msg { return harnessDoneMsg{err: fmt.Errorf("configure %s: %w", name, err)} }
		}
		return func() tea.Msg {
			return harnessDoneMsg{text: "configured " + name, andDefault: flow.andDefault, resume: flow.resume}
		}
	case p.primary:
		m.closeWorkspace()
		if m.attach != nil {
			// The attach is over when its session is, exactly as a plain
			// `discobox attach` is: the window goes, with whatever ended it.
			return m.exit(msg.Err)
		}
		if msg.Err != nil {
			return tea.Batch(m.refresh(), m.report(true, "%s: %v", action, msg.Err))
		}
		return tea.Batch(m.refresh(), status("the session ended"))

	case p.tool != "":
		// A tool is the program, not a transcript of one: quitting difftui or
		// fresh is how you say you are done with it, and a held screen
		// captioned with its exit would be one more thing to dismiss after
		// every look at the diff. So the window goes when the program does,
		// however it ended, and there is nothing left in the strip to reopen.
		//
		// A stream that broke is the other thing, and is still reported: the
		// screen that would have carried the reason went with the window.
		gone := tea.Batch(m.dropTool(p, false), m.refresh())
		if msg.Err != nil {
			return tea.Batch(gone, m.report(true, "%s: %v", action, msg.Err))
		}
		return gone

	case msg.Err == nil:
		// Keep the last screen up to be read; the keys that mean done take it
		// away.
		p.exited = true
		p.status, p.failed = exitVerdict(p.stream)
		return m.refresh()

	case p == m.overlay:
		m.closeOverlay()
		m.layout()
		return tea.Batch(m.refresh(), m.report(true, "%s: %v", action, msg.Err))

	default:
		m.closeTab(p)
		return tea.Batch(m.refresh(), m.report(true, "%s: %v", action, msg.Err))
	}
}

func exitCode(stream Terminal) (int, bool) {
	reporter, ok := stream.(ExitReporter)
	if !ok {
		return 0, false
	}
	return reporter.ExitStatus()
}

// exitVerdict is what a pane that has ended says it was, and whether that is
// bad news.
//
// A command that failed says so. The stream ending is not the same fact as the
// command succeeding — an apply that could not cherry-pick prints its reason
// and exits nonzero, and a pane that answered "finished" over it would be
// contradicting the screen it is captioning. Where there is no exit status to
// have (a session in a discobox, a stream that never was a process), the
// stream ending is all there is to say.
func exitVerdict(stream Terminal) (string, bool) {
	reporter, ok := stream.(ExitReporter)
	if !ok {
		return "finished", false
	}
	code, done := reporter.ExitStatus()
	if !done || code == 0 {
		return "finished", false
	}
	return fmt.Sprintf("failed · exit %d", code), true
}

// readFinished handles a key on a pane whose command has finished.
//
// Only the keys that mean "done" close it. Anything else would take the screen
// away mid-read, which for output longer than the pane is exactly when you are
// still working through it — except left and right, which on a shell tab keep
// meaning what they mean everywhere on the workspace: go look at another pane,
// leaving this one held.
func (m *Model) readFinished(p *pane, key tea.KeyPressMsg) tea.Cmd {
	_, rows := m.paneCells(m.paneWidthOf(p))
	switch keyName(key) {
	case "up", "k":
		p.term.Scroll(1)
	case "down", "j":
		p.term.Scroll(-1)
	case "pgup", "ctrl+b":
		p.term.Scroll(rows - 1)
	case "pgdown", "ctrl+f", " ":
		p.term.Scroll(-(rows - 1))
	case "home", "g":
		p.term.Scroll(p.term.ScrollbackLen())
	case "end", "G":
		p.term.Scroll(-p.term.ScrollbackLen())
	case "left", "h":
		if !m.hasScreen(p) {
			return m.movePane(-1)
		}
	case "right", "l":
		if !m.hasScreen(p) {
			return m.movePane(1)
		}
	case "q", "esc", "enter", paneInterruptKey:
		return m.dismissPane(p)
	}
	return nil
}

// dismissPane takes a finished pane off the screen: the overlay back to the
// workspace, a shell tab out of the strip.
func (m *Model) dismissPane(p *pane) tea.Cmd {
	switch {
	case p.tool != "":
		// Its session is already over, so there is nothing to end: this is the
		// screen being put down, not the tool being closed.
		return m.dropTool(p, false)
	case p == m.overlay:
		m.closeOverlay()
		m.layout()
	default:
		m.closeTab(p)
	}
	return nil
}

// routeMouse sends a mouse event to the pane it belongs to: the one a
// left-button gesture started in while one is open, and otherwise the pane
// under the pointer. A left press also focuses the pane it landed in — with a
// mouse in hand, pointing at the thing is how you say which one you mean. The
// pane's own HandleMouse decides what the event does: selection, forwarding
// to an application that asked for the mouse, or the wheel.
func (m *Model) routeMouse(msg tea.MouseMsg) tea.Cmd {
	// A gesture the chrome latched stays the chrome's; see chrome.go.
	if m.chromeCapture {
		return m.windowMouse(msg)
	}
	var x, y int
	p := m.paneByID(m.mouseCapture)
	if p != nil {
		x, y = m.paneOrigin(p)
	} else {
		mouse := msg.Mouse()
		p, x, y = m.paneAt(mouse.X, mouse.Y)
	}
	if p == nil {
		// Nobody's grid — but a press may still point at something: a tab
		// label means that tab, a pane's border means that pane. The gesture
		// then continues into the chrome's selection either way, so border
		// text stays drag-selectable.
		if click, ok := msg.(tea.MouseClickMsg); ok && click.Button == tea.MouseLeft {
			// A tool window's buttons are the one control that answers a press
			// outright: the gesture asked for the window to go, not for the
			// text under it to be selected.
			if cmd, taken := m.focusChromeAt(click.X, click.Y); taken {
				return cmd
			}
		}
		// Everything else on the chrome is the window's own: the offers on the
		// status line are buttons for their chords, and what is left drives
		// the selection. See mouse.go.
		return m.windowMouse(msg)
	}
	switch ev := msg.(type) {
	case tea.MouseClickMsg:
		if ev.Button == tea.MouseLeft {
			m.mouseCapture = p.id
			m.focusPane(p)
			// One selection on screen at a time.
			m.chromeSel.Clear()
			m.clearPaneSelections(p)
		}
	case tea.MouseReleaseMsg:
		if ev.Button == tea.MouseLeft {
			m.mouseCapture = 0
		}
	case tea.MouseWheelMsg:
		// A finished pane has no application left to consult about the
		// wheel; its held screen scrolls directly.
		if p.exited {
			p.term.Scroll(wheelLines(ev))
			return nil
		}
	}
	p.term.SetSeized(m.mouseSeized)
	return fromPane(p.id, p.term.HandleMouse(translateMouse(msg, x, y)))
}

// paneAt is the pane whose grid is under a screen position, with the origin
// that grid was drawn at. The overlay owns the whole screen while it is up;
// otherwise the terminal is on the left and the visible shell tab on the
// right, or the one maximized box owns the screen alone. The chrome between and
// around them belongs to no pane.
func (m *Model) paneAt(x, y int) (*pane, int, int) {
	for _, p := range m.onScreen() {
		if p == nil {
			continue
		}
		ox, oy := m.paneOrigin(p)
		cols, rows := m.paneCells(m.paneWidthOf(p))
		if x >= ox && x < ox+cols && y >= oy && y < oy+rows {
			return p, ox, oy
		}
	}
	return nil, 0, 0
}

// onScreen is the panes actually drawn: the overlay alone while one is up, the
// two columns of a split, and otherwise the single box the screen is — the
// terminal with no tabs beside it, or whichever side was maximized.
//
// Only these can be pointed at. A pane left off the screen is still emulating,
// but the position the mouse names belongs to whatever is drawn there.
func (m *Model) onScreen() []*pane {
	switch {
	case m.screenPane() != nil:
		return []*pane{m.screenPane()}
	case m.split():
		return []*pane{m.terminals.visible(), m.shells.visible()}
	case m.maximized && m.onShells:
		return []*pane{m.shells.visible()}
	default:
		return []*pane{m.terminals.visible()}
	}
}

// focusPane moves focus to a pane the mouse chose.
func (m *Model) focusPane(p *pane) {
	if m.hasScreen(p) || p.tool != "" {
		return
	}
	col, i := m.paneColumn(p)
	if col == nil {
		return
	}
	m.onShells = col == &m.shells
	col.active = i
}

// tabSpan is where one tab label sits on the boxes' top border row, in
// absolute screen columns, both ends inclusive. shells says which of the two
// columns drew it, since both wear a strip once they hold more than one pane.
type tabSpan struct {
	shells     bool
	index      int
	start, end int
}

// zoomSpan is where one box's maximize control sits on the boxes' top border
// row, in absolute screen columns, both ends inclusive. shells says which of
// the two columns it belongs to.
type zoomSpan struct {
	shells     bool
	start, end int
}

// focusChromeAt applies a press on the chrome to what the cell means: a tool
// window's [-] and [x] are that window put away or ended, the maximize button
// is that box taking the window or giving it back, a tab label is that tab, and
// any other cell of a pane's box is that pane.
//
// It reports what the press asked for — ending a session is a request to the
// server rather than a change to the screen — and whether it owns the gesture.
// Only the tool window's buttons do: everything else on the chrome moves focus
// and lets the press go on into the chrome's own selection, so border text
// stays drag-selectable.
func (m *Model) focusChromeAt(x, y int) (tea.Cmd, bool) {
	// The banner is a button spanning the window, and it is tested first: it
	// is drawn over nothing else, and it is the one thing on this screen that
	// is asking to be pressed. It owns the gesture rather than falling through
	// to the chrome's selection — a bar you click to answer a question must
	// not also start a drag-select of its own text.
	if m.credentialBannerAt(x, y) {
		return m.openCredentialDialog(m.paneBox.ID), true
	}
	if m.showingTool() != nil {
		button, ok := m.buttonAt(x, y)
		if !ok {
			return nil, false
		}
		if button == buttonClose {
			return m.closeTool(), true
		}
		return m.minimizeTool(), true
	}
	if m.overlay != nil {
		// One box with nothing beside it; a border press chooses nothing.
		return nil, false
	}
	if shells, ok := m.zoomAt(x, y); ok {
		m.toggleMaximized(shells)
		return nil, false
	}
	if shells, i, ok := m.tabAt(x, y); ok {
		m.onShells = shells
		m.column().active = i
		return nil, false
	}
	if p := m.paneBoxAt(x, y); p != nil {
		m.focusPane(p)
	}
	return nil, false
}

// zoomAt is the box whose maximize control is under a screen position. The
// spans were recorded when the boxes were drawn, and only a box actually on
// screen records one.
func (m *Model) zoomAt(x, y int) (shells, ok bool) {
	if y != 1+m.credentialBannerTop() {
		return false, false
	}
	for _, s := range m.zoomSpans {
		if x >= s.start && x <= s.end {
			return s.shells, true
		}
	}
	return false, false
}

// toggleMaximized gives one column the whole window, or hands the window back
// to the split.
//
// Which column is maximized follows the focus, so pressing the control also
// moves focus onto the box it was pressed on: the box you asked to see is the
// box the keys should be going to, and the alternative is a screen showing one
// pane while another takes the typing.
func (m *Model) toggleMaximized(shells bool) {
	m.maximized = !m.maximized
	if m.maximized {
		m.onShells = shells && m.shells.len() > 0
	}
	m.layout()
}

// tabAt is the tab whose label is under a screen position, with the column it
// is in. The strips are the boxes' top border row, and the spans were recorded
// when they were drawn.
func (m *Model) tabAt(x, y int) (shells bool, index int, ok bool) {
	if y != 1+m.credentialBannerTop() {
		return false, -1, false
	}
	for _, s := range m.tabSpans {
		if x >= s.start && x <= s.end {
			return s.shells, s.index, true
		}
	}
	return false, -1, false
}

// paneBoxAt is the pane whose box — border and title row included — is under
// a screen position: the body rows between the header above and the hints
// below, split down the middle when there are tabs beside the terminal and
// belonging to the one box on screen when there are not.
func (m *Model) paneBoxAt(x, y int) *pane {
	top := 1 + m.credentialBannerTop()
	if y < top || y > m.paneRows()+2+m.credentialBannerTop() {
		return nil
	}
	if m.split() && x >= m.width/2 {
		return m.shells.visible()
	}
	if !m.split() && m.maximized && m.onShells {
		return m.shells.visible()
	}
	return m.terminals.visible()
}

// copyText puts selected text on the clipboard: the OS clipboard first, and
// OSC 52 only when there is none to write — an SSH session, a box with no
// clipboard tool. Not both: they can land on the same clipboard (WSL's is
// Windows'), the last writer wins, and OSC 52 is the path terminals are
// known to mis-decode. See osClipboard.
func (m *Model) copyText(text string) tea.Cmd {
	copyOS := m.copyOS
	return tea.Batch(
		func() tea.Msg {
			if copyOS(text) == nil {
				return nil
			}
			return tea.SetClipboard(text)()
		},
		status("copied"),
	)
}

// wheelLines is how far one turn of the wheel moves the view.
func wheelLines(wheel tea.MouseWheelMsg) int {
	switch wheel.Button {
	case tea.MouseWheelUp:
		return 3
	case tea.MouseWheelDown:
		return -3
	default:
		return 0
	}
}

// paneRows is the height every pane gets: the banner, the border's own two
// edges, and the status line at the bottom.
// paneRows is the body rows the workspace has for its boxes: the window less
// the header, the status line, and the box's own two edges — less what the
// credential band costs when there is one, which is two rows taken from the
// panes rather than added to the frame.
func (m *Model) paneRows() int { return m.height - 4 - m.credentialBannerCost() }

// paneCells is the terminal size a pane of the given box width implies: the
// box, less its border and a cell of air inside it on each side.
func (m *Model) paneCells(width int) (cols, rows int) {
	return max(width-2-2*boxPad, 1), max(m.paneRows(), 1)
}

// split reports whether the workspace is drawn as two columns: it has tabs to
// put beside the terminal, and neither side has been maximized over the other.
func (m *Model) split() bool { return m.shells.len() > 0 && !m.maximized }

// columns is the box widths of the workspace's two sides, for the given number
// of tabs. It takes the count rather than reading it off the model because the
// first attaches are sized before their panes exist, and a primary sized for a
// window it is about to share is a terminal that draws itself wrong before
// anything can correct it.
//
// In a split the two share the window and the right side takes any odd cell, so
// they are never more than one apart. Otherwise only one box is on screen — no
// tabs at all, or one side maximized over the other — and whichever it is has
// the whole window. The hidden side is sized for the window too: it keeps
// emulating off-screen, and flipping to it must show a screen drawn at the size
// it is shown at.
func (m *Model) columns(tabs int) (term, shells int) {
	if tabs == 0 || m.maximized {
		full := max(m.width, 4)
		return full, full
	}
	return max(m.width/2, 4), max(m.width-m.width/2, 4)
}

// paneWidthOf is the width of the box a pane is drawn in. The overlay has the
// whole window whatever the workspace under it is doing; everything else takes
// its side's column.
func (m *Model) paneWidthOf(p *pane) int {
	if p == m.overlay || p.tool != "" {
		// A tool has the window whenever it is showing, and is drawn at that
		// width even while it is put away: it keeps emulating off-screen, and
		// showing it again must show a screen drawn at the size it is shown at.
		return max(m.width, 4)
	}
	term, shells := m.columns(m.shells.len())
	if m.shells.index(p) >= 0 {
		return shells
	}
	return term
}

// shellLeft is the column the shell box's border starts at: past the
// terminal's half in a split, and the window's own left edge when the shells
// are the maximized side.
func (m *Model) shellLeft() int {
	if m.split() {
		return m.width / 2
	}
	return 0
}

// paneOrigin is where a pane's grid was drawn: past its border's left edge
// and the cell of air inside it, and down past the top band and the border's
// top edge.
//
// The cursor and every mouse event are placed against this, so it is worked
// out once rather than counted twice.
func (m *Model) paneOrigin(p *pane) (x, y int) {
	// The credential band's top row sits between the header and the boxes, so
	// the grids start a row lower while it is up — one row, not two: the band
	// below the boxes takes a row from their height and nothing from where
	// they begin. This is the one place that answers where a pane's first cell
	// is: the hardware cursor is placed through it and every mouse event is
	// translated through it, so an offset applied anywhere else would move one
	// and not the other.
	top := 2 + m.credentialBannerTop()
	if p != nil && m.shells.index(p) >= 0 {
		// A shell tab: its box starts where the terminals' box ends, or at the
		// window's edge when it is the maximized one.
		return m.shellLeft() + 1 + boxPad, top
	}
	return 1 + boxPad, top
}

// viewPaneWindow draws the whole screen for the open workspace: the captions
// above, the bordered terminals side by side, and the keys below.
//
// The grid rows come back from the library already fitted to the cell and are
// put between the border's sides without passing through anything that could
// re-wrap them.
func (m *Model) viewPaneWindow() string {
	inner := max(m.width-2, 1)
	pad := strings.Repeat(" ", boxPad)

	// The drawing pass owns where the border's controls landed: they are only
	// there when they were drawn this frame, and a span left over from a box
	// that is no longer on screen is a click target pointing at nothing.
	m.tabSpans, m.zoomSpans, m.buttonSpans = m.tabSpans[:0], m.zoomSpans[:0], m.buttonSpans[:0]
	m.credentials = credentialSpan{}

	headerW := max(inner-2*boxPad, 1)
	m.zones.push(1+boxPad, 0)
	header := m.viewPaneHeader(headerW)
	m.zones.pop()
	rows := []string{
		" " + pad + header + pad + " ",
	}
	// A credential request on this discobox gets a band of its own under the
	// header *and* one above the keys. The agent in front of you is blocked on
	// a person, and the one thing this screen has to do is say so — on a screen
	// that is mostly terminal, whichever end of it you are reading. Both are
	// rows the workspace only has while a request is waiting, so nothing is
	// spent on either the rest of the time.
	banner := m.viewCredentialBanner(headerW)
	if banner != "" {
		rows = append(rows, " "+pad+banner+pad+" ")
	}

	// The overlay has the screen while it is up. What is under it is not drawn
	// half-covered or dimmed behind it: it is not there, and it is all still
	// there when the command exits.
	var body string
	switch {
	case m.overlay != nil:
		body = m.viewOverlayBox(m.width)
	case m.showingTool() != nil:
		body = m.viewToolBox(m.showingTool(), m.width)
	case m.split():
		// The terminals on the left, the shells on the right; the right half
		// takes any cell the division left over so the row comes out exactly
		// the width.
		left := m.viewColumnBox(&m.terminals, false, 0, m.width/2, !m.onShells)
		right := m.viewColumnBox(&m.shells, true, m.width/2, m.width-m.width/2, m.onShells)
		body = lipgloss.JoinHorizontal(lipgloss.Top, left, right)
	case m.maximized && m.onShells:
		// Maximized: the column with focus has the window and the other is not
		// drawn at all — still attached, still emulating, and back where it was
		// the moment the button gives the window back.
		body = m.viewColumnBox(&m.shells, true, 0, m.width, true)
	default:
		body = m.viewColumnBox(&m.terminals, false, 0, m.width, true)
	}
	rows = append(rows, strings.Split(body, "\n")...)
	if banner != "" {
		rows = append(rows, " "+pad+banner+pad+" ")
		// The whole band is the target, not the words in it: a bar this size
		// that only answered on its text would be a bar that mostly does
		// nothing. Both rows are recorded here, as they are drawn, rather than
		// where they ought to be — the index into these rows *is* the screen
		// row, so a band that moved would take its hit test with it.
		m.credentials = credentialSpan{
			rows:  []int{1, len(rows) - 1},
			start: 0,
			end:   max(m.width-1, 0),
			live:  true,
		}
	}
	room := max(inner-2*boxPad, 1)
	m.zones.push(1+boxPad, len(rows))
	status := m.statusLine(room)
	m.zones.pop()
	rows = append(rows, " "+pad+padANSI(status, room)+pad+" ")
	return strings.Join(rows, "\n")
}

// viewPaneHeader is the workspace's banner: where you are on the left, which
// discobox this is and what it is doing in the middle, and the way out on the
// right.
//
// The middle is what this window is about; the edges are context that is
// written down elsewhere on the screen. So a row too narrow for all three gives
// up the edges first, one at a time, and only a middle that still does not fit
// starts dropping its own fields.
//
// The order is what each edge is worth here:
//
//   - the keys, first, because the hints line under the grid says the same
//     thing and is never dropped;
//   - then "discobox", which names the program rather than the work — you know
//     which program you are in, you are looking at it;
//   - then the folder, which every row of the list this workspace was opened
//     from already shared, and which the list is one key away.
//
// What the transport is doing is not among them. It displaces the keys while it
// is happening — it is the more urgent of the two — and unlike them it is
// written down nowhere else, so it holds its place all the way down.
func (m *Model) viewPaneHeader(w int) string {
	if p := m.overlay; p != nil && p.configure != nil {
		middle := "Configuring " + p.configure.harness.displayName()
		if p.notice != "" {
			middle += "  ·  " + p.notice
		}
		return padANSI(m.st.statusWA.Render(middle), w)
	}
	folder := m.viewFolder(false)
	full := m.viewHeaderBrand() + folder

	// The keys are measured as plain text and drawn only once the row has
	// settled which of its edges it can afford: styling costs no cells, so the
	// measurement is exact either way, and an offer the row gave up is one
	// that is neither drawn nor marked.
	hints := m.headerHints()
	keys, pinned := strings.Repeat(" ", hintsWidth(hints, headerSep)), false
	if p := m.focusedPane(); p != nil && p.status != "" {
		// A command that failed is not the same news as one that is
		// reconnecting or one that is over, and the banner is where it is read.
		style := m.st.statusWA
		if p.failed {
			style = m.st.statusER
		}
		keys, pinned = style.Render(p.status), true
	}

	// Each concession is the row with one more edge given up, widest first.
	concessions := [][2]string{{full, keys}}
	if !pinned {
		concessions = append(concessions, [2]string{full, ""})
		keys = ""
	}
	concessions = append(concessions, [2]string{folder, keys}, [2]string{"", keys})

	// The keys, wherever this row ends up putting them, are buttons for
	// themselves as they are on every other screen. A row that gave them up to
	// fit has nothing there to press, and a pane whose status has displaced
	// them is reporting rather than offering — so they are drawn from the
	// concession that actually won.
	drawKeys := func(right string) string {
		if pinned || right == "" {
			return right
		}
		return m.viewHints(hints, w-lipgloss.Width(right), headerSep)
	}

	fields := m.paneHeaderFields()
	middle := strings.Join(fields, "  ")
	for _, sides := range concessions {
		if lipgloss.Width(middle) <= centerRoom(sides[0], sides[1], w) {
			return spreadCenter(sides[0], middle, drawKeys(sides[1]), w)
		}
	}
	// Nothing left on the edges to give. What the middle can still drop is its
	// own, against the room the barest row leaves it.
	bare := concessions[len(concessions)-1]
	return spreadCenter(bare[0], dropToFit(fields, "  ", centerRoom(bare[0], bare[1], w)), drawKeys(bare[1]), w)
}

// paneHeaderFields is the middle of the workspace's header, field by field:
// which discobox this is, where its work sits in git, and what it is serving.
//
// Every pane is the same discobox, so it is identified once — folded into the
// banner rather than given a line of its own. The id rather than the name,
// because the id is what you would type at a shell to act on this one, and
// muted rather than foreground, because it is there to be looked up when
// wanted rather than read on every glance. What each pane is running is in its
// own border.
//
// The git position, its state spelled out, and the diffstat are the list's own
// columns, drawn by the list's own functions in the list's own colors, and read
// live off the listing (currentBox) rather than off the snapshot the workspace
// was opened on — a header saying "clean" over a session that has been
// committing for an hour is worse than no header at all. The listing refreshes
// itself on the tick whichever screen is up, so this follows along for free.
//
// The listening ports come last because they are the only field with no bound
// on their width — a compose stack brings up as many as it likes — so dropping
// them whole is what keeps the git fields, whose widths are known, on screen.
// They are also the field a pane can answer for itself: the server is in one of
// these terminals.
//
// They are returned in the order they are given up in, last first, once
// viewPaneHeader has run out of edges to drop: the ports, then the diffstat,
// which the apply report gives you anyway, then the word, whose mark is on the
// position regardless. From the diffstat down that is the list's own drop
// order. The id leads and never goes — it is what identifies the window.
func (m *Model) paneHeaderFields() []string {
	box := m.currentBox()
	git := gitStyle(m.st, box)

	fields := []string{m.st.dimText.Render(box.ID)}
	if base := box.base(); base != "" {
		fields = append(fields, git.Render(base))
	}
	// The list pads this into a column, so it spells the empty answer as a
	// dash; here there is no column to fill and nothing to say, so it is left
	// off entirely.
	if changes := box.changes(); changes != "-" {
		fields = append(fields, git.Render(changes))
	}
	if stat := diffText(m.st, box); stat != "" {
		fields = append(fields, stat)
	}
	if listening := portsText(m.st, box, m.forwardedPorts()); listening != "" {
		fields = append(fields, listening)
	}
	return fields
}

// viewColumnBox draws one side of the workspace: the visible pane's grid, with
// the column's tab strip — or, while it holds one pane, that pane's title —
// laid into the top border. Only the visible pane is drawn; the others keep
// emulating off-screen, so flipping to one shows where it is now.
//
// The border of the box with focus is lit and the other is not. With two
// terminals side by side and every key going to one of them, which one that is
// has to be visible without looking for it.
func (m *Model) viewColumnBox(col *column, shells bool, left, width int, focused bool) string {
	p := col.visible()
	if p == nil {
		return ""
	}
	edge := m.st.frame
	if !focused {
		edge = m.st.rule
	}
	inner := max(width-2, 1)
	// A workspace of one pane is not a strip: it wears its own name, the way a
	// box with nothing to choose between always has. The moment there is a
	// second pane anywhere on the screen every one of them wears the number it
	// answers to, this box's own included — the numbers appear exactly when
	// they mean something.
	//
	// The strip is built only when it is drawn: building it records where its
	// labels landed, and a click target for a tab nobody can see points at
	// nothing.
	// The maximize button is the box's whichever way its top is drawn, and it
	// is worked out once: drawing it records where it landed, and a second
	// span for the same button is a click target nothing needs.
	control := m.zoomControl(edge, shells, left, width)
	top := titledEdge(m.st, edge, p.name(), control, inner)
	if len(m.panes()) > 1 {
		top = m.tabbedEdge(col, shells, left, control, edge, inner)
	}
	return m.viewPaneBox(p, top, edge, width)
}

// viewOverlayBox draws the command that has the screen over the workspace. It
// is one pane with the window to itself, so it wears its own name and no
// control: there is nothing beside it to maximize over.
func (m *Model) viewOverlayBox(width int) string {
	edge := m.st.frame
	inner := max(width-2, 1)
	return m.viewPaneBox(m.overlay, titledEdge(m.st, edge, m.overlay.name(), "", inner), edge, width)
}

// viewToolBox draws the tool that has the screen. It is one pane with the
// window to itself like the overlay, and unlike the overlay it wears the two
// buttons that say what can be done to a window that outlives being looked at:
// put it away, or end it. See toolControls.
func (m *Model) viewToolBox(p *pane, width int) string {
	edge := m.st.frame
	inner := max(width-2, 1)
	control := m.toolControls(edge, width)
	return m.viewPaneBox(p, titledEdge(m.st, edge, p.name(), control, inner), edge, width)
}

// viewPaneBox draws one pane's grid inside a box whose top border has already
// been built — a strip of tabs, or a title — with a cell of air between the
// border and the terminal's own output.
func (m *Model) viewPaneBox(p *pane, top string, edge lipgloss.Style, width int) string {
	inner := max(width-2, 1)
	grid := max(inner-2*boxPad, 1)
	side := edge.Render("│")
	pad := strings.Repeat(" ", boxPad)

	rows := []string{top}
	for _, line := range p.term.View() {
		rows = append(rows, side+pad+padANSI(line, grid)+pad+side)
	}
	rows = append(rows, edge.Render("╰"+strings.Repeat("─", inner)+"╯"))
	return strings.Join(rows, "\n")
}

// zoomControl is a box's maximize button, set into the right end of its top
// border the way the title is set into the middle of it — `[+]` to take the
// window, `[-]` to give it back — and records where it landed so a click on it
// can be routed back to the box that drew it.
//
// It is drawn only when there are two columns to choose between: with a single
// box on screen — an overlay, or a terminal with no tabs beside it — there is
// nothing to maximize over, and a button whose two states look the same is a
// button that lies about what it does.
//
// zoomMinWidth is the box width below which the button goes rather than the
// border: a control that overruns its own corner is worse than no control, and
// the keys reach the same toggle at any width.
func (m *Model) zoomControl(edge lipgloss.Style, shells bool, left, width int) string {
	const zoomMinWidth = 12
	if m.screenPane() != nil || m.shells.len() == 0 || width < zoomMinWidth {
		return ""
	}
	glyph := "+"
	if m.maximized {
		glyph = "-"
	}
	// `…[+]─╮`: the bracketed cells end two columns short of the box's right
	// edge, leaving the rule cell that keeps it off the corner.
	end := left + width - 3
	m.zoomSpans = append(m.zoomSpans, zoomSpan{shells: shells, start: end - 2, end: end})
	return edge.Render("[") + m.st.dimText.Render(glyph) + edge.Render("]")
}

// paneCursor is where the hardware cursor goes: the focused pane's own idea of
// it, offset by where that pane's grid was drawn.
func (m *Model) paneCursor() *tea.Cursor {
	p := m.focusedPane()
	if p == nil {
		return nil
	}
	x, y := m.paneOrigin(p)
	return p.term.Cursor(x, y)
}

// mouseMode is what the window asks the real terminal to report. Every frame
// the window draws on the alternate screen reports the mouse: clicks, drags
// and the wheel are how the window is worked with a pointer, and they are
// needed even when nothing in a box asked for a mouse — which is the bargain
// every multiplexer strikes, native selection traded for the window's own.
//
// The opening prompt is the exception. It is drawn inline, in the shell's own
// scrollback, where a mouse coordinate is the terminal's screen rather than
// this frame and the terminal's own selection is still the one that belongs.
// See ADR 0085 §1.
//
// All-motion, because a control the pointer is resting on is drawn as live
// before it is pressed, and a pointer that has not moved yet reports nothing.
// A bare move — no button down — is the window's own: it is answered here and
// forwarded to a sandbox only when that sandbox asked for motion, so an
// application that subscribed to buttons alone is sent no more than it was
// before. See Model.hover.
func (m *Model) mouseMode() tea.MouseMode {
	if !m.takesScreen() {
		return tea.MouseModeNone
	}
	return tea.MouseModeAllMotion
}

// translateMouse moves an event from the screen into a pane's grid.
func translateMouse(msg tea.MouseMsg, originX, originY int) tea.MouseMsg {
	switch event := msg.(type) {
	case tea.MouseClickMsg:
		event.X, event.Y = event.X-originX, event.Y-originY
		return event
	case tea.MouseReleaseMsg:
		event.X, event.Y = event.X-originX, event.Y-originY
		return event
	case tea.MouseWheelMsg:
		event.X, event.Y = event.X-originX, event.Y-originY
		return event
	case tea.MouseMotionMsg:
		event.X, event.Y = event.X-originX, event.Y-originY
		return event
	}
	return msg
}

// displayName is what to call a discobox in the pane's caption: its name, or its
// ID when it has none worth reading.
func displayName(s Sandbox) string {
	if s.Name != "" {
		return s.Name
	}
	return s.ID
}
