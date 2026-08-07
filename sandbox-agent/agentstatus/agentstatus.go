// Package agentstatus computes the sandbox-agent-reported status a pool
// agent polls periodically: per-source git status, per-terminal harness
// session state, and active attach connection counts (see ADR 0030). It is
// computed fresh on every call, never cached or pushed on its own initiative
// — sandbox-agent only ever answers inbound authenticated requests.
package agentstatus

import "time"

// GitSourceStatus is the observed git state of one mounted source.
type GitSourceStatus struct {
	Slug       string    `json:"slug"`
	Target     string    `json:"target"`
	Clean      bool      `json:"clean"`
	Branch     string    `json:"branch,omitempty"`
	HeadCommit string    `json:"headCommit,omitempty"`
	Ahead      int       `json:"ahead,omitempty"`
	Behind     int       `json:"behind,omitempty"`
	Porcelain  string    `json:"porcelain,omitempty"`
	Truncated  bool      `json:"truncated,omitempty"`
	Error      string    `json:"error,omitempty"`
	ObservedAt time.Time `json:"observedAt"`
}

// SessionStatus is the observed state of one harness terminal.
type SessionStatus struct {
	TerminalID    string     `json:"terminalId"`
	HarnessID     string     `json:"harnessId,omitempty"`
	Primary       bool       `json:"primary"`
	Name          string     `json:"name,omitempty"`
	State         string     `json:"state"`
	LastEvent     string     `json:"lastEvent,omitempty"`
	LastEventAt   *time.Time `json:"lastEventAt,omitempty"`
	StartedAt     *time.Time `json:"startedAt,omitempty"`
	AttacherCount int        `json:"attacherCount"`
	ExecStatus    string     `json:"execStatus"`
}

// Response is the full status payload sandbox-agent's status endpoint
// returns, and what pool-agent relays (opaquely) to discobox-server.
type Response struct {
	Sources    []GitSourceStatus `json:"sources"`
	Sessions   []SessionStatus   `json:"sessions"`
	ObservedAt time.Time         `json:"observedAt"`
}
