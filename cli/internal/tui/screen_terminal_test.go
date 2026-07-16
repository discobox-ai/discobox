package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func newTestTerminal(t *testing.T) (*terminalScreen, *fakeSource) {
	t.Helper()
	f := &fakeSource{sandboxes: makeSandboxes(1)}
	s := newTerminalScreen(context.Background(), f, defaultKeyMap(), defaultStyles(), f.sandboxes[0])

	// A resize with known dimensions triggers the attach.
	_, cmd := s.Update(resizeMsg{width: 80, height: 24})
	opened := runCmd(cmd)
	msg, ok := opened.(ttyOpenedMsg)
	if !ok {
		t.Fatalf("open cmd returned %T, want ttyOpenedMsg", opened)
	}
	next, _ := s.Update(msg)
	s = next.(*terminalScreen)
	if !s.ready {
		t.Fatal("terminal not ready after open")
	}
	return s, f
}

func TestTerminalRendersOutput(t *testing.T) {
	s, f := newTestTerminal(t)

	go f.terminal.feed("hello-world")
	msg := readNext(s.reader)()
	out, ok := msg.(ttyOutputMsg)
	if !ok {
		t.Fatalf("readNext returned %T, want ttyOutputMsg", msg)
	}
	s.Update(out)

	view := s.View(80, 24)
	if !strings.Contains(view, "hello-world") {
		t.Fatalf("terminal view missing output:\n%s", view)
	}
}

func TestTerminalForwardsKeys(t *testing.T) {
	s, f := newTestTerminal(t)

	_, cmd := s.Update(keyPress("a"))
	runCmd(cmd)

	select {
	case got := <-f.terminal.writes:
		if string(got) != "a" {
			t.Fatalf("forwarded %q, want %q", got, "a")
		}
	case <-time.After(time.Second):
		t.Fatal("key not forwarded to terminal")
	}
}

// TestTerminalPaneNeverWraps guards the invariant that keeps the hardware cursor
// aligned with the emulator grid: every rendered pane line is exactly the pane
// width, and the pane is exactly the requested height. A wrapped line (as the
// title once did) would break both and shift the cursor.
func TestTerminalPaneNeverWraps(t *testing.T) {
	s, f := newTestTerminal(t)
	s.Update(resizeMsg{width: 100, height: 24})

	// A line wider than the pane must be truncated, not wrapped.
	go f.terminal.feed("\x1b[2J\x1b[H" + strings.Repeat("X", 300))
	s.Update(readNext(s.reader)())

	const width, height = 100, 24
	out := s.View(width, height)
	lines := strings.Split(out, "\n")
	if len(lines) != height {
		t.Fatalf("pane has %d lines, want %d", len(lines), height)
	}
	for i, ln := range lines {
		if w := ansi.StringWidth(ln); w != width {
			t.Fatalf("line %d width = %d, want %d", i, w, width)
		}
	}
}

// TestTerminalCursorFollowsEmulator checks the cursor lands on the emulator's
// reported cell offset by the frame's border and one-line title.
func TestTerminalCursorFollowsEmulator(t *testing.T) {
	s, f := newTestTerminal(t)

	// Move the emulator cursor to row 6, col 11 (1-indexed) => cell (x=10, y=5).
	go f.terminal.feed("\x1b[6;11H")
	s.Update(readNext(s.reader)())

	cur := s.cursor(0, headerHeight) // body starts at row headerHeight (1)
	if cur == nil {
		t.Fatal("cursor is nil while ready")
	}
	// x = originX(0) + border(1) + 10; y = originY(1) + border(1) + title(1) + 5.
	if cur.X != 11 || cur.Y != 8 {
		t.Fatalf("cursor = (%d,%d), want (11,8)", cur.X, cur.Y)
	}
}

func TestTerminalForwardsShiftedCharacters(t *testing.T) {
	s, f := newTestTerminal(t)

	// Shift+a arrives as base code 'a' + ModShift with the shifted Text "A".
	// It must forward the uppercase letter, not be dropped by the key encoder.
	for _, tc := range []struct {
		name string
		key  tea.Key
		want string
	}{
		{"uppercase", tea.Key{Code: 'a', ShiftedCode: 'A', Text: "A", Mod: tea.ModShift}, "A"},
		{"shifted symbol", tea.Key{Code: '1', Text: "!", Mod: tea.ModShift}, "!"},
	} {
		s.Update(tea.KeyPressMsg(tc.key))
		select {
		case got := <-f.terminal.writes:
			if string(got) != tc.want {
				t.Fatalf("%s: forwarded %q, want %q", tc.name, got, tc.want)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s: not forwarded", tc.name)
		}
	}
}

func TestTerminalForwardsPaste(t *testing.T) {
	s, f := newTestTerminal(t)

	s.Update(tea.PasteMsg{Content: "pasted text"})

	select {
	case got := <-f.terminal.writes:
		// No app has enabled bracketed paste, so the text arrives raw.
		if string(got) != "pasted text" {
			t.Fatalf("forwarded %q, want %q", got, "pasted text")
		}
	case <-time.After(time.Second):
		t.Fatal("paste not forwarded to terminal")
	}
}

func TestTerminalDetachPrefix(t *testing.T) {
	s, _ := newTestTerminal(t)

	// Ctrl-A arms the prefix without sending anything.
	_, cmd := s.Update(tea.KeyPressMsg(tea.Key{Code: 'a', Mod: tea.ModCtrl}))
	if !s.prefixArmed {
		t.Fatal("ctrl+a did not arm the detach prefix")
	}
	if runCmd(cmd) != nil {
		t.Fatal("ctrl+a should not emit a command")
	}

	// d after the prefix detaches back to the list.
	_, cmd = s.Update(keyPress("d"))
	if _, ok := runCmd(cmd).(backMsg); !ok {
		t.Fatal("ctrl+a d did not detach")
	}
}

func TestTerminalConnectionEventsProduceStatus(t *testing.T) {
	s, f := newTestTerminal(t)

	f.terminal.events <- TerminalEvent{State: TerminalReconnecting}
	msg := waitTerminalEvent(s.ctx, s.term)()
	next, cmd := s.Update(msg)
	s = next.(*terminalScreen)
	status, ok := statusFromBatch(cmd)
	if !ok || status.text != "reconnecting…" {
		t.Fatalf("reconnecting status = %#v, want reconnecting…", status)
	}

	f.terminal.events <- TerminalEvent{State: TerminalReconnected}
	msg = waitTerminalEvent(s.ctx, s.term)()
	_, cmd = s.Update(msg)
	status, ok = statusFromBatch(cmd)
	if !ok || status.text != "reconnected" {
		t.Fatalf("reconnected status = %#v, want reconnected", status)
	}
}

func statusFromBatch(cmd tea.Cmd) (statusMsg, bool) {
	batch, ok := runCmd(cmd).(tea.BatchMsg)
	if !ok {
		return statusMsg{}, false
	}
	for _, cmd := range batch {
		if status, ok := runCmd(cmd).(statusMsg); ok {
			return status, true
		}
	}
	return statusMsg{}, false
}
