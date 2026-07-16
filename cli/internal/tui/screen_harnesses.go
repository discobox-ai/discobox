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

	loaded      bool
	newSelected bool        // the "new agent" selector above the table has focus
	selectID    string      // config to focus after the next load (e.g. just saved)
	confirm     confirmKind // which action is awaiting a y/n confirmation
	pending     string      // id the pending confirmation acts on
}

// confirmKind is the action a confirmation dialog is gating.
type confirmKind int

const (
	confirmNone confirmKind = iota
	confirmDelete
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
		newSelected:   true, // start on "new agent"
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

	case harnessDeletedMsg:
		return s, s.refreshCmd()

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
	case key.Matches(msg, s.keys.New):
		return s, func() tea.Msg { return openHarnessFormMsg{} }
	case key.Matches(msg, s.keys.Edit):
		if cfg, ok := s.cursorConfig(); ok {
			c := cfg
			return s, func() tea.Msg { return openHarnessFormMsg{edit: &c} }
		}
		return s, nil
	case key.Matches(msg, s.keys.Delete):
		s.beginDelete()
		return s, nil
	case key.Matches(msg, s.keys.Configure):
		if cfg, ok := s.cursorConfig(); ok {
			c := cfg
			return s, func() tea.Msg { return runConfigureMsg{harness: c} }
		}
		return s, nil
	case key.Matches(msg, s.keys.Up):
		return s, s.moveUp()
	case key.Matches(msg, s.keys.Down):
		s.moveDown()
		return s, nil
	case key.Matches(msg, s.keys.Top):
		s.selectNew()
		return s, nil
	case key.Matches(msg, s.keys.Bottom):
		s.selectRows()
		s.table.GotoBottom()
		return s, nil
	case key.Matches(msg, s.keys.Default):
		// enter: create from the selector, otherwise make the cursor agent default.
		if s.newSelected {
			return s, func() tea.Msg { return openHarnessFormMsg{} }
		}
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
		case confirmDelete:
			return s, s.deleteCmd(id)
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

func (s *harnessesScreen) beginDelete() {
	cfg, ok := s.cursorConfig()
	if !ok {
		return
	}
	s.pending = cfg.ID
	s.confirm = confirmDelete
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

// selectNew moves focus to the "new agent" selector above the table.
func (s *harnessesScreen) selectNew() {
	s.newSelected = true
	s.applyTableFocus()
}

// selectRows moves focus into the table, unless it is empty.
func (s *harnessesScreen) selectRows() {
	if len(s.configs) == 0 {
		return
	}
	s.newSelected = false
	s.applyTableFocus()
}

// moveUp walks up the table, then off its top row onto the "new" selector, and
// finally off the selector up onto the tab bar (via focusTabsCmd).
func (s *harnessesScreen) moveUp() tea.Cmd {
	if s.newSelected {
		return focusTabsCmd()
	}
	if s.table.Cursor() <= 0 {
		s.selectNew()
		return nil
	}
	s.table.MoveUp(1)
	return nil
}

// moveDown steps off the "new" selector into the table, then down its rows.
func (s *harnessesScreen) moveDown() {
	if s.newSelected {
		s.selectRows()
		return
	}
	s.table.MoveDown(1)
}

// applyTableFocus swaps the table styles so only the focused region highlights.
func (s *harnessesScreen) applyTableFocus() {
	if s.newSelected {
		s.table.SetStyles(s.tableInactive)
	} else {
		s.table.SetStyles(s.tableActive)
	}
}

func (s *harnessesScreen) title() string { return "coding agents" }

func (s *harnessesScreen) helpBindings() []key.Binding {
	return []key.Binding{
		s.keys.Up, s.keys.Down, s.keys.New, s.keys.Edit, s.keys.Configure, s.keys.Default, s.keys.Delete, s.keys.Refresh, s.keys.Help, s.keys.Quit,
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
	selector := s.newSelectorLine(width)
	bodyHeight := height - 1 // the selector occupies the first row
	var body string
	switch {
	case !s.loaded:
		body = s.centered(width, bodyHeight, s.styles.status.Render("loading coding agents…"))
	case len(s.configs) == 0:
		body = s.centered(width, bodyHeight, s.styles.status.Render("no coding agents yet — press n or enter to create one"))
	default:
		body = s.table.View()
	}
	return selector + "\n" + body
}

// newSelectorLine renders the full-width "new agent" affordance above the table,
// highlighted when it holds focus.
func (s *harnessesScreen) newSelectorLine(width int) string {
	label := "+ New agent"
	if s.newSelected {
		return s.styles.newActive.Width(width).Render(label)
	}
	return s.styles.newInactive.Render(label)
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

func (s *harnessesScreen) deleteCmd(id string) tea.Cmd {
	ds, ctx := s.ds, s.ctx
	return func() tea.Msg {
		err := ds.DeleteHarness(ctx, id)
		return harnessDeletedMsg{ids: []string{id}, errs: []error{err}}
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
				s.newSelected = false
				s.table.SetCursor(i)
				s.selectID = ""
				break
			}
		}
	}
	s.applyTableFocus()
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

// row renders one agent: a ★ default marker, id, display name, slug, image, age.
func (s *harnessesScreen) row(cfg HarnessConfig) table.Row {
	star := ""
	if cfg.Default {
		star = lipgloss.NewStyle().Foreground(colorMark).Render("★")
	}
	return table.Row{
		star,
		cfg.ID,
		harnessDisplayName(cfg),
		cfg.Slug,
		cfg.Image,
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
		ageW  = 12
		// Each column carries 2 cells of horizontal padding from the Cell style.
		padPerCol = 2
		numCols   = 6
	)
	fixed := starW + idW + slugW + ageW + padPerCol*numCols
	remaining := width - fixed
	if remaining < 20 {
		remaining = 20
	}
	nameW := remaining / 2
	if nameW < 12 {
		nameW = 12
	}
	imageW := remaining - nameW
	if imageW < 8 {
		imageW = 8
	}
	return []table.Column{
		{Title: "", Width: starW},
		{Title: "ID", Width: idW},
		{Title: "NAME", Width: nameW},
		{Title: "SLUG", Width: slugW},
		{Title: "IMAGE", Width: imageW},
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

// cursorConfig returns the highlighted agent, or false when the "new" selector
// holds focus or the list is empty.
func (s *harnessesScreen) cursorConfig() (HarnessConfig, bool) {
	if s.newSelected {
		return HarnessConfig{}, false
	}
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

// dialogBox renders content inside the harness dialog frame at a stable width:
// at least two-thirds of the screen, expanding only when the content is wider.
// A fixed floor keeps the box from resizing as its contents change (e.g. the
// form's fields or an open dropdown). Used by the harness create/edit form.
func dialogBox(style lipgloss.Style, screenWidth int, content string) string {
	// lipgloss Width sets the total rendered box width (border and padding
	// included), so floor against the naturally-sized box.
	natural := lipgloss.Width(style.Render(content))
	target := screenWidth * 2 / 3
	if target > natural {
		return style.Width(target).Render(content)
	}
	return style.Render(content)
}

// harnessDisplayName prefers a config's name, then its slug, then its ID.
func harnessDisplayName(cfg HarnessConfig) string {
	if cfg.Name != "" {
		return cfg.Name
	}
	if cfg.Slug != "" {
		return cfg.Slug
	}
	return cfg.ID
}
