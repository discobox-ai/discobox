// Package agentstatus reads what more than one reader takes from a sandbox
// agent's status (SandboxAgentStatusResponse in api/openapi/sandbox.yaml) as
// the control plane stores it: one rule for each, so the server's events and
// the client's listings say the same thing about the same box.
//
// The payload is decoded through local structs, not the generated schema
// type: it is whatever the box's agent sent, and the schema refuses the
// fields a newer agent adds. Only the fields read here are the contract.
package agentstatus

import (
	"encoding/json"
	"time"

	"github.com/discobox-ai/discobox/pkg/programstatus"
)

// Program is what the program in a terminal last reported of itself over
// OSC 7501 (ADR 26-10-16-424), as the agent posted it
// (SandboxAgentProgramStatus). Its strings are the program's, unescaped.
type Program struct {
	State programstatus.State
	// Kind is what a blocked program needs, one the protocol names, or empty.
	Kind       programstatus.Kind
	Progress   *int
	App        string
	Title      string
	Msg        string
	ReportedAt time.Time
}

// PrimaryProgram is the program status of the sandbox's primary terminal:
// of the primary sessions whose program reported a state the protocol names,
// the newest. A kind the protocol does not name reads as none. It reports
// false when there is no such session or status does not decode.
//
// It does not know whether the sandbox runs. A stopped sandbox keeps the
// last status its agent posted, so a caller reads it only while it does.
func PrimaryProgram(status json.RawMessage) (Program, bool) {
	if len(status) == 0 {
		return Program{}, false
	}
	var decoded struct {
		Sessions []struct {
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
		} `json:"sessions"`
	}
	if err := json.Unmarshal(status, &decoded); err != nil {
		return Program{}, false
	}
	var (
		found     Program
		startedAt time.Time
		ok        bool
	)
	for _, session := range decoded.Sessions {
		reported := session.ProgramStatus
		if !session.Primary || reported == nil || !knownState(programstatus.State(reported.State)) {
			continue
		}
		if ok && !session.StartedAt.After(startedAt) {
			continue
		}
		found = Program{
			State:      programstatus.State(reported.State),
			Progress:   reported.Progress,
			App:        reported.App,
			Title:      reported.Title,
			Msg:        reported.Msg,
			ReportedAt: reported.ReportedAt,
		}
		switch kind := programstatus.Kind(reported.Kind); kind {
		case programstatus.Permission, programstatus.Question, programstatus.Auth:
			found.Kind = kind
		}
		startedAt, ok = session.StartedAt, true
	}
	return found, ok
}

func knownState(state programstatus.State) bool {
	switch state {
	case programstatus.Idle, programstatus.Working, programstatus.Done, programstatus.Blocked, programstatus.Error:
		return true
	}
	return false
}
