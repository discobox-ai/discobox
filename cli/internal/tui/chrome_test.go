package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// headerCol finds the display column where text begins on the header row.
func headerCol(t *testing.T, m *Model, text string) int {
	t.Helper()
	header := ansi.Strip(strings.Split(rawFrame(m), "\n")[0])
	idx := strings.Index(header, text)
	if idx < 0 {
		t.Fatalf("the header does not show %q: %q", text, header)
	}
	return ansi.StringWidth(header[:idx])
}

// The header is text too: double-clicking the sandbox id selects the whole id
// — ids are word characters throughout — and releasing copies it.
func TestDoubleClickTheHeaderIdCopiesIt(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")

	col := headerCol(t, m, "sbx_one") + 2
	for range 2 {
		d.dispatch(tea.MouseClickMsg{X: col, Y: 0, Button: tea.MouseLeft})
		d.dispatch(tea.MouseReleaseMsg{X: col, Y: 0, Button: tea.MouseLeft})
	}
	d.wait("the copy", func() bool { return m.status == "copied" })
	if got := m.chromeSel.Text(); got != "sbx_one" {
		t.Fatalf("selected %q, want the id", got)
	}
	// The selection is painted where it was made.
	if header := strings.Split(rawFrame(m), "\n")[0]; !strings.Contains(header, "\x1b[7m") {
		t.Fatalf("the header should carry the highlight: %q", header)
	}
}

// A press in a pane replaces the chrome's selection, and a press on the
// chrome replaces the panes': one selection on screen at a time.
func TestChromeAndPaneSelectionsAreExclusive(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "enter")
	term.send("hello world")
	d.wait("output", func() bool { return strings.Contains(frameText(m), "hello world") })

	col := headerCol(t, m, "sbx_one") + 2
	for range 2 {
		d.dispatch(tea.MouseClickMsg{X: col, Y: 0, Button: tea.MouseLeft})
		d.dispatch(tea.MouseReleaseMsg{X: col, Y: 0, Button: tea.MouseLeft})
	}
	if !m.chromeSel.Active() {
		t.Fatal("no chrome selection to displace")
	}

	ox, oy := m.paneOrigin(m.focusedPane())
	d.dispatch(tea.MouseClickMsg{X: ox, Y: oy, Button: tea.MouseLeft})
	d.dispatch(tea.MouseMotionMsg{X: ox + 4, Y: oy, Button: tea.MouseLeft})
	d.dispatch(tea.MouseReleaseMsg{X: ox + 4, Y: oy, Button: tea.MouseLeft})
	if m.chromeSel.Active() {
		t.Fatal("a pane press should clear the chrome selection")
	}
	if !m.focusedPane().term.HasSelection() {
		t.Fatal("the pane should have selected")
	}

	for range 2 {
		d.dispatch(tea.MouseClickMsg{X: col, Y: 0, Button: tea.MouseLeft})
		d.dispatch(tea.MouseReleaseMsg{X: col, Y: 0, Button: tea.MouseLeft})
	}
	if m.focusedPane().term.HasSelection() {
		t.Fatal("a chrome press should clear the pane's selection")
	}
}

// Clicking a tab's label in the strip selects that tab, the way the leader's
// digits do.
func TestClickingATabLabelSelectsIt(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	for want := 1; want <= 2; want++ {
		d.key("ctrl+a")
		d.key("s")
		d.wait("the tabs", func() bool { return len(m.shells) == want })
	}
	d.wait("the second tab focused", func() bool { return m.onShells && m.activeShell == 1 })

	// Drawing the strip is what records where the labels are.
	_ = rawFrame(m)
	if len(m.tabSpans) != 2 {
		t.Fatalf("tabSpans = %v, want both tabs", m.tabSpans)
	}
	span := m.tabSpans[0]
	x := m.width/2 + (span.start+span.end)/2
	d.dispatch(tea.MouseClickMsg{X: x, Y: 1, Button: tea.MouseLeft})
	d.dispatch(tea.MouseReleaseMsg{X: x, Y: 1, Button: tea.MouseLeft})
	if !m.onShells || m.activeShell != 0 {
		t.Fatalf("onShells=%v activeShell=%d, want the clicked tab", m.onShells, m.activeShell)
	}
}

// Clicking anywhere on a pane's box — its border, its title — focuses that
// pane, not just clicks on its grid.
func TestClickingABorderFocusesItsPane(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key("s")
	d.wait("the tab", func() bool { return len(m.shells) == 1 })
	d.wait("the tab focused", func() bool { return m.onShells })

	// The terminal's title row, left of the split.
	d.dispatch(tea.MouseClickMsg{X: 2, Y: 1, Button: tea.MouseLeft})
	d.dispatch(tea.MouseReleaseMsg{X: 2, Y: 1, Button: tea.MouseLeft})
	if m.onShells {
		t.Fatal("clicking the terminal's border should focus the terminal")
	}

	// The shell box's bottom border, right of the split.
	x, y := m.width/2+2, m.paneRows()+2
	d.dispatch(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	d.dispatch(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
	if !m.onShells {
		t.Fatal("clicking the shell box's border should focus the tab")
	}
}

// Clicking a box's [+] gives that column the whole window, and the [-] it
// turns into gives the window back to the split.
func TestClickingTheMaximizeButtonTakesTheWindow(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key("s")
	d.wait("the tab", func() bool { return len(m.shells) == 1 })
	d.wait("the tab focused", func() bool { return m.onShells })

	// Drawing the boxes is what records where their buttons are.
	_ = rawFrame(m)
	term, _ := zoomButtons(t, m)

	// The terminal's, which maximizes it and takes the focus with it.
	clickAt(d, term, 1)
	if !m.maximized || m.onShells {
		t.Fatalf("maximized=%v onShells=%v, want the terminal with the window", m.maximized, m.onShells)
	}
	if got := m.paneWidthOf(m.terminal); got != m.width {
		t.Fatalf("the terminal is %d cells wide, want the whole window (%d)", got, m.width)
	}
	if strings.Contains(frameText(m), "1 zsh") {
		t.Fatalf("the tab strip should be off the screen:\n%s", frameText(m))
	}

	// The one button on screen is now the restore, and it gives the split back.
	_ = rawFrame(m)
	restore, _ := zoomButtons(t, m)
	clickAt(d, restore, 1)
	if m.maximized {
		t.Fatal("clicking the restore button should give the window back")
	}

	// And the shell box's own button maximizes that side instead.
	_ = rawFrame(m)
	_, shells := zoomButtons(t, m)
	clickAt(d, shells, 1)
	if !m.maximized || !m.onShells {
		t.Fatalf("maximized=%v onShells=%v, want the tabs with the window", m.maximized, m.onShells)
	}
	if got := m.paneWidthOf(m.shells[0]); got != m.width {
		t.Fatalf("the tab is %d cells wide, want the whole window (%d)", got, m.width)
	}
}

// A maximized shell box still takes clicks on its own tabs and its own grid,
// which have moved to where the terminal's used to be.
func TestAMaximizedShellBoxIsStillClickable(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	for want := 1; want <= 2; want++ {
		d.key("ctrl+a")
		d.key("s")
		d.wait("the tabs", func() bool { return len(m.shells) == want })
	}
	d.wait("the second tab focused", func() bool { return m.onShells && m.activeShell == 1 })

	m.toggleMaximized(true)
	_ = rawFrame(m)
	if len(m.tabSpans) != 2 {
		t.Fatalf("tabSpans = %v, want both tabs", m.tabSpans)
	}
	span := m.tabSpans[0]
	clickAt(d, (span.start+span.end)/2, 1)
	if !m.onShells || m.activeShell != 0 {
		t.Fatalf("onShells=%v activeShell=%d, want the clicked tab", m.onShells, m.activeShell)
	}

	// The grid moved with the box: a press past the old split lands in it.
	ox, oy := m.paneOrigin(m.focusedPane())
	if ox != 1+boxPad {
		t.Fatalf("the maximized box draws its grid at %d, want the window's own edge", ox)
	}
	d.dispatch(tea.MouseClickMsg{X: m.width - 4, Y: oy, Button: tea.MouseLeft})
	d.dispatch(tea.MouseMotionMsg{X: m.width - 2, Y: oy, Button: tea.MouseLeft})
	d.dispatch(tea.MouseReleaseMsg{X: m.width - 2, Y: oy, Button: tea.MouseLeft})
	if m.chromeSel.Active() {
		t.Fatal("a press inside the maximized grid should belong to the pane, not the chrome")
	}
}

// zoomButtons is the middle column of each maximize button on screen, by the
// column it belongs to. A button that is not drawn comes back as -1.
func zoomButtons(t *testing.T, m *Model) (term, shells int) {
	t.Helper()
	term, shells = -1, -1
	for _, s := range m.zoomSpans {
		if s.shells {
			shells = (s.start + s.end) / 2
		} else {
			term = (s.start + s.end) / 2
		}
	}
	return term, shells
}

// clickAt presses and releases the left button on one cell.
func clickAt(d *driver, x, y int) {
	d.dispatch(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	d.dispatch(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
}

// The copy chords work over a chrome selection the way they do over a
// pane's, and are swallowed rather than reaching the sandbox.
func TestCopyChordOverAChromeSelection(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "enter")

	col := headerCol(t, m, "sbx_one") + 2
	for range 2 {
		d.dispatch(tea.MouseClickMsg{X: col, Y: 0, Button: tea.MouseLeft})
		d.dispatch(tea.MouseReleaseMsg{X: col, Y: 0, Button: tea.MouseLeft})
	}
	d.wait("the first copy", func() bool { return m.status == "copied" })

	d.dispatch(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if m.chromeSel.Active() {
		t.Fatal("the chord should clear the chrome selection")
	}
	term.send("MARK")
	d.wait("the marker", func() bool { return strings.Contains(frameText(m), "MARK") })
	if got := term.typed(""); strings.Contains(got, "\x03") {
		t.Fatalf("the chord reached the sandbox: %q", got)
	}
}
