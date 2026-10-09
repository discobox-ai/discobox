module github.com/discobox-ai/discobox/sandbox-agent

go 1.27.1

replace github.com/discobox-ai/discobox => ..

require (
	aidanwoods.dev/go-paseto v1.6.0
	github.com/charmbracelet/ultraviolet v0.0.0-20260811164956-006e29f97886
	github.com/charmbracelet/x/ansi v0.11.8
	github.com/charmbracelet/x/vt v0.0.0-20260713092006-0d683c34c74b
	github.com/coder/websocket v1.8.14
	github.com/coreos/go-systemd/v22 v22.5.0
	github.com/creack/pty v1.1.24
	github.com/discobox-ai/discobox v0.0.0
	github.com/discobox-ai/x v0.0.0-20260928053835-39c36487e906
	github.com/distribution/reference v0.6.0
	github.com/fsnotify/fsnotify v1.10.1
	github.com/go-chi/chi/v5 v5.3.0
	github.com/go-faster/jx v1.2.0
	github.com/godbus/dbus/v5 v5.2.2
	github.com/klauspost/compress v1.20.0
	github.com/tailscale/hujson v0.0.0-20260727124030-b80ff77dac4f
	golang.org/x/net v0.59.0
	golang.org/x/sys v0.48.0
	gorm.io/gorm v1.31.2
)

require (
	aidanwoods.dev/go-result v0.3.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/charmbracelet/colorprofile v0.4.3 // indirect
	github.com/charmbracelet/x/exp/ordered v0.1.0 // indirect
	github.com/charmbracelet/x/term v0.2.2 // indirect
	github.com/charmbracelet/x/termios v0.1.1 // indirect
	github.com/charmbracelet/x/windows v0.2.2 // indirect
	github.com/clipperhouse/displaywidth v0.11.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/dlclark/regexp2 v1.12.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/ebitengine/purego v0.10.2 // indirect
	github.com/elazarl/goproxy v1.9.0 // indirect
	github.com/fatih/color v1.19.0 // indirect
	github.com/ghodss/yaml v1.0.0 // indirect
	github.com/glebarez/go-sqlite v1.21.2 // indirect
	github.com/glebarez/sqlite v1.11.0 // indirect
	github.com/go-faster/errors v0.7.1 // indirect
	github.com/go-faster/yaml v0.4.6 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/pprof v0.0.0-20260802141513-ef3492d7dac3 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/pgx/v5 v5.10.0 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/jinzhu/now v1.1.5 // indirect
	github.com/lucasb-eyer/go-colorful v1.4.1 // indirect
	github.com/mattn/go-colorable v0.1.15 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/mattn/go-runewidth v0.0.27 // indirect
	github.com/mdlayher/socket v0.6.0 // indirect
	github.com/mdlayher/vsock v1.3.0 // indirect
	github.com/muesli/cancelreader v0.2.2 // indirect
	github.com/ogen-go/ogen v1.20.3 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/segmentio/asm v1.2.1 // indirect
	github.com/shopspring/decimal v1.4.0 // indirect
	github.com/things-go/go-socks5 v0.1.1 // indirect
	github.com/tursodatabase/turso-go-platform-libs v0.7.2 // indirect
	github.com/xo/terminfo v1.0.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel v1.46.0 // indirect
	go.opentelemetry.io/otel/metric v1.46.0 // indirect
	go.opentelemetry.io/otel/trace v1.46.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.uber.org/zap v1.28.0 // indirect
	golang.org/x/crypto v0.57.0 // indirect
	golang.org/x/exp v0.0.0-20260824195058-e88cd73687aa // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	golang.org/x/time v0.16.0 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	gorm.io/driver/postgres v1.6.2 // indirect
	modernc.org/libc v1.22.5 // indirect
	modernc.org/mathutil v1.5.0 // indirect
	modernc.org/memory v1.5.0 // indirect
	modernc.org/sqlite v1.23.1 // indirect
	turso.tech/database/tursogo v0.7.2 // indirect
)

replace github.com/charmbracelet/x/ansi => github.com/discobox-ai/charm-x/ansi v0.11.9-0.20260926040046-9d904b1c7255

replace github.com/charmbracelet/x/vt => github.com/discobox-ai/charm-x/vt v0.0.0-20261009234112-360d701e4806

replace github.com/charmbracelet/ultraviolet => github.com/discobox-ai/ultraviolet v0.0.0-20260926043624-eff2d8acf8a4
