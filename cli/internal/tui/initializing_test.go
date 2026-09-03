package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func initModel(title string, updates <-chan string) *Model {
	m := &Model{st: newStyles(true), width: 80}
	WithInitialization(title, updates)(m)
	return m
}

// Nothing to say, nothing shown — the line exists only while there is work.
func TestInitializationIsSilentWithNothingToReport(t *testing.T) {
	if line := initModel("Server initialization", nil).viewInitialization(); line != "" {
		t.Fatalf("viewInitialization() = %q, want nothing", line)
	}
}

// The header says what the line is about, because the user did not ask for any
// of this and needs to know it is one-time setup rather than their command.
func TestInitializationCarriesItsHeaderAndLine(t *testing.T) {
	m := initModel("Server initialization", make(chan string))
	m.applyInitialization(initializationMsg{line: "Downloading images (1 of 4): harness-shell:v1"})

	line := m.viewInitialization()
	if !strings.Contains(line, "Server initialization") {
		t.Fatalf("viewInitialization() = %q, want the header", line)
	}
	if !strings.Contains(line, "Downloading images (1 of 4)") {
		t.Fatalf("viewInitialization() = %q, want the progress", line)
	}
}

// Finishing takes the line away, which is the whole point: a window with
// nothing to report looks like a window with nothing wrong.
func TestInitializationDisappearsWhenDone(t *testing.T) {
	m := initModel("Server initialization", make(chan string))
	m.applyInitialization(initializationMsg{line: "Initializing resource pool"})
	if m.viewInitialization() == "" {
		t.Fatal("nothing shown while there was something to report")
	}

	if cmd := m.applyInitialization(initializationMsg{done: true}); cmd != nil {
		t.Fatal("asked for another update after the work finished")
	}
	if line := m.viewInitialization(); line != "" {
		t.Fatalf("viewInitialization() = %q after finishing, want nothing", line)
	}
}

// A closed channel is how "finished" arrives, so the subscription has to
// translate it rather than block or spin.
func TestInitializationEndsOnAClosedChannel(t *testing.T) {
	updates := make(chan string)
	close(updates)
	m := initModel("Server initialization", updates)

	cmd := m.awaitInitialization()
	if cmd == nil {
		t.Fatal("no subscription")
	}
	msg, ok := cmd().(initializationMsg)
	if !ok || !msg.done {
		t.Fatalf("msg = %#v, want a done initializationMsg", msg)
	}
}

// The window must not move because this text changed length, so it is one row
// whatever it says.
func TestInitializationStaysOnOneRow(t *testing.T) {
	m := initModel("Server initialization", make(chan string))
	m.width = 40
	m.applyInitialization(initializationMsg{
		line: "Downloading images (1 of 4): a-very-long-image-reference:with-a-long-tag — 1.9 GiB of 2.1 GiB, 12/41 layers",
	})
	if got := strings.Count(m.viewInitialization(), "\n"); got != 0 {
		t.Fatalf("line spans %d extra rows, want one row", got)
	}
}

// The report outlives the screen under it: the window opens out into the
// alternate screen while the images are still coming down, and a report the
// transition silences is one nobody can rely on. It rides the status row every
// screen already draws, so there is no screen it can fall off.
func TestInitializationSurvivesTheWindowOpeningOut(t *testing.T) {
	m := newTestModel(t, &fakeSource{})
	WithInitialization("Server initialization", make(chan string))(m)
	m.applyInitialization(initializationMsg{line: "Downloading images (1 of 4)"})

	view := m.View()
	if !view.AltScreen {
		t.Fatal("the opened-out window is not on the alternate screen")
	}
	lines := strings.Split(view.Content, "\n")
	// The report is on the status row, at the end of it — not on a row of its
	// own, and not displacing the keys.
	status := ansi.Strip(lines[len(lines)-2])
	if !strings.Contains(status, "Downloading images (1 of 4)") {
		t.Fatalf("status row = %q, want the report on it", status)
	}
	if rest := status[strings.Index(status, "Server initialization"):]; strings.Trim(
		strings.TrimPrefix(rest, "Server initialization: Downloading images (1 of 4)"), " │") != "" {
		t.Fatalf("status row = %q, want nothing after the report but the border", status)
	}
	// And it costs the window nothing: the frame is the terminal, exactly.
	if len(lines) != m.height {
		t.Fatalf("frame is %d rows on a %d-row terminal", len(lines), m.height)
	}
}

// The workspace draws its own status row rather than going through viewStatus,
// so it has to pin the report itself — and it is the screen where an
// unexplained wait is least explicable, being otherwise all terminal.
func TestInitializationShowsInTheWorkspace(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	_, m, _ := openWorkspace(t, ds, "enter")
	WithInitialization("Server initialization", make(chan string))(m)
	m.applyInitialization(initializationMsg{line: "Downloading images (1 of 4)"})

	lines := strings.Split(m.View().Content, "\n")
	status := ansi.Strip(lines[len(lines)-1])
	if !strings.Contains(status, "Downloading images (1 of 4)") {
		t.Fatalf("workspace status row = %q, want the report on it", status)
	}
	if !strings.Contains(status, "detach") {
		t.Fatalf("workspace status row = %q, want the keys still on it", status)
	}
	if len(lines) != m.height {
		t.Fatalf("frame is %d rows on a %d-row terminal", len(lines), m.height)
	}
}

// The keys give way to the report, never the report to the keys: a row too
// narrow for both cuts the left back. The report is the one field on the row
// that nothing else on screen accounts for, and F1 spells the keys out anyway.
func TestInitializationIsNotDroppedOnANarrowRow(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	_, m, _ := openWorkspace(t, ds, "enter")
	WithInitialization("Server initialization", make(chan string))(m)
	m.applyInitialization(initializationMsg{line: "Downloading images (1 of 4)"})

	for _, width := range []int{120, 100, 80, 60} {
		send(t, m, tea.WindowSizeMsg{Width: width, Height: 40})
		lines := strings.Split(m.View().Content, "\n")
		status := ansi.Strip(lines[len(lines)-1])
		if !strings.Contains(status, "Downloading images (1 of 4)") {
			t.Fatalf("at %d cells the status row is %q, want the report still on it", width, status)
		}
	}
}

// Finishing takes the report away, and takes nothing else with it: the row it
// rode on belongs to the window either way.
func TestInitializationDisappearsFromTheStatusRow(t *testing.T) {
	m := newTestModel(t, &fakeSource{})
	WithInitialization("Server initialization", make(chan string))(m)
	m.applyInitialization(initializationMsg{line: "Downloading images (1 of 4)"})
	if !strings.Contains(ansi.Strip(m.View().Content), "Downloading images") {
		t.Fatal("nothing shown while there was something to report")
	}

	m.applyInitialization(initializationMsg{done: true})
	if strings.Contains(ansi.Strip(m.View().Content), "Downloading images") {
		t.Fatal("the report outlived the work")
	}
	if len(strings.Split(m.View().Content, "\n")) != m.height {
		t.Fatal("the frame did not fill the terminal")
	}
}

// It is text on screen, so it is text you can select. It is inside the frame
// like everything else, which is what makes that true without anything having
// to arrange it.
func TestInitializationReportIsSelectable(t *testing.T) {
	m := newTestModel(t, &fakeSource{})
	slowClock(m)
	var copied []string
	m.copyOS = func(text string) error {
		copied = append(copied, text)
		return nil
	}
	WithInitialization("Server initialization", make(chan string))(m)
	m.applyInitialization(initializationMsg{line: "Downloading images (1 of 4)"})

	lines := strings.Split(m.View().Content, "\n")
	y := len(lines) - 2
	row := ansi.Strip(lines[y])
	at := strings.Index(row, "Downloading")
	if at < 0 {
		t.Fatalf("status row = %q, want the report on it", row)
	}
	// Cells, not bytes: the keys on this row carry · and ↑, so a byte offset
	// into the text is several columns past where it is drawn.
	x := lipgloss.Width(row[:at])

	send(t, m,
		tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft},
		tea.MouseMotionMsg{X: x + 10, Y: y, Button: tea.MouseLeft},
		tea.MouseReleaseMsg{X: x + 10, Y: y, Button: tea.MouseLeft},
	)

	if len(copied) != 1 || copied[0] != "Downloading" {
		t.Fatalf("copied %q, want the cells dragged over on the report", copied)
	}
}
