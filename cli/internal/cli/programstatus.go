package cli

import (
	"encoding/json"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/programstatus"
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
// The sessions are decoded through a local struct, the server's display
// name's way (services.SandboxDisplayName): the payload is whatever the box's
// agent sent, and the generated schema type refuses the fields a newer agent
// adds. The fields below are the contract, SandboxAgentProgramStatus in
// server.yaml. A state or kind this client does not know reads as none.
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
	var sessions []struct {
		Primary       bool      `json:"primary"`
		StartedAt     time.Time `json:"startedAt"`
		ProgramStatus *struct {
			State      string    `json:"state"`
			Kind       string    `json:"kind"`
			Progress   *int      `json:"progress"`
			App        string    `json:"app"`
			Title      string    `json:"title"`
			Msg        string    `json:"msg"`
			ReportedAt time.Time `json:"reportedAt"`
		} `json:"programStatus"`
	}
	if err := json.Unmarshal(raw, &sessions); err != nil {
		return programstatus.Status{}, false
	}
	var (
		found     programstatus.Status
		startedAt time.Time
		foundOK   bool
	)
	for _, session := range sessions {
		reported := session.ProgramStatus
		if !session.Primary || reported == nil || !knownState(programstatus.State(reported.State)) {
			continue
		}
		if foundOK && !session.StartedAt.After(startedAt) {
			continue
		}
		found = programstatus.Status{
			State:      programstatus.State(reported.State),
			Progress:   reported.Progress,
			App:        terminalSafe(reported.App),
			Title:      terminalSafe(programstatus.Displayable(reported.Title, programstatus.MaxTitle)),
			Msg:        terminalSafe(programstatus.Displayable(reported.Msg, programstatus.MaxMsg)),
			ReportedAt: reported.ReportedAt,
		}
		switch kind := programstatus.Kind(reported.Kind); kind {
		case programstatus.Permission, programstatus.Question, programstatus.Auth:
			found.Kind = kind
		}
		startedAt, foundOK = session.StartedAt, true
	}
	return found, foundOK
}

func knownState(state programstatus.State) bool {
	switch state {
	case programstatus.Idle, programstatus.Working, programstatus.Done, programstatus.Blocked, programstatus.Error:
		return true
	}
	return false
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
