package tui

import (
	"strings"
	"testing"
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
