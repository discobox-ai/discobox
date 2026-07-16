package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"
)

// Messages are the events that drive Update. IO-producing commands return one of
// these; Update never performs IO itself.

// sandboxesLoadedMsg carries the result of a successful list refresh.
type sandboxesLoadedMsg struct {
	sandboxes []Sandbox
}

// errMsg reports a failed command. The context string names the operation for
// the status line (e.g. "list", "delete").
type errMsg struct {
	context string
	err     error
}

// deletedMsg reports the outcome of a delete of one or more sandboxes.
type deletedMsg struct {
	ids  []string
	errs []error
}

// tickMsg drives periodic auto-refresh of the sandbox list.
type tickMsg struct {
	at time.Time
}

// selectSandboxMsg asks the root model to open the terminal pane for a sandbox.
type selectSandboxMsg struct {
	sandbox Sandbox
}

// fullscreenSandboxMsg asks the root model to suspend the TUI and attach the
// sandbox's primary terminal directly to the real terminal.
type fullscreenSandboxMsg struct {
	sandbox Sandbox
}

// fullscreenFinishedMsg reports that a fullscreen terminal attach returned and
// the TUI resumed.
type fullscreenFinishedMsg struct {
	sandbox Sandbox
	err     error
}

// backMsg asks the root model to return to the sandbox list.
type backMsg struct{}

// focusTabsMsg asks the root model to move focus up onto the tab bar. A
// top-level tab screen emits it when the user presses Up while already at the
// top of its content.
type focusTabsMsg struct{}

// focusTabsCmd is the command form of focusTabsMsg, returned by a screen's key
// handler when Up should surface focus to the tab bar.
func focusTabsCmd() tea.Cmd {
	return func() tea.Msg { return focusTabsMsg{} }
}

// openNewMsg asks the root model to open the new-session form.
type openNewMsg struct{}

// newFormDataMsg delivers the form's async-loaded harness and path options.
type newFormDataMsg struct {
	harnesses []Harness
	paths     []string
	err       error
}

// sessionCreatedMsg reports a sandbox created from the new-session form.
type sessionCreatedMsg struct {
	sandbox Sandbox
}

// ttyOpenedMsg reports that a terminal connection is established and hands the
// pane the stream to drive.
type ttyOpenedMsg struct {
	sandboxID string
	terminal  Terminal
	reader    *ttyReader
}

// ttyOutputMsg carries a chunk of terminal output read from the connection.
type ttyOutputMsg struct {
	data []byte
}

// ttyClosedMsg reports that the terminal connection ended, optionally with an
// error.
type ttyClosedMsg struct {
	err error
}

type ttyConnectionMsg struct {
	event TerminalEvent
}

// statusMsg sets the shared status line. It lets any screen report a transient
// success or error without a message type of its own.
type statusMsg struct {
	text string
	err  bool
}

// openHarnessesMsg asks the root to switch to the coding-agents screen.
type openHarnessesMsg struct{}

// openHarnessFormMsg asks the root to open the harness create/edit form. A nil
// edit target opens the create form; otherwise the form edits that config.
type openHarnessFormMsg struct {
	edit *HarnessConfig
}

// harnessFormBackMsg asks the root to return from the form to the coding-agents
// screen.
type harnessFormBackMsg struct{}

// harnessesLoadedMsg carries a refreshed list of harness configs.
type harnessesLoadedMsg struct {
	configs []HarnessConfig
}

// harnessFormDataMsg delivers the form's async-loaded harness definitions.
type harnessFormDataMsg struct {
	definitions []HarnessDefinition
	err         error
}

// harnessSavedMsg reports a harness config created or updated from the form.
type harnessSavedMsg struct {
	config  HarnessConfig
	created bool
}

// harnessDeletedMsg reports the outcome of a harness config delete.
type harnessDeletedMsg struct {
	ids  []string
	errs []error
}

// runConfigureMsg asks the root to run an agent's interactive configure flow,
// suspending the TUI so the harness can take over the terminal.
type runConfigureMsg struct {
	harness HarnessConfig
}

// harnessConfiguredMsg reports that a configure run finished (the TUI has
// resumed), carrying the agent name and any error/non-zero exit.
type harnessConfiguredMsg struct {
	name string
	err  error
}
