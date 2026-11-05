package tui

import (
	"strings"
	"testing"
	"testing/synctest"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// openBare is an attach window on the first discobox, past its wait screen,
// with nothing in it but the primary terminal: the bare window (bare.go).
func openBare(t *testing.T, ds *fakeSource) (*driver, *Model, *fakeTerminal) {
	t.Helper()
	d, m := openAttached(t, ds, ds.sandboxes[0])
	d.wait("the primary terminal", func() bool { return m.primary() != nil && m.dialog == nil })
	if !m.bare() {
		t.Fatalf("an attach with only its primary should be bare:\n%s", plainFrame(m))
	}
	return d, m, ds.execTerm(ExecPrimary)
}

// bareStatus is the bare window's last row, as plain text.
func bareStatus(m *Model) string {
	lines := strings.Split(plainFrame(m), "\n")
	return lines[len(lines)-1]
}

// A bare attach is the terminal over the whole screen but its last row, at the
// screen's origin, and one line under it saying where the box was cut from
// and the two ways on.
func TestABareAttachIsTheTerminalAndOneLine(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(testSandboxes()...)
		d, m, term := openBare(t, ds)

		d.wait("the primary at the whole screen", func() bool { return term.size() == [2]int{120, 39} })
		if x, y := m.paneOrigin(m.primary()); x != 0 || y != 0 {
			t.Fatalf("the grid starts at %d,%d, want the screen's origin", x, y)
		}

		term.send("abc")
		d.wait("output", func() bool { return strings.Contains(plainFrame(m), "abc") })
		lines := strings.Split(plainFrame(m), "\n")
		if len(lines) != 40 {
			t.Fatalf("frame = %d rows, want the window's 40", len(lines))
		}
		for i, line := range lines {
			if w := ansi.StringWidth(line); w != 120 {
				t.Fatalf("line %d is %d cells, want 120: %q", i, w, line)
			}
		}
		if !strings.HasPrefix(lines[0], "abc") {
			t.Fatalf("the first row = %q, want the terminal's own first row", lines[0])
		}
		if cursor := m.View().Cursor; cursor == nil || cursor.X != 3 || cursor.Y != 0 {
			t.Fatalf("cursor = %v, want it after the text at 3,0", cursor)
		}
		for _, chrome := range []string{"╭", "╰", "│", "sbx_one"} {
			if strings.Contains(strings.Join(lines[:39], "\n"), chrome) {
				t.Fatalf("a bare window draws no chrome, found %q:\n%s", chrome, plainFrame(m))
			}
		}

		status := bareStatus(m)
		if !strings.HasPrefix(status, " discobox  /src/disco2") {
			t.Fatalf("status = %q, want discobox and where the box was cut from on the left", status)
		}
		if !strings.HasSuffix(strings.TrimRight(status, " "), "ctrl+a . actions · ctrl+a d detach") {
			t.Fatalf("status = %q, want the actions and detach on the right", status)
		}
		if strings.Contains(status, "quit") {
			t.Fatalf("status = %q, want no quit beside a detach that does the same", status)
		}
	})
}

// The bare window is still the multiplexer: the mouse is reported, and a box
// that asked for it gets a click on the grid at the cell it is on, the grid
// being the screen.
func TestABareAttachStillReportsTheMouse(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(testSandboxes()...)
		d, m, term := openBare(t, ds)

		if m.View().MouseMode == tea.MouseModeNone {
			t.Fatal("a bare window should still report the mouse")
		}
		term.send("\x1b[?1000h\x1b[?1006hCLICKS")
		d.wait("mouse mode", func() bool { return m.primary().term.MouseMode() != 0 })
		d.dispatch(tea.MouseClickMsg{X: 4, Y: 2, Button: tea.MouseLeft})
		if got := term.typed("\x1b[<"); !strings.Contains(got, "\x1b[<0;5;3") {
			t.Fatalf("typed %q, want a click at grid cell 4,2", got)
		}
		// The status line is the window's, not the box's.
		before := len(term.typed(""))
		d.dispatch(tea.MouseClickMsg{X: 1, Y: 39, Button: tea.MouseLeft})
		if after := len(term.typed("")); after != before {
			t.Fatal("a click on the status line reached the box")
		}
	})
}

// Something for the chrome to arrange brings it back: a shell opens in the
// split, under the header and in borders, and the primary is resized into its
// box.
func TestOpeningAShellRestoresTheChrome(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(testSandboxes()...)
		d, m, term := openBare(t, ds)
		d.wait("the primary at the whole screen", func() bool { return term.size() == [2]int{120, 39} })

		d.leader("s")
		d.wait("the shell", func() bool { return m.shells.len() == 1 })
		if m.bare() {
			t.Fatal("a workspace with a shell in it is not bare")
		}
		cols, rows := m.paneCells(m.paneWidthOf(m.primary()))
		if cols == 120 || rows == 39 {
			t.Fatalf("the primary's box is %dx%d, want it inside the chrome", cols, rows)
		}
		d.wait("the primary resized into its box", func() bool { return term.size() == [2]int{cols, rows} })
		if x, y := m.paneOrigin(m.primary()); x == 0 || y == 0 {
			t.Fatalf("the grid starts at %d,%d, want it inside the border", x, y)
		}
		lines := strings.Split(plainFrame(m), "\n")
		if !strings.Contains(lines[0], "sbx_one") {
			t.Fatalf("the header should be back: %q", lines[0])
		}
		if !strings.HasPrefix(lines[1], "╭") {
			t.Fatalf("the border should be back: %q", lines[1])
		}
	})
}

// A message is said on the bare window's one line, beside where the box was
// cut from, with the keys still on the right.
func TestABareAttachSaysItsMessageOnTheStatusLine(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(testSandboxes()...)
		d, m, _ := openBare(t, ds)

		d.dispatch(statusMsg{text: "cannot archive: no space left on device", err: true})
		status := bareStatus(m)
		if !strings.Contains(status, "✗ cannot archive: no space left on device") {
			t.Fatalf("status = %q, want the message on it", status)
		}
		if !strings.HasPrefix(status, " discobox  /src/disco2") || !strings.Contains(status, "ctrl+a d detach") {
			t.Fatalf("status = %q, want the message beside the rest of the line", status)
		}
		if !m.bare() {
			t.Fatal("a message is no reason for the chrome")
		}
	})
}

// The desktop and the forwarded ports sit in the middle of the line, where
// they can be found.
func TestABareAttachCentersItsLinks(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(desktopSandboxes()...)
		d, m, _ := openBare(t, ds)
		d.wait("the forward", func() bool { return m.forward != nil })
		ds.forward.bind(Binding{Port: 6900, Local: 6901})
		d.wait("the desktop", func() bool { return strings.Contains(bareStatus(m), "Desktop") })

		status := bareStatus(m)
		cell := func(text string) int {
			i := strings.Index(status, text)
			if i < 0 {
				t.Fatalf("status = %q, want %q on it", status, text)
			}
			return ansi.StringWidth(status[:i])
		}
		start, end := cell("Desktop"), cell("tcp:5432")+len("tcp:5432")
		if start <= cell("/src/disco2")+len("/src/disco2") || end >= cell("ctrl+a . actions") {
			t.Fatalf("status = %q, want the links between the two sides", status)
		}
		if mid := (start + end) / 2; mid < 120/2-2 || mid > 120/2+2 {
			t.Fatalf("status = %q, want the links centered on the line (middle at %d)", status, mid)
		}
	})
}

func TestRunningServicesAloneKeepAttachBare(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(testSandboxes()...)
		ds.services = []Service{runningService("discobox-api", "Discobox API", "exec_svc1")}
		ds.execs = []Exec{serviceExecRecord("exec_svc1", "discobox-api", "Discobox API")}
		d, m := openAttached(t, ds, ds.sandboxes[0])
		d.wait("primary and service loaded", func() bool {
			return m.primary() != nil && m.dialog == nil && m.terminals.len() == 2
		})
		if !m.bare() {
			t.Fatal("running services alone restored the chrome; want the minimal attach screen")
		}
	})
}

func TestBareAttachActionsOfferServices(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(testSandboxes()...)
		d, m, _ := openBare(t, ds)
		d.leader(boxMenuKey)
		for _, label := range boxMenuLabels(t, m) {
			if label == "services" {
				return
			}
		}
		t.Fatalf("actions = %q, want services reachable from the minimal attach screen", boxMenuLabels(t, m))
	})
}

func TestRunWithServicesStartsBare(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(testSandboxes()...)
		ds.services = []Service{runningService("api", "API", "exec_api")}
		d, m := openRun(t, ds, RunRequest{IncludeDirty: "false"})
		d.wait("primary and service", func() bool { return m.primary() != nil && m.dialog == nil && m.terminals.len() == 2 })
		if !m.bare() {
			t.Fatalf("run with services should be bare:\n%s", plainFrame(m))
		}
	})
}

func TestServicesArrivingDuringAttachKeepItBare(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(testSandboxes()...)
		d, m, term := openBare(t, ds)
		ds.mu.Lock()
		ds.services = []Service{runningService("api", "API", "exec_api")}
		ds.serviceLogs = map[string][]byte{"api": []byte("API is listening\r\n")}
		ds.mu.Unlock()
		d.dispatch(workspaceTickMsg{gen: m.wsGen})
		d.wait("service pane", func() bool { return m.terminals.len() == 2 })
		if !m.bare() {
			t.Fatal("service arrival restored chrome")
		}
		d.wait("full screen primary", func() bool { return term.size() == [2]int{120, 39} })

		d.leader(boxMenuKey)
		d.key(paneServicesKey)
		d.wait("services picker", func() bool { return m.dialog != nil && strings.HasPrefix(m.dialog.title, "Services") })
		d.key("1")
		d.wait("service actions", func() bool { return m.dialog != nil && m.dialog.title == "API" })
		d.key("v")
		d.wait("service output", func() bool { return m.dialog == nil && strings.Contains(plainFrame(m), "API is listening") })
		if m.bare() {
			t.Fatal("viewing a service should restore chrome")
		}
		ds.execTerm("exec_api").send("request served\r\n")
		d.wait("live service output", func() bool { return strings.Contains(plainFrame(m), "request served") })
		d.leader("a")
		d.wait("bare primary again", func() bool { return m.bare() && term.size() == [2]int{120, 39} })

		d.leader(boxMenuKey)
		d.key(allBoxesKey)
		d.wait("all boxes", func() bool { return m.attach == nil })
		if m.bare() {
			t.Fatal("explicit all discoboxes should restore chrome")
		}
	})
}

func TestAdditionalInteractivePanesAndBannersRestoreAttachChrome(t *testing.T) {
	for _, extra := range []string{"terminal", "shell", "banner"} {
		t.Run(extra, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				ds := newFakeSource(testSandboxes()...)
				ds.services = []Service{runningService("api", "API", "exec_api")}
				d, m, _ := openBare(t, ds)
				d.wait("service pane", func() bool { return m.terminals.len() == 2 })
				switch extra {
				case "shell":
					d.leader("s")
					d.wait("shell", func() bool { return m.shells.len() == 1 })
				case "terminal":
					ds.mu.Lock()
					ds.execs = append(ds.execs, Exec{ID: "exec_other", Harness: "shell", Live: true, Tty: true})
					ds.mu.Unlock()
					d.dispatch(workspaceTickMsg{gen: m.wsGen})
					d.wait("second terminal", func() bool { return m.terminals.len() == 3 })
				case "banner":
					ds.mu.Lock()
					ds.requests = []CredentialRequest{waitingRequest()}
					ds.mu.Unlock()
					d.dispatch(tickMsg{})
					d.wait("credential band", func() bool { return m.bannerShowing() != bannerNone })
				}
				if m.bare() {
					t.Fatalf("%s should restore chrome with background services", extra)
				}
			})
		})
	}
}

func TestBareAttachActionsCanInspectBrokenServices(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(testSandboxes()...)
		ds.services = []Service{{ID: "broken", Name: "Broken", Status: "stopped", Problem: "script is not executable"}}
		d, m, _ := openBare(t, ds)
		d.wait("diagnostic pane", func() bool { return m.terminals.len() == 2 })
		d.leader(boxMenuKey)
		d.key(paneServicesKey)
		d.wait("services", func() bool { return m.dialog != nil && strings.HasPrefix(m.dialog.title, "Services") })
		d.key("1")
		d.wait("service actions", func() bool { return m.dialog != nil && m.dialog.title == "Broken" })
		d.key("v")
		d.wait("diagnostic output", func() bool { return m.dialog == nil && strings.Contains(plainFrame(m), "script is not executable") })
		if m.bare() {
			t.Fatal("diagnostic output should restore chrome")
		}
	})
}
