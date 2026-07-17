package tui

import (
	"context"
	"fmt"
	"sort"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// harnessesScreen is the "coding agents" tab: a live, k9s-style table of the
// project's harness configs, styled to match the sandbox list. It carries the
// full CRUD plus set-default and configure: `n` new, `e` edit (rename), `c`
// configure, `d` delete, `enter` set default (both `c`/`enter` confirm). A
// full-width "+ New agent" selector sits above the table (mirroring the sandbox
// list's "new session" affordance); moving up off row 0 selects it, moving up
// off it surfaces focus to the tab bar.
type harnessesScreen struct {
	ctx    context.Context
	ds     DataSource
	keys   keyMap
	styles styles

	table         table.Model
	tableActive   table.Styles
	tableInactive table.Styles
	configs       []HarnessConfig

	width  int
	height int

	loaded   bool
	selectID string      // config to focus after the next load (e.g. just saved)
	confirm  confirmKind // which action is awaiting a y/n confirmation
	pending  string      // id the pending confirmation acts on
}

// confirmKind is the action a confirmation dialog is gating.
type confirmKind int

const (
	confirmNone confirmKind = iota
	confirmDeconfigure
	confirmSetDefault
)

func newHarnessesScreen(ctx context.Context, ds DataSource, keys keyMap, st styles) *harnessesScreen {
	active := st.table
	inactive := st.table
	// When the "new" selector holds focus the table shows no highlighted row; the
	// Selected style must be plain (not Cell) so it adds no padding that would
	// shift the cursor row. Matches sandboxesScreen.
	inactive.Selected = lipgloss.NewStyle()
	t := table.New(
		table.WithFocused(true),
		table.WithKeyMap(navKeyMap(keys)),
		table.WithStyles(inactive),
	)
	return &harnessesScreen{
		ctx:           ctx,
		ds:            ds,
		keys:          keys,
		styles:        st,
		table:         t,
		tableActive:   active,
		tableInactive: inactive,
	}
}

func (s *harnessesScreen) Init() tea.Cmd {
	return s.refreshCmd()
}

func (s *harnessesScreen) Update(msg tea.Msg) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case resizeMsg:
		s.setSize(msg.width, msg.height)
		return s, nil

	case harnessesLoadedMsg:
		s.applyConfigs(msg.configs)
		return s, nil

	case tea.KeyPressMsg:
		return s.handleKey(msg)
	}
	return s, nil
}

func (s *harnessesScreen) handleKey(msg tea.KeyPressMsg) (screen, tea.Cmd) {
	if s.confirm != confirmNone {
		return s.handleConfirmKey(msg)
	}
	switch {
	case key.Matches(msg, s.keys.Quit):
		return s, tea.Quit
	case key.Matches(msg, s.keys.Help):
		return s, func() tea.Msg { return toggleHelpMsg{} }
	case key.Matches(msg, s.keys.Back):
		return s, func() tea.Msg { return backMsg{} }
	case key.Matches(msg, s.keys.Refresh):
		return s, s.refreshCmd()
	case key.Matches(msg, s.keys.Configure):
		// Configure is the "enable" half: it runs the agent's interactive flow,
		// and only a configured agent can be run.
		if cfg, ok := s.cursorConfig(); ok {
			c := cfg
			return s, func() tea.Msg { return runConfigureMsg{harness: c} }
		}
		return s, nil
	case key.Matches(msg, s.keys.Deconfigure):
		s.beginDeconfigure()
		return s, nil
	case key.Matches(msg, s.keys.Up):
		return s, s.moveUp()
	case key.Matches(msg, s.keys.Down):
		s.moveDown()
		return s, nil
	case key.Matches(msg, s.keys.Top):
		s.table.GotoTop()
		return s, nil
	case key.Matches(msg, s.keys.Bottom):
		s.table.GotoBottom()
		return s, nil
	case key.Matches(msg, s.keys.Default):
		return s, s.beginSetDefault()
	default:
		var cmd tea.Cmd
		s.table, cmd = s.table.Update(msg)
		return s, cmd
	}
}

func (s *harnessesScreen) handleConfirmKey(msg tea.KeyPressMsg) (screen, tea.Cmd) {
	switch msg.String() {
	case "y", "enter":
		kind, id := s.confirm, s.pending
		s.confirm = confirmNone
		s.pending = ""
		if id == "" {
			return s, nil
		}
		cfg, _ := s.configByID(id)
		switch kind {
		case confirmDeconfigure:
			return s, s.deconfigureCmd(id)
		case confirmSetDefault:
			return s, s.setDefaultCmd(cfg)
		}
		return s, nil
	case "n", "esc", "q":
		s.confirm = confirmNone
		s.pending = ""
		return s, nil
	}
	return s, nil
}

// beginDeconfigure asks to confirm undoing an agent's configuration. An agent
// that was never configured is a no-op reported on the status line.
func (s *harnessesScreen) beginDeconfigure() {
	cfg, ok := s.cursorConfig()
	if !ok {
		return
	}
	if !cfg.Configured {
		return
	}
	s.pending = cfg.ID
	s.confirm = confirmDeconfigure
}

// beginSetDefault asks to confirm making the cursor agent the project default.
// The current default is a no-op reported on the status line, with no dialog.
func (s *harnessesScreen) beginSetDefault() tea.Cmd {
	cfg, ok := s.cursorConfig()
	if !ok {
		return nil
	}
	if cfg.Default {
		return func() tea.Msg { return statusMsg{text: harnessDisplayName(cfg) + " is already the default"} }
	}
	s.pending = cfg.ID
	s.confirm = confirmSetDefault
	return nil
}

// moveUp walks up the table, then off its top row onto the tab bar (via
// focusTabsCmd).
func (s *harnessesScreen) moveUp() tea.Cmd {
	if s.table.Cursor() <= 0 {
		return focusTabsCmd()
	}
	s.table.MoveUp(1)
	return nil
}

func (s *harnessesScreen) moveDown() {
	s.table.MoveDown(1)
}

func (s *harnessesScreen) title() string { return "coding agents" }

func (s *harnessesScreen) helpBindings() []key.Binding {
	return []key.Binding{
		s.keys.Up, s.keys.Down, s.keys.Configure, s.keys.Deconfigure, s.keys.Default, s.keys.Refresh, s.keys.Help, s.keys.Quit,
	}
}

func (s *harnessesScreen) fullHelpBindings() [][]key.Binding {
	return [][]key.Binding{
		{s.keys.Up, s.keys.Down, s.keys.Top, s.keys.Bottom},
		{s.keys.New, s.keys.Edit, s.keys.Configure, s.keys.Default, s.keys.Delete},
		{s.keys.Refresh, s.keys.Back, s.keys.Help, s.keys.Quit},
	}
}

// cursor satisfies the screen interface (no live hardware cursor in the table).
func (s *harnessesScreen) cursor(int, int) *tea.Cursor { return nil }

func (s *harnessesScreen) count() int { return len(s.configs) }

func (s *harnessesScreen) View(width, height int) string {
	s.setSize(width, height)
	if s.confirm != confirmNone {
		return s.centered(width, height, s.confirmDialog())
	}
	switch {
	case !s.loaded:
		return s.centered(width, height, s.styles.status.Render("loading coding agents…"))
	case len(s.configs) == 0:
		return s.centered(width, height, s.styles.status.Render("no coding agents"))
	}
	return s.table.View()
}

func (s *harnessesScreen) confirmDialog() string {
	cfg, _ := s.configByID(s.pending)
	var prompt string
	switch s.confirm {
	case confirmSetDefault:
		prompt = fmt.Sprintf("Set %s as the default coding agent?", harnessDisplayName(cfg))
	default:
		prompt = fmt.Sprintf("Delete coding agent %s?", harnessDisplayName(cfg))
	}
	yes := s.styles.dialogKey.Render("y")
	no := s.styles.dialogKey.Render("n")
	return s.styles.dialog.Render(fmt.Sprintf("%s   %s confirm   %s cancel", prompt, yes, no))
}

// refreshCmd loads the harness configs off the UI goroutine.
func (s *harnessesScreen) refreshCmd() tea.Cmd {
	return func() tea.Msg {
		configs, err := s.ds.ListHarnessConfigs(s.ctx)
		if err != nil {
			return errMsg{context: "list agents", err: err}
		}
		return harnessesLoadedMsg{configs: configs}
	}
}

// deconfigureCmd undoes an agent's configuration: the secrets and files its
// configure flow created are removed and it becomes unrunnable until configured
// again. The agent itself stays.
func (s *harnessesScreen) deconfigureCmd(id string) tea.Cmd {
	ds, ctx := s.ds, s.ctx
	return func() tea.Msg {
		if err := ds.DeconfigureHarness(ctx, id); err != nil {
			return errMsg{context: "deconfigure agent", err: err}
		}
		return harnessDeconfiguredMsg{id: id}
	}
}

func (s *harnessesScreen) setDefaultCmd(cfg HarnessConfig) tea.Cmd {
	if cfg.Default {
		return func() tea.Msg { return statusMsg{text: harnessDisplayName(cfg) + " is already the default"} }
	}
	ds, ctx := s.ds, s.ctx
	s.selectID = cfg.ID
	return func() tea.Msg {
		if err := ds.SetDefaultHarness(ctx, cfg.ID); err != nil {
			return errMsg{context: "set default", err: err}
		}
		return statusMsg{text: "default coding agent set to " + harnessDisplayName(cfg)}
	}
}

// applyConfigs stores a fresh list (sorted by creation), rebuilds the table, and
// keeps the cursor on a queued selection or clamps it into range.
func (s *harnessesScreen) applyConfigs(list []HarnessConfig) {
	sorted := make([]HarnessConfig, len(list))
	copy(sorted, list)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Created.Before(sorted[j].Created) })
	s.configs = sorted
	s.loaded = true
	s.rebuildRows()

	// Focus a just-saved agent once it appears in the list.
	if s.selectID != "" {
		for i, cfg := range sorted {
			if cfg.ID == s.selectID {
				s.table.SetCursor(i)
				s.selectID = ""
				break
			}
		}
	}
	s.table.SetStyles(s.tableActive)
}

func (s *harnessesScreen) rebuildRows() {
	cursor := s.table.Cursor()
	s.table.SetColumns(s.columns(s.width))
	rows := make([]table.Row, 0, len(s.configs))
	for _, cfg := range s.configs {
		rows = append(rows, s.row(cfg))
	}
	s.table.SetRows(rows)
	if cursor >= len(rows) {
		cursor = len(rows) - 1
	}
	if cursor < 0 {
		cursor = 0
	}
	s.table.SetCursor(cursor)
}

// row renders one agent: a ★ default marker, id, display name, slug, whether it
// is configured (an unconfigured agent cannot be run), and age.
func (s *harnessesScreen) row(cfg HarnessConfig) table.Row {
	star := ""
	if cfg.Default {
		star = lipgloss.NewStyle().Foreground(colorMark).Render("★")
	}
	configured := lipgloss.NewStyle().Foreground(colorMuted).Render("no")
	if cfg.Configured {
		configured = lipgloss.NewStyle().Foreground(colorMark).Render("yes")
	}
	return table.Row{
		star,
		cfg.ID,
		harnessDisplayName(cfg),
		cfg.Slug,
		configured,
		humanizeAge(cfg.Updated),
	}
}

// columns allocates fixed widths for the structured columns and splits the
// remainder between NAME and IMAGE, which hold the most variable content.
func (s *harnessesScreen) columns(width int) []table.Column {
	const (
		starW = 2
		idW   = 22
		slugW = 16
		cfgW  = 10
		ageW  = 12
		// Each column carries 2 cells of horizontal padding from the Cell style.
		padPerCol = 2
		numCols   = 6
	)
	fixed := starW + idW + slugW + cfgW + ageW + padPerCol*numCols
	remaining := width - fixed
	if remaining < 20 {
		remaining = 20
	}
	nameW := remaining
	if nameW < 12 {
		nameW = 12
	}
	return []table.Column{
		{Title: "", Width: starW},
		{Title: "ID", Width: idW},
		{Title: "NAME", Width: nameW},
		{Title: "SLUG", Width: slugW},
		{Title: "CONFIGURED", Width: cfgW},
		{Title: "AGE", Width: ageW},
	}
}

func (s *harnessesScreen) setSize(width, height int) {
	if width == s.width && height == s.height {
		return
	}
	s.width, s.height = width, height
	s.table.SetWidth(width)
	// Reserve one row for the "new" selector and one for the table header.
	rows := height - 2
	if rows < 1 {
		rows = 1
	}
	s.table.SetHeight(rows)
	if s.loaded {
		s.rebuildRows()
	}
}

// cursorConfig returns the highlighted agent, or false when the list is empty.
func (s *harnessesScreen) cursorConfig() (HarnessConfig, bool) {
	i := s.table.Cursor()
	if i < 0 || i >= len(s.configs) {
		return HarnessConfig{}, false
	}
	return s.configs[i], true
}

func (s *harnessesScreen) configByID(id string) (HarnessConfig, bool) {
	for _, cfg := range s.configs {
		if cfg.ID == id {
			return cfg, true
		}
	}
	return HarnessConfig{}, false
}

func (s *harnessesScreen) centered(width, height int, content string) string {
	if width <= 0 || height <= 0 {
		return content
	}
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, content)
}
func harnessDisplayName(cfg HarnessConfig) string {
	if cfg.Name != "" {
		return cfg.Name
	}
	if cfg.Slug != "" {
		return cfg.Slug
	}
	return cfg.ID
}
