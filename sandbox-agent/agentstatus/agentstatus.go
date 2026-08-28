// Package agentstatus computes the sandbox-agent-reported status a pool
// agent polls periodically: per-source git status, per-terminal session
// records, and active attach connection counts (see ADR 0030). It is
// computed fresh on every call, never cached or pushed on its own initiative
// — sandbox-agent only ever answers inbound authenticated requests.
//
// Listening ports are the one exception to "computed fresh". They are
// discovered and classified by a standing watcher ([ports.Watcher]) and read
// here as a snapshot, because classifying a port means connecting to a user's
// process and the answer does not change while its socket lives (ADR 0046).
package agentstatus

import (
	"time"

	"github.com/discobox-ai/discobox/sandbox-agent/ports"
	"github.com/discobox-ai/discobox/sandbox-agent/resources"
)

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

	// DiffBase is the commit the diff counts below are measured against: the
	// commit the source was spawned at per the manifest, forwarded to the
	// merge base with the manifest's upstream tracking ref once the sandbox
	// has fetched (see resolveDiffBase). Empty when the manifest recorded no
	// base commit or the repository does not have it, in which case the
	// counts mean nothing and are omitted with it.
	DiffBase    string `json:"diffBase,omitempty"`
	DiffFiles   int    `json:"diffFiles"`
	DiffAdded   int    `json:"diffAdded"`
	DiffDeleted int    `json:"diffDeleted"`
}

// SessionStatus is the observed record of one harness terminal.
type SessionStatus struct {
	TerminalID string `json:"terminalId"`
	HarnessID  string `json:"harnessId,omitempty"`
	Primary    bool   `json:"primary"`
	// Title is the window title the program in the session last set (OSC
	// 0/2), read from its shim's screen emulator. It is the session's own
	// name for itself — a harness usually titles it after what it is doing —
	// and is deliberately not backfilled from anything else: the sandbox's
	// name and its prompt are already on the record, and a session that
	// never titled itself reports no title at all.
	Title string `json:"title,omitempty"`
	// LastAccessedAt is the last time a client acted on this session —
	// attached, typed, or is attached right now — as the shim reports it.
	// Absent when no client ever has.
	LastAccessedAt *time.Time `json:"lastAccessedAt,omitempty"`
	StartedAt      *time.Time `json:"startedAt,omitempty"`
	AttacherCount  int        `json:"attacherCount"`
	// ExecStatus is the underlying exec process's status, verbatim. It is the
	// only liveness this payload carries: what the harness inside the session
	// is doing is not reported (see ComputeSessionStatus).
	ExecStatus string `json:"execStatus"`
}

// Response is the full status payload sandbox-agent's status endpoint
// returns, and what pool-agent relays (opaquely) to discobox-server.
type Response struct {
	Sources  []GitSourceStatus `json:"sources"`
	Sessions []SessionStatus   `json:"sessions"`
	// Ports are the TCP ports the sandbox user's own processes are listening
	// on, each classified by what it speaks, so a client can offer a forward
	// onto one without being told its number.
	Ports []ports.Port `json:"ports"`
	// Resources is this sandbox's own CPU and memory consumption, as
	// cumulative counters rather than rates (ADR 0071). Turning them into
	// "how busy" is the pool agent's job: it polls every sandbox in the pool
	// on one tick, so it can difference them all over the same window and the
	// resulting ranking compares like with like.
	Resources  *resources.Usage `json:"resources,omitempty"`
	ObservedAt time.Time        `json:"observedAt"`
}
