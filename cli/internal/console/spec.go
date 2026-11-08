package console

import (
	"github.com/discobox-ai/discobox/assets/brand"
	"github.com/discobox-ai/discobox/pkg/programstatus"
)

// The console's spec (ADR 26-11-07-876 §4): what every frontend draws the
// same way, as data — the commands with each frontend's bindings, the state
// and agent glyphs with their color tokens, and the color tokens in both
// palettes (26-11-07-363 §3–4), read from the design language's tokens. The
// desktop app is handed it when it connects; the TUI reads the same glyphs
// and draws each token by its xterm index. Nothing in it is read back out of
// a renderer.

// SpecVersion is the spec's shape. It changes when a field is removed or
// changes meaning, never for a field added.
const SpecVersion = 2

// Glyph is how a state is drawn: its shape, the color token it is drawn in,
// and the word that stands for it where there is no color. An empty Color is
// the surrounding text's.
type Glyph struct {
	Glyph string `json:"glyph"`
	Color string `json:"color"`
	Word  string `json:"word"`
}

// States is every state a box is listed in, in the order the spec lists them.
var States = []State{StateRunning, StateStarting, StateRepairing, StateStopped, StateSuspended, StateArchived, StateError}

// AgentStates is every state a box's agent reports (ADR 26-10-16-424 §5).
var AgentStates = []programstatus.State{programstatus.Working, programstatus.Idle, programstatus.Blocked, programstatus.Done, programstatus.Error}

// StateGlyph is a box state's mark. A state the console does not know is drawn
// as stopped.
func StateGlyph(s State) Glyph {
	word := string(s)
	switch s {
	case StateRunning:
		return Glyph{Glyph: "●", Color: "ok", Word: word}
	case StateStarting, StateRepairing:
		return Glyph{Glyph: "◐", Color: "gold", Word: word}
	case StateSuspended:
		return Glyph{Glyph: "‖", Color: "gold", Word: word}
	case StateError:
		return Glyph{Glyph: "✗", Color: "err", Word: word}
	case StateArchived:
		return Glyph{Glyph: "▪", Color: "text-faint", Word: word}
	default:
		return Glyph{Glyph: "○", Color: "text-faint", Word: word}
	}
}

// AgentGlyph is what a box's agent reports as a mark: each its own shape as
// well as its own color, so working and blocked are told apart where the
// colors are close. No report is a blank cell; a state this console does
// not know is a blank cell too, worded as reported.
func AgentGlyph(s programstatus.State) Glyph {
	switch s {
	case programstatus.Working:
		return Glyph{Glyph: "▶", Color: "text", Word: string(s)}
	case programstatus.Idle:
		return Glyph{Glyph: "◇", Color: "text-faint", Word: string(s)}
	case programstatus.Blocked:
		return Glyph{Glyph: "◆", Color: "err", Word: string(s)}
	case programstatus.Done:
		return Glyph{Glyph: "✓", Color: "green", Word: string(s)}
	case programstatus.Error:
		return Glyph{Glyph: "×", Color: "err", Word: "failed"}
	default:
		return Glyph{Glyph: " ", Word: string(s)}
	}
}

// Spec is the console's spec as a frontend is handed it.
type Spec struct {
	Version int `json:"version"`
	// Colors is every color token by name, in both palettes.
	Colors map[string]SpecColor `json:"colors"`
	// States and Agents are the glyphs, by state.
	States   map[string]Glyph `json:"states"`
	Agents   map[string]Glyph `json:"agents"`
	Commands []SpecCommand    `json:"commands"`
}

// SpecColor is one color token: its dark and light values as #rrggbb, and
// the xterm-256 index the TUI draws it with (0 where it uses none).
type SpecColor struct {
	Dark  string `json:"dark"`
	Light string `json:"light"`
	XTerm int    `json:"xterm,omitempty"`
}

// SpecCommand is one registry command as a frontend is handed it. Every list
// is present, empty where there is nothing in it.
type SpecCommand struct {
	ID      string   `json:"id"`
	Label   string   `json:"label"`
	Detail  string   `json:"detail"`
	Aliases []string `json:"aliases"`
	// Target is "box" for a command that takes a box, "window" for one that
	// takes none.
	Target    string       `json:"target"`
	Lifecycle bool         `json:"lifecycle"`
	Bindings  SpecBindings `json:"bindings"`
}

// SpecBindings is a command's bindings, keyed by frontend (598 §2).
type SpecBindings struct {
	TUI     SpecTUIBinding     `json:"tui"`
	Desktop SpecDesktopBinding `json:"desktop"`
}

// SpecTUIBinding is TUIBinding on the wire.
type SpecTUIBinding struct {
	Keys   []string `json:"keys"`
	Leader []string `json:"leader"`
	Menu   []string `json:"menu"`
}

// SpecDesktopBinding is DesktopBinding on the wire.
type SpecDesktopBinding struct {
	Keys []string `json:"keys"`
}

// NewSpec is the console's spec.
func NewSpec() Spec {
	spec := Spec{
		Version:  SpecVersion,
		Colors:   map[string]SpecColor{},
		States:   map[string]Glyph{},
		Agents:   map[string]Glyph{},
		Commands: make([]SpecCommand, 0, len(registry)),
	}
	for _, c := range brand.Colors() {
		spec.Colors[c.Name] = SpecColor{Dark: c.Dark, Light: c.Light, XTerm: c.XTerm}
	}
	for _, s := range States {
		spec.States[string(s)] = StateGlyph(s)
	}
	for _, s := range AgentStates {
		spec.Agents[string(s)] = AgentGlyph(s)
	}
	for _, c := range registry {
		target := "window"
		if c.Target == TargetBox {
			target = "box"
		}
		spec.Commands = append(spec.Commands, SpecCommand{
			ID: c.ID, Label: c.Label, Detail: c.Detail, Aliases: nonNil(c.Aliases),
			Target: target, Lifecycle: c.Lifecycle,
			Bindings: SpecBindings{
				TUI:     SpecTUIBinding{Keys: nonNil(c.Bindings.TUI.Keys), Leader: nonNil(c.Bindings.TUI.Leader), Menu: nonNil(c.Bindings.TUI.Menu)},
				Desktop: SpecDesktopBinding{Keys: nonNil(c.Bindings.Desktop.Keys)},
			},
		})
	}
	return spec
}

// nonNil is a copy that is never nil, so the wire carries [] rather than null.
func nonNil(s []string) []string { return append([]string{}, s...) }
