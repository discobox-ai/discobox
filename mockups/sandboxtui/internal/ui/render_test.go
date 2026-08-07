package ui

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// send drives the model the way the runtime would — including feeding the
// commands it returns back in as messages — so a frame can be looked at
// without a terminal.
func send(t *testing.T, m tea.Model, msgs ...tea.Msg) Model {
	t.Helper()
	var cmd tea.Cmd
	for _, msg := range msgs {
		m, cmd = m.Update(msg)
		for depth := 0; cmd != nil && depth < 4; depth++ {
			next, ok := runQuickly(cmd)
			if !ok {
				break
			}
			switch next.(type) {
			case statusMsg, runActionMsg, showCommandMsg:
			default:
				cmd = nil
				continue
			}
			m, cmd = m.Update(next)
		}
	}
	return m.(Model)
}

// runQuickly runs a command, giving up on the ones that are timers. The cursor
// blink, the status expiry and the resize settle are all sleeps, and this
// package's own immediate commands are the only ones worth feeding back, so
// waiting for anything slow only buys a slower test.
func runQuickly(cmd tea.Cmd) (tea.Msg, bool) {
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		return msg, true
	case <-time.After(10 * time.Millisecond):
		return nil, false
	}
}

func key(s string) tea.Msg {
	switch s {
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEscape}
	case " ":
		return tea.KeyMsg{Type: tea.KeySpace}
	case "ctrl+d":
		return tea.KeyMsg{Type: tea.KeyCtrlD}
	case "ctrl+j":
		return tea.KeyMsg{Type: tea.KeyCtrlJ}
	case "shift+tab":
		return tea.KeyMsg{Type: tea.KeyShiftTab}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func typeString(s string) tea.Msg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// size is a terminal wide enough for the mark beside the list; narrow is one
// that has to give it up.
func size() tea.Msg   { return tea.WindowSizeMsg{Width: 150, Height: 32} }
func narrow() tea.Msg { return tea.WindowSizeMsg{Width: 92, Height: 32} }

// TestFrames renders the states the mockup exists to show. Run it with -v to
// look at them:
//
//	go test ./internal/ui -run TestFrames -v
func TestFrames(t *testing.T) {
	// Plain ASCII, so the frames stay readable in the test log — and so the
	// mark's own escapes get stripped along with everything else.
	lipgloss.SetColorProfile(termenv.Ascii)

	frames := []struct {
		name string
		msgs []tea.Msg
	}{
		{"prompt", []tea.Msg{size(), typeString("make the pool reaper stop leaking volumes")}},
		{"narrow", []tea.Msg{narrow(), typeString("no room for the mark")}},
		{"list", []tea.Msg{size(), typeString("wip"), key("up"), key("up")}},
		{"multiselect", []tea.Msg{size(), key("up"), key(" "), key(" "), key(" ")}},
		{"visual", []tea.Msg{size(), key("up"), key("V"), key("down"), key("down")}},
		{"actions", []tea.Msg{size(), key("up"), key(".")}},
		{"options", []tea.Msg{size(), typeString("fix the tests"), key("tab")}},
		{"run", []tea.Msg{size(), typeString("fix the tests"), key("enter")}},
	}
	for _, f := range frames {
		m := send(t, New(), f.msgs...)
		view := m.View()
		if strings.TrimSpace(view) == "" {
			t.Fatalf("%s: empty frame", f.name)
		}
		t.Logf("\n=== %s ===\n%s", f.name, view)
	}
}

// TestNoColorDropsTheMark checks that NO_COLOR reaches the mark, which is pure
// colour: stripped of it there is nothing left worth drawing.
func TestNoColorDropsTheMark(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	lipgloss.SetColorProfile(termenv.TrueColor) // as if a colour terminal
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.Ascii) })

	m := send(t, New(), size())
	if lipgloss.ColorProfile() != termenv.Ascii {
		t.Fatalf("NO_COLOR should force the renderer to plain ASCII")
	}
	if m.showLogo() || m.logo.height() != 0 {
		t.Fatalf("NO_COLOR should drop the mark entirely")
	}
	if view := m.View(); strings.Contains(view, "\x1b") {
		t.Fatalf("no frame should carry escapes under NO_COLOR")
	}
	if m.list.width != m.width {
		t.Fatalf("the list should take the columns the mark gave up")
	}
}

// TestCursorRowTakesABackground checks difftui's full-row selection over
// content that carries its own foreground colour: the background has to be
// re-asserted after every reset, or it stops at the first one.
func TestCursorRowTakesABackground(t *testing.T) {
	if os.Getenv("NO_COLOR") != "" {
		t.Skip("NO_COLOR is set: the coloured path is not the one under test")
	}
	lipgloss.SetColorProfile(termenv.ANSI256)
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.Ascii) })

	m := send(t, New(), size(), key("up"))
	row := m.list.row(m.st, m.list.rows()[0], 0, true)
	bg := backgroundSeq(colHighlightBG)
	if !strings.HasPrefix(row, bg) {
		t.Fatalf("the cursor row should open with the background: %q", row)
	}
	if n := strings.Count(row, bg); n <= 1 {
		t.Fatalf("the background should be re-asserted after each reset, found %d", n)
	}
	if !strings.Contains(row, "❯") {
		t.Fatalf("the cursor should be a chevron")
	}
	plain := m.list.row(m.st, m.list.rows()[1], 1, true)
	if strings.Contains(plain, bg) {
		t.Fatalf("a row off the cursor should carry no background")
	}

	// With the prompt focused there is nothing to act on, so no row is picked
	// out at all — the cursor belongs to the pane that has focus.
	blurred := m.list.row(m.st, m.list.rows()[0], 0, false)
	if strings.Contains(blurred, bg) || strings.Contains(blurred, "❯") {
		t.Fatalf("an unfocused list should show no cursor: %q", blurred)
	}
}

// TestSelectionIsABackground checks the three bands a row can be in. "Both"
// needs its own colour, or moving the cursor onto a selected row would hide
// that a command is about to act on it.
func TestSelectionIsABackground(t *testing.T) {
	if os.Getenv("NO_COLOR") != "" {
		t.Skip("NO_COLOR is set: there are no backgrounds to check")
	}
	lipgloss.SetColorProfile(termenv.ANSI256)
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.Ascii) })

	// Select the first two rows, leaving the cursor on the second of them.
	m := send(t, New(), size(), key("tab"), key(" "), key(" "), key("up"))
	rows := m.list.rows()

	bands := map[string]string{
		"selected, off the cursor":      backgroundSeq(colSelectedBG),
		"under the cursor and selected": backgroundSeq(colBothBG),
		"under the cursor alone":        backgroundSeq(colHighlightBG),
	}
	if len(map[string]bool{bands["selected, off the cursor"]: true,
		bands["under the cursor and selected"]: true,
		bands["under the cursor alone"]:        true}) != 3 {
		t.Fatalf("the three bands must be three different colours")
	}

	if got := m.list.row(m.st, rows[0], 0, true); !strings.Contains(got, bands["selected, off the cursor"]) {
		t.Fatalf("a selected row off the cursor should take the selected background: %q", got)
	}
	if got := m.list.row(m.st, rows[1], 1, true); !strings.Contains(got, bands["under the cursor and selected"]) {
		t.Fatalf("a selected row under the cursor should take its own background: %q", got)
	}
	if got := m.list.row(m.st, rows[2], 2, true); strings.Contains(got, bands["selected, off the cursor"]) {
		t.Fatalf("an unselected row should take no background: %q", got)
	}
	if got := m.list.row(m.st, rows[0], 0, true); strings.Contains(got, "•") {
		t.Fatalf("selection should cost no column: %q", got)
	}
}

// TestStateReadsAsGlyphOrWord: the state is a coloured glyph when there is
// colour to carry it, and the word spelled out when there is not.
func TestStateReadsAsGlyphOrWord(t *testing.T) {
	if os.Getenv("NO_COLOR") != "" {
		t.Skip("NO_COLOR is set: only the colourless half can be checked")
	}
	m := send(t, New(), size())
	running := m.list.rows()[0]

	lipgloss.SetColorProfile(termenv.ANSI256)
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.Ascii) })
	coloured := m.list.row(m.st, running, 0, true)
	if !strings.Contains(coloured, stateDot(running)) {
		t.Fatalf("a colour terminal should get the glyph: %q", coloured)
	}
	if strings.Contains(coloured, string(running.state)) {
		t.Fatalf("and not the word as well: %q", coloured)
	}

	lipgloss.SetColorProfile(termenv.Ascii)
	plain := m.list.row(m.st, running, 0, true)
	if !strings.Contains(plain, string(running.state)) {
		t.Fatalf("without colour the word should come back: %q", plain)
	}
	if strings.Contains(plain, stateDot(running)) {
		t.Fatalf("and the glyph should go: %q", plain)
	}
}

// TestUsageOnlyCountsWhenTheSandboxIsUp: three percentages are the sandbox's
// running cost, and a sandbox that is not running has none — three zeroes
// would read as "idle" rather than "off".
func TestUsageOnlyCountsWhenTheSandboxIsUp(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	m := send(t, New(), size())
	st := newStyles()

	rows := m.list.rows()
	if got := usage(st, rows[0]); !strings.Contains(got, "61%") {
		t.Fatalf("a running sandbox should report what it is using, got %q", got)
	}
	if got := usage(st, rows[0]); !strings.Contains(got, "2.3 GiB") {
		t.Fatalf("the disk should read as bytes, not a share, got %q", got)
	}
	if got := usage(st, rows[2]); strings.Contains(got, "%") {
		t.Fatalf("a stopped sandbox should report nothing, got %q", got)
	}
}

// TestNameScrolls: a name too long for its column is ellipsized, and the row
// under the cursor can be walked sideways to read the rest.
func TestNameScrolls(t *testing.T) {
	m := send(t, New(), size(), key("tab"))
	_ = m.View() // the column width is only known once something is drawn
	if m.list.nameFull <= m.list.nameWidth {
		t.Fatalf("the first row's name is meant to be longer than its column")
	}

	m = send(t, m, key("right"), key("right"))
	if m.list.nameScroll == 0 {
		t.Fatalf("right should walk the name sideways")
	}
	scrolled := m.View()
	if !strings.Contains(scrolled, "…") {
		t.Fatalf("a scrolled name should say there is more to the left")
	}

	// Scrolled all the way, the end of the name is on screen. The leading
	// ellipsis costs a cell, so the bound has to allow for it or the last
	// character stays one press out of reach.
	far := m
	for range 100 {
		far = send(t, far, key("right"))
		_ = far.View()
	}
	full := far.list.rows()[far.list.cursor].name
	tail := full[strings.LastIndex(full, " ")+1:]
	if !strings.Contains(far.View(), tail) {
		t.Fatalf("scrolling to the end should show the end of the name (%q)", tail)
	}
	if far.list.nameScroll > far.list.maxNameScroll() {
		t.Fatalf("scrolling should stop at the end of the name")
	}
	if back := send(t, far, key("down"), key("up")); back.list.nameScroll != 0 {
		t.Fatalf("moving the cursor should reset the scroll")
	}

	// A name that fits does not move at all.
	fits := send(t, New(), size(), key("tab"), key("down"))
	_ = fits.View()
	fits = send(t, fits, key("right"))
	if fits.list.nameScroll != 0 {
		t.Fatalf("a name that fits its column should not scroll")
	}
}

// TestHumanBytes: the disk figure is the number you want to read, so it is
// written the way df -h writes it.
func TestHumanBytes(t *testing.T) {
	for _, c := range []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1_572_864, "1.5 MiB"},
		{2_483_027_968, "2.3 GiB"},
		{15_264_268_288, "14 GiB"},
		{1 << 42, "4.0 TiB"},
	} {
		if got := humanBytes(c.in); got != c.want {
			t.Fatalf("humanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestProjectIsASessionSetting: the project comes from the command line, is
// named in the header only when it is not the default, and rides along in
// every command the window would run.
func TestProjectIsASessionSetting(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)

	plain := send(t, New(), size())
	if strings.Contains(plain.viewHeader(plain.width), defaultProject) {
		t.Fatalf("the default project should not be named in the header")
	}
	if got := plain.opts.command("do a thing"); strings.Contains(got, "-p") {
		t.Fatalf("the default project needs no flag, got %q", got)
	}

	named := send(t, New(Project("obot")), size())
	if !strings.Contains(named.viewHeader(named.width), "obot") {
		t.Fatalf("a project other than the default should be named in the header")
	}
	if got := named.opts.command("do a thing"); !strings.Contains(got, "-p obot") {
		t.Fatalf("a project other than the default should ride along, got %q", got)
	}
	for _, o := range named.opts.opts {
		if o.label == "Project" {
			t.Fatalf("the project should not be an option in the panel")
		}
	}
}

// TestStatusDoesNotOutliveItsMoment: a message answers the last key, so the
// next key takes it away — and if no key comes, a timer does.
func TestStatusDoesNotOutliveItsMoment(t *testing.T) {
	m := send(t, New(), size(), key("tab"), key("A"))
	if m.status == "" {
		t.Fatalf("showing the archived sandboxes should report that it did")
	}

	if next := send(t, m, key("down")); next.status != "" {
		t.Fatalf("the next key should clear the message, got %q", next.status)
	}

	// The timer only clears the message it was started for.
	stale, _ := m.Update(statusExpiredMsg{generation: m.statusGen - 1})
	if stale.(Model).status == "" {
		t.Fatalf("a timer from an older message should not clear a newer one")
	}
	expired, _ := m.Update(statusExpiredMsg{generation: m.statusGen})
	if expired.(Model).status != "" {
		t.Fatalf("its own timer should clear it")
	}
}

// TestTabSwitchesPanes pins the one key that moves between the two halves of
// the window, with the options a layer over it on Shift-Tab.
func TestTabSwitchesPanes(t *testing.T) {
	m := send(t, New(), size(), key("tab"))
	if m.focus != focusList {
		t.Fatalf("Tab should move to the sandbox list")
	}
	m = send(t, m, key("tab"))
	if m.focus != focusPrompt {
		t.Fatalf("Tab should come back to the prompt")
	}

	opts := send(t, m, key("shift+tab"))
	if !opts.optionsOpen {
		t.Fatalf("Shift-Tab should open the run options")
	}
	if closed := send(t, opts, key("shift+tab")); closed.optionsOpen {
		t.Fatalf("Shift-Tab should close them again")
	}
	if fromList := send(t, m, key("tab"), key("shift+tab")); !fromList.optionsOpen {
		t.Fatalf("Shift-Tab should open the run options from the list too")
	}
}

// TestCtrlDQuitsOnAnEmptyPrompt pins the shell habit: EOF on an empty line
// leaves, and with text in the buffer it does not.
func TestCtrlDQuitsOnAnEmptyPrompt(t *testing.T) {
	typed := send(t, New(), size(), typeString("still writing"), key("ctrl+d"))
	if typed.quit {
		t.Fatalf("Ctrl-D with text in the prompt should not quit")
	}
	empty := send(t, New(), size(), key("ctrl+d"))
	if !empty.quit {
		t.Fatalf("Ctrl-D on an empty prompt should quit")
	}
}

// TestFrameNeverWraps is the other half of surviving a resize: a line wider
// than the terminal wraps, and a wrapped line puts the inline renderer's line
// accounting out by one for the rest of the session.
func TestFrameNeverWraps(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI256)
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.Ascii) })

	states := map[string][]tea.Msg{
		"prompt":  {typeString("a prompt long enough to need more than one line of the composer to hold it all")},
		"list":    {key("tab")},
		"visual":  {key("tab"), key("V"), key("down")},
		"actions": {key("tab"), key(".")},
		"options": {key("shift+tab")},
		"help":    {tea.KeyMsg{Type: tea.KeyF1}},
		"run":     {typeString("fix it"), key("enter")},
	}
	for _, width := range []int{60, 79, 80, 99, 100, 101, 130, 200} {
		for name, msgs := range states {
			m := send(t, New(), append([]tea.Msg{tea.WindowSizeMsg{Width: width, Height: 30}}, msgs...)...)
			for i, line := range strings.Split(m.View(), "\n") {
				if got := lipgloss.Width(line); got > width {
					t.Fatalf("%s at %d columns: line %d is %d wide:\n%s", name, width, i, got, line)
				}
			}
		}
	}
}

// TestResizeClearsTheScreen: a width change leaves a reflowed frame on screen
// that the inline renderer cannot paint over, so it is erased first.
func TestResizeClearsTheScreen(t *testing.T) {
	clears := func(m tea.Model, msg tea.Msg) bool {
		_, cmd := m.Update(msg)
		if cmd == nil {
			return false
		}
		return fmt.Sprintf("%T", cmd()) == fmt.Sprintf("%T", tea.ClearScreen())
	}

	m := New()
	if clears(m, tea.WindowSizeMsg{Width: 120, Height: 40}) {
		t.Fatalf("the window opening should not wipe the scrollback")
	}

	opened, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if clears(opened, tea.WindowSizeMsg{Width: 100, Height: 40}) {
		t.Fatalf("a width change should wait for the drag to settle, not clear at once")
	}
	if clears(opened, tea.WindowSizeMsg{Width: 120, Height: 20}) {
		t.Fatalf("height alone reflows nothing and should not clear")
	}

	// A drag is many messages; only the last one's timer clears.
	dragged := send(t, New(), tea.WindowSizeMsg{Width: 120, Height: 40})
	for width := 119; width > 110; width-- {
		next, _ := dragged.Update(tea.WindowSizeMsg{Width: width, Height: 40})
		dragged = next.(Model)
	}
	if clears(dragged, resizeSettledMsg{generation: 1}) {
		t.Fatalf("a timer from earlier in the drag should not clear")
	}
	if !clears(dragged, resizeSettledMsg{generation: dragged.resize}) {
		t.Fatalf("the last timer out should clear the screen")
	}
}

// TestRunCommand checks the preview the whole options panel exists to produce.
func TestRunCommand(t *testing.T) {
	o := newOptions(defaultProject)
	if got, want := o.command("fix the tests"), "disco run -- 'fix the tests'"; got != want {
		t.Fatalf("defaults: got %q, want %q", got, want)
	}
	o.opts[optHarness].idx = 1
	o.opts[optDirty].idx = 2
	o.opts[optDetach].value = "on"
	o.opts[optEnv].items = []string{"GITHUB_TOKEN", "MODE=test"}
	o.opts[optSecret].items = []string{"OPENAI_API_KEY=<sec_123>"}
	o.opts[optSource].idx = 1
	o.project = "obot"
	want := "disco -p obot -C '~/src/disco2@HEAD~1' run --harness codex --include-dirty=false -d " +
		"-e GITHUB_TOKEN -e MODE=test -s 'OPENAI_API_KEY=<sec_123>' -- 'fix the tests'"
	if got := o.command("fix the tests"); got != want {
		t.Fatalf("configured:\n got %q\nwant %q", got, want)
	}
}

// TestChipsCarryEverything: the mode line answers "what will Enter do" without
// anything being opened, so it shows every option — except a count of none.
func TestChipsCarryEverything(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	o := newOptions(defaultProject)

	chips := o.chips(newStyles())
	for _, want := range []string{"claude", "dirty auto", currentDir} {
		if !strings.Contains(chips, want) {
			t.Fatalf("the default mode line should carry %q, got %q", want, chips)
		}
	}
	// What was always going to happen, and a count of none, are not worth a
	// word; the project is a session-wide setting and lives in the header.
	for _, unwanted := range []string{"attach", "env", "secret", "project"} {
		if strings.Contains(chips, unwanted) {
			t.Fatalf("the default mode line should not carry %q, got %q", unwanted, chips)
		}
	}

	o.opts[optEnv].items = []string{"A=1", "B=2"}
	o.opts[optSecret].items = []string{"C=3"}
	o.opts[optDetach].value = "on"
	o.opts[optDirty].idx = 2
	chips = o.chips(newStyles())
	for _, want := range []string{"2 env", "1 secret", "detached", "clean"} {
		if !strings.Contains(chips, want) {
			t.Fatalf("the mode line should carry %q, got %q", want, chips)
		}
	}
}

// TestActionsFollowTheTargets checks that an action offered by the menu is one
// the selected sandboxes can actually take.
func TestActionsFollowTheTargets(t *testing.T) {
	m := send(t, New(), size(), key("up"))
	enabled := func(targets []sandbox) map[string]bool {
		out := map[string]bool{}
		for _, a := range m.actions(targets) {
			out[a.key] = a.enabled
		}
		return out
	}

	rows := m.list.rows()
	if got := enabled(rows[:1]); !got["a"] || !got["d"] || got["u"] || got["T"] {
		t.Fatalf("a running, up-to-date sandbox with changes: %v", got)
	}
	if got := enabled(rows[1:2]); !got["u"] {
		t.Fatalf("a sandbox with an upgrade available should offer upgrade")
	}
	if got := enabled(rows[3:4]); got["d"] || got["y"] {
		t.Fatalf("a sandbox that has changed nothing should not offer diff or apply")
	}
	if got := enabled(rows[:2]); got["a"] || got["s"] || !got["x"] {
		t.Fatalf("attach takes one sandbox, delete takes any number: %v", got)
	}
}

// TestVisualRange checks difftui's visual select, transplanted onto the list:
// V anchors, moving extends, a command acts on the whole range, and Space
// turns the range into marks that outlive the mode.
func TestVisualRange(t *testing.T) {
	// The model holds its list by pointer, so each case starts from a fresh
	// one rather than branching off a shared range.
	threeRows := func(t *testing.T, more ...tea.Msg) Model {
		t.Helper()
		msgs := append([]tea.Msg{size(), key("up"), key("V"), key("down"), key("down")}, more...)
		return send(t, New(), msgs...)
	}

	m := threeRows(t)
	if !m.list.visual {
		t.Fatalf("V should start visual select")
	}
	if got := m.list.targets(); len(got) != 3 {
		t.Fatalf("a range over three rows should target three sandboxes, got %d", len(got))
	}

	// Down at the bottom of the list must not fall through to the prompt
	// while a range is being drawn.
	end := threeRows(t, key("G"), key("down"))
	if end.focus != focusList || !end.list.visual {
		t.Fatalf("visual select should hold focus in the list")
	}

	marked := threeRows(t, key(" "))
	if marked.list.visual {
		t.Fatalf("Space should end visual select")
	}
	if n := marked.list.selectionCount(); n != 3 {
		t.Fatalf("Space should keep the range as marks, got %d", n)
	}

	acted := threeRows(t, key("x"))
	if acted.list.visual {
		t.Fatalf("a command should end visual select")
	}
	if acted.dialog == nil || !strings.Contains(acted.dialog.body, "3 sandboxes") {
		t.Fatalf("delete should act on the whole range, got %+v", acted.dialog)
	}

	cancelled := threeRows(t, key("V"))
	if cancelled.list.visual || cancelled.list.selectionCount() != 0 {
		t.Fatalf("V again should cancel without selecting anything")
	}
}

// TestArrowsMoveByLineThenLeave pins how the composer's arrows behave, which
// is the one thing a prompt has to get right: they move a line at a time
// through the text, and only from the row they cannot move off do they take
// you anywhere else.
func TestArrowsMoveByLineThenLeave(t *testing.T) {
	// Two lines, cursor left at the end of the second.
	m := send(t, New(), size(), typeString("one"), key("ctrl+j"), typeString("two"))
	if m.prompt.Value() != "one\ntwo" {
		t.Fatalf("Ctrl-J should insert a newline, got %q", m.prompt.Value())
	}

	m = send(t, m, key("up"))
	if m.prompt.Line() != 0 || m.prompt.LineInfo().ColumnOffset != 3 {
		t.Fatalf("Up should move a line and keep the column, got line %d col %d",
			m.prompt.Line(), m.prompt.LineInfo().ColumnOffset)
	}
	if m.focus != focusPrompt {
		t.Fatalf("Up should not leave the prompt while there is a line above")
	}

	m = send(t, m, key("down"))
	if m.prompt.Line() != 1 {
		t.Fatalf("Down should move back down a line, got line %d", m.prompt.Line())
	}

	// Off the top row: to the start of the line, and only then to the list.
	m = send(t, m, key("up"), key("up"))
	if m.focus != focusPrompt || !m.atPromptStart() {
		t.Fatalf("Up on the first row should park at the start of the text")
	}
	m = send(t, m, key("up"))
	if m.focus != focusList {
		t.Fatalf("Up from the start of the text should move to the sandbox list")
	}

	// Back to the prompt, cursor still where it was left.
	m = send(t, m, key("esc"))
	if m.focus != focusPrompt || !m.atPromptStart() {
		t.Fatalf("Esc should come back to the prompt, where the cursor was")
	}
	if !strings.Contains(m.prompt.Value(), "two") {
		t.Fatalf("the prompt should keep what was typed, got %q", m.prompt.Value())
	}

	// Off the bottom row: to the end of the line, and only then to the list.
	m = send(t, m, key("down"))
	if m.focus != focusPrompt || m.prompt.Line() != 1 {
		t.Fatalf("Down should move to the last line first")
	}
	m = send(t, m, key("down"))
	if m.focus != focusPrompt || !m.atPromptEnd() {
		t.Fatalf("Down on the last row should park at the end of the text")
	}
	m = send(t, m, key("down"))
	if m.focus != focusList {
		t.Fatalf("Down from the end of the text should move to the sandbox list")
	}
	if m.list.cursor != 0 {
		t.Fatalf("arriving from below should land on the first sandbox, got row %d", m.list.cursor)
	}
}

// TestArrowsWalkWrappedRows: a line long enough to wrap occupies several rows,
// and the arrows have to walk through them rather than treating the logical
// line as one row.
func TestArrowsWalkWrappedRows(t *testing.T) {
	m := send(t, New(), tea.WindowSizeMsg{Width: 60, Height: 30})
	m = send(t, m, typeString(strings.Repeat("wrap ", 40)))
	if m.prompt.LineInfo().Height < 3 {
		t.Fatalf("the test needs a line that wraps, got %d rows", m.prompt.LineInfo().Height)
	}

	m = send(t, m, key("up"))
	if m.focus != focusPrompt {
		t.Fatalf("Up should walk up a wrapped row, not leave the field")
	}
	if m.prompt.LineInfo().RowOffset == 0 {
		t.Fatalf("Up should move one displayed row at a time")
	}
}

// TestEditorCommand checks the resolution order every tool that shells out to
// an editor uses, and that a variable carrying arguments still works.
func TestEditorCommand(t *testing.T) {
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "cat")
	cmd, err := editorCommand("/tmp/prompt.md")
	if err != nil {
		t.Fatalf("$EDITOR should resolve: %v", err)
	}
	if got := cmd.Args[len(cmd.Args)-1]; got != "/tmp/prompt.md" {
		t.Fatalf("the file should be the last argument, got %q", got)
	}

	t.Setenv("VISUAL", "cat -u")
	cmd, err = editorCommand("/tmp/prompt.md")
	if err != nil {
		t.Fatalf("$VISUAL with arguments should resolve: %v", err)
	}
	if want := []string{"-u", "/tmp/prompt.md"}; len(cmd.Args) != 3 ||
		cmd.Args[1] != want[0] || cmd.Args[2] != want[1] {
		t.Fatalf("$VISUAL should win and keep its arguments, got %q", cmd.Args)
	}

	t.Setenv("VISUAL", "no-such-editor-anywhere")
	if _, err := editorCommand("/tmp/prompt.md"); err == nil {
		t.Fatalf("an editor that is not installed should be an error, not a crash")
	}
}

// TestEmptyEnterCreatesASandbox: an empty prompt is not an error, it is the
// other thing you come here for.
func TestEmptyEnterCreatesASandbox(t *testing.T) {
	m := send(t, New(), size(), key("enter"))
	if m.dialog == nil || m.dialog.err {
		t.Fatalf("an empty prompt should run, not complain: %+v", m.dialog)
	}
	if m.dialog.command != "disco run" {
		t.Fatalf("an empty prompt should run bare, got %q", m.dialog.command)
	}
	if !strings.Contains(m.dialog.body, currentDir) {
		t.Fatalf("it should say what the sandbox is cut from, got %q", m.dialog.body)
	}
}

// TestArchiveAndPurge: archiving is reversible and asks nothing, purging is
// not and asks first — and purge is not offered at all until the sandbox is
// archived.
func TestArchiveAndPurge(t *testing.T) {
	m := send(t, New(), size(), key("tab"), key("x"))
	if m.dialog == nil || m.dialog.kind == dlgConfirm {
		t.Fatalf("archiving is reversible and should not ask: %+v", m.dialog)
	}
	if !strings.Contains(m.dialog.command, "disco delete sbx_") {
		t.Fatalf("archive should run delete, got %q", m.dialog.command)
	}

	// Purge on a running sandbox is refused, with the reason.
	refused := send(t, New(), size(), key("tab"), key("P"))
	if refused.dialog == nil || !refused.dialog.err {
		t.Fatalf("purge should be refused on a running sandbox: %+v", refused.dialog)
	}

	// On an archived one it asks, and only then runs.
	archived := send(t, New(), size(), key("tab"), key("A"), key("G"))
	if got := archived.list.current(); got == nil || got.state != stateArchived {
		t.Fatalf("expected to be on an archived sandbox, got %+v", got)
	}
	archived = send(t, archived, key("P"))
	if archived.dialog == nil || archived.dialog.kind != dlgConfirm {
		t.Fatalf("purge should ask first: %+v", archived.dialog)
	}
	archived = send(t, archived, key("y"))
	if archived.dialog == nil || !strings.Contains(archived.dialog.command, "disco sandbox purge sbx_") {
		t.Fatalf("a confirmed purge should show the command, got %+v", archived.dialog)
	}
}

// TestArchivedAreHiddenUntilAskedFor: archived sandboxes are history, kept and
// out of the way until A says otherwise.
func TestArchivedAreHiddenUntilAskedFor(t *testing.T) {
	m := send(t, New(), size(), key("tab"))
	for _, s := range m.list.rows() {
		if s.state == stateArchived {
			t.Fatalf("archived sandboxes should be hidden by default")
		}
	}
	if m.list.archivedCount() == 0 {
		t.Fatalf("the fake data is meant to have archived sandboxes to hide")
	}
	if !strings.Contains(m.View(), "archived, A shows them") {
		t.Fatalf("the title bar should say there are archived sandboxes to see")
	}

	shown := send(t, m, key("A"))
	found := false
	for _, s := range shown.list.rows() {
		found = found || s.state == stateArchived
	}
	if !found {
		t.Fatalf("A should show the archived sandboxes")
	}

	// Unarchive is offered on them, and not on anything else.
	enabled := func(m Model) map[string]bool {
		out := map[string]bool{}
		for _, a := range m.actions(m.list.targets()) {
			out[a.key] = a.enabled
		}
		return out
	}
	if got := enabled(send(t, shown, key("G"))); !got["U"] || !got["P"] || got["x"] {
		t.Fatalf("an archived sandbox should offer unarchive and purge, not archive: %v", got)
	}
	if got := enabled(send(t, shown, key("g"))); got["U"] || got["P"] || !got["x"] {
		t.Fatalf("a live sandbox should offer archive, not unarchive or purge: %v", got)
	}
}

// TestKeysFollowTheSandbox: the line along the bottom offers what the
// sandboxes under the cursor can actually do, and nothing else.
func TestKeysFollowTheSandbox(t *testing.T) {
	upgradable := send(t, New(), size(), key("tab"), key("down"))
	if !strings.Contains(upgradable.hints(), "u upgrade") {
		t.Fatalf("a sandbox with an upgrade available should offer it: %q", upgradable.hints())
	}
	if got := send(t, New(), size(), key("tab")); strings.Contains(got.hints(), "u upgrade") {
		t.Fatalf("a sandbox already on the current image should not: %q", got.hints())
	}
	if got := send(t, New(), size(), key("tab")); strings.Contains(got.hints(), "P purge") {
		t.Fatalf("purge should not be offered on a running sandbox: %q", got.hints())
	}
	archived := send(t, New(), size(), key("tab"), key("A"), key("G"))
	if h := archived.hints(); !strings.Contains(h, "U unarchive") || !strings.Contains(h, "P purge") {
		t.Fatalf("an archived sandbox should offer unarchive and purge: %q", h)
	}
}
