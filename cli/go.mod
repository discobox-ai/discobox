module github.com/discobox-ai/discobox/cli

go 1.26.1

require (
	github.com/coder/websocket v1.8.14
	github.com/discobox-ai/discobox v0.0.0
	github.com/discobox-ai/x v0.0.0-20260903144420-ff3709c36e16
	github.com/go-faster/jx v1.2.0
	github.com/spf13/cobra v1.10.2
	github.com/spf13/pflag v1.0.10
	golang.org/x/crypto v0.53.0
	golang.org/x/sys v0.47.0
	golang.org/x/term v0.44.0
)

require (
	github.com/discobox-ai/iroh-go v0.2.0 // indirect
	github.com/discobox-ai/iroh-go/libs/darwin_amd64 v0.2.0 // indirect
	github.com/discobox-ai/iroh-go/libs/darwin_arm64 v0.2.0 // indirect
	github.com/discobox-ai/iroh-go/libs/linux_amd64 v0.2.0 // indirect
	github.com/discobox-ai/iroh-go/libs/linux_amd64_musl v0.2.0 // indirect
	github.com/discobox-ai/iroh-go/libs/linux_arm64 v0.2.0 // indirect
	github.com/discobox-ai/iroh-go/libs/linux_arm64_musl v0.2.0 // indirect
	github.com/discobox-ai/iroh-go/libs/windows_amd64 v0.2.0 // indirect
	github.com/discobox-ai/iroh-go/libs/windows_arm64 v0.2.0 // indirect
	github.com/ebitengine/purego v0.10.2 // indirect
	golang.org/x/mod v0.37.0 // indirect
)

require (
	github.com/atotto/clipboard v0.1.4
	github.com/charmbracelet/x/exp/ordered v0.1.0 // indirect
	github.com/charmbracelet/x/vt v0.0.0-20260713092006-0d683c34c74b
	github.com/dlclark/regexp2 v1.12.0 // indirect
)

require (
	charm.land/bubbles/v2 v2.2.1
	charm.land/bubbletea/v2 v2.0.8
	charm.land/lipgloss/v2 v2.0.5
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/adrg/xdg v0.5.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/charmbracelet/colorprofile v0.4.3
	github.com/charmbracelet/ultraviolet v0.0.0-20260703014108-f5a850f9c2b7
	github.com/charmbracelet/x/ansi v0.11.7
	github.com/charmbracelet/x/term v0.2.2 // indirect
	github.com/charmbracelet/x/termios v0.1.1 // indirect
	github.com/charmbracelet/x/windows v0.2.2 // indirect
	github.com/clipperhouse/displaywidth v0.11.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/creack/pty v1.1.24
	github.com/discobox-ai/discobox/termpane v0.0.0
	github.com/fatih/color v1.19.0 // indirect
	github.com/ghodss/yaml v1.0.0 // indirect
	github.com/go-faster/errors v0.7.1 // indirect
	github.com/go-faster/yaml v0.4.6 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/lucasb-eyer/go-colorful v1.4.1 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.21 // indirect
	github.com/mattn/go-runewidth v0.0.27 // indirect
	github.com/muesli/cancelreader v0.2.2 // indirect
	github.com/ogen-go/ogen v1.20.3 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/segmentio/asm v1.2.1 // indirect
	github.com/shopspring/decimal v1.4.0 // indirect
	github.com/xo/terminfo v0.0.0-20220910002029-abceb7e1c41e // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel v1.43.0 // indirect
	go.opentelemetry.io/otel/metric v1.43.0 // indirect
	go.opentelemetry.io/otel/trace v1.43.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.uber.org/zap v1.27.1 // indirect
	golang.org/x/exp v0.0.0-20250819193227-8b4c13bb791b // indirect
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/sync v0.21.0 // indirect
	golang.org/x/text v0.38.0 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
)

replace github.com/discobox-ai/discobox => ..

replace github.com/discobox-ai/discobox/termpane => ../termpane

replace github.com/charmbracelet/x/ansi => github.com/discobox-ai/charm-x/ansi v0.11.9-0.20260813023456-57e8cef06953

replace github.com/charmbracelet/x/vt => github.com/discobox-ai/charm-x/vt v0.0.0-20260827051753-424bd566a6bf
