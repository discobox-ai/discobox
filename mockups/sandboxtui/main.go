// Command sandboxtui is a mockup of a disco launcher: one window that opens
// with the cursor in a prompt for a new sandbox, and the sandboxes you already
// have one press of Up away.
//
// It is wired to nothing. The sandboxes are invented, and every action shows
// the CLI command it would have run instead of running it.
package main

import (
	"flag"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/obot-platform/discobox/mockups/sandboxtui/internal/ui"
)

func main() {
	// Inline is the point: the window leaves the terminal's scrollback alone
	// and what it printed stays above it when it exits. The cost is resize —
	// see the comment on WindowSizeMsg in internal/ui/model.go — so -alt is
	// here to compare against the layout that has no such problem.
	altScreen := flag.Bool("alt", false, "run in the alternate screen instead of inline")
	project := flag.String("project", "", "project for this session, as `disco -p` sets it")
	flag.Parse()

	var options []tea.ProgramOption
	if *altScreen {
		options = append(options, tea.WithAltScreen())
	}
	p := tea.NewProgram(ui.New(ui.Project(*project)), options...)
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "sandboxtui:", err)
		os.Exit(1)
	}
}
