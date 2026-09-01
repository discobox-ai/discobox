module github.com/discobox-ai/discobox/termpane

go 1.26.1

require (
	charm.land/bubbletea/v2 v2.0.8
	github.com/charmbracelet/ultraviolet v0.0.0-20260703014108-f5a850f9c2b7
	github.com/charmbracelet/x/ansi v0.11.7
	github.com/charmbracelet/x/vt v0.0.0-20260713092006-0d683c34c74b
	github.com/discobox-ai/x v0.0.0-20260828205057-2567df0ccf27
)

require (
	github.com/aymanbagabas/go-udiff v0.4.1 // indirect
	github.com/charmbracelet/colorprofile v0.4.3 // indirect
	github.com/charmbracelet/x/exp/golden v0.0.0-20250806222409-83e3a29d542f // indirect
	github.com/charmbracelet/x/exp/ordered v0.1.0 // indirect
	github.com/charmbracelet/x/term v0.2.2 // indirect
	github.com/charmbracelet/x/termios v0.1.1 // indirect
	github.com/charmbracelet/x/windows v0.2.2 // indirect
	github.com/clipperhouse/displaywidth v0.11.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/lucasb-eyer/go-colorful v1.4.1 // indirect
	github.com/mattn/go-runewidth v0.0.27 // indirect
	github.com/muesli/cancelreader v0.2.2 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/xo/terminfo v0.0.0-20220910002029-abceb7e1c41e // indirect
	golang.org/x/exp v0.0.0-20250819193227-8b4c13bb791b // indirect
	golang.org/x/sync v0.21.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
)

replace github.com/charmbracelet/x/ansi => github.com/discobox-ai/charm-x/ansi v0.11.9-0.20260813023456-57e8cef06953

replace github.com/charmbracelet/x/vt => github.com/discobox-ai/charm-x/vt v0.0.0-20260827051753-424bd566a6bf
