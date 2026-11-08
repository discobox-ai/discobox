package tui

import (
	"fmt"
	"strconv"

	"github.com/discobox-ai/discobox/cli/internal/console"
	"github.com/discobox-ai/discobox/pkg/programstatus"
)

// This file is the console's UX as data, for the desktop app (`discobox
// desktop`), which draws the same console natively and must agree with this
// one about what things are called, what they look like and which commands
// exist. It is read from the window's own constants and glyph functions, so
// the two cannot drift while this window is the source.
//
// Prototype: it is an exporter beside the window, not yet a spec the window
// reads. Which way round it should be is what the prototype is for.

// UXSpec is what the desktop app is handed when it connects.
type UXSpec struct {
	Version  int               `json:"version"`
	Leader   string            `json:"leader"`
	Colors   map[string]string `json:"colors"`
	States   map[string]Glyph  `json:"states"`
	Agents   map[string]Glyph  `json:"agents"`
	Commands []Command         `json:"commands"`
}

// Glyph is one state's mark: its shape, the color token it is drawn in, and the
// word that replaces it where there is no color.
type Glyph struct {
	Glyph string `json:"glyph"`
	Color string `json:"color"`
	Word  string `json:"word"`
}

// Command is one thing the console does, by a stable id, with how each
// frontend reaches it. Parity is per command: every one is reachable in both,
// each the way that frontend's platform expects.
//
// Keys are keyed by frontend. "tui" keys are the window's own, with "leader+"
// standing for the leader. "desktop" keys use "primary+", which is Cmd on
// macOS and Ctrl+Shift elsewhere, where Ctrl belongs to the terminal — so a
// desktop key never adds shift to primary, which off macOS is the same chord.
type Command struct {
	ID    string              `json:"id"`
	Title string              `json:"title"`
	Group string              `json:"group"`
	Keys  map[string][]string `json:"keys"`
}

// Spec is the window's UX for a frontend that is not this one.
func Spec(leader string) UXSpec {
	return UXSpec{
		Version:  1,
		Leader:   leader,
		Colors:   specColors(),
		States:   specStates(),
		Agents:   specAgents(),
		Commands: specCommands(),
	}
}

func specColors() map[string]string {
	tokens := map[string]string{
		"mark":      colMark,
		"gold":      colGold,
		"onGold":    colBlack,
		"inactive":  colInactive,
		"band":      colInactive,
		"dim":       colDim,
		"grey":      colGrey,
		"ok":        colOK,
		"err":       colErr,
		"warn":      colWarn,
		"info":      colInfo,
		"add":       colGreen,
		"del":       colSalmon,
		"rowCursor": colHighlightBG,
		"selected":  colSelectedBG,
		"both":      colBothBG,
		"greyedOut": colGreyedOut,
	}
	out := make(map[string]string, len(tokens))
	for token, spec := range tokens {
		out[token] = colorHex(spec)
	}
	return out
}

// colorHex is a lipgloss color spec — a hex string, or an xterm-256 index — as
// the hex it stands for.
func colorHex(spec string) string {
	if len(spec) > 0 && spec[0] == '#' {
		return spec
	}
	n, err := strconv.Atoi(spec)
	if err != nil || n < 0 || n > 255 {
		return "#ff00ff"
	}
	base := [16]string{
		"#000000", "#800000", "#008000", "#808000", "#000080", "#800080", "#008080", "#c0c0c0",
		"#808080", "#ff0000", "#00ff00", "#ffff00", "#0000ff", "#ff00ff", "#00ffff", "#ffffff",
	}
	switch {
	case n < 16:
		return base[n]
	case n < 232:
		n -= 16
		step := func(v int) int {
			if v == 0 {
				return 0
			}
			return 55 + v*40
		}
		return fmt.Sprintf("#%02x%02x%02x", step(n/36), step(n/6%6), step(n%6))
	default:
		v := 8 + (n-232)*10
		return fmt.Sprintf("#%02x%02x%02x", v, v, v)
	}
}

func specStates() map[string]Glyph {
	color := func(s console.State) string {
		switch s {
		case console.StateRunning:
			return "ok"
		case console.StateStarting, console.StateRepairing, console.StateSuspended:
			return "gold"
		case console.StateError:
			return "err"
		default:
			return "dim"
		}
	}
	out := map[string]Glyph{}
	for _, s := range []console.State{console.StateRunning, console.StateStarting, console.StateRepairing, console.StateStopped, console.StateSuspended, console.StateArchived, console.StateError} {
		box := console.Sandbox{State: s}
		out[string(s)] = Glyph{Glyph: stateDot(box), Color: color(s), Word: box.StateWord()}
	}
	return out
}

func specAgents() map[string]Glyph {
	color := map[programstatus.State]string{
		programstatus.Working: "text",
		programstatus.Idle:    "dim",
		programstatus.Blocked: "err",
		programstatus.Error:   "err",
		programstatus.Done:    "add",
	}
	out := map[string]Glyph{}
	for state, c := range color {
		word := string(state)
		if state == programstatus.Error {
			word = "failed"
		}
		out[string(state)] = Glyph{Glyph: agentMark(state), Color: c, Word: word}
	}
	return out
}

// specIDs names registry commands by the prototype's IDs, which the desktop
// app still reads.
var specIDs = map[string]string{
	"attach":        "box.attach",
	"shell":         "box.shell",
	"review config": "box.reviewConfig",
	"rename":        "box.rename",
	"settings":      "box.settings",
	"upgrade":       "box.upgrade",
	"repair":        "box.repair",
	"stop":          "box.stop",
	"start":         "box.start",
	"archive":       "box.archive",
	"unarchive":     "box.unarchive",
	"purge":         "box.purge",
}

func specCommands() []Command {
	cmd := func(id, title, group string, tui, desktop []string) Command {
		return Command{ID: id, Title: title, Group: group, Keys: map[string][]string{"tui": tui, "desktop": desktop}}
	}
	// Never nil: a command no frontend binds yet is an empty list, not null.
	keys := func(k ...string) []string { return append([]string{}, k...) }
	// A registry command's desktop chords are the registry's, carried by the
	// one entry here that stands for it. Shell's are carried by pane.newShell,
	// which the app answers in the box on screen; box.shell is reached through
	// menus and the palette.
	carriers := map[string]string{"shell": "pane.newShell"}
	desktop := func(specID, id string) []string {
		if carrier, ok := carriers[id]; ok && carrier != specID {
			return keys()
		}
		c, _ := console.Lookup(id)
		return keys(c.Bindings.Desktop.Keys...)
	}
	var out []Command
	// The box's verbs: the list's single letters in the window; the box menu,
	// a row's context menu and the palette on the desktop.
	for _, c := range console.Commands() {
		if c.Lifecycle {
			out = append(out, cmd(specIDs[c.ID], c.Label, "Box", keys(c.Bindings.TUI.Keys[0]), desktop(specIDs[c.ID], c.ID)))
		}
	}
	return append(out,
		cmd("pane.newShell", "New shell", "Session", keys("leader+s"), desktop("pane.newShell", "shell")),
		cmd("pane.newTerminal", "New harness terminal", "Session", keys("leader+c"), desktop("pane.newTerminal", "terminal")),
		cmd("pane.end", "End session", "Session", keys("leader+X"), desktop("pane.end", "end")),
		cmd("pane.next", "Next pane", "Session", keys("leader+l", "leader+right"), keys("ctrl+tab")),
		cmd("pane.prev", "Previous pane", "Session", keys("leader+h", "leader+left"), keys("ctrl+shift+tab")),
		cmd("pane.maximize", "Maximize pane", "View", keys("leader+z"), desktop("pane.maximize", "maximize")),
		cmd("box.next", "Next discobox", "View", keys("leader+down", "ctrl+shift+j"), keys("primary+alt+down")),
		cmd("box.prev", "Previous discobox", "View", keys("leader+up", "ctrl+shift+k"), keys("primary+alt+up")),
		cmd("view.sidebar", "Show discoboxes", "View", keys("leader+h"), keys("primary+b")),
		cmd("view.home", "All discoboxes", "View", keys("leader+d"), desktop("view.home", "detach")),
		cmd("app.palette", "Find a box or command", "View", keys("leader+k", ":"), desktop("app.palette", "palette")),
		cmd("box.new", "New discobox", "Box", keys("leader+n", "n"), desktop("box.new", "new")),
		cmd("app.quit", "Quit", "App", keys("leader+q"), desktop("app.quit", "quit")),
	)
}

// Action is one entry of a box's actions menu, as the window offers it: the
// command it runs, whether it can now, and why not when it cannot.
type Action struct {
	Command string `json:"command"`
	Key     string `json:"key"`
	Label   string `json:"label"`
	Detail  string `json:"detail"`
	Enabled bool   `json:"enabled"`
	Why     string `json:"why,omitempty"`
}

// ActionsFor is the window's actions menu for these boxes, for a frontend that
// draws its own: the same entries, rules and reasons.
func ActionsFor(targets []console.Sandbox) []Action {
	menu := actions(targets)
	out := make([]Action, 0, len(menu))
	for _, a := range menu {
		out = append(out, Action{
			Command: specIDs[a.key], Key: a.press, Label: a.label, Detail: a.detail,
			Enabled: a.enabled, Why: a.why,
		})
	}
	return out
}
