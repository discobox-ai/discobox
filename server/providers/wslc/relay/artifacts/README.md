# Guest control-plane relay artifacts

`discobox-cp-relay.linux-amd64.gz` is produced by `task build:cp-relay` (which
`task build` runs) and embedded into the server binary.

It is a build artifact, not source: it is gitignored, so a built tree stays
clean. This README is committed so the directory always exists and `go:embed`
— and therefore a plain `go build ./...` in a fresh checkout — keeps working.

Without the artifact the server still compiles and runs; only the wslc provider
fails, with a message telling you to run the build task.
