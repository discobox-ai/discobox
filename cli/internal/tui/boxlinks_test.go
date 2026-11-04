package tui

import (
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/discobox-ai/discobox/termpane"
)

// printBoxLink has the primary terminal print text with a box link in it, at
// the start of its first row, and waits for the frame to mark it. It returns
// the cell the grid starts at, in the window's coordinates (windowMouse).
func printBoxLink(t *testing.T, d *driver, m *Model, term *fakeTerminal, output string) (x, y int) {
	t.Helper()
	term.send(output)
	d.wait("the box link", func() bool {
		_ = m.View()
		_, ok := m.zones.find(hitBoxLink)
		return ok
	})
	return m.paneOrigin(m.primary())
}

// A bare discobox:///<id> in a pane is a link to that box on the pane's own
// server, and a click on it switches to it — even while the program in the
// pane has the mouse, which never hears of the press (ADR 26-11-03-113 §3).
func TestClickingABareBoxLinkFocusesTheBox(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(testSandboxes()...)
		d, m, term := openWorkspace(t, ds, "enter")
		term.send("\x1b[?1000h\x1b[?1006h")
		d.wait("mouse mode", func() bool { return m.primary().term.MouseMode() != termpane.MouseNone })

		x, y := printBoxLink(t, d, m, term, "discobox:///sbx_two is the worker")
		d.windowMouse(tea.MouseClickMsg{X: x + 3, Y: y, Button: tea.MouseLeft})
		d.windowMouse(tea.MouseReleaseMsg{X: x + 3, Y: y, Button: tea.MouseLeft})

		d.wait("the linked box", func() bool { return m.paneBox.ID == "sbx_two" && m.primary() != nil })
		if got := term.typed("\x1b[<"); strings.Contains(got, "\x1b[<") {
			t.Fatalf("the program was sent %q, want the press and release kept from it", got)
		}
	})
}

// A link the program emitted to a box loses its OSC 8 on the way out, since
// nothing but the window can open it, and is still the window's link: a
// Ctrl-click on it, which the terminal would otherwise have followed, focuses
// the box too.
func TestAnEmittedBoxLinkIsTheWindowsToOpen(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(testSandboxes()...)
		d, m, term := openWorkspace(t, ds, "enter")

		x, y := printBoxLink(t, d, m, term, ansi.SetHyperlink("discobox:///sbx_two")+"worker two"+ansi.ResetHyperlink())
		if strings.Contains(rawFrame(m), "\x1b]8;;discobox") || strings.Contains(rawFrame(m), ";discobox:///sbx_two") {
			t.Fatalf("the frame hands the terminal an OSC 8 to the box: %q", rawFrame(m))
		}
		if !strings.Contains(plainFrame(m), "worker two") {
			t.Fatalf("the link's text is gone:\n%s", plainFrame(m))
		}

		d.windowMouse(tea.MouseClickMsg{X: x + 1, Y: y, Button: tea.MouseLeft, Mod: tea.ModCtrl})
		d.windowMouse(tea.MouseReleaseMsg{X: x + 1, Y: y, Button: tea.MouseLeft, Mod: tea.ModCtrl})
		d.wait("the linked box", func() bool { return m.paneBox.ID == "sbx_two" && m.primary() != nil })
	})
}

// The full form naming a server the window lists opens the box there, however
// the window knows that server: the data source says which it is.
func TestAFullBoxLinkOnAListedServerFocusesTheBox(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const server = "discobox://box.example.com"
		ds := newFakeSource(testSandboxes()...)
		ds.servers = map[string]string{server: ""}
		d, m, term := openWorkspace(t, ds, "enter")

		x, y := printBoxLink(t, d, m, term, "see "+server+"/sbx_two")
		clickWindow(d, x+6, y)
		d.wait("the linked box", func() bool { return m.paneBox.ID == "sbx_two" && m.primary() != nil })
	})
}

// A link to a server the window does not list says so, naming the command
// that would reach it, and reaches nothing itself.
func TestABoxLinkOnAnUnknownServerSaysSo(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(testSandboxes()...)
		d, m, term := openWorkspace(t, ds, "enter")

		x, y := printBoxLink(t, d, m, term, "discobox://elsewhere.example.com/sbx_two")
		clickWindow(d, x+1, y)
		d.wait("the refusal", func() bool { return strings.Contains(m.status, "doesn't know") })
		if !strings.Contains(m.status, `discobox attach "discobox://elsewhere.example.com/sbx_two"`) {
			t.Fatalf("status = %q, want the command that registers the server", m.status)
		}
		if m.paneBox.ID != "sbx_one" {
			t.Fatalf("the workspace moved to %s, want it left where it was", m.paneBox.ID)
		}
	})
}

// A link to a box the server does not list says that; one to a box that is
// not running points at it on the list rather than starting it, since a click
// on text a program printed is no reason to start a box.
func TestABoxLinkToNoBoxOrAStoppedOne(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(testSandboxes()...)
		d, m, term := openWorkspace(t, ds, "enter")

		x, y := printBoxLink(t, d, m, term, "discobox:///sbx_nope")
		clickWindow(d, x+1, y)
		d.wait("the miss", func() bool { return strings.Contains(m.status, `no discobox "sbx_nope" on this server`) })

		term.send("\r\x1b[2Kdiscobox:///sbx_three")
		d.wait("the second link", func() bool {
			_ = m.View()
			z, ok := m.zones.find(hitBoxLink)
			return ok && z.what.url == "discobox:///sbx_three"
		})
		// Past a double click, which the same cell pressed again at once
		// would be.
		time.Sleep(2 * clickRun)
		clickWindow(d, x+1, y)
		d.wait("the stopped box", func() bool { return strings.Contains(m.status, "enter starts it") })
		if m.paneBox.ID != "sbx_one" {
			t.Fatalf("the workspace moved to %s, want a stopped box pointed at, not opened", m.paneBox.ID)
		}
	})
}

// The link under the pointer is lit, as the window's other links are, and with
// the mouse seized (leader m) it is plain text to select: neither marked nor
// lit.
func TestABoxLinkLightsAndASeizedMouseSelectsIt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(testSandboxes()...)
		d, m, term := openWorkspace(t, ds, "enter")

		x, y := printBoxLink(t, d, m, term, "discobox:///sbx_two")
		unlit := rawFrame(m)
		d.windowMouse(tea.MouseMotionMsg{X: x + 2, Y: y})
		if lit := rawFrame(m); lit == unlit {
			t.Fatal("the link under the pointer is drawn as it is without it")
		}

		m.mouseSeized = true
		_ = m.View()
		if _, ok := m.zones.find(hitBoxLink); ok {
			t.Fatal("a seized mouse still marks the link")
		}
		clickWindow(d, x+2, y)
		if m.paneBox.ID != "sbx_one" {
			t.Fatalf("a seized press followed the link to %s", m.paneBox.ID)
		}
	})
}

// In a pane, discobox:///<id> is a box on the server of the pane's own box —
// where the program that printed it runs — and is looked for there alone
// (ADR 26-11-03-113 §2).
func TestALocalBoxLinkIsOnThePanesServer(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		boxes := testSandboxes()
		boxes[0].Server, boxes[1].Server = "beta", "alpha"
		ds := newFakeSource(boxes...)
		ds.session.Servers = []string{"alpha", "beta"}
		d, m, term := openWorkspace(t, ds, "enter")
		if m.paneBox.ID != "sbx_one" {
			t.Fatalf("opened %s, want sbx_one", m.paneBox.ID)
		}

		x, y := printBoxLink(t, d, m, term, "discobox:///sbx_two")
		clickWindow(d, x+1, y)
		d.wait("the miss", func() bool { return strings.Contains(m.status, `no discobox "sbx_two" on beta`) })
	})
}

// What a box link says on the status line is the program's text, and the
// status line is drawn outside any pane's emulator: a control in it — here an
// ESC the link percent-encodes, which parsing decodes — is shown escaped,
// never handed to the terminal (cli/REVIEW.md).
func TestABoxLinksStatusIsEscaped(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(testSandboxes()...)
		d, m, term := openWorkspace(t, ds, "enter")

		x, y := printBoxLink(t, d, m, term, "discobox:///%1b]52;c;aGk=%07")
		clickWindow(d, x+1, y)
		d.wait("the miss", func() bool { return strings.Contains(m.status, "no discobox") })
		if strings.ContainsAny(m.status, "\x1b\x07") {
			t.Fatalf("status = %q, want its controls escaped", m.status)
		}
		if !strings.Contains(m.status, `\x1b]52`) {
			t.Fatalf("status = %q, want the ESC shown as \\x1b", m.status)
		}
	})
}

// A window that is one command's own (discobox attach) shows its one box and
// never the list, so a box link there switches to nothing: it says which box
// it is and how to open it.
func TestABoxLinkInAnAttachWindowSaysHowToOpenIt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(testSandboxes()...)
		d, m, term := openBare(t, ds)

		// Attach opens from its given box before the listing arrives. The
		// link needs that listing to identify the other box.
		d.wait("the box listing", func() bool { return m.list.loaded })

		x, y := printBoxLink(t, d, m, term, "discobox:///sbx_two")
		clickWindow(d, x+1, y)
		d.wait("the refusal", func() bool { return strings.Contains(m.status, `discobox attach "sbx_two"`) })
		if m.paneBox.ID != "sbx_one" || !m.bare() {
			t.Fatalf("the attach window moved to %s (bare %v), want it left on its own box", m.paneBox.ID, m.bare())
		}
	})
}

// A link the window cannot match while a listed server did not say who it is
// says so, rather than that the server is unknown: it may be that one.
func TestABoxLinkWhenAServerDidNotAnswer(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(testSandboxes()...)
		ds.unsureServers = errors.New("gamma did not say what peer ID it has")
		d, m, term := openWorkspace(t, ds, "enter")

		x, y := printBoxLink(t, d, m, term, "discobox://elsewhere.example.com/sbx_two")
		clickWindow(d, x+1, y)
		d.wait("the doubt", func() bool { return strings.Contains(m.status, "gamma did not say") })
		if strings.Contains(m.status, "doesn't know") {
			t.Fatalf("status = %q, want it not to call the server unknown", m.status)
		}
	})
}

// An answer about a link's server that arrives after the person moved to
// another box is for the screen it was asked from, and is dropped.
func TestALateBoxLinkAnswerIsDropped(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(testSandboxes()...)
		_, m, _ := openWorkspace(t, ds, "enter")

		if cmd := m.boxLinkServer(boxLinkServerMsg{uri: "discobox://x/sbx_two", sandbox: "sbx_two", known: true, from: "sbx_other"}); cmd != nil {
			t.Fatalf("a late answer did something: %#v", cmd())
		}
		if m.paneBox.ID != "sbx_one" {
			t.Fatalf("the workspace moved to %s on a late answer", m.paneBox.ID)
		}
	})
}
