package agentstatus

import (
	"encoding/json"
	"testing"

	"github.com/discobox-ai/discobox/pkg/programstatus"
)

// The primary program is the newest primary session whose program reported a
// state the protocol names; other sessions, unknown states and unknown kinds
// do not count, and a payload that does not decode is none.
func TestPrimaryProgram(t *testing.T) {
	status := json.RawMessage(`{"sessions":[
		{"primary":true,"startedAt":"2026-10-30T10:00:00Z","programStatus":{"state":"blocked","kind":"question","msg":"which?","reportedAt":"2026-10-30T10:00:00Z"}},
		{"primary":false,"startedAt":"2026-10-30T11:00:00Z","programStatus":{"state":"working","reportedAt":"2026-10-30T11:00:00Z"}},
		{"primary":true,"startedAt":"2026-10-30T12:00:00Z","programStatus":{"state":"pondering","reportedAt":"2026-10-30T12:00:00Z"}},
		{"primary":true,"startedAt":"2026-10-30T13:00:00Z"}
	],"futureField":1}`)
	program, ok := PrimaryProgram(status)
	if !ok || program.State != programstatus.Blocked || program.Kind != programstatus.Question || program.Msg != "which?" {
		t.Fatalf("program = %+v, %v", program, ok)
	}

	program, ok = PrimaryProgram(json.RawMessage(`{"sessions":[{"primary":true,"startedAt":"2026-10-30T10:00:00Z","programStatus":{"state":"blocked","kind":"bribe"}}]}`))
	if !ok || program.Kind != "" {
		t.Fatalf("unknown kind = %+v, %v; want it dropped", program, ok)
	}
	for _, none := range []string{``, `{}`, `{"sessions":"no"}`, `not json`} {
		if program, ok := PrimaryProgram(json.RawMessage(none)); ok {
			t.Fatalf("%q = %+v, want none", none, program)
		}
	}
}
