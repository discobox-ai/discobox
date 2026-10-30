package cli

import (
	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/pkg/agentstatus"
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
// Which session that is, and which states and kinds count, is
// agentstatus.PrimaryProgram's rule, the one the server's events read too
// (ADR 26-10-30-868 §3), so a listing and an event never disagree.
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
	sessions, ok := agentStatus["sessions"]
	if !ok {
		return programstatus.Status{}, false
	}
	status := make([]byte, 0, len(sessions)+len(`{"sessions":}`))
	status = append(append(append(status, `{"sessions":`...), sessions...), '}')
	program, ok := agentstatus.PrimaryProgram(status)
	if !ok {
		return programstatus.Status{}, false
	}
	return programstatus.Status{
		State:      program.State,
		Kind:       program.Kind,
		Progress:   program.Progress,
		App:        terminalSafe(program.App),
		Title:      terminalSafe(programstatus.Displayable(program.Title, programstatus.MaxTitle)),
		Msg:        terminalSafe(programstatus.Displayable(program.Msg, programstatus.MaxMsg)),
		ReportedAt: program.ReportedAt,
	}, true
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
