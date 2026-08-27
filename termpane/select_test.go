package termpane

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func press(x, y int) tea.MouseClickMsg {
	return tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft}
}

func rightPress(x, y int) tea.MouseClickMsg {
	return tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseRight}
}

func drag(x, y int) tea.MouseMotionMsg {
	return tea.MouseMotionMsg{X: x, Y: y, Button: tea.MouseLeft}
}

func release(x, y int) tea.MouseReleaseMsg {
	return tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft}
}

// copied runs the command HandleMouse returned and expects the finished
// selection's text on it.
func copied(t *testing.T, cmd tea.Cmd) string {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a CopyMsg command, got none")
	}
	msg, ok := runWithin(cmd, time.Second)
	if !ok {
		t.Fatal("CopyMsg command did not return")
	}
	copyMsg, ok := msg.(CopyMsg)
	if !ok {
		t.Fatalf("got %T, want CopyMsg", msg)
	}
	return copyMsg.Text
}

func TestDragSelectsAndCopies(t *testing.T) {
	m, stream, cmd := attach(t, 40, 5)
	stream.send("hello world")
	pump(t, m, cmd, "hello world")

	if cmd := m.HandleMouse(press(0, 0)); cmd != nil {
		t.Fatal("a press alone should not copy")
	}
	m.HandleMouse(drag(4, 0))
	if got := copied(t, m.HandleMouse(release(4, 0))); got != "hello" {
		t.Fatalf("copied %q, want %q", got, "hello")
	}
	if !m.HasSelection() || m.SelectionText() != "hello" {
		t.Fatalf("selection should persist after release; got %q", m.SelectionText())
	}
}

func TestSelectionIsHighlightedInTheView(t *testing.T) {
	m, stream, cmd := attach(t, 40, 5)
	stream.send("hello world")
	pump(t, m, cmd, "hello world")

	before := m.View()[0]
	if strings.Contains(before, "\x1b[7m") {
		t.Fatal("reverse video before any selection")
	}
	m.HandleMouse(press(0, 0))
	m.HandleMouse(drag(4, 0))
	m.HandleMouse(release(4, 0))
	if after := m.View()[0]; !strings.Contains(after, "\x1b[7m") {
		t.Fatalf("selected row should carry reverse video:\n%q", after)
	}
	m.ClearSelection()
	if cleared := m.View()[0]; strings.Contains(cleared, "\x1b[7m") {
		t.Fatalf("highlight survived ClearSelection:\n%q", cleared)
	}
}

func TestDoubleClickCopiesAWord(t *testing.T) {
	m, stream, cmd := attach(t, 40, 5)
	stream.send("hello world")
	pump(t, m, cmd, "hello world")

	m.HandleMouse(press(8, 0))
	m.HandleMouse(release(8, 0))
	m.HandleMouse(press(8, 0))
	if got := copied(t, m.HandleMouse(release(8, 0))); got != "world" {
		t.Fatalf("copied %q, want %q", got, "world")
	}
}

func TestSelectionReachesTheScrollback(t *testing.T) {
	m, stream, cmd := attach(t, 40, 5)
	stream.send(numberedLines(20))
	pump(t, m, cmd, "line 20")

	// Ten lines back, the top row shows line 6 out of the scrollback.
	m.Scroll(10)
	m.HandleMouse(press(0, 0))
	m.HandleMouse(drag(5, 0))
	if got := copied(t, m.HandleMouse(release(5, 0))); got != "line 6" {
		t.Fatalf("copied %q, want %q", got, "line 6")
	}
}

func TestSelectionSurvivesScrollingIntoHistory(t *testing.T) {
	m, stream, cmd := attach(t, 40, 5)
	stream.send(numberedLines(20))
	cmd = pump(t, m, cmd, "line 20")

	// Select "line 16" on the top screen row, then let output push it into
	// the scrollback: the same absolute line still reads the same text, so
	// the selection rides along.
	m.HandleMouse(press(0, 0))
	m.HandleMouse(drag(6, 0))
	m.HandleMouse(release(6, 0))
	stream.send("\r\nline 21")
	pump(t, m, cmd, "line 21")
	if !m.HasSelection() || m.SelectionText() != "line 16" {
		t.Fatalf("selection should survive scrolling; got %q", m.SelectionText())
	}
}

func TestOverwritingTheSelectionClearsIt(t *testing.T) {
	m, stream, cmd := attach(t, 40, 5)
	stream.send(numberedLines(20))
	cmd = pump(t, m, cmd, "line 20")

	// Select the bottom row, then overwrite it in place: the text the
	// selection named no longer exists.
	m.HandleMouse(press(0, 4))
	m.HandleMouse(drag(6, 4))
	m.HandleMouse(release(6, 4))
	if !m.HasSelection() {
		t.Fatal("no selection to begin with")
	}
	stream.send("\x1b[5;1HXXXXXXX")
	pump(t, m, cmd, "XXXXXXX")
	if m.HasSelection() {
		t.Fatalf("selection survived being overwritten: %q", m.SelectionText())
	}
}

func TestResizeClearsTheSelection(t *testing.T) {
	m, stream, cmd := attach(t, 40, 5)
	stream.send("hello world")
	pump(t, m, cmd, "hello world")

	m.HandleMouse(press(0, 0))
	m.HandleMouse(drag(4, 0))
	m.HandleMouse(release(4, 0))
	m.SetSize(60, 8)
	if m.HasSelection() {
		t.Fatal("selection survived a resize")
	}
}

func TestApplicationMouseWinsUnlessSeized(t *testing.T) {
	m, stream, cmd := attach(t, 40, 5)
	// The application asks for the mouse; the marker proves it was processed.
	stream.send("\x1b[?1000hREADY")
	pump(t, m, cmd, "READY")

	// Events forward to the application; selection stays inert.
	m.HandleMouse(press(0, 0))
	m.HandleMouse(drag(4, 0))
	if cmd := m.HandleMouse(release(4, 0)); cmd != nil {
		t.Fatal("selection ran while the application owned the mouse")
	}
	if m.HasSelection() {
		t.Fatal("selection active while the application owned the mouse")
	}
	// All three, not just the first: what is still in flight would arrive after
	// the clear below and read as an event forwarded while seized.
	if got := stream.sentN(t, "\x1b[M", 3); strings.Count(got, "\x1b[M") != 3 {
		t.Fatalf("mouse events were not forwarded: %q", got)
	}

	// Seized, the same gesture selects and nothing more is forwarded.
	stream.mu.Lock()
	stream.written = nil
	stream.mu.Unlock()
	m.SetSeized(true)
	m.HandleMouse(press(0, 0))
	m.HandleMouse(drag(4, 0))
	if got := copied(t, m.HandleMouse(release(4, 0))); got != "READY" {
		t.Fatalf("copied %q, want %q", got, "READY")
	}
	stream.mu.Lock()
	forwarded := strings.Contains(string(stream.written), "\x1b[M")
	stream.mu.Unlock()
	if forwarded {
		t.Fatal("mouse events forwarded while seized")
	}
}

func TestDragPastTheEdgeScrollsAndKeepsSelecting(t *testing.T) {
	m, stream, cmd := attach(t, 40, 5)
	stream.send(numberedLines(20))
	pump(t, m, cmd, "line 20")

	// Start on the top screen row and drag two rows above the pane: the view
	// scrolls back and the selection follows into history.
	m.HandleMouse(press(6, 0))
	m.HandleMouse(drag(0, -2))
	if m.ScrollOffset() == 0 {
		t.Fatal("dragging past the top edge should scroll back")
	}
	got := copied(t, m.HandleMouse(release(0, 0)))
	if !strings.Contains(got, "line 14") || !strings.Contains(got, "line 16") {
		t.Fatalf("copied %q, want lines 14 through 16", got)
	}
}

func wheel(up bool) tea.MouseWheelMsg {
	button := tea.MouseWheelDown
	if up {
		button = tea.MouseWheelUp
	}
	return tea.MouseWheelMsg{Button: button}
}

func TestWheelScrollsTheScrollbackWhenTheMouseIsFree(t *testing.T) {
	m, stream, cmd := attach(t, 40, 5)
	stream.send(numberedLines(20))
	pump(t, m, cmd, "line 20")

	m.HandleMouse(wheel(true))
	if got := m.ScrollOffset(); got != 3 {
		t.Fatalf("offset = %d after one tick up, want 3", got)
	}
	m.HandleMouse(wheel(false))
	if got := m.ScrollOffset(); got != 0 {
		t.Fatalf("offset = %d after a tick back down, want the live screen", got)
	}
}

func TestWheelForwardsWhenTheApplicationHasTheMouse(t *testing.T) {
	m, stream, cmd := attach(t, 40, 5)
	stream.send("\x1b[?1000hREADY")
	pump(t, m, cmd, "READY")

	m.HandleMouse(wheel(true))
	if got := stream.sent(t, "\x1b[M"); !strings.Contains(got, "\x1b[M") {
		t.Fatalf("wheel was not forwarded: %q", got)
	}
	if m.ScrollOffset() != 0 {
		t.Fatal("the pane scrolled a wheel that belonged to the application")
	}
}

func TestWheelBecomesArrowKeysOnTheAltScreen(t *testing.T) {
	m, stream, cmd := attach(t, 40, 5)
	stream.send("\x1b[?1049hPAGER")
	pump(t, m, cmd, "PAGER")

	m.HandleMouse(wheel(true))
	if got := stream.sent(t, "\x1b[A\x1b[A\x1b[A"); !strings.Contains(got, "\x1b[A\x1b[A\x1b[A") {
		t.Fatalf("wheel up should be three Up keys: %q", got)
	}
	m.HandleMouse(wheel(false))
	stream.sent(t, "\x1b[B\x1b[B\x1b[B")
	if m.ScrollOffset() != 0 {
		t.Fatal("the alternate screen has no scrollback to have scrolled")
	}
}

func chord(code rune, mod tea.KeyMod) tea.KeyPressMsg {
	return tea.KeyPressMsg(tea.Key{Code: code, Mod: mod})
}

// selectHello drags out the word "hello" on the top row.
func selectHello(t *testing.T, m *Model) {
	t.Helper()
	m.HandleMouse(press(0, 0))
	m.HandleMouse(drag(4, 0))
	m.HandleMouse(release(4, 0))
	if !m.HasSelection() {
		t.Fatal("no selection to copy")
	}
}

func TestRightClickCopiesTheSelection(t *testing.T) {
	m, stream, cmd := attach(t, 40, 5)
	stream.send("hello world")
	pump(t, m, cmd, "hello world")

	selectHello(t, m)
	if got := copied(t, m.HandleMouse(rightPress(2, 0))); got != "hello" {
		t.Fatalf("copied %q, want %q", got, "hello")
	}
	if m.HasSelection() {
		t.Fatal("copying should clear the selection")
	}
}

func TestRightClickWithoutASelectionDoesNothing(t *testing.T) {
	m, stream, cmd := attach(t, 40, 5)
	stream.send("hello world")
	pump(t, m, cmd, "hello world")

	if cmd := m.HandleMouse(rightPress(2, 0)); cmd != nil {
		t.Fatal("a right click with nothing selected should not copy")
	}
	// And it starts no gesture: the release that follows copies nothing.
	if cmd := m.HandleMouse(tea.MouseReleaseMsg{X: 2, Y: 0, Button: tea.MouseRight}); cmd != nil {
		t.Fatal("the right button should not drive selection")
	}
	if m.HasSelection() {
		t.Fatal("the right button selected something")
	}
}

func TestRightClickForwardsWhenTheApplicationHasTheMouse(t *testing.T) {
	m, stream, cmd := attach(t, 40, 5)
	stream.send("\x1b[?1000hREADY")
	pump(t, m, cmd, "READY")

	if cmd := m.HandleMouse(rightPress(2, 0)); cmd != nil {
		t.Fatal("copied while the application owned the mouse")
	}
	if got := stream.sent(t, "\x1b[M"); !strings.Contains(got, "\x1b[M") {
		t.Fatalf("the right click was not forwarded: %q", got)
	}
}

func TestCopyChordsCopyTheSelection(t *testing.T) {
	for name, mod := range map[string]tea.KeyMod{
		"ctrl+c":       tea.ModCtrl,
		"ctrl+shift+c": tea.ModCtrl | tea.ModShift,
		"super+c":      tea.ModSuper,
	} {
		t.Run(name, func(t *testing.T) {
			m, stream, cmd := attach(t, 40, 5)
			stream.send("hello world")
			pump(t, m, cmd, "hello world")

			selectHello(t, m)
			_, keyCmd := m.Update(chord('c', mod))
			if got := copied(t, keyCmd); got != "hello" {
				t.Fatalf("copied %q, want %q", got, "hello")
			}
			if m.HasSelection() {
				t.Fatal("copying should clear the selection")
			}
			// The chord was swallowed: the far end sees the marker typed after
			// it, and nothing of the chord itself before that.
			m.SendText("MARK")
			if got := stream.sent(t, "MARK"); strings.Contains(got, "\x03") {
				t.Fatalf("the chord reached the application: %q", got)
			}
		})
	}
}

func TestCtrlCWithoutASelectionInterrupts(t *testing.T) {
	m, stream, cmd := attach(t, 40, 5)
	stream.send("hello world")
	pump(t, m, cmd, "hello world")

	if _, keyCmd := m.Update(chord('c', tea.ModCtrl)); keyCmd != nil {
		t.Fatal("ctrl+c with no selection should not produce a command")
	}
	if got := stream.sent(t, "\x03"); !strings.Contains(got, "\x03") {
		t.Fatalf("the interrupt never reached the application: %q", got)
	}
}

func TestCopyOutranksACtrlCDetachKeyOnceThenDetaches(t *testing.T) {
	m, stream, cmd := attach(t, 40, 5, WithPrefix("ctrl+a", "ctrl+c"))
	stream.send("hello world")
	pump(t, m, cmd, "hello world")

	selectHello(t, m)
	_, keyCmd := m.Update(chord('c', tea.ModCtrl))
	if got := copied(t, keyCmd); got != "hello" {
		t.Fatalf("copied %q, want %q", got, "hello")
	}
	// The selection is gone, so the next press means what it always meant.
	_, keyCmd = m.Update(chord('c', tea.ModCtrl))
	msg, ok := runWithin(keyCmd, time.Second)
	if !ok {
		t.Fatal("second ctrl+c produced no message")
	}
	if _, ok := msg.(DetachMsg); !ok {
		t.Fatalf("got %T, want DetachMsg", msg)
	}
}

// numberedLines is n lines with no newline after the last, so the geometry of
// what scrolled off is exact.
func numberedLines(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		if i > 1 {
			b.WriteString("\r\n")
		}
		fmt.Fprintf(&b, "line %d", i)
	}
	return b.String()
}
