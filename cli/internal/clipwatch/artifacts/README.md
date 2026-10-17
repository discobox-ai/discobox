# Clipboard watch helper artifacts

`discobox-clipwatch.windows-<arch>.exe.gz` is produced by
`go tool task build:clipwatch` (which `task build:cli` runs): it cross-compiles
`cli/cmd/discobox-clipwatch` for Windows and compresses it with
`internal/cmd/discobox-gzip`. The Linux CLI embeds this directory through
`//go:embed artifacts` in `../start_linux.go` and runs the helper from WSL to
watch the Windows clipboard. One is built per Windows architecture — `amd64`
and `arm64` — because WSL2 does not emulate: a distribution runs its host's
architecture, so a CLI reads the helper matching its own `GOARCH`.

They are build artifacts, not source: they are gitignored, so a built tree
stays clean. This README is committed so the directory always exists and
`go:embed` — and therefore a plain `go build ./...` in a fresh checkout — keeps
working.

Without the artifact the CLI still builds and runs; on WSL, dictation pasted
with Ctrl-V falls back to `^V`, and the console's clipboard watch reports the
build task to run.
