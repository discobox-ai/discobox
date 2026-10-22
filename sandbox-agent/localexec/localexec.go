// Package localexec is the sandbox agent's exec API for the sandbox's own
// user, on a local Unix socket (ADR 26-10-22-853 §2): what the in-sandbox AHP
// host (`discobox-sandbox-agent ahp`) needs of the discobox's terminals —
// list them, create a shell or a harness terminal, start, attach, type into
// and end one — and nothing else.
//
// The agent's own routes are for the control plane and the pool, under signed
// tokens, and the shims' sockets are root's. This socket is reachable by any
// process in the sandbox, so it only ever serves execs that run as the
// sandbox's default user and only ever creates them as that user: it gives
// the sandbox user nothing it could not already do as itself.
package localexec

import (
	"path/filepath"

	"github.com/discobox-ai/discobox/pkg/programstatus"
)

// DefaultSocketPath is where the socket is with the agent's default runtime
// directory, which is where `ahp` looks for it.
const DefaultSocketPath = "/run/discobox/ahp/execs.sock"

// SocketPath is the socket beside the agent's runtime directory, as the hook
// collector's is: a directory of its own that every user may enter (0711),
// and the socket in it every user may connect to (0666).
func SocketPath(runtimeDir string) string {
	return filepath.Join(filepath.Dir(runtimeDir), "ahp", "execs.sock")
}

// Box is the discobox as the socket describes it.
type Box struct {
	SandboxID string `json:"sandboxId"`
	// Harness is the harness the discobox runs, by its id, and HarnessName
	// how a person reads it.
	Harness     string `json:"harness,omitempty"`
	HarnessName string `json:"harnessName,omitempty"`
	// Workdir is the primary source's working tree, where an exec that names
	// none starts.
	Workdir string `json:"workdir,omitempty"`
	// DiffBase is the commit the working tree's changes are measured from —
	// what the discobox was cut at, moved forward past what it has since
	// pulled (agentstatus.ResolveDiffBase) — empty when there is none.
	DiffBase string `json:"diffBase,omitempty"`
	// Agent is what the primary terminal's program last reported over OSC
	// 7501, absent before it has.
	Agent *programstatus.Status `json:"agent,omitempty"`
}

// Exec is one exec the socket serves.
type Exec struct {
	ID      string `json:"id"`
	Harness string `json:"harness,omitempty"`
	Primary bool   `json:"primary,omitempty"`
	Live    bool   `json:"live"`
	Service string `json:"service,omitempty"`
	TTY     bool   `json:"tty"`
}

// CreateRequest creates a shell, or another session of the discobox's harness.
type CreateRequest struct {
	Shell bool `json:"shell"`
	Cols  int  `json:"cols,omitempty"`
	Rows  int  `json:"rows,omitempty"`
}

// InputRequest types text into an exec and presses Enter.
type InputRequest struct {
	Text string `json:"text"`
}
