package tui

import (
	"context"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// chrome line budget: one header line plus a two-line footer (help + status)
// frame the screen body.
const (
	headerHeight = 1
	footerHeight = 2
)

// screen is one interactive view. Screens are pure state: Update returns the
// next screen and an optional command, and IO happens only inside commands.
type screen interface {
	Init() tea.Cmd
	Update(msg tea.Msg) (screen, tea.Cmd)
	// View renders the body sized to the content area between header and footer.
	View(width, height int) string
	// title labels the screen in the header.
	title() string
	// helpBindings are the key hints shown in the collapsed footer.
	helpBindings() []key.Binding
	// fullHelpBindings are the grouped hints shown in the expanded footer.
	fullHelpBindings() [][]key.Binding
	// cursor optionally places the hardware cursor, given the body's origin on
	// screen. Screens without a live cursor return nil.
	cursor(originX, originY int) *tea.Cursor
}

// tabID identifies one of the top-level tabs shown in the header tab bar. The
// tabs are peers reached by focusing the bar (Up from a tab body) and moving
// with h/l; the ordering here is the left-to-right order in the bar.
type tabID int

const (
	tabSandboxes tabID = iota
	tabAgents
	tabSecrets
)

// tabTitles labels each tab in the bar, indexed by tabID.
var tabTitles = []string{
	tabSandboxes: "sandboxes",
	tabAgents:    "agents",
	tabSecrets:   "secrets",
}

// resizeMsg gives the active screen its content-area size (window minus chrome).
type resizeMsg struct {
	width  int
	height int
}

// toggleHelpMsg asks the root to expand or collapse the footer help.
type toggleHelpMsg struct{}

// Model is the root Bubble Tea model. It owns the window size and shared chrome
// (header, footer help, status line) and routes messages to the active screen.
type Model struct {
	ctx    context.Context
	ds     DataSource
	keys   keyMap
	styles styles

	width  int
	height int

	help         help.Model
	showFullHelp bool

	statusText  string
	statusError bool

	// activeTab is the tab whose body is shown; tabFocused is true while the tab
	// bar itself holds focus (h/l switch tabs, Down/Enter drop into the body).
	activeTab  tabID
	tabFocused bool

	active      screen
	list        *sandboxesScreen
	terminal    *terminalScreen
	form        *newSessionScreen
	harnesses   *harnessesScreen
	harnessForm *harnessFormScreen
	secrets     *secretsScreen
}

// New builds the root model with the sandbox list as the initial screen.
func New(ctx context.Context, ds DataSource) *Model {
	st := defaultStyles()
	keys := defaultKeyMap()
	list := newSandboxesScreen(ctx, ds, keys, st)
	return &Model{
		ctx:    ctx,
		ds:     ds,
		keys:   keys,
		styles: st,
		help:   help.New(),
		active: list,
		list:   list,
	}
}

// Init starts the initial screen.
func (m *Model) Init() tea.Cmd {
	return m.active.Init()
}

// Update handles global concerns (resize, help, screen routing, status) and
// delegates everything else to the active screen.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// While the tab bar holds focus, the root owns key input so h/l switch tabs
	// and Down/Enter drop into the body. Guard on isTabScreen so a stale focus
	// flag can never swallow keys meant for a sub-screen.
	if key, ok := msg.(tea.KeyPressMsg); ok && m.tabFocused && m.isTabScreen(m.active) {
		return m, m.handleTabKey(key)
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.help.SetWidth(msg.Width)
		return m, m.resizeActive()

	case toggleHelpMsg:
		m.showFullHelp = !m.showFullHelp
		return m, m.resizeActive()

	case selectSandboxMsg:
		return m, m.enterTerminal(msg.sandbox)

	case fullscreenSandboxMsg:
		return m, m.runFullscreenAttach(msg.sandbox)

	case fullscreenFinishedMsg:
		m.fullscreenFinished(msg)

	case openNewMsg:
		return m, m.enterNewForm()

	case sessionCreatedMsg:
		return m, m.sessionCreated(msg.sandbox)

	case backMsg:
		return m, m.goToList()

	case focusTabsMsg:
		m.tabFocused = true
		return m, nil

	case openHarnessesMsg:
		return m, m.switchTab(tabAgents, false)

	case openHarnessFormMsg:
		return m, m.enterHarnessForm(msg.edit)

	case harnessFormBackMsg:
		return m, m.goToHarnesses()

	case harnessSavedMsg:
		return m, m.harnessSaved(msg)

	case runConfigureMsg:
		return m, m.runConfigure(msg.harness)

	case harnessConfiguredMsg:
		return m, m.harnessConfigured(msg)

	case harnessDeletedMsg:
		m.setHarnessDeleteStatus(msg)

	case statusMsg:
		m.statusError = msg.err
		m.statusText = msg.text

	case errMsg:
		m.statusError = true
		m.statusText = fmt.Sprintf("%s: %v", msg.context, msg.err)

	case deletedMsg:
		m.setDeleteStatus(msg)

	case sandboxesLoadedMsg:
		// A successful refresh clears a stale error banner.
		if m.statusError {
			m.statusError = false
			m.statusText = ""
		}
	}

	next, cmd := m.active.Update(msg)
	m.active = next
	m.syncActive(next)
	return m, cmd
}

// View composes header, body, and footer into a full-screen alternate buffer.
func (m *Model) View() tea.View {
	if m.width == 0 || m.height == 0 {
		return tea.NewView("")
	}
	bodyHeight := m.footerTop() - headerHeight
	if bodyHeight < 0 {
		bodyHeight = 0
	}

	header := m.renderHeader()
	body := fitVertical(m.active.View(m.width, bodyHeight), bodyHeight)
	footer := m.renderFooter()

	content := lipgloss.JoinVertical(lipgloss.Left, header, body, footer)

	view := tea.NewView(content)
	view.AltScreen = true
	if cur := m.active.cursor(0, headerHeight); cur != nil {
		view.Cursor = cur
	}
	return view
}

// footerTop is the first row occupied by the footer.
func (m *Model) footerTop() int {
	return m.height - m.footerHeight()
}

func (m *Model) footerHeight() int {
	if m.showFullHelp {
		// Full help renders one row per binding column group plus the status line.
		return lipgloss.Height(m.help.FullHelpView(m.currentFullHelp())) + 1
	}
	return footerHeight
}

// currentShortHelp and currentFullHelp pick the footer key hints: the tab-bar
// motions while the bar holds focus, otherwise the active screen's own hints.
func (m *Model) currentShortHelp() []key.Binding {
	if m.tabFocused {
		return []key.Binding{m.keys.Left, m.keys.Right, m.keys.Down, m.keys.Help, m.keys.Quit}
	}
	return m.active.helpBindings()
}

func (m *Model) currentFullHelp() [][]key.Binding {
	if m.tabFocused {
		return [][]key.Binding{{m.keys.Left, m.keys.Right, m.keys.Down}, {m.keys.Help, m.keys.Quit}}
	}
	return m.active.fullHelpBindings()
}

func (m *Model) resizeActive() tea.Cmd {
	h := m.height - headerHeight - m.footerHeight()
	if h < 0 {
		h = 0
	}
	next, cmd := m.active.Update(resizeMsg{width: m.width, height: h})
	m.active = next
	m.syncActive(next)
	return cmd
}

// syncActive keeps the typed screen pointers aligned with the active screen so
// routing helpers can reach concrete state.
func (m *Model) syncActive(s screen) {
	switch v := s.(type) {
	case *sandboxesScreen:
		m.list = v
	case *terminalScreen:
		m.terminal = v
	case *newSessionScreen:
		m.form = v
	case *harnessesScreen:
		m.harnesses = v
	case *harnessFormScreen:
		m.harnessForm = v
	case *secretsScreen:
		m.secrets = v
	}
}

func (m *Model) enterTerminal(sb Sandbox) tea.Cmd {
	term := newTerminalScreen(m.ctx, m.ds, m.keys, m.styles, sb)
	m.terminal = term
	m.active = term
	m.tabFocused = false
	m.statusError = false
	m.statusText = fmt.Sprintf("attaching to %s…", displayName(sb))
	return tea.Batch(term.Init(), m.resizeActive())
}

// runFullscreenAttach suspends the TUI, restoring the normal screen buffer,
// while the CLI terminal attach flow owns the real terminal.
func (m *Model) runFullscreenAttach(sb Sandbox) tea.Cmd {
	m.statusError = false
	m.statusText = fmt.Sprintf("attaching to %s fullscreen…", displayName(sb))
	exec := &terminalAttachExec{ctx: m.ctx, ds: m.ds, sandboxID: sb.ID}
	return tea.Exec(exec, func(err error) tea.Msg {
		return fullscreenFinishedMsg{sandbox: sb, err: err}
	})
}

func (m *Model) fullscreenFinished(msg fullscreenFinishedMsg) {
	if msg.err != nil {
		m.statusError = true
		m.statusText = fmt.Sprintf("fullscreen attach to %s failed: %v", displayName(msg.sandbox), msg.err)
		return
	}
	m.statusError = false
	m.statusText = fmt.Sprintf("detached from %s", displayName(msg.sandbox))
}

func (m *Model) enterNewForm() tea.Cmd {
	form := newNewSessionScreen(m.ctx, m.ds, m.keys, m.styles)
	m.form = form
	m.active = form
	m.tabFocused = false
	m.statusError = false
	m.statusText = ""
	return tea.Batch(form.Init(), m.resizeActive())
}

// handleTabKey routes key input while the tab bar holds focus: h/l move between
// tabs, Down/Enter drop into the active tab's body, and the shared quit/help
// keys still work.
func (m *Model) handleTabKey(msg tea.KeyPressMsg) tea.Cmd {
	switch {
	case key.Matches(msg, m.keys.Quit):
		return tea.Quit
	case key.Matches(msg, m.keys.Help):
		m.showFullHelp = !m.showFullHelp
		return m.resizeActive()
	case key.Matches(msg, m.keys.Left):
		return m.switchTab(m.activeTab-1, true)
	case key.Matches(msg, m.keys.Right):
		return m.switchTab(m.activeTab+1, true)
	case key.Matches(msg, m.keys.Down), key.Matches(msg, m.keys.Enter):
		m.tabFocused = false
		return nil
	}
	return nil
}

// switchTab makes the given tab active, building and initializing its screen on
// first use. keepBarFocus preserves tab-bar focus (so h/l keep browsing tabs);
// callers that jump straight into a tab body pass false. Out-of-range indices
// are clamped, so h at the first tab and l at the last are no-ops.
func (m *Model) switchTab(t tabID, keepBarFocus bool) tea.Cmd {
	if t < tabSandboxes {
		t = tabSandboxes
	}
	if t > tabSecrets {
		t = tabSecrets
	}
	m.activeTab = t
	m.tabFocused = keepBarFocus
	m.statusError = false
	m.statusText = ""

	var initCmd tea.Cmd
	switch t {
	case tabSandboxes:
		m.harnessForm = nil
		m.active = m.list
	case tabAgents:
		if m.harnesses == nil {
			m.harnesses = newHarnessesScreen(m.ctx, m.ds, m.keys, m.styles)
			initCmd = m.harnesses.Init()
		}
		m.harnessForm = nil
		m.active = m.harnesses
	case tabSecrets:
		if m.secrets == nil {
			m.secrets = newSecretsScreen(m.keys, m.styles)
			initCmd = m.secrets.Init()
		}
		m.active = m.secrets
	}
	return tea.Batch(initCmd, m.resizeActive())
}

// isTabScreen reports whether s is one of the top-level tab bodies (as opposed
// to a sub-screen such as a terminal or form), so the root only renders and
// arms the tab bar when a tab body is showing.
func (m *Model) isTabScreen(s screen) bool {
	switch {
	case s == nil:
		return false
	case s == screen(m.list):
		return true
	case m.harnesses != nil && s == screen(m.harnesses):
		return true
	case m.secrets != nil && s == screen(m.secrets):
		return true
	}
	return false
}

// enterHarnessForm opens the harness create/edit form. A nil edit target creates.
func (m *Model) enterHarnessForm(edit *HarnessConfig) tea.Cmd {
	form := newHarnessFormScreen(m.ctx, m.ds, m.keys, m.styles, edit)
	m.harnessForm = form
	m.active = form
	m.tabFocused = false
	m.statusError = false
	m.statusText = ""
	return tea.Batch(form.Init(), m.resizeActive())
}

// goToHarnesses returns to the coding-agents tab, tearing down the form.
func (m *Model) goToHarnesses() tea.Cmd {
	m.harnessForm = nil
	if m.harnesses == nil {
		m.harnesses = newHarnessesScreen(m.ctx, m.ds, m.keys, m.styles)
	}
	m.activeTab = tabAgents
	m.tabFocused = false
	m.active = m.harnesses
	return tea.Batch(m.harnesses.refreshCmd(), m.resizeActive())
}

// harnessSaved returns to the coding-agents screen, queues the saved config for
// selection, and refreshes so it appears. A freshly created agent then runs its
// configure flow (the form's submit is "Run configure").
func (m *Model) harnessSaved(msg harnessSavedMsg) tea.Cmd {
	if m.harnesses != nil {
		m.harnesses.selectID = msg.config.ID
	}
	cmd := m.goToHarnesses()
	m.statusError = false
	if msg.created {
		m.statusText = fmt.Sprintf("created coding agent %s — running configure…", harnessDisplayName(msg.config))
		return tea.Batch(cmd, m.runConfigureCmd(msg.config))
	}
	m.statusText = fmt.Sprintf("updated coding agent %s", harnessDisplayName(msg.config))
	return cmd
}

// runConfigure makes the agents screen active and starts an agent's configure
// flow (from the list's `c` key).
func (m *Model) runConfigure(cfg HarnessConfig) tea.Cmd {
	if m.harnesses != nil {
		m.active = m.harnesses
	}
	m.statusError = false
	m.statusText = "running configure for " + harnessDisplayName(cfg) + "…"
	return m.runConfigureCmd(cfg)
}

// runConfigureCmd suspends the TUI via tea.Exec and hands the terminal to the
// harness configure flow, resuming with a harnessConfiguredMsg when it exits.
func (m *Model) runConfigureCmd(cfg HarnessConfig) tea.Cmd {
	exec := &configureExec{ctx: m.ctx, ds: m.ds, harnessID: cfg.ID}
	name := harnessDisplayName(cfg)
	return tea.Exec(exec, func(err error) tea.Msg {
		return harnessConfiguredMsg{name: name, err: err}
	})
}

// harnessConfigured reports the outcome of a configure run and refreshes the
// agents list.
func (m *Model) harnessConfigured(msg harnessConfiguredMsg) tea.Cmd {
	if msg.err != nil {
		m.statusError = true
		m.statusText = fmt.Sprintf("configure %s failed: %v", msg.name, msg.err)
	} else {
		m.statusError = false
		m.statusText = "configured " + msg.name
	}
	if m.harnesses != nil {
		return m.harnesses.refreshCmd()
	}
	return nil
}

func (m *Model) setHarnessDeleteStatus(msg harnessDeletedMsg) {
	var failed int
	var firstErr error
	for _, err := range msg.errs {
		if err != nil {
			failed++
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if failed == 0 {
		m.statusError = false
		m.statusText = fmt.Sprintf("deleted %s", countLabel(len(msg.ids), "coding agent", "coding agents"))
		return
	}
	m.statusError = true
	if len(msg.ids) == 1 {
		// A single delete surfaces the server's reason (e.g. the default agent
		// cannot be deleted) rather than a bare count.
		m.statusText = fmt.Sprintf("delete failed: %v", firstErr)
		return
	}
	m.statusText = fmt.Sprintf("delete failed for %d of %d: %v", failed, len(msg.ids), firstErr)
}

// sessionCreated returns to the list, queues the new sandbox for selection, and
// refreshes so it appears.
func (m *Model) sessionCreated(sb Sandbox) tea.Cmd {
	m.list.selectID = sb.ID
	m.statusError = false
	m.statusText = fmt.Sprintf("created %s", displayName(sb))
	cmd := m.goToList()
	return tea.Batch(cmd, m.list.refreshCmd())
}

// goToList makes the sandbox list active again, tearing down any live terminal.
func (m *Model) goToList() tea.Cmd {
	if m.terminal != nil {
		m.terminal.close()
		m.terminal = nil
	}
	m.form = nil
	m.harnessForm = nil
	m.activeTab = tabSandboxes
	m.tabFocused = false
	m.active = m.list
	// Refresh the list on return and resize it to the reclaimed space.
	return tea.Batch(m.list.refreshCmd(), m.resizeActive())
}

func (m *Model) setDeleteStatus(msg deletedMsg) {
	var failed int
	for _, err := range msg.errs {
		if err != nil {
			failed++
		}
	}
	if failed == 0 {
		m.statusError = false
		m.statusText = fmt.Sprintf("deleted %s", countLabel(len(msg.ids), "sandbox", "sandboxes"))
		return
	}
	m.statusError = true
	m.statusText = fmt.Sprintf("delete failed for %d of %d", failed, len(msg.ids))
}

func (m *Model) renderHeader() string {
	if m.isTabScreen(m.active) {
		return m.renderTabBar()
	}
	// Sub-screens (terminal, forms) show a breadcrumb rather than the tab bar,
	// since the tab bar is not reachable from within them.
	title := m.styles.header.Render("discobox")
	sub := m.styles.headerValue.Render(m.active.title())
	return title + m.styles.headerKey.Render(" › ") + sub
}

// renderTabBar draws the top-level tab strip: the brand, then each tab styled by
// state (inactive, active, or active while the bar holds focus), plus a
// right-aligned count for the active tab.
func (m *Model) renderTabBar() string {
	brand := m.styles.header.Render("discobox")
	cells := make([]string, len(tabTitles))
	for i := range tabTitles {
		label := " " + tabTitles[i] + " "
		switch {
		case tabID(i) == m.activeTab && m.tabFocused:
			cells[i] = m.styles.tabFocused.Render(label)
		case tabID(i) == m.activeTab:
			cells[i] = m.styles.tabActive.Render(label)
		default:
			cells[i] = m.styles.tab.Render(label)
		}
	}
	left := brand + "  " + strings.Join(cells, " ")

	right := m.tabCount()
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return left
	}
	return left + strings.Repeat(" ", gap) + right
}

// tabCount renders the right-aligned count for the active tab. The secrets
// placeholder has nothing to count.
func (m *Model) tabCount() string {
	switch m.activeTab {
	case tabSandboxes:
		if m.list != nil {
			return m.styles.headerKey.Render("sandboxes ") + m.styles.headerValue.Render(fmt.Sprint(m.list.count()))
		}
	case tabAgents:
		if m.harnesses != nil {
			return m.styles.headerKey.Render("agents ") + m.styles.headerValue.Render(fmt.Sprint(m.harnesses.count()))
		}
	}
	return ""
}

func (m *Model) renderFooter() string {
	var helpView string
	if m.showFullHelp {
		helpView = m.help.FullHelpView(m.currentFullHelp())
	} else {
		helpView = m.help.ShortHelpView(m.currentShortHelp())
	}

	status := ""
	if m.statusText != "" {
		if m.statusError {
			status = m.styles.statusError.Render(m.statusText)
		} else {
			status = m.styles.status.Render(m.statusText)
		}
	}
	return lipgloss.JoinVertical(lipgloss.Left, helpView, status)
}

// displayName prefers a sandbox's name, falling back to its ID.
func displayName(sb Sandbox) string {
	if strings.TrimSpace(sb.Name) != "" {
		return sb.Name
	}
	return sb.ID
}

// countLabel formats a count with a singular/plural noun ("1 sandbox").
func countLabel(n int, singular, plural string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %s", n, plural)
}
