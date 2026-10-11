module github.com/discobox-ai/discobox/termpane

go 1.27.1

require (
	charm.land/bubbletea/v2 v2.0.10
	github.com/charmbracelet/ultraviolet v0.0.0-20260811164956-006e29f97886
	github.com/charmbracelet/x/ansi v0.11.8
	github.com/charmbracelet/x/vt v0.0.0-20260713092006-0d683c34c74b
	github.com/discobox-ai/x v0.0.0-20260928053835-39c36487e906
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
	github.com/xo/terminfo v1.0.0 // indirect
	golang.org/x/exp v0.0.0-20260824195058-e88cd73687aa // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
)

replace github.com/charmbracelet/x/ansi => github.com/discobox-ai/charm-x/ansi v0.11.9-0.20260926040046-9d904b1c7255

replace github.com/charmbracelet/x/vt => github.com/discobox-ai/charm-x/vt v0.0.0-20261009234112-360d701e4806

replace github.com/charmbracelet/ultraviolet => github.com/discobox-ai/ultraviolet v0.0.0-20260926043624-eff2d8acf8a4
