// Package tui implements the interactive discobox terminal UI. It follows the
// Elm/Model-View-Update architecture built on Bubble Tea v2: a root model routes
// messages to the active screen, screens are pure state that produce commands,
// and all IO happens in commands rather than in Update.
//
// The package is deliberately decoupled from the API client. Screens talk to a
// [DataSource], a narrow interface the CLI implements over the generated ogen
// client, so the UI can be unit-tested against an in-memory fake and never
// imports transport or ogen types.
package tui

import (
	"context"
	"io"
	"time"
)

// Sandbox is the UI-facing view of a sandbox. It carries only what the screens
// render, keeping the ogen model out of this package.
type Sandbox struct {
	ID      string
	Name    string
	State   string
	Message string
	Updated time.Time
	Created time.Time
}

// Harness is a runnable harness option offered in the new-session form.
type Harness struct {
	// Name is the identifier passed back on create (slug, name, or ID); empty
	// means the project default.
	Name string
	// Label is the human-facing option text.
	Label string
	// Default marks the project's default harness so the form can preselect it.
	Default bool
}

// NewSessionRequest describes a sandbox to create from the new-session form. An
// empty Harness uses the project default and an empty Prompt is valid.
type NewSessionRequest struct {
	Harness string
	Path    string
	Prompt  string
}

// HarnessConfig is the UI-facing view of a harness config (a "coding agent")
// managed on the coding-agents screen.
type HarnessConfig struct {
	ID string
	// Name is the human-facing harness name.
	Name string
	// Slug is the stable, URL-safe selector used with `run -H`.
	Slug string
	// Image is the resolved harness image (custom harnesses set it directly;
	// definition-backed ones inherit it from the definition).
	Image string
	// DefinitionID names the built-in definition this config was created from;
	// empty for custom images.
	DefinitionID string
	// Default marks the project's default harness.
	Default bool
	Created time.Time
	Updated time.Time
}

// HarnessDefinition is a built-in harness definition offered as a starting point
// when creating a new coding agent.
type HarnessDefinition struct {
	ID          string
	Name        string
	Description string
	Image       string
}

// SaveHarnessRequest creates or updates a harness config. An empty ID creates a
// new config; a non-empty ID updates that config's name. A non-empty
// DefinitionID creates the config from a built-in definition; otherwise Image
// (with Name/Slug) defines a custom harness.
type SaveHarnessRequest struct {
	ID           string
	DefinitionID string
	Name         string
	Slug         string
	Image        string
}

// DataSource is the set of operations the TUI performs against the control
// plane. The CLI provides the concrete implementation; tests provide a fake.
type DataSource interface {
	// ListSandboxes returns every sandbox in the active project.
	ListSandboxes(ctx context.Context) ([]Sandbox, error)
	// DeleteSandbox deletes a single sandbox by ID.
	DeleteSandbox(ctx context.Context, id string) error
	// OpenTerminal creates and attaches an interactive harness terminal to the
	// sandbox, sized to the given cell dimensions. The returned Terminal streams
	// raw terminal bytes and is closed by the caller when the pane is dismissed.
	OpenTerminal(ctx context.Context, sandboxID string, cols, rows int) (Terminal, error)
	// AttachTerminal attaches the sandbox's primary terminal to the supplied
	// streams and blocks until it exits or detaches. The TUI invokes it through
	// tea.Exec so these are the restored real-terminal streams.
	AttachTerminal(ctx context.Context, sandboxID string, stdin io.Reader, stdout, stderr io.Writer) error
	// ListHarnesses returns the harnesses offered in the new-session form.
	ListHarnesses(ctx context.Context) ([]Harness, error)
	// PathOptions returns candidate source paths for the new-session form, drawn
	// from the sources of existing sandboxes.
	PathOptions(ctx context.Context) ([]string, error)
	// DefaultPath is the source path the new-session form starts on, typically
	// the current working directory.
	DefaultPath() string
	// CreateSession creates a sandbox from the new-session form and returns it.
	CreateSession(ctx context.Context, req NewSessionRequest) (Sandbox, error)
	// ListHarnessConfigs returns the project's harness configs (coding agents),
	// marking the project default.
	ListHarnessConfigs(ctx context.Context) ([]HarnessConfig, error)
	// ListHarnessDefinitions returns the built-in harness definitions offered as
	// starting points when creating a new coding agent.
	ListHarnessDefinitions(ctx context.Context) ([]HarnessDefinition, error)
	// SaveHarness creates or updates a harness config and returns it.
	SaveHarness(ctx context.Context, req SaveHarnessRequest) (HarnessConfig, error)
	// DeleteHarness deletes a single harness config by ID.
	DeleteHarness(ctx context.Context, id string) error
	// SetDefaultHarness makes the harness config the project default.
	SetDefaultHarness(ctx context.Context, id string) error
	// ConfigureHarness runs the agent's interactive configure flow against the
	// given terminal streams, returning when it exits. It is invoked with the
	// real terminal (via tea.Exec) so the harness can drive the screen directly.
	ConfigureHarness(ctx context.Context, id string, stdin io.Reader, stdout, stderr io.Writer) error
}

// Terminal is a live, bidirectional connection to a sandbox terminal. Read
// yields terminal output bytes, Write forwards keyboard input, and Resize
// reports a new cell size. It is the transport the embedded pane drives.
type Terminal interface {
	io.ReadWriteCloser
	Resize(cols, rows int) error
	Events() <-chan TerminalEvent
}

type TerminalConnectionState string

const (
	TerminalReconnecting TerminalConnectionState = "reconnecting"
	TerminalReconnected  TerminalConnectionState = "reconnected"
)

// TerminalEvent reports transport state without mixing connection notices into
// the terminal byte stream.
type TerminalEvent struct {
	State TerminalConnectionState
}
