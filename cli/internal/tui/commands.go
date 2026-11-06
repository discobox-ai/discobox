package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/discobox-ai/discobox/termpane"
)

// consoleCommand is a searchable command and its binding. Targeted commands
// take a freshly resolved box, never the visual selection behind the card.
type consoleCommand struct {
	id, label, detail, key string
	aliases                []string
	keyAliases             []string
	prefixKeys             []string
	menuKeys               []string
	contextWhy             string
	action                 bool
	countsAtEntry          bool
	targeted               bool
	enabled                bool
	why                    string
	run                    func(*Model, Sandbox) tea.Cmd
}

func (m *Model) consoleCommands(box *Sandbox) []consoleCommand {
	targets := []Sandbox{}
	if box != nil {
		targets = append(targets, *box)
	}
	var out []consoleCommand
	for _, a := range m.actions(targets) {
		key := a.key
		aliases := []string{a.label}
		if a.label == "archive" {
			aliases = append(aliases, "rm")
		}
		if a.label == "attach" {
			aliases = append(aliases, "open")
		}
		out = append(out, consoleCommand{id: a.label, label: a.label, detail: a.detail, key: key, aliases: aliases,
			prefixKeys: []string{key}, action: true, targeted: true, enabled: a.enabled, why: a.why,
			run: func(m *Model, s Sandbox) tea.Cmd { return m.actOn(key, []Sandbox{s}) }})
	}
	add := func(id, label, detail, key string, aliases []string, targeted bool, run func(*Model, Sandbox) tea.Cmd) {
		c := consoleCommand{prefixKeys: []string{key}, id: id, label: label, detail: detail, key: key, aliases: append([]string{label}, aliases...), targeted: targeted, enabled: true, run: run}
		if targeted {
			c.enabled = box != nil && box.attachable()
			c.why = attachWhy(box != nil, targets)
		}
		out = append(out, c)
	}
	add("tools", "tools", "open an editor or a tool", toolsKey, nil, true, func(m *Model, s Sandbox) tea.Cmd { return tea.Batch(m.switchTo(s), m.openTools(s)) })
	add("issue", "issue", "open the tagged issue", issueKey, nil, true, func(m *Model, s Sandbox) tea.Cmd {
		if _, ok := s.workNumber(false); !ok {
			return m.toggleIssue(s, false)
		}
		return tea.Batch(m.switchTo(s), m.toggleIssue(s, false))
	})
	add("pr", "pull request", "open the tagged pull request", pullKey, []string{"pr"}, true, func(m *Model, s Sandbox) tea.Cmd {
		if _, ok := s.workNumber(true); !ok {
			return m.toggleIssue(s, true)
		}
		return tea.Batch(m.switchTo(s), m.toggleIssue(s, true))
	})
	add("audit", "audit", "follow the box's activity", "", nil, true, func(m *Model, s Sandbox) tea.Cmd { return tea.Batch(m.switchTo(s), m.openAudit(s)) })
	add("new", "new discobox", "open the new discobox card", "n", []string{"new"}, false, func(m *Model, _ Sandbox) tea.Cmd {
		if m.hasWorkspace() || m.oneShot() {
			if cmd, ok := m.treeEntry(treeNew); ok {
				return cmd
			}
			return status("new discobox opens beside the list; open discobox console")
		}
		m.openNew()
		return nil
	})
	add("credentials", "credentials", "manage credentials", secretsKey, []string{"secrets"}, false, func(m *Model, _ Sandbox) tea.Cmd {
		if m.inPanes() {
			if cmd, ok := m.treeEntry(treeSecrets); ok {
				return cmd
			}
			return status("the pages open beside the list; this window has no tree")
		}
		return m.openSecrets()
	})
	add("harnesses", "harnesses", "manage harnesses", harnessesKey, nil, false, func(m *Model, _ Sandbox) tea.Cmd {
		if m.inPanes() {
			if cmd, ok := m.treeEntry(treeHarnesses); ok {
				return cmd
			}
			return status("the pages open beside the list; this window has no tree")
		}
		return m.openHarnesses()
	})
	add("quit", "quit", "leave this window", paneQuitKey, []string{"exit"}, false, func(m *Model, _ Sandbox) tea.Cmd { return m.closeWindow() })
	add("help", "help", "show the console keys", "f1", nil, false, func(m *Model, _ Sandbox) tea.Cmd { m.dialog = m.helpDialog(); return nil })
	add("refresh", "refresh", "read the boxes again", "r", nil, false, func(m *Model, _ Sandbox) tea.Cmd { return m.refresh() })
	add("servers", "servers", "choose the server, folder or tags", "", []string{"tags", "filters"}, false, func(m *Model, _ Sandbox) tea.Cmd { m.dialog = m.filterDialog(); return nil })
	add("all", "all discoboxes", "show the console list", "", []string{"boxes"}, false, func(m *Model, _ Sandbox) tea.Cmd { return m.showAllBoxes() })
	add("services", "services", "view and manage this box's services", "", nil, true, func(m *Model, s Sandbox) tea.Cmd { return tea.Batch(m.switchTo(s), m.openServices()) })
	add("requests", "credential requests", "answer this box's credential requests", credentialsKey, []string{"requests"}, true, func(m *Model, s Sandbox) tea.Cmd { return m.openCredentialDialog(s) })
	add("rejections", "rejected credentials", "remedy a refused credential", rejectedKey, []string{"rejections"}, true, func(m *Model, s Sandbox) tea.Cmd { return m.openRejectedRemedy(s) })
	add("files", "files changed", "review the pull request's changes", pullFilesKey, []string{"diff"}, true, func(m *Model, s Sandbox) tea.Cmd {
		switchCmd := m.switchTo(s)
		number, ok := s.workNumber(true)
		if !ok {
			return m.toggleIssue(s, true)
		}
		openCmd := m.openIssue(s, number, true)
		p := m.issueTab(s.Repository, number)
		if p != nil {
			if p.issue.issue != nil && p.issue.issue.Pull != nil {
				return tea.Batch(switchCmd, openCmd, m.openReview(p))
			}
			p.issue.reviewOnRead = true
		}
		return tea.Batch(switchCmd, openCmd)
	})

	add("archived", "show archived", "show or hide archived boxes", "A", nil, false, func(m *Model, _ Sandbox) tea.Cmd {
		m.list.showArchived = !m.list.showArchived
		m.list.clamp()
		m.layout()
		if m.list.showArchived {
			return status("showing archived discoboxes")
		}
		return status("hiding archived discoboxes")
	})
	add("options", "run options", "choose defaults for the next discobox", optionsKey, []string{"options"}, false, func(m *Model, _ Sandbox) tea.Cmd {
		m.optionsOpen = true
		m.harnessesOpen = false
		m.secretsOpen = false
		m.layout()
		return tea.Batch(m.loadPools(), m.refreshGitCredentialPreview())
	})
	add("repaint", "repaint", "redraw and reread the console", repaintKey, nil, false, func(m *Model, _ Sandbox) tea.Cmd { return tea.Batch(tea.ClearScreen, m.rereadNow()) })

	// These commands own a workspace pane rather than a box chosen from the list.
	// Their original handlers retain service/tool semantics and confirmations.
	paneCommand := func(id, label, detail, key string, msg tea.Msg) {
		add(id, label, detail, "", nil, false, func(m *Model, _ Sandbox) tea.Cmd {
			p := m.focusedPane()
			if p == nil {
				return status("requires a workspace pane")
			}
			return m.updatePaneMsg(paneMsg{id: p.id, msg: msg})
		})
		c := &out[len(out)-1]
		c.prefixKeys = []string{key}
		if m.focusedPane() == nil || !m.workspaceShown() {
			c.contextWhy = "requires a workspace pane"
		}
	}
	paneCommand("terminal", "new terminal", "open another harness terminal", paneTerminalKey, newTerminalMsg{})
	paneCommand("end", "end terminal", "end the focused session", paneEndKey, endPaneMsg{})
	paneCommand("maximize", "maximize", "maximize or restore the focused column", paneZoomKey, zoomPaneMsg{})
	paneCommand("detach", "detach", "leave the workspace, keeping sessions running", paneDetachAlt, termpane.DetachMsg{})
	paneCommand("mouse", "toggle mouse", "take or return the application's mouse", paneMouseKey, toggleMouseMsg{})
	paneCommand("actions", "actions", "open the focused box's action menu", boxMenuKey, boxMenuMsg{})
	paneCommand("dismiss", "dismiss banner", "dismiss the current notice", bannerDismissKey, dismissBannerMsg{})
	add("palette", "command palette", "find a box or command", ":", []string{"palette"}, false, func(m *Model, _ Sandbox) tea.Cmd { return m.openPalette() })
	out[len(out)-1].prefixKeys = []string{paletteKey}

	for i := range out {
		c := &out[i]
		c.countsAtEntry = c.action
		if c.id == "palette" || c.id == "new" || c.id == "tools" || c.id == "audit" || c.id == "harnesses" || c.id == "credentials" || c.id == "services" {
			c.countsAtEntry = true
		}
		switch c.id {
		case "audit":
			c.prefixKeys = []string{auditKey}
		case "options", "repaint":
			c.prefixKeys = nil
		case "archived":
			c.prefixKeys = nil
		case "help":
			c.keyAliases = []string{"?"}
		case "archive":
			c.keyAliases = []string{"delete"}
		case "actions":
			c.key = boxMenuKey
			if !m.workspaceShown() {
				c.contextWhy = ""
				c.run = func(m *Model, _ Sandbox) tea.Cmd {
					targets := m.list.targets()
					m.dialog = actionsDialog(actionTitle(targets), "", m.actions(targets), chooseAction(targets))
					return nil
				}
			}

		case "requests":
			c.enabled = box != nil && len(m.pendingFor(*box)) > 0
			c.why = "nothing waiting on this discobox"
			c.prefixKeys = []string{credentialsLeaderKey}
		case "rejections":
			if box != nil {
				_, c.enabled = m.shownRejection(*box)
			} else {
				c.enabled = false
			}
			c.why = "nothing refused on this discobox"
		case "services":
			c.prefixKeys = []string{paneServicesKey, paneServicesMenuKey}
		case "all":
			c.prefixKeys = nil
			c.menuKeys = []string{boxMenuKey, allBoxesKey}
			if !m.oneShot() || m.screenWidth < treeMinScreen {
				c.contextWhy = "available in a wide attach window"
			}
		case "new", "credentials", "harnesses":
			if m.hasWorkspace() && !m.treeAvailable() {
				c.contextWhy = "the pages open beside the list; this window has no tree"
			}
		}
		if m.oneShot() && (c.id == "new" || c.id == "credentials" || c.id == "harnesses" || c.id == "servers" || c.id == "options") {
			c.contextWhy = "open discobox console for this page"
		}
		if box != nil && (c.id == "issue" || c.id == "pr" || c.id == "files") {
			if _, ok := box.workNumber(c.id != "issue"); !ok {
				c.enabled = false
				c.why = "no matching repository tag"
			}
		}
		if box != nil && m.oneShot() && (box.ID != m.paneBox.ID || m.session.serverName(box.Server) != m.session.serverName(m.paneBox.Server)) {
			c.enabled = false
			c.why = "this window belongs to one box; open another with discobox attach"
		}
		if c.contextWhy != "" {
			c.enabled = false
			c.why = c.contextWhy
		}
	}
	return out
}

// consoleCommandMsg is a descriptor's leader binding, delivered by a pane.
type consoleCommandMsg struct{ id string }

func (m *Model) dispatchConsoleCommand(id string, box Sandbox) tea.Cmd {
	for _, c := range m.consoleCommands(&box) {
		if c.id != id {
			continue
		}

		return m.executeConsoleCommand(c, box)
	}
	return nil
}

// executeConsoleCommand owns window-context validation and use accounting.
// Target lifecycle actions count at actOn so batch menus and bare shortcuts
// enter the same flow exactly once.
func (m *Model) executeConsoleCommand(c consoleCommand, box Sandbox) tea.Cmd {
	if c.targeted && m.ds.ForServer(box.Server) == nil {
		return status("server %q is no longer available", box.Server)
	}
	if c.contextWhy != "" {
		return status("%s", c.contextWhy)
	}
	if !c.countsAtEntry {
		m.recordConsoleUse(c.id)
	}
	return c.run(m, box)
}
func (m *Model) recordConsoleUse(id string) {
	if m.paletteUsage == nil {
		m.paletteUsage = map[string]int{}
	}
	m.paletteUsage[id]++
}
func (m *Model) consoleCommandBinding(c consoleCommand) string {
	if m.inPanes() || m.wsOpening || m.pageOpen() {
		if len(c.prefixKeys) > 0 && c.prefixKeys[0] != "" {
			return m.leader() + " " + strings.Join(c.prefixKeys, " ")
		}
		if len(c.menuKeys) > 0 {
			return m.leader() + " " + strings.Join(c.menuKeys, " then ")
		}
		return "palette"
	}
	if c.key != "" {
		return c.key
	}
	if len(c.prefixKeys) > 0 && c.prefixKeys[0] != "" {
		return m.leader() + " " + strings.Join(c.prefixKeys, " ")
	}
	return "palette"
}
