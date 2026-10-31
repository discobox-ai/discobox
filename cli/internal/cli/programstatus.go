package cli

import (
	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/pkg/programstatus"
)

// sandboxProgramStatus is what the program in the discobox's primary terminal
// last reported of itself over OSC 7501, as the discobox's agent last posted
// it (ADR 26-10-16-424 §§3, 5). It reports false while the discobox is not
// running, since a stopped discobox's row keeps the last status it posted and
// nothing is working, blocked or idle in a box that is off, and when the
// program reported nothing.
//
// Running is the runtime state, not the display state: a running box with a
// latched error displays as `error`, and its agent is still there to be
// blocked.
//
// Which session's status it is, and how the posted sessions are read, is
// programstatus.PrimaryPosted's, which the server's sandbox events share: the
// payload is whatever the box's agent sent, and the generated schema type
// refuses the fields a newer agent adds. A state or kind this client does not
// know reads as none.
//
// Its text is the program's, made displayable here: control characters,
// bidirectional overrides and invisible formatting never reach a terminal.
func sandboxProgramStatus(sb apimodel.Sandbox) (programstatus.Status, bool) {
	if state, ok := sb.Runtime.RuntimeState.Get(); !ok || state != apiclientgen.SandboxRuntimeRuntimeStateRunning {
		return programstatus.Status{}, false
	}
	agentStatus, ok := sb.Runtime.AgentStatus.Get()
	if !ok {
		return programstatus.Status{}, false
	}
	raw, ok := agentStatus["sessions"]
	if !ok {
		return programstatus.Status{}, false
	}
	status, ok := programstatus.PrimaryPosted(raw)
	if !ok {
		return programstatus.Status{}, false
	}
	status.App = terminalSafe(status.App)
	status.Title = terminalSafe(programstatus.Displayable(status.Title, programstatus.MaxTitle))
	status.Msg = terminalSafe(programstatus.Displayable(status.Msg, programstatus.MaxMsg))
	return status, true
}

// programStatusColumn is the AGENT column of `discobox ls`: the state, a
// blocked one with what it is blocked on, and `-` with none.
func programStatusColumn(sb apimodel.Sandbox) string {
	status, ok := sandboxProgramStatus(sb)
	if !ok {
		return "-"
	}
	if status.State == programstatus.Blocked && status.Kind != "" {
		return string(status.State) + ":" + string(status.Kind)
	}
	return string(status.State)
}
