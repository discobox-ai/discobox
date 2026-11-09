package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/discobox-ai/discobox/cli/internal/console"
	"github.com/discobox-ai/discobox/termpane"
)

// The window is one frontend of the core's command registry
// (console.Registry): every command is dispatched through console.Dispatch,
// which checks it can run and counts its use, and then enters the flow below.
// The flows are the window's: dialogs, workspaces and pages are drawn here.

var _ console.Frontend[tea.Cmd] = (*Model)(nil)

// commandContext is what the registry is told about where a command is asked
// from.
func (m *Model) commandContext() console.Context {
	ctx := console.Context{
		Session:        m.session,
		HasWorkspace:   m.hasWorkspace(),
		WorkspaceShown: m.workspaceShown(),
		PaneFocused:    m.focusedPane() != nil,
		ListHasKeys:    m.listInFront(),
		Tree:           m.treeAvailable(),
		TreeShown:      m.treeShown(),
		TreeFits:       m.screenWidth >= treeMinScreen,
		PageShown:      m.pageOpen() && !m.listInFront(),
		Waiting:        func(box console.Sandbox) bool { return len(m.pendingFor(box)) > 0 },
		Refused: func(box console.Sandbox) bool {
			_, ok := m.shownRejection(box)
			return ok
		},
	}
	if m.oneShot() {
		box := m.Box
		ctx.OneShot = &box
	}
	if m.hasWorkspace() {
		box := m.Box
		ctx.Workspace = &box
	}
	return ctx
}

// dispatch runs a command on explicit targets: none for the window's own
// commands, the chosen box, or the list's batch for a lifecycle command.
func (m *Model) dispatch(id string, targets ...console.Sandbox) tea.Cmd {
	return console.Dispatch(m.commands, m, m.commandContext(), id, targets)
}

// commandEntries is every command as it stands for one box, or for none.
func (m *Model) commandEntries(box *console.Sandbox) []console.Entry {
	var targets []console.Sandbox
	if box != nil {
		targets = []console.Sandbox{*box}
	}
	return m.commands.Entries(m.commandContext(), targets)
}

// act runs a list key's lifecycle command on the visual range and ends the
// mode, the way staging a visual selection does in discobox-review.
func (m *Model) act(id string) tea.Cmd {
	targets := m.list.targets()
	m.list.visual = false
	return m.dispatch(id, targets...)
}

// commandBinding is how a command is reached from where the window is now,
// for the palette's rows.
func (m *Model) commandBinding(c console.Command) string {
	b := c.Bindings.TUI
	menu := func() string {
		var keys []string
		for _, key := range b.Menu {
			keys = append(keys, keyLabel(key))
		}
		return m.leader() + " " + strings.Join(keys, " then ")
	}
	if m.inPanes() || m.wsOpening || m.pageOpen() {
		if len(b.Leader) > 0 {
			return m.leaderKeys(c.ID)
		}
		if len(b.Menu) > 0 {
			return menu()
		}
		return "palette"
	}
	if len(b.Keys) > 0 {
		return keyLabel(b.Keys[0])
	}
	if len(b.Leader) > 0 {
		return m.leaderKeys(c.ID)
	}
	return "palette"
}

// The window's keys are the registry's (ADR 26-11-07-598 §3): the constants
// its hints, help and menus name a key by are read from it here, so a key
// moved in the registry moves everywhere the window spells it.

// command is a registry command the window names a key of. A missing one is
// the window naming a command the registry does not have.
func command(id string) console.Command {
	c, ok := console.Lookup(id)
	if !ok {
		panic("tui: no command " + id + " in the registry")
	}
	return c
}

// bareKey is a command's key on the list: the first, which hints show.
func bareKey(id string) string { return command(id).Bindings.TUI.Keys[0] }

// leaderSeq is a command's sequence after the leader: the first, which hints
// show.
func leaderSeq(id string) []string { return command(id).Bindings.TUI.Leader[0] }

// leaderKey is a command's one key after the leader.
func leaderKey(id string) string { return leaderSeq(id)[0] }

// chordKey is a command's leaderless chord.
func chordKey(id string) string { return command(id).Bindings.TUI.Chords[0] }

// menuKey is the key of a command's entry in the menu that reaches it.
func menuKey(id string) string {
	path := command(id).Bindings.TUI.Menu
	return path[len(path)-1]
}

// quitAnywhereKey is quit's key on every screen but a pane: Ctrl-C, which
// inside one is the program's.
var quitAnywhereKey = command("quit").Bindings.TUI.Keys[1]

// bareHint is a key-line offer of a command's list key.
func bareHint(id, label string) hint { return keyed(keyLabel(bareKey(id)), bareKey(id), label) }

// leaderHint is a key-line offer of a command's sequence after the leader.
func (m *Model) leaderHint(id, label string) hint {
	return pressing(m.leaderKeys(id)+" "+label, append([]string{m.leader()}, leaderSeq(id)...)...)
}

// leaderCommand is the command one key after the leader runs, by ID, if any.
func leaderCommand(key string) string {
	c, _ := console.ForTUILeader(key)
	return c.ID
}

// chordPress is a leaderless chord as the key press a pane hands back to its
// program, where the window has nothing for it to do: `ctrl+shift+j`.
func chordPress(chord string) tea.KeyPressMsg {
	parts := strings.Split(chord, "+")
	var press tea.KeyPressMsg
	for _, mod := range parts[:len(parts)-1] {
		switch mod {
		case "ctrl":
			press.Mod |= tea.ModCtrl
		case "shift":
			press.Mod |= tea.ModShift
		case "alt":
			press.Mod |= tea.ModAlt
		}
	}
	press.Code = []rune(parts[len(parts)-1])[0]
	return press
}

// consoleCommandMsg is a command's binding, delivered by a pane. chord is the
// leaderless chord that asked, if one did: where the command cannot act, it
// goes on to the program instead, as it would have unbound.
type consoleCommandMsg struct {
	id    string
	chord *tea.KeyPressMsg
}

// Unavailable says it on the status line.
func (m *Model) Unavailable(why string) tea.Cmd { return status("%s", why) }

// Cannot says it in a dialog: the command was asked of these boxes, and they
// are what refuses it.
func (m *Model) Cannot(c console.Command, why string) tea.Cmd {
	m.dialog = errorDialog("Cannot "+c.Label, why)
	return nil
}

func (m *Model) OfferUpgrade(box console.Sandbox) tea.Cmd {
	m.offerImageUpgrade(box)
	return nil
}

// Attach on the workspace screen of the same box goes back to the primary,
// which is what attach means there: it is the session the workspace is a view
// onto. From the list it is the screen to open.
func (m *Model) Attach(box console.Sandbox) tea.Cmd {
	if m.inPanes() && m.boxRef(box) == m.boxRef(m.Box) {
		// By pointer rather than by number: the primary is 0 in the digits,
		// but the services sit ahead of it in the strip and a jump by
		// position would land on one of those.
		if primary := m.Primary(); primary != nil {
			m.focusPane(primary)
		}
		return nil
	}
	return m.openFromList(console.InteractAttach, box)
}

func (m *Model) Shell(box console.Sandbox) tea.Cmd {
	if m.inPanes() && m.boxRef(box) == m.boxRef(m.Box) {
		return m.newShell()
	}
	return m.openFromList(console.InteractShell, box)
}

func (m *Model) ReviewConfig(box console.Sandbox) tea.Cmd { return m.reviewHarnessChanges(box) }

func (m *Model) Rename(box console.Sandbox) tea.Cmd { return m.askRename(box) }

func (m *Model) Settings(box console.Sandbox) tea.Cmd { return m.askSettings(box) }

// Confirm asks the question the core puts before a verb (purging's, which
// nothing can undo), and sends the verb only on yes.
func (m *Model) Confirm(q console.Question, verb console.Verb, targets []console.Sandbox) tea.Cmd {
	m.dialog = questionDialog(q, func(key string) tea.Cmd {
		if key != console.AnswerYes {
			return nil
		}
		return func() tea.Msg { return runVerbMsg{verb: verb, targets: targets} }
	})
	return nil
}

// Do sends a verb. One that asks first came through Confirm.
func (m *Model) Do(verb console.Verb, targets []console.Sandbox) tea.Cmd {
	if verb == console.VerbArchive && m.inPanes() && len(targets) == 1 && m.boxRef(targets[0]) == m.boxRef(m.Box) {
		// From the workspace the one target is the discobox it shows.
		return m.archiveWorkspace(targets[0])
	}
	return m.runVerb(verb, targets, verbThen{})
}

func (m *Model) Tools(box console.Sandbox) tea.Cmd {
	return tea.Batch(m.switchTo(box), m.openTools(box))
}

func (m *Model) Issue(box console.Sandbox) tea.Cmd {
	if _, ok := box.WorkNumber(false); !ok {
		return m.toggleIssue(box, false)
	}
	return tea.Batch(m.switchTo(box), m.toggleIssue(box, false))
}

func (m *Model) PullRequest(box console.Sandbox) tea.Cmd {
	if _, ok := box.WorkNumber(true); !ok {
		return m.toggleIssue(box, true)
	}
	return tea.Batch(m.switchTo(box), m.toggleIssue(box, true))
}

func (m *Model) FilesChanged(box console.Sandbox) tea.Cmd {
	switchCmd := m.switchTo(box)
	number, ok := box.WorkNumber(true)
	if !ok {
		return m.toggleIssue(box, true)
	}
	openCmd := m.openIssue(box, number, true)
	p := m.issueTab(box.Repository, number)
	if p != nil {
		if p.issue.issue != nil && p.issue.issue.Pull != nil {
			return tea.Batch(switchCmd, openCmd, m.openReview(p))
		}
		p.issue.reviewOnRead = true
	}
	return tea.Batch(switchCmd, openCmd)
}

func (m *Model) Audit(box console.Sandbox) tea.Cmd {
	return tea.Batch(m.switchTo(box), m.openAudit(box))
}

func (m *Model) Services(box console.Sandbox) tea.Cmd {
	return tea.Batch(m.switchTo(box), m.openServices())
}

func (m *Model) CredentialRequests(box console.Sandbox) tea.Cmd { return m.openCredentialDialog(box) }

func (m *Model) RejectedCredentials(box console.Sandbox) tea.Cmd { return m.openRejectedRemedy(box) }

func (m *Model) NewBox() tea.Cmd {
	if m.hasWorkspace() || m.oneShot() {
		if cmd, ok := m.treeEntry(treeNew); ok {
			return cmd
		}
		return status("new discobox opens beside the list; open discobox console")
	}
	m.openNew()
	return nil
}

func (m *Model) Credentials() tea.Cmd {
	if m.inPanes() {
		if cmd, ok := m.treeEntry(treeSecrets); ok {
			return cmd
		}
		return status("the pages open beside the list; this window has no tree")
	}
	return m.openSecrets()
}

func (m *Model) Harnesses() tea.Cmd {
	if m.inPanes() {
		if cmd, ok := m.treeEntry(treeHarnesses); ok {
			return cmd
		}
		return status("the pages open beside the list; this window has no tree")
	}
	return m.openHarnesses()
}

func (m *Model) Quit() tea.Cmd { return m.closeWindow() }

func (m *Model) Help() tea.Cmd {
	m.dialog = m.helpDialog()
	return nil
}

func (m *Model) Refresh() tea.Cmd { return m.refresh() }

func (m *Model) Filters() tea.Cmd {
	m.dialog = m.filterDialog()
	return nil
}

func (m *Model) AllBoxes() tea.Cmd { return m.showAllBoxes() }

func (m *Model) ToggleArchived() tea.Cmd {
	m.list.Filter.Archived = !m.list.Filter.Archived
	m.list.clamp()
	m.layout()
	if m.list.Filter.Archived {
		return status("showing archived discoboxes")
	}
	return status("hiding archived discoboxes")
}

func (m *Model) RunOptions() tea.Cmd {
	m.optionsOpen = true
	m.harnessesOpen = false
	m.secretsOpen = false
	m.layout()
	return tea.Batch(m.loadPools(), m.refreshGitCredentialPreview())
}

func (m *Model) Repaint() tea.Cmd { return tea.Batch(tea.ClearScreen, m.rereadNow()) }

func (m *Model) Palette() tea.Cmd { return m.openPalette() }

// The workspace's own commands go to the focused pane, whose handlers keep
// their service and tool semantics and confirmations.

func (m *Model) NewTerminal() tea.Cmd { return m.toFocusedPane(newTerminalMsg{}) }

func (m *Model) EndSession() tea.Cmd { return m.toFocusedPane(endPaneMsg{}) }

func (m *Model) Maximize() tea.Cmd { return m.toFocusedPane(zoomPaneMsg{}) }

func (m *Model) Detach() tea.Cmd { return m.toFocusedPane(termpane.DetachMsg{}) }

func (m *Model) ToggleMouse() tea.Cmd { return m.toFocusedPane(toggleMouseMsg{}) }

func (m *Model) DismissBanner() tea.Cmd { return m.toFocusedPane(dismissBannerMsg{}) }

// Actions is the focused box's menu on a workspace, and the list's away from
// one or wherever the list has the keys beside it.
func (m *Model) Actions() tea.Cmd {
	if !m.workspaceShown() || m.listHasKeys() {
		targets := m.list.targets()
		m.dialog = actionsDialog(console.TargetsTitle(targets), "", actions(targets), chooseAction(targets))
		return nil
	}
	return m.toFocusedPane(boxMenuMsg{})
}

// NextPane and PreviousPane move along the strip; off the leftmost pane is
// the list beside it. They are the one implementation of both, from a pane's
// leader key, the window's leader beside a page or the list, and the palette:
// with the list holding the keys beside a workspace the next pane is the
// workspace's, and from a page beside the tree, or a workspace still opening,
// the previous one is the tree. Where neither can act — a page covering the
// panes, the list with nothing left of it — the registry says so
// (console.Context.ListHasKeys, PageShown), and Dispatch never gets here.
func (m *Model) NextPane() tea.Cmd {
	// The list has the keys, drawn beside the right side or expanded over
	// it: the next pane puts it back beside and gives the right side the keys.
	if m.listInFront() {
		m.backToPanes()
		return nil
	}
	return m.toFocusedPane(movePaneMsg{delta: 1})
}

func (m *Model) PreviousPane() tea.Cmd {
	if (m.pageOpen() || m.wsOpening) && m.treeShown() {
		m.focusTree()
		return nil
	}
	return m.toFocusedPane(movePaneMsg{delta: -1})
}

// NextBox and PreviousBox walk the list beside the workspace: from a pane,
// carrying the leader on through a run of them; from anywhere else, the walk
// alone.
func (m *Model) NextBox() tea.Cmd { return m.walkBox(1) }

func (m *Model) PreviousBox() tea.Cmd { return m.walkBox(-1) }

func (m *Model) walkBox(delta int) tea.Cmd {
	if m.inPanes() && m.focusedPane() != nil {
		return m.toFocusedPane(switchBoxMsg{delta: delta})
	}
	return m.switchBox(delta)
}

// ToggleSidebar hides the list beside the workspace, or shows it again.
func (m *Model) ToggleSidebar() tea.Cmd {
	if !m.treeHidden {
		return m.treeAct(treeHide)
	}
	m.treeHidden = false
	if m.syncWidth() {
		m.layout()
	}
	return nil
}

func (m *Model) toFocusedPane(msg tea.Msg) tea.Cmd {
	p := m.focusedPane()
	if p == nil {
		return status("requires a workspace pane")
	}
	return m.updatePaneMsg(paneMsg{id: p.id, msg: msg})
}

// The help's keys are the registry's too: each row names a command by the
// keys the registry gives it, and the rows that only list commands are
// generated from it.

// keyLabel is a key as the help spells it: ↑ for up, Enter, F3, Ctrl-O.
func keyLabel(key string) string {
	if key == " " {
		return "Space"
	}
	parts := strings.Split(key, "+")
	name := parts[len(parts)-1]
	switch name {
	case "up":
		name = "↑"
	case "down":
		name = "↓"
	case "left":
		name = "←"
	case "right":
		name = "→"
	case "delete":
		name = "Del"
	case "pgup":
		name = "PgUp"
	case "pgdown":
		name = "PgDn"
	default:
		if len(name) > 1 {
			name = strings.ToUpper(name[:1]) + name[1:]
		}
	}
	mods := parts[:len(parts)-1]
	if len(mods) > 0 && len(name) == 1 {
		// A modified letter is written in capitals, as terminals print it.
		name = strings.ToUpper(name)
	}
	for i := len(mods) - 1; i >= 0; i-- {
		name = strings.ToUpper(mods[i][:1]) + mods[i][1:] + "-" + name
	}
	return name
}

// seqLabel is a sequence after the leader as the help spells it: `h`, `S0`.
func seqLabel(seq []string) string {
	out := ""
	for _, key := range seq {
		out += keyLabel(key)
	}
	return out
}

// leaderKeys is a command's first sequence after the leader, with the
// leader: `ctrl+b h`.
func (m *Model) leaderKeys(id string) string {
	return m.leader() + " " + seqLabel(leaderSeq(id))
}

// otherLeaderKeys is a command's other sequences after the leader, without
// it: the `l` beside `→`.
func (m *Model) otherLeaderKeys(id string) string {
	var out []string
	for _, seq := range command(id).Bindings.TUI.Leader[1:] {
		out = append(out, seqLabel(seq))
	}
	return strings.Join(out, " and ")
}

// gestureKeys is a numbered gesture's sequences as one range, with the
// leader: `ctrl+b 0-9`, `ctrl+b S1-9`.
func (m *Model) gestureKeys(id string) string {
	g, _ := console.LookupGesture(id)
	seqs := g.Bindings.TUI.Leader
	first, last := seqs[0], seqs[len(seqs)-1]
	return m.leader() + " " + seqLabel(first) + "-" + last[len(last)-1]
}

// helpEntry is one row of the help's key reference.
type helpEntry struct{ keys, text string }

// helpRow is a key and what it does, as the help lays one out: the key in a
// column of its own and the text beside it, or under it when the key is
// wider than the column.
func helpRow(keys string, text ...string) []string {
	const column = 15
	indent := strings.Repeat(" ", 4+column)
	var out []string
	if w := lipgloss.Width(keys); w < column {
		out = append(out, "    "+keys+strings.Repeat(" ", column-w)+text[0])
		text = text[1:]
	} else {
		out = append(out, "    "+keys)
	}
	for _, line := range text {
		out = append(out, indent+line)
	}
	return out
}

// helpRows is a run of rows, each a command's description wrapped to the
// help's width beside its key.
func helpRows(entries []helpEntry) []string {
	var out []string
	for _, e := range entries {
		out = append(out, helpRow(e.keys, wrapWords(e.text, helpTextWidth)...)...)
	}
	return out
}

// helpTextWidth is the help's text column: its rule, less the key column.
const helpTextWidth = 63 - 4 - 15

// wrapWords breaks text into lines of at most width cells, between words.
func wrapWords(text string, width int) []string {
	var lines []string
	line := ""
	for _, word := range strings.Fields(text) {
		if line != "" && lipgloss.Width(line)+1+lipgloss.Width(word) > width {
			lines = append(lines, line)
			line = ""
		}
		if line != "" {
			line += " "
		}
		line += word
	}
	return append(lines, line)
}

// listCommands is every command the list has a key for, with its keys and
// what it does.
func listCommands() []helpEntry {
	var out []helpEntry
	for _, c := range console.Commands() {
		var keys []string
		for _, key := range c.Bindings.TUI.Keys {
			keys = append(keys, keyLabel(key))
		}
		if len(keys) > 0 {
			out = append(out, helpEntry{strings.Join(keys, " / "), c.Detail})
		}
	}
	return out
}

// leaderCommands is commands by their keys after the leader.
func (m *Model) leaderCommands(ids ...string) []helpEntry {
	var out []helpEntry
	for _, id := range ids {
		out = append(out, helpEntry{m.leaderKeys(id), command(id).Detail})
	}
	return out
}

// lifecycleBesideTheScreen is the box's own commands the workspace reaches
// behind the leader beyond attach and shell, which its help describes apart.
func lifecycleBesideTheScreen() []string {
	var out []string
	for _, c := range console.Commands() {
		if c.Lifecycle && c.ID != "attach" && c.ID != "shell" {
			out = append(out, c.ID)
		}
	}
	return out
}
