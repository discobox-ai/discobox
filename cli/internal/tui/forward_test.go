package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/discobox-ai/discobox/sandboxservices"
)

// forwardSandboxes is the listing with the first sandbox serving something, so
// the workspace's header has ports to draw arrows on.
func forwardSandboxes() []Sandbox {
	sandboxes := testSandboxes()
	sandboxes[0].Ports = []Port{
		{Number: 8080, Protocol: "http"},
		{Number: 5432, Protocol: "tcp"},
	}
	return sandboxes
}

// The workspace opens a forward with itself: the header lists what the
// discobox is serving, and the point is that those ports are reachable from
// here without asking for anything.
func TestWorkspaceOpensAForward(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(forwardSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the forward", func() bool { return m.forward != nil })

	ds.mu.Lock()
	opened := ds.forwardsOpen
	ds.mu.Unlock()
	if opened != 1 {
		t.Fatalf("forwards opened = %d, want 1", opened)
	}
}

// A port that gets bound while the screen is up appears on it without anything
// being pressed: the bind wakes the window, and the header redraws from the
// forward.
func TestWorkspaceHeaderShowsBoundPortsAsTheyArrive(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(forwardSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the forward", func() bool { return m.forward != nil })

	if frame := ansi.Strip(frameText(m)); !strings.Contains(frame, "http:8080") {
		t.Fatalf("the header should list the port before it is bound:\n%s", frame)
	}

	ds.forward.bind(Binding{Port: 8080, Local: 8082})
	ds.forward.bind(Binding{Port: 5432, Local: 5433})
	d.wait("the arrows", func() bool {
		return strings.Contains(ansi.Strip(frameText(m)), "http:8082->8080")
	})
	if frame := ansi.Strip(frameText(m)); !strings.Contains(frame, "tcp:5433->5432") {
		t.Fatalf("a forwarded tcp port should show its local port too:\n%s", frame)
	}
	// The web port is a link to the local end of the forward; the database is
	// not a link at all.
	if raw := rawFrame(m); !strings.Contains(raw, "http://localhost:8082") {
		t.Errorf("the forwarded http port should link to localhost:8082:\n%q", raw)
	}
	if raw := rawFrame(m); strings.Contains(raw, "localhost:5433") {
		t.Errorf("a forwarded tcp port should carry no link:\n%q", raw)
	}
}

// Detaching releases the local ports. They were taken to serve a screen that
// is gone, and nothing on screen would account for them.
func TestDetachingClosesTheForward(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(forwardSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the forward", func() bool { return m.forward != nil })

	d.key("ctrl+a")
	d.key("d")
	d.wait("the workspace to close", func() bool { return m.forward == nil })

	ds.mu.Lock()
	closed := ds.forwardsClose
	ds.mu.Unlock()
	if closed != 1 {
		t.Fatalf("forwards closed = %d, want 1", closed)
	}
}

// A forward that cannot be opened is reported and costs nothing else. The
// terminals are the screen; the ports ride on its header, which goes on saying
// what the discobox is serving with no arrows on it.
//
// The message is asserted on the model rather than the frame because the
// workspace screen draws hints where every other screen draws the status line
// — so it is read on the way back out, which is where the window can be acted
// on anyway.
func TestAForwardThatFailsLeavesTheWorkspaceOpen(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(forwardSandboxes()...)
	ds.forwardErr = errors.New("no route to the sandbox")
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the failure", func() bool {
		return strings.Contains(m.status, "ports are not being forwarded")
	})

	if m.primary() == nil {
		t.Fatal("the workspace should still be open")
	}
	if m.forward != nil {
		t.Fatal("a forward that failed should leave nothing to close")
	}
	frame := ansi.Strip(frameText(m))
	if !strings.Contains(frame, "http:8080") {
		t.Fatalf("the header should still list what the discobox is serving:\n%s", frame)
	}
	if strings.Contains(frame, "->8080") {
		t.Fatalf("nothing is forwarded, so no arrow should be drawn:\n%s", frame)
	}
}

// desktopSandboxes is the listing with the first sandbox serving a dev server
// and a graphical desktop, which is every kind of header link at once.
func desktopSandboxes() []Sandbox {
	sandboxes := testSandboxes()
	sandboxes[0].Ports = []Port{
		{Number: 8080, Protocol: "http"},
		{Number: 5432, Protocol: "tcp"},
		{Number: 6900, Protocol: "http", ServiceID: sandboxservices.DesktopID, ServiceName: "Desktop"},
	}
	return sandboxes
}

// A header link is a link to the terminal — Ctrl-click follows the OSC 8 — but
// the gesture people actually make at one is a plain click, and that opens it
// too, through whatever this machine opens URLs with.
func TestClickingAHeaderLinkOpensIt(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(desktopSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the forward", func() bool { return m.forward != nil })

	opened := make(chan string, 4)
	m.openOS = func(url string) error { opened <- url; return nil }

	ds.forward.bind(Binding{Port: 8080, Local: 8082})
	ds.forward.bind(Binding{Port: 5432, Local: 5433})
	ds.forward.bind(Binding{Port: 6900, Local: 6901})
	d.wait("the links", func() bool {
		return strings.Contains(plainFrame(m), "Desktop") &&
			strings.Contains(plainFrame(m), "http:8082->8080")
	})

	x, y := at(t, m, "Desktop")
	tap(t, m, x, y)
	if got := <-opened; got != "http://localhost:6901" {
		t.Errorf("clicking the desktop opened %q, want the local end of its forward", got)
	}

	// The port opens as the local end too, arrow and all: the number the
	// header draws is not the one to open.
	x, y = at(t, m, "8082->8080")
	tap(t, m, x, y)
	if got := <-opened; got != "http://localhost:8082" {
		t.Errorf("clicking the web port opened %q, want the local end of its forward", got)
	}

	// A port with nothing to open is not a button: the press falls through to
	// the selection, the way the rest of the header does.
	x, y = at(t, m, "5433->5432")
	tap(t, m, x, y)
	select {
	case got := <-opened:
		t.Errorf("clicking a tcp port opened %q, want nothing opened", got)
	default:
	}

	// A double click is reflexive on a link, and a second tab is not what it
	// meant: the page opens once.
	x, y = at(t, m, "Desktop")
	doublePress(t, m, x, y)
	if got := <-opened; got != "http://localhost:6901" {
		t.Errorf("double-clicking the desktop opened %q first, want the local end of its forward", got)
	}
	select {
	case got := <-opened:
		t.Errorf("double-clicking the desktop opened %q a second time, want it opened once", got)
	default:
	}

	// Ctrl-click is the terminal's: it follows the OSC 8 itself, and a
	// terminal that reports the press as well must not get a second page.
	slowClock(m)
	send(t, m,
		tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft, Mod: tea.ModCtrl},
		tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft, Mod: tea.ModCtrl},
	)
	select {
	case got := <-opened:
		t.Errorf("a Ctrl-click opened %q from the window, want it left to the terminal", got)
	default:
	}
}

// A link says it is one before it is pressed, the same way a key hint does:
// the pointer resting on it lights that span and nothing beside it.
func TestAHeaderLinkLightsUnderThePointer(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(desktopSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the forward", func() bool { return m.forward != nil })
	// The frame is rebuilt with color on, so what a real terminal is sent is
	// what the test looks at.
	m.st = newStyles(true)

	ds.forward.bind(Binding{Port: 6900, Local: 6901})
	d.wait("the desktop link", func() bool { return strings.Contains(plainFrame(m), "Desktop") })

	x, y := at(t, m, "Desktop")
	send(t, m, tea.MouseMotionMsg{X: x, Y: y, Button: tea.MouseNone})

	live := hyperlink("http://localhost:6901", m.st.hover.Render("Desktop"))
	if raw := rawFrame(m); !strings.Contains(raw, live) {
		t.Errorf("the pointer on the desktop link did not light it:\n%q", raw)
	}
	// Lit or not, it stays a link: the OSC 8 is what a Ctrl-click follows, and
	// losing it under the pointer would take that away exactly there.
	if raw := rawFrame(m); !strings.Contains(raw, "http://localhost:6901") {
		t.Errorf("the lit link lost its OSC 8:\n%q", raw)
	}

	// The pointer somewhere else on the row leaves it as it was.
	send(t, m, tea.MouseMotionMsg{X: 0, Y: y, Button: tea.MouseNone})
	if raw := rawFrame(m); strings.Contains(raw, live) {
		t.Errorf("the desktop link stayed lit with the pointer off it:\n%q", raw)
	}
}
