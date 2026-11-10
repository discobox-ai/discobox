package console

import (
	"slices"

	"github.com/discobox-ai/discobox/assets/brand"
	"github.com/discobox-ai/discobox/pkg/programstatus"
)

// The console's spec (ADR 26-11-07-876 §4): what every frontend draws the
// same way, as data — the commands with each frontend's bindings, the state
// and agent glyphs with their color tokens, the color tokens in both
// palettes and the space, radius and stroke tokens (26-11-07-363 §3–4), read
// from the design language's tokens. The
// desktop app is handed it when it connects; the TUI reads the same glyphs
// and draws each token by its xterm index. Nothing in it is read back out of
// a renderer.

// SpecVersion is the spec's shape. It changes when a field is removed or
// changes meaning, and when a field is added that a frontend requires: every
// field on the wire is required (the generated types have no defaults), so an
// app reading a newer spec than its CLI's would otherwise stop on a missing
// field rather than on this version. 4 added Dimensions; 5 the desktop's
// Menus, and a desktop binding's Menu; 6 LinkSchemes.
const SpecVersion = 6

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
	// Dimensions is every space, radius and stroke token by name, in points.
	Dimensions map[string]float64 `json:"dimensions"`
	// States and Agents are the glyphs, by state.
	States   map[string]Glyph `json:"states"`
	Agents   map[string]Glyph `json:"agents"`
	Commands []SpecCommand    `json:"commands"`
	// Gestures are the bindings that are not commands (Gesture): every key a
	// frontend's list, tree, leader and panes bind is a command's or one of
	// these.
	Gestures []SpecGesture `json:"gestures"`
	// Menus are the desktop's menus, in the order its menu bar draws them.
	Menus []Menu `json:"menus"`
	// LinkSchemes are the schemes of the links in a pane the console answers
	// itself (BoxLinkSchemes): a press on one, plain or with the link
	// modifier, is the console's and never the program's or the OS's.
	LinkSchemes []string `json:"linkSchemes"`
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
	ID        string       `json:"id"`
	Label     string       `json:"label"`
	Detail    string       `json:"detail"`
	Aliases   []string     `json:"aliases"`
	Target    SpecTarget   `json:"target"`
	Lifecycle bool         `json:"lifecycle"`
	Bindings  SpecBindings `json:"bindings"`
}

// SpecTarget is what a command takes, as a frontend is handed it.
type SpecTarget string

// The targets.
const (
	// SpecTargetBox is a command that takes a box.
	SpecTargetBox SpecTarget = "box"
	// SpecTargetWindow is one that takes none: it acts on the window.
	SpecTargetWindow SpecTarget = "window"
)

// SpecBindings is a command's bindings, keyed by frontend (598 §2).
type SpecBindings struct {
	TUI     SpecTUIBinding     `json:"tui"`
	Desktop SpecDesktopBinding `json:"desktop"`
}

// SpecTUIBinding is TUIBinding on the wire.
type SpecTUIBinding struct {
	Keys    []string   `json:"keys"`
	Leader  [][]string `json:"leader"`
	Repeats bool       `json:"repeats"`
	Chords  []string   `json:"chords"`
	Menu    []string   `json:"menu"`
}

// SpecDesktopBinding is DesktopBinding on the wire. Menu is omitted for a
// gesture.
type SpecDesktopBinding struct {
	Keys []string `json:"keys"`
	Menu Menu     `json:"menu,omitempty"`
}

// SpecGesture is one gesture as a frontend is handed it.
type SpecGesture struct {
	ID       string       `json:"id"`
	Label    string       `json:"label"`
	Bindings SpecBindings `json:"bindings"`
}

// NewSpec is the console's spec.
func NewSpec() Spec {
	spec := Spec{
		Version:     SpecVersion,
		Colors:      map[string]SpecColor{},
		Dimensions:  map[string]float64{},
		States:      map[string]Glyph{},
		Agents:      map[string]Glyph{},
		Commands:    make([]SpecCommand, 0, len(registry)),
		Gestures:    make([]SpecGesture, 0, len(gestures)),
		Menus:       slices.Clone(Menus),
		LinkSchemes: slices.Clone(BoxLinkSchemes),
	}
	for _, c := range brand.Colors() {
		spec.Colors[c.Name] = SpecColor{Dark: c.Dark, Light: c.Light, XTerm: c.XTerm}
	}
	for _, d := range brand.Dimensions() {
		spec.Dimensions[d.Name] = d.Points
	}
	for _, s := range States {
		spec.States[string(s)] = StateGlyph(s)
	}
	for _, s := range AgentStates {
		spec.Agents[string(s)] = AgentGlyph(s)
	}
	for _, c := range registry {
		target := SpecTargetWindow
		if c.Target == TargetBox {
			target = SpecTargetBox
		}
		spec.Commands = append(spec.Commands, SpecCommand{
			ID: c.ID, Label: c.Label, Detail: c.Detail, Aliases: nonNil(c.Aliases),
			Target: target, Lifecycle: c.Lifecycle,
			Bindings: specBindings(c.Bindings),
		})
	}
	for _, g := range gestures {
		spec.Gestures = append(spec.Gestures, SpecGesture{ID: g.ID, Label: g.Label, Bindings: specBindings(g.Bindings)})
	}
	return spec
}

func specBindings(b Bindings) SpecBindings {
	leader := make([][]string, 0, len(b.TUI.Leader))
	for _, seq := range b.TUI.Leader {
		leader = append(leader, nonNil(seq))
	}
	return SpecBindings{
		TUI: SpecTUIBinding{Keys: nonNil(b.TUI.Keys), Leader: leader, Repeats: b.TUI.Repeats,
			Chords: nonNil(b.TUI.Chords), Menu: nonNil(b.TUI.Menu)},
		Desktop: SpecDesktopBinding{Keys: nonNil(b.Desktop.Keys), Menu: b.Desktop.Menu},
	}
}

// nonNil is a copy that is never nil, so the wire carries [] rather than null.
func nonNil(s []string) []string { return append([]string{}, s...) }
