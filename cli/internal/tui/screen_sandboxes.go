package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// refreshInterval is how often the list auto-reloads from the control plane.
const refreshInterval = 3 * time.Second

// sandboxesScreen is the primary screen: a live, k9s-style table of the
// project's sandboxes with vim navigation, multi-row marking, confirm-delete,
// and attach-on-enter.
type sandboxesScreen struct {
	ctx    context.Context
	ds     DataSource
	keys   keyMap
	styles styles

	table         table.Model
	tableActive   table.Styles
	tableInactive table.Styles
	sandboxes     []Sandbox
	marked        map[string]bool

	width  int
	height int

	loaded      bool
	confirming  bool
	pending     []Sandbox // sandboxes queued for deletion once confirmed
	newSelected bool      // the "new session" selector above the table has focus
	selectID    string    // sandbox to focus after the next load (e.g. just created)

	// visual (vim-style range select): while active, moving the cursor paints the
	// rows between the anchor and the cursor as marked, on top of visualBase (the
	// marks that existed when visual mode began).
	visual     bool
	anchor     int
	visualBase map[string]bool
}

func newSandboxesScreen(ctx context.Context, ds DataSource, keys keyMap, st styles) *sandboxesScreen {
	active := st.table
	inactive := st.table
	// When the "new" selector holds focus, the table shows no highlighted row.
	// The Selected style wraps the whole rendered row, so it must be a plain
	// style: reusing Cell here would add its horizontal padding and shift the
	// cursor row one space to the right of every other row.
	inactive.Selected = lipgloss.NewStyle()
	t := table.New(
		table.WithFocused(true),
		table.WithKeyMap(navKeyMap(keys)),
		table.WithStyles(inactive),
	)
	return &sandboxesScreen{
		ctx:           ctx,
		ds:            ds,
		keys:          keys,
		styles:        st,
		table:         t,
		tableActive:   active,
		tableInactive: inactive,
		marked:        map[string]bool{},
		newSelected:   true, // start on "new session"
	}
}

// navKeyMap restricts the table to navigation only, keeping space and d free for
// the screen's mark and delete actions.
func navKeyMap(k keyMap) table.KeyMap {
	return table.KeyMap{
		LineUp:       k.Up,
		LineDown:     k.Down,
		GotoTop:      k.Top,
		GotoBottom:   k.Bottom,
		PageUp:       key.NewBinding(key.WithKeys("pgup", "ctrl+b")),
		PageDown:     key.NewBinding(key.WithKeys("pgdown", "ctrl+f")),
		HalfPageUp:   key.NewBinding(key.WithKeys("ctrl+u")),
		HalfPageDown: key.NewBinding(key.WithKeys("ctrl+d")),
	}
}

func (s *sandboxesScreen) Init() tea.Cmd {
	return tea.Batch(s.refreshCmd(), s.tickCmd())
}

func (s *sandboxesScreen) Update(msg tea.Msg) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case resizeMsg:
		s.setSize(msg.width, msg.height)
		return s, nil

	case sandboxesLoadedMsg:
		s.applySandboxes(msg.sandboxes)
		return s, nil

	case tickMsg:
		return s, tea.Batch(s.refreshCmd(), s.tickCmd())

	case deletedMsg:
		// Drop marks for anything that deleted cleanly, then reload.
		for i, id := range msg.ids {
			if i < len(msg.errs) && msg.errs[i] == nil {
				delete(s.marked, id)
			}
		}
		return s, s.refreshCmd()

	case tea.KeyPressMsg:
		return s.handleKey(msg)
	}
	return s, nil
}

func (s *sandboxesScreen) handleKey(msg tea.KeyPressMsg) (screen, tea.Cmd) {
	if s.confirming {
		return s.handleConfirmKey(msg)
	}
	switch {
	case key.Matches(msg, s.keys.Quit):
		return s, tea.Quit
	case key.Matches(msg, s.keys.Help):
		return s, func() tea.Msg { return toggleHelpMsg{} }
	case key.Matches(msg, s.keys.New):
		return s, openNewCmd()
	case key.Matches(msg, s.keys.Agents):
		return s, func() tea.Msg { return openHarnessesMsg{} }
	case key.Matches(msg, s.keys.Refresh):
		return s, s.refreshCmd()
	case key.Matches(msg, s.keys.Up):
		return s, s.moveUp()
	case key.Matches(msg, s.keys.Down):
		s.moveDown()
		return s, nil
	case key.Matches(msg, s.keys.Top):
		if s.visual {
			s.table.GotoTop()
			s.repaintVisual()
		} else {
			s.selectNew()
		}
		return s, nil
	case key.Matches(msg, s.keys.Bottom):
		if s.visual {
			s.table.GotoBottom()
			s.repaintVisual()
		} else {
			s.selectRows()
			s.table.GotoBottom()
		}
		return s, nil
	case key.Matches(msg, s.keys.Visual):
		if !s.newSelected {
			s.toggleVisual()
		}
		return s, nil
	case key.Matches(msg, s.keys.Back):
		if s.visual {
			s.cancelVisual()
		}
		return s, nil
	case key.Matches(msg, s.keys.Mark):
		if !s.newSelected {
			s.toggleMark()
		}
		return s, nil
	case key.Matches(msg, s.keys.SelectAll):
		if !s.newSelected {
			s.toggleSelectAll()
		}
		return s, nil
	case key.Matches(msg, s.keys.Delete):
		if !s.newSelected {
			s.exitVisual() // commit the visual selection, then confirm
			s.beginDelete()
		}
		return s, nil
	case key.Matches(msg, s.keys.Enter):
		if s.newSelected {
			return s, openNewCmd()
		}
		if sb, ok := s.cursorSandbox(); ok {
			return s, func() tea.Msg { return selectSandboxMsg{sandbox: sb} }
		}
		return s, nil
	case key.Matches(msg, s.keys.Fullscreen):
		if sb, ok := s.cursorSandbox(); ok && !s.newSelected {
			return s, func() tea.Msg { return fullscreenSandboxMsg{sandbox: sb} }
		}
		return s, nil
	default:
		var cmd tea.Cmd
		s.table, cmd = s.table.Update(msg)
		return s, cmd
	}
}

func openNewCmd() tea.Cmd {
	return func() tea.Msg { return openNewMsg{} }
}

// selectNew moves focus to the "new session" selector above the table.
func (s *sandboxesScreen) selectNew() {
	s.newSelected = true
	s.applyTableFocus()
}

// selectRows moves focus into the table, unless it is empty.
func (s *sandboxesScreen) selectRows() {
	if len(s.sandboxes) == 0 {
		return
	}
	s.newSelected = false
	s.applyTableFocus()
}

// moveUp walks up the table, then off its top row onto the "new" selector, and
// finally off the "new" selector up onto the tab bar (via focusTabsCmd). Visual
// mode stays within the table and repaints the range.
func (s *sandboxesScreen) moveUp() tea.Cmd {
	if s.newSelected {
		return focusTabsCmd()
	}
	if s.table.Cursor() <= 0 {
		if !s.visual {
			s.selectNew()
		}
		return nil
	}
	s.table.MoveUp(1)
	if s.visual {
		s.repaintVisual()
	}
	return nil
}

// moveDown steps off the "new" selector into the table, then down its rows.
func (s *sandboxesScreen) moveDown() {
	if s.newSelected {
		s.selectRows()
		return
	}
	s.table.MoveDown(1)
	if s.visual {
		s.repaintVisual()
	}
}

// toggleVisual enters vim-style range select at the cursor, or commits the
// current selection (keeping the painted marks) when already active.
func (s *sandboxesScreen) toggleVisual() {
	if s.visual {
		s.exitVisual()
		return
	}
	i := s.table.Cursor()
	if i < 0 || i >= len(s.sandboxes) {
		return
	}
	s.visual = true
	s.anchor = i
	s.visualBase = make(map[string]bool, len(s.marked))
	for id := range s.marked {
		s.visualBase[id] = true
	}
	s.repaintVisual()
}

// repaintVisual sets the marks to the base plus the anchor..cursor range.
func (s *sandboxesScreen) repaintVisual() {
	marked := make(map[string]bool, len(s.visualBase))
	for id := range s.visualBase {
		marked[id] = true
	}
	lo, hi := s.anchor, s.table.Cursor()
	if lo > hi {
		lo, hi = hi, lo
	}
	for i := lo; i <= hi; i++ {
		if i >= 0 && i < len(s.sandboxes) {
			marked[s.sandboxes[i].ID] = true
		}
	}
	s.marked = marked
	s.rebuildRows()
}

// exitVisual commits the range, keeping the painted marks.
func (s *sandboxesScreen) exitVisual() {
	s.visual = false
	s.visualBase = nil
}

// cancelVisual abandons the range, restoring the marks from before it began.
func (s *sandboxesScreen) cancelVisual() {
	s.visual = false
	if s.visualBase != nil {
		s.marked = s.visualBase
	}
	s.visualBase = nil
	s.rebuildRows()
}

// applyTableFocus swaps the table styles so only the focused region highlights.
func (s *sandboxesScreen) applyTableFocus() {
	if s.newSelected {
		s.table.SetStyles(s.tableInactive)
	} else {
		s.table.SetStyles(s.tableActive)
	}
}

func (s *sandboxesScreen) handleConfirmKey(msg tea.KeyPressMsg) (screen, tea.Cmd) {
	switch msg.String() {
	case "y", "enter":
		targets := s.pending
		s.confirming = false
		s.pending = nil
		if len(targets) == 0 {
			return s, nil
		}
		ids := make([]string, len(targets))
		for i, sb := range targets {
			ids[i] = sb.ID
		}
		return s, s.deleteCmd(ids)
	case "n", "esc", "q":
		s.confirming = false
		s.pending = nil
		return s, nil
	}
	return s, nil
}

// beginDelete stages the marked rows (or the cursor row when none are marked)
// and opens the confirmation dialog. It captures the sandboxes themselves so the
// dialog can name them and the delete targets survive a background refresh.
func (s *sandboxesScreen) beginDelete() {
	targets := s.markedSandboxes()
	if len(targets) == 0 {
		if sb, ok := s.cursorSandbox(); ok {
			targets = []Sandbox{sb}
		}
	}
	if len(targets) == 0 {
		return
	}
	s.pending = targets
	s.confirming = true
}

// markedSandboxes returns the marked sandboxes in list order.
func (s *sandboxesScreen) markedSandboxes() []Sandbox {
	out := make([]Sandbox, 0, len(s.marked))
	for _, sb := range s.sandboxes {
		if s.marked[sb.ID] {
			out = append(out, sb)
		}
	}
	return out
}

func (s *sandboxesScreen) toggleMark() {
	sb, ok := s.cursorSandbox()
	if !ok {
		return
	}
	if s.marked[sb.ID] {
		delete(s.marked, sb.ID)
	} else {
		s.marked[sb.ID] = true
	}
	s.rebuildRows()
}

// toggleSelectAll marks every sandbox, or clears the marks when all are already
// marked (so ^a is both select-all and, pressed again, clear-all).
func (s *sandboxesScreen) toggleSelectAll() {
	if len(s.sandboxes) == 0 {
		return
	}
	if len(s.marked) >= len(s.sandboxes) {
		s.marked = map[string]bool{}
	} else {
		for _, sb := range s.sandboxes {
			s.marked[sb.ID] = true
		}
	}
	s.rebuildRows()
}

func (s *sandboxesScreen) View(width, height int) string {
	s.setSize(width, height)
	if s.confirming {
		return s.centered(width, height, s.confirmDialog())
	}
	selector := s.newSelectorLine(width)
	bodyHeight := height - 1 // the selector occupies the first row
	var body string
	switch {
	case !s.loaded:
		body = s.centered(width, bodyHeight, s.styles.status.Render("loading sandboxes…"))
	case len(s.sandboxes) == 0:
		body = s.centered(width, bodyHeight, s.styles.status.Render("no sandboxes yet — press n or enter to create one"))
	default:
		body = s.table.View()
	}
	return selector + "\n" + body
}

// newSelectorLine renders the full-width "new session" affordance above the
// table, highlighted when it holds focus.
func (s *sandboxesScreen) newSelectorLine(width int) string {
	label := "+ New session"
	if s.newSelected {
		return s.styles.newActive.Width(width).Render(label)
	}
	left := s.styles.newInactive.Render(label)
	if s.visual {
		badge := s.styles.visualBadge.Render("-- VISUAL --")
		if gap := width - lipgloss.Width(left) - lipgloss.Width(badge); gap > 0 {
			return left + strings.Repeat(" ", gap) + badge
		}
	}
	return left
}

// confirmDialog names the sandboxes staged for deletion so the user can see
// exactly what a multi-delete will remove, capping a long list with a summary.
func (s *sandboxesScreen) confirmDialog() string {
	const maxList = 8
	n := len(s.pending)

	var b strings.Builder
	fmt.Fprintf(&b, "Delete %s?\n", countLabel(n, "sandbox", "sandboxes"))
	for i, sb := range s.pending {
		if i == maxList {
			fmt.Fprintf(&b, "  …and %d more\n", n-maxList)
			break
		}
		fmt.Fprintf(&b, "  • %s\n", truncateName(displayName(sb), 48))
	}
	yes := s.styles.dialogKey.Render("y")
	no := s.styles.dialogKey.Render("n")
	fmt.Fprintf(&b, "\n%s confirm   %s cancel", yes, no)
	return s.styles.dialog.Render(b.String())
}

// truncateName shortens a name to width runes with an ellipsis when it overflows.
func truncateName(name string, width int) string {
	runes := []rune(name)
	if len(runes) <= width {
		return name
	}
	return string(runes[:width-1]) + "…"
}

func (s *sandboxesScreen) title() string { return "sandboxes" }

func (s *sandboxesScreen) helpBindings() []key.Binding {
	return []key.Binding{
		s.keys.Up, s.keys.Down, s.keys.New, s.keys.Mark, s.keys.SelectAll, s.keys.Visual, s.keys.Enter, s.keys.Fullscreen, s.keys.Delete, s.keys.Agents, s.keys.Refresh, s.keys.Help, s.keys.Quit,
	}
}

func (s *sandboxesScreen) fullHelpBindings() [][]key.Binding {
	return [][]key.Binding{
		{s.keys.Up, s.keys.Down, s.keys.Top, s.keys.Bottom},
		{s.keys.New, s.keys.Mark, s.keys.SelectAll, s.keys.Visual, s.keys.Enter, s.keys.Fullscreen, s.keys.Delete},
		{s.keys.Agents, s.keys.Refresh, s.keys.Help, s.keys.Quit},
	}
}

func (s *sandboxesScreen) cursor(int, int) *tea.Cursor { return nil }

func (s *sandboxesScreen) count() int { return len(s.sandboxes) }

// refreshCmd loads the sandbox list off the UI goroutine.
func (s *sandboxesScreen) refreshCmd() tea.Cmd {
	return func() tea.Msg {
		sandboxes, err := s.ds.ListSandboxes(s.ctx)
		if err != nil {
			return errMsg{context: "list", err: err}
		}
		return sandboxesLoadedMsg{sandboxes: sandboxes}
	}
}

func (s *sandboxesScreen) tickCmd() tea.Cmd {
	return tea.Tick(refreshInterval, func(t time.Time) tea.Msg { return tickMsg{at: t} })
}

// deleteCmd deletes ids concurrently and reports per-id outcomes in order.
func (s *sandboxesScreen) deleteCmd(ids []string) tea.Cmd {
	ds := s.ds
	ctx := s.ctx
	return func() tea.Msg {
		errs := make([]error, len(ids))
		var wg sync.WaitGroup
		for i, id := range ids {
			wg.Add(1)
			go func(i int, id string) {
				defer wg.Done()
				errs[i] = ds.DeleteSandbox(ctx, id)
			}(i, id)
		}
		wg.Wait()
		return deletedMsg{ids: ids, errs: errs}
	}
}

// applySandboxes stores a fresh list, drops marks for vanished sandboxes, and
// rebuilds the table while preserving the cursor position.
func (s *sandboxesScreen) applySandboxes(list []Sandbox) {
	sorted := make([]Sandbox, len(list))
	copy(sorted, list)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Created.Before(sorted[j].Created) })
	s.sandboxes = sorted
	s.loaded = true

	present := make(map[string]bool, len(sorted))
	for _, sb := range sorted {
		present[sb.ID] = true
	}
	for id := range s.marked {
		if !present[id] {
			delete(s.marked, id)
		}
	}
	s.rebuildRows()

	// Focus a just-created sandbox once it appears in the list.
	if s.selectID != "" {
		for i, sb := range sorted {
			if sb.ID == s.selectID {
				s.newSelected = false
				s.table.SetCursor(i)
				s.selectID = ""
				break
			}
		}
	}
	s.applyTableFocus()
}

func (s *sandboxesScreen) rebuildRows() {
	cursor := s.table.Cursor()
	cols := s.columns(s.width)
	s.table.SetColumns(cols)
	rows := make([]table.Row, 0, len(s.sandboxes))
	for _, sb := range s.sandboxes {
		rows = append(rows, s.row(sb))
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

func (s *sandboxesScreen) row(sb Sandbox) table.Row {
	marker := ""
	if s.marked[sb.ID] {
		marker = lipgloss.NewStyle().Foreground(colorMark).Render("●")
	}
	state := lipgloss.NewStyle().Foreground(stateColor(sb.State)).Render(sb.State)
	return table.Row{
		marker,
		sb.ID,
		displayName(sb),
		state,
		humanizeAge(sb.Updated),
		sb.Message,
	}
}

// columns allocates fixed widths for the structured columns and gives the
// remainder to NAME and MESSAGE, which hold the most variable content.
func (s *sandboxesScreen) columns(width int) []table.Column {
	const (
		markerW = 2
		idW     = 22
		stateW  = 10
		ageW    = 12
		// Each column carries 2 cells of horizontal padding from the Cell style.
		padPerCol = 2
		numCols   = 6
	)
	fixed := markerW + idW + stateW + ageW + padPerCol*numCols
	remaining := width - fixed
	if remaining < 20 {
		remaining = 20
	}
	nameW := remaining / 3
	if nameW < 12 {
		nameW = 12
	}
	msgW := remaining - nameW
	if msgW < 8 {
		msgW = 8
	}
	return []table.Column{
		{Title: "", Width: markerW},
		{Title: "ID", Width: idW},
		{Title: "NAME", Width: nameW},
		{Title: "STATE", Width: stateW},
		{Title: "AGE", Width: ageW},
		{Title: "MESSAGE", Width: msgW},
	}
}

func (s *sandboxesScreen) setSize(width, height int) {
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

func (s *sandboxesScreen) cursorSandbox() (Sandbox, bool) {
	i := s.table.Cursor()
	if i < 0 || i >= len(s.sandboxes) {
		return Sandbox{}, false
	}
	return s.sandboxes[i], true
}

func (s *sandboxesScreen) markedIDs() []string {
	ids := make([]string, 0, len(s.marked))
	for _, sb := range s.sandboxes {
		if s.marked[sb.ID] {
			ids = append(ids, sb.ID)
		}
	}
	return ids
}

func (s *sandboxesScreen) centered(width, height int, content string) string {
	if width <= 0 || height <= 0 {
		return content
	}
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, content)
}

// humanizeAge renders a compact relative age like k9s ("3m", "2h", "5d").
func humanizeAge(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
