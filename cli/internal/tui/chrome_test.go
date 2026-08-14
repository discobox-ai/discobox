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
