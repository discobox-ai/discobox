# 0053 — iroh is a development-only capability until it builds for macOS and Windows

- **Status**: Accepted
- **Date**: 2026-08-19

## Context

[ADR 0052](0052-iroh-is-an-optional-endpoint-scheme.md) made iroh an optional
endpoint scheme behind an `iroh` build tag, bound to the
`git.coopcloud.tech/decentral1se/iroh-go` bindings. The transport works: a
server binds `iroh://`, a client dials it, and the whole product rides it —
REST, websocket exec, sandbox lifecycle.

It builds for `linux/amd64` and `linux/arm64` and nowhere else. That is a
packaging choice rather than a platform limit — the target list is one line in
the bindings' build script — but nobody has produced the other artifacts yet.
Apple targets need a macOS runner, which the `cross` tool the bindings use
cannot provide, and Windows needs `x86_64-pc-windows-gnu` because cgo links
through gcc and cannot consume an MSVC `.lib`.

`disco` is a cross-platform CLI. Its users are on macOS and Windows as much as
on Linux.

This supersedes ADR 0052 §4's expectation that Discobox would build the
remaining targets rather than wait. The transport landed first; the artifacts
did not.

## Decision

### 1. Release artifacts are built without the tag

`task build`, `task build:cli`, and `task build:server` pass no build tags, and
none is added. Only the `dev` tasks set the tag (§2), so the capability exists
in a development loop and in nothing that ships.

Shipping it on Linux alone was rejected. A remote-access transport that exists
for some of a team and not the rest is worse than one that exists for nobody:
it becomes the documented way to reach a server, and then does not work for the
colleague on a Mac. "Run the server anywhere, connect from anywhere" is a claim
about every client or it is not the feature.

Gating it behind a runtime flag instead of a build tag was also rejected. It
would put cgo, a C toolchain requirement, and 81 MB of Rust staticlib into
every build of a CLI that is otherwise pure Go and cross-compiles cleanly —
paying the whole cost of the dependency to ship it disabled.

### 2. The development loop enables it only where it builds

`task dev` sets `-tags=iroh` on `linux/amd64` and `linux/arm64`, and nothing
elsewhere. A dev task that fails to link on half the team's machines is worse
than one that quietly offers less on those machines, and the alternative — a
Mac developer editing the Taskfile or exporting an opt-out before they can run
the server at all — makes the default hostile to exactly the platforms this ADR
exists to wait for.

### 3. The tagged configuration is still checked, where it can be

`task check` and `task test:all` run `check:iroh` and `test:iroh`, which lint,
compile, and test the tagged build. Both declare
`platforms: [linux/amd64, linux/arm64]`, so they run where the bindings exist
and are skipped elsewhere rather than failing a macOS developer's `task check`
over an artifact they cannot produce.

Dropping the checks entirely until the other platforms exist was rejected: the
transport is being developed now, and unchecked code behind a build tag rots
without anyone noticing (ADR 0052 §4).

### 4. Revisit when the artifacts exist

This decision is reconsidered when `disco` can carry iroh on macOS and Windows.
Two routes are open, and either is sufficient:

- Build the remaining Rust targets — a fork of the bindings' build script plus
  a macOS runner and a mingw Windows target — and add the matching `#cgo`
  lines.
- Move to a purego/dlopen loader instead of cgo linking. This repository
  already ships a Rust library that way: `turso.tech/database/tursogo` reaches
  one through `ebitengine/purego` with a separate platform-libs module and no
  cgo. That shape keeps `CGO_ENABLED=0` and leaves cross-compilation working,
  which the cgo route does not.

The second is the better end state and the larger piece of work, because
nobody has done it for iroh-ffi yet.

## Consequences

- `disco` and `discobox-server` releases behave exactly as before this work:
  `iroh://` parses and is rejected with "this build does not include iroh
  support; rebuild with -tags iroh".
- `task dev` carries the tag on `linux/amd64` and `linux/arm64` and omits it
  everywhere else, so a developer on macOS or Windows gets a working dev loop
  without the transport rather than a cgo link failure. `DISCOBOX_DEV_IROH=0`
  turns it off where it would otherwise apply.
- The dependency stays in `go.mod` for every build regardless of the tag, since
  build tags exclude files rather than modules. Every build resolves it, and
  the root module's minimum Go version stays at the one the bindings require.
- The iroh-specific pieces — `endpoint/iroh*.go`, `server/internal/irohd`,
  `disco box iroh-id`, `authorized_ids` — are live code with tests, not
  scaffolding, and are maintained as such.
