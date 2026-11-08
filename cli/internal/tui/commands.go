package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
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
		ListHasKeys:    m.workspaceShown() && m.listHasKeys(),
		Tree:           m.treeAvailable(),
		TreeFits:       m.screenWidth >= treeMinScreen,
		Waiting:        func(box console.Sandbox) bool { return len(m.pendingFor(box)) > 0 },
		Refused: func(box console.Sandbox) bool {
			_, ok := m.shownRejection(box)
			return ok
		},
	}
	if m.oneShot() {
		box := m.paneBox
		ctx.OneShot = &box
	}
	if m.hasWorkspace() {
		box := m.paneBox
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
	if m.inPanes() || m.wsOpening || m.pageOpen() {
		if len(b.Leader) > 0 {
			return m.leader() + " " + strings.Join(b.Leader, " ")
		}
		if len(b.Menu) > 0 {
			return m.leader() + " " + strings.Join(b.Menu, " then ")
		}
		return "palette"
	}
	if len(b.Keys) > 0 {
		return b.Keys[0]
	}
	if len(b.Leader) > 0 {
		return m.leader() + " " + strings.Join(b.Leader, " ")
	}
	return "palette"
}

// consoleCommandMsg is a command's leader binding, delivered by a pane.
type consoleCommandMsg struct{ id string }

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
	if m.inPanes() && m.boxRef(box) == m.boxRef(m.paneBox) {
		// By pointer rather than by number: the primary is 0 in the digits,
		// but the services sit ahead of it in the strip and a jump by
		// position would land on one of those.
		if primary := m.primary(); primary != nil {
			m.focusPane(primary)
		}
		return nil
	}
	return m.openFromList(console.InteractAttach, box)
}

func (m *Model) Shell(box console.Sandbox) tea.Cmd {
	if m.inPanes() && m.boxRef(box) == m.boxRef(m.paneBox) {
		return m.newShell()
	}
	return m.openFromList(console.InteractShell, box)
}

func (m *Model) ReviewConfig(box console.Sandbox) tea.Cmd { return m.reviewHarnessChanges(box) }

func (m *Model) Rename(box console.Sandbox) tea.Cmd { return m.askRename(box) }

func (m *Model) Settings(box console.Sandbox) tea.Cmd { return m.askSettings(box) }

// Do sends a verb. Archiving is reversible and needs no ceremony; purging is
// not, and asks.
func (m *Model) Do(verb console.Verb, targets []console.Sandbox) tea.Cmd {
	if verb == console.VerbPurge {
		question := "Purge " + actionTitle(targets) + "? The disk and everything on it goes, and unarchive cannot bring it back."
		d := confirmDialog("Purge", question, func(string) tea.Cmd {
			return func() tea.Msg { return runVerbMsg{verb: console.VerbPurge, targets: targets} }
		})
		// Enter means no: a destruction nobody can undo has to be typed for,
		// not fallen into by answering the dialog the way every other one is.
		d.defaultNo = true
		m.dialog = d
		return nil
	}
	if verb == console.VerbArchive && m.inPanes() && len(targets) == 1 && m.boxRef(targets[0]) == m.boxRef(m.paneBox) {
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
		m.dialog = actionsDialog(actionTitle(targets), "", actions(targets), chooseAction(targets))
		return nil
	}
	return m.toFocusedPane(boxMenuMsg{})
}

func (m *Model) toFocusedPane(msg tea.Msg) tea.Cmd {
	p := m.focusedPane()
	if p == nil {
		return status("requires a workspace pane")
	}
	return m.updatePaneMsg(paneMsg{id: p.id, msg: msg})
}
