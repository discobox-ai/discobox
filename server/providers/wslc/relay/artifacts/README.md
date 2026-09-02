# Guest control-plane relay artifacts

`discobox-cp-relay.linux-<arch>.gz` is produced by `task build:cp-relay` (which
`task build` runs) and embedded into the server binary. One is built per
Windows target architecture — `amd64` and `arm64` — because WSL2 does not
emulate: a guest runs its host's architecture, so a server reads the relay
matching its own `GOARCH`.

They are build artifacts, not source: they are gitignored, so a built tree
stays clean. This README is committed so the directory always exists and
`go:embed` — and therefore a plain `go build ./...` in a fresh checkout —
keeps working.

Without the artifact the server still compiles and runs; only the wslc provider
fails, with a message telling you to run the build task.
