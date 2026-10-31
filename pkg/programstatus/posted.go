package programstatus

import (
	"encoding/json"
	"time"
)

// PrimaryPosted is what the program in a sandbox's primary terminal last
// reported of itself, read from the sessions its sandbox agent posts
// (SandboxAgentStatusResponse.sessions in the OpenAPI document): the status of
// the most recently started primary session that has one, as `discobox ls`,
// the console and the server's sandbox events all read it. It reports false
// when no primary session has a status.
//
// The sessions are whatever the box's agent sent, so they are read through a
// local struct, which a field a newer agent adds does not break. A state this
// package does not know is none, and a kind it does not know is left out. The
// text is as posted: a caller that shows it makes it displayable.
func PrimaryPosted(sessions []byte) (Status, bool) {
	var posted []struct {
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
	if json.Unmarshal(sessions, &posted) != nil {
		return Status{}, false
	}
	var (
		found     Status
		startedAt time.Time
		ok        bool
	)
	for _, session := range posted {
		reported := session.ProgramStatus
		if !session.Primary || reported == nil || !KnownState(State(reported.State)) {
			continue
		}
		if ok && !session.StartedAt.After(startedAt) {
			continue
		}
		found = Status{
			State:      State(reported.State),
			Progress:   reported.Progress,
			App:        reported.App,
			Title:      reported.Title,
			Msg:        reported.Msg,
			ReportedAt: reported.ReportedAt,
		}
		if KnownKind(Kind(reported.Kind)) {
			found.Kind = Kind(reported.Kind)
		}
		startedAt, ok = session.StartedAt, true
	}
	return found, ok
}

// KnownState reports whether state is one the protocol defines.
func KnownState(state State) bool {
	switch state {
	case Idle, Working, Done, Blocked, Error:
		return true
	}
	return false
}

// KnownKind reports whether kind is one the protocol defines.
func KnownKind(kind Kind) bool {
	switch kind {
	case Permission, Question, Auth:
		return true
	}
	return false
}
