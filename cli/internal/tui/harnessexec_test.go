package tui

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// tea.Exec puts the window back on the primary screen, which still holds the
// shell prompt and everything printed on the way here. A flow that starts
// writing into the middle of that reads as two screens drawn over each other.
func TestHarnessExecClearsTheScreenBeforeTheFlowWrites(t *testing.T) {
	var out bytes.Buffer
	var sawBefore string
	exec := &harnessExec{
		title: "Configuring Claude Code",
		run: func(_ io.Reader, stdout, _ io.Writer) error {
			sawBefore = out.String()
			_, err := io.WriteString(stdout, "first line of the flow")
			return err
		},
	}
	exec.SetStdout(&out)
	exec.SetStderr(io.Discard)
	if err := exec.Run(); err != nil {
		t.Fatal(err)
	}

	// Clear, and clear the scrollback with it: rows that can be scrolled back
	// into are still on the screen as far as the user is concerned.
	for _, want := range []string{"\x1b[2J", "\x1b[3J"} {
		if !strings.Contains(sawBefore, want) {
			t.Fatalf("screen was not cleared before the flow ran: %q", sawBefore)
		}
	}
	if !strings.Contains(sawBefore, "Configuring Claude Code") {
		t.Fatalf("cleared screen does not say what took it: %q", sawBefore)
	}
	if !strings.HasSuffix(out.String(), "first line of the flow") {
		t.Fatalf("flow output = %q, want it last", out.String())
	}
}

func TestClearScreenToleratesNoOutput(*testing.T) {
	clearScreen(nil, "anything")
}
