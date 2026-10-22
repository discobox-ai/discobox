package cli

import (
	"testing"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/pkg/programstatus"
)

func programStatusSandbox(state apiclientgen.SandboxRuntimeRuntimeState, sessions string) apimodel.Sandbox {
	sb := apimodel.Sandbox{}
	sb.Runtime.RuntimeState = apiclientgen.NewOptSandboxRuntimeRuntimeState(state)
	// A running box with a latched error displays as error; its agent is
	// still there.
	sb.Runtime.DisplayState = apiclientgen.NewOptSandboxRuntimeDisplayState(apiclientgen.SandboxRuntimeDisplayStateError)
	status := apiclientgen.SandboxRuntimeAgentStatus{"sessions": []byte(sessions)}
	sb.Runtime.AgentStatus = apiclientgen.NewOptNilSandboxRuntimeAgentStatus(status)
	return sb
}

// The column is the newest primary session's reported state, with what a
// blocked one is blocked on, and `-` for anything else.
func TestProgramStatusColumn(t *testing.T) {
	const running = apiclientgen.SandboxRuntimeRuntimeStateRunning
	for name, tc := range map[string]struct {
		state    apiclientgen.SandboxRuntimeRuntimeState
		sessions string
		want     string
	}{
		"blocked": {running, `[{"primary":true,"programStatus":{"state":"blocked","kind":"permission","reportedAt":"2026-10-16T00:00:00Z"}}]`, "blocked:permission"},
		"working": {running, `[{"primary":true,"programStatus":{"state":"working","progress":40,"futureField":1,"reportedAt":"2026-10-16T00:00:00Z"}}]`, "working"},
		"newest primary wins": {running, `[
			{"primary":true,"startedAt":"2026-10-16T01:00:00Z","programStatus":{"state":"idle","reportedAt":"2026-10-16T00:00:00Z"}},
			{"primary":true,"startedAt":"2026-10-16T00:00:00Z","programStatus":{"state":"error","reportedAt":"2026-10-16T00:00:00Z"}}]`, "idle"},
		"not the primary":  {running, `[{"primary":false,"programStatus":{"state":"working","reportedAt":"2026-10-16T00:00:00Z"}}]`, "-"},
		"nothing reported": {running, `[{"primary":true}]`, "-"},
		"unknown state":    {running, `[{"primary":true,"programStatus":{"state":"sleeping","reportedAt":"2026-10-16T00:00:00Z"}}]`, "-"},
		// A stopped box keeps the last status it posted; nothing is working in it.
		"stopped": {apiclientgen.SandboxRuntimeRuntimeStateStopped, `[{"primary":true,"programStatus":{"state":"working","reportedAt":"2026-10-16T00:00:00Z"}}]`, "-"},
	} {
		if got := programStatusColumn(programStatusSandbox(tc.state, tc.sessions)); got != tc.want {
			t.Errorf("%s: column = %q, want %q", name, got, tc.want)
		}
	}
}

// The program's text never reaches a terminal with a control sequence or a
// bidirectional override in it.
func TestSandboxProgramStatusMakesTextDisplayable(t *testing.T) {
	sb := programStatusSandbox(apiclientgen.SandboxRuntimeRuntimeStateRunning,
		`[{"primary":true,"programStatus":{"state":"blocked","msg":"Run \u202erm\u001b[2J?","reportedAt":"2026-10-16T00:00:00Z"}}]`)
	status, ok := sandboxProgramStatus(sb)
	if !ok || status.State != programstatus.Blocked || status.Msg != "Run rm[2J?" {
		t.Fatalf("status = %+v, %v", status, ok)
	}
}
