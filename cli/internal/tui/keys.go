package tui

import "charm.land/bubbles/v2/key"

// keyMap is the full set of key bindings for the TUI. Navigation is vim-first
// (j/k, g/G) with arrow-key equivalents, matching how a k9s user expects to
// move. Bindings double as the source of truth for the footer help, so every
// user-facing key carries a help label.
type keyMap struct {
	Up         key.Binding
	Down       key.Binding
	Left       key.Binding
	Right      key.Binding
	Top        key.Binding
	Bottom     key.Binding
	Mark       key.Binding
	SelectAll  key.Binding
	Visual     key.Binding
	Enter      key.Binding
	Fullscreen key.Binding
	New        key.Binding
	Edit       key.Binding
	Delete     key.Binding
	Agents     key.Binding
	Default    key.Binding
	Configure  key.Binding
	Refresh    key.Binding
	Help       key.Binding
	Back       key.Binding
	Quit       key.Binding
	Tab        key.Binding
	ShiftTab   key.Binding
}

func defaultKeyMap() keyMap {
	return keyMap{
		Up: key.NewBinding(
			key.WithKeys("up", "k"),
			key.WithHelp("↑/k", "up"),
		),
		Down: key.NewBinding(
			key.WithKeys("down", "j"),
			key.WithHelp("↓/j", "down"),
		),
		Left: key.NewBinding(
			key.WithKeys("left", "h"),
			key.WithHelp("←/h", "prev tab"),
		),
		Right: key.NewBinding(
			key.WithKeys("right", "l"),
			key.WithHelp("→/l", "next tab"),
		),
		Top: key.NewBinding(
			key.WithKeys("g", "home"),
			key.WithHelp("g", "top"),
		),
		Bottom: key.NewBinding(
			key.WithKeys("G", "end"),
			key.WithHelp("G", "bottom"),
		),
		Mark: key.NewBinding(
			key.WithKeys("space"),
			key.WithHelp("space", "mark"),
		),
		SelectAll: key.NewBinding(
			key.WithKeys("ctrl+a"),
			key.WithHelp("^a", "all"),
		),
		Visual: key.NewBinding(
			key.WithKeys("v"),
			key.WithHelp("v", "range"),
		),
		Enter: key.NewBinding(
			key.WithKeys("enter"),
			key.WithHelp("enter", "attach"),
		),
		Fullscreen: key.NewBinding(
			key.WithKeys("f"),
			key.WithHelp("f", "fullscreen"),
		),
		New: key.NewBinding(
			key.WithKeys("n"),
			key.WithHelp("n", "new"),
		),
		Edit: key.NewBinding(
			key.WithKeys("e"),
			key.WithHelp("e", "edit"),
		),
		Delete: key.NewBinding(
			key.WithKeys("d"),
			key.WithHelp("d", "delete"),
		),
		Agents: key.NewBinding(
			key.WithKeys("a"),
			key.WithHelp("a", "agents"),
		),
		Default: key.NewBinding(
			key.WithKeys("enter"),
			key.WithHelp("enter", "set default"),
		),
		Configure: key.NewBinding(
			key.WithKeys("c"),
			key.WithHelp("c", "configure"),
		),
		Refresh: key.NewBinding(
			key.WithKeys("r"),
			key.WithHelp("r", "refresh"),
		),
		Help: key.NewBinding(
			key.WithKeys("?"),
			key.WithHelp("?", "help"),
		),
		Back: key.NewBinding(
			key.WithKeys("esc"),
			key.WithHelp("esc", "back"),
		),
		Quit: key.NewBinding(
			key.WithKeys("q", "ctrl+c"),
			key.WithHelp("q", "quit"),
		),
		Tab: key.NewBinding(
			key.WithKeys("tab"),
			key.WithHelp("tab", "next field"),
		),
		ShiftTab: key.NewBinding(
			key.WithKeys("shift+tab"),
			key.WithHelp("shift+tab", "prev field"),
		),
	}
}
