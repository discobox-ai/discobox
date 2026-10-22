package ahphost

import (
	"io"
	"time"

	"github.com/discobox-ai/discobox/pkg/programstatus"
)

// BoxState is where a discobox is in its life, as the host acts on it.
type BoxState string

const (
	BoxRunning   BoxState = "running"
	BoxStarting  BoxState = "starting"
	BoxStopped   BoxState = "stopped"
	BoxArchived  BoxState = "archived"
	BoxError     BoxState = "error"
	BoxRepairing BoxState = "repairing"
)

// Box is one discobox as the host shows it.
type Box struct {
	ID   string
	Name string
	// Server is the server the discobox is on, by the name it is listed
	// under; empty when there is only one.
	Server string
	State  BoxState
	// Agent is what the program in the primary terminal last reported over
	// OSC 7501 (ADR 26-10-16-424).
	Agent programstatus.Status
	// Harness is the slug of the harness the discobox runs, HarnessName how a
	// person reads it.
	Harness     string
	HarnessName string
	// Source is the directory the discobox was cut from; SourceRemote when
	// that is a repository URL rather than a directory.
	Source       string
	SourceRemote bool
	// DiffBase is the commit the discobox's changes are measured from, which
	// a host in the discobox offers its branch changeset against; empty when
	// there is none.
	DiffBase string
	Created  time.Time
}

// Listing is every discobox Boxes could reach. Quiet names the servers that
// did not answer: a discobox missing from the listing is gone only when its
// server is not one of them.
type Listing struct {
	Boxes []Box
	Quiet []string
}

// Exec is one exec in a running discobox: a harness terminal, a shell, a tool
// or a service.
type Exec struct {
	ID string
	// Harness is the harness slug of a harness terminal, empty otherwise.
	Harness string
	// Tool and Service name a tool session's tool and a service's id; an exec
	// with either is not a terminal.
	Tool    string
	Service string
	// Primary marks the discobox's primary harness terminal.
	Primary bool
	// Live is whether it is starting or running.
	Live bool
}

// Harness is one harness a discobox can be created on.
type Harness struct {
	Slug string
	Name string
}

// CreateRequest is a discobox to create, as `discobox new` takes it.
type CreateRequest struct {
	Harness string
	// Source is the directory the discobox is cut from; empty is the
	// directory the creating program runs in.
	Source string
	// Prompt is what the harness is given to do, empty for nothing.
	Prompt string
}

// Verb is what can be done to a discobox as a whole.
type Verb string

const (
	VerbStart     Verb = "start"
	VerbArchive   Verb = "archive"
	VerbUnarchive Verb = "unarchive"
)

// Stream is an exec's attach: its output to read, its input to write, its
// size to set, and a repaint to ask for. It ends with io.EOF when the exec
// exits cleanly and with an ExitError carrying any other code
// (execstream/client), and with any other error when the attach is lost.
type Stream interface {
	io.ReadWriteCloser
	Resize(cols, rows int) error
	Repaint() error
}
