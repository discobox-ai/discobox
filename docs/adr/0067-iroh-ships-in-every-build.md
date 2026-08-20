# 0067 — iroh ships in every build

- **Status**: Proposed
- **Date**: 2026-08-20

## Context

[ADR 0053](0053-iroh-is-development-only-until-it-builds-everywhere.md) made
iroh a development-only capability and deferred the decision with a condition
in its §4: revisit when `discobox` can carry iroh on macOS and Windows. It named
two sufficient routes — build the remaining cgo targets, or move to a
purego/dlopen loader — and called the second the better end state and the
larger piece of work, because nobody had done it for iroh-ffi.

Somebody has. `github.com/discobox-ai/iroh-go` wraps the same iroh 1.0.3
through [purego](https://github.com/ebitengine/purego) and a prebuilt cdylib
per platform, with no cgo anywhere. The transport moved onto v0.2.0 in
`feat(endpoint): serve iroh through CGO-free bindings`.

The condition is met, and checked rather than assumed:

- `CGO_ENABLED=0` builds of `discobox` and `discobox-server` succeed with the
  transport compiled in wherever the rest of the tree cross-compiles today:
  `windows/amd64`, `windows/arm64` and `linux/arm64`, plus a `musl`-tagged
  `linux/amd64`. The Apple targets do not cross-compile from Linux at all —
  `Code-Hex/vz`, the macOS provider's dependency, needs cgo and Apple's SDK —
  and they fail identically without iroh in the tree, so what iroh blocked is
  no longer what blocks them. A Mac builds both binaries natively, and the
  bindings ship a library for each Apple target.
- The bindings' release workflow runs their Go suite on real hardware for each
  of the eight platform artifacts before publishing them, so no platform ships
  a library that has never been executed.

What remains is not a question of capability but of cost, and that is what this
ADR decides. It supersedes ADR 0053.

## Decision

### 1. The `iroh` build tag goes away

Every build carries the transport. `endpoint/iroh_default.go` goes with the
tag, along with the function variables it installs and `ErrIrohUnsupported`:
no build can produce that error any more, so the sentence telling an operator
to "rebuild with -tags iroh" has nobody left to tell. `task check` and
`task test` absorb `check:iroh` and `test:iroh`, losing their `platforms:`
restrictions and their `CGO_ENABLED=1`, and the `go-lsp` hook's ignore list
empties out — gopls analyzes one build configuration, and that is now the only
one there is.

Keeping the tag for the server alone, or on Linux alone, is the partial
availability ADR 0053 §1 already rejected, for the reason it gave: a
remote-access transport that exists for some of a team is worse than one that
exists for nobody, because it becomes the documented way to reach a server and
then does not work for the colleague on a Mac.

### 2. The cost is 14 MB of binary, paid by every user

`discobox` goes from 78 MB to 92 MB: the per-platform cdylib is embedded in the
binary. That is the entire cost. ADR 0053 §1's objection was to cgo, a C
toolchain requirement in every build, and 81 MB of Rust staticlib; none of
those survive the move, and `go build` still downloads only the one platform
module it needs.

A runtime opt-in — a plugin, or a library fetched on first use — was rejected.
It trades a fixed 14 MB for a download path, a version-skew failure mode
between binary and library, and a capability that works on the machine of
whoever built it and not on the user's. That is a worse trade on a binary that
is already 78 MB.

### 3. Nothing is loaded until an iroh endpoint is used

Verified, not assumed. The cdylib is extracted and `dlopen`'d on the first call
across the FFI boundary, and discobox makes none unless a listen endpoint or
`--server` names `iroh://`:

| command | files in the library cache |
| --- | --- |
| `discobox --help` | 0 |
| `discobox --server unix://… ls` | 0 |
| `discobox --server iroh://… ls` | 1 (14 MB) |

So a user who never reaches for iroh pays the size of the binary and nothing
else — no extraction, no startup cost, no cache directory. This is what makes
§2 a decision about bytes rather than about behavior.

### 4. `task dev` binds `iroh://` on every platform

ADR 0053 §2 restricted the development listener to the platforms the bindings
built for, so that a Mac developer got a working dev loop rather than a link
failure. There is no such platform now, so `IROH_TAG` and the conditional in
`DEV_LISTEN` collapse: a development server binds the local socket and
`iroh://` everywhere. `DISCOBOX_DEV_IROH=0` stays as the opt-out for anyone who
wants the socket alone.

## Consequences

- Release artifacts grow by roughly 14 MB per binary, per platform.
- The first `iroh://` command on a machine writes ~14 MB into the user cache
  directory, which must be writable and not mounted `noexec`.
  `IROH_GO_CACHE_DIR` and `IROH_GO_LIBRARY` are the escape hatches, and an
  environment that allows neither loses iroh rather than the CLI.
- macOS and Windows users can reach a server over `iroh://` for the first time.
  The scheme stops being one that parses and then refuses.
- The root module's Go floor becomes whatever the bindings declare, currently
  1.26. Discobox is already there, so this costs nothing today, but it is now a
  shared floor rather than ours alone.
- Everything ADR 0052 established is unchanged: the scheme's shape,
  `authorized_ids`, `discobox admin iroh-id`, one identity per server, and the
  HTTP-over-one-stream framing that keeps hijack working.
- The transport is exercised only over loopback. Nothing in any suite reaches a
  relay, traverses a NAT, or crosses a lossy link, so shipping to macOS and
  Windows means the first real holepunch from those platforms happens in a
  user's hands. What is ours above the bindings is platform-independent Go and
  is tested; what is not is iroh's own path discovery, which is upstream code
  with upstream's test surface. A two-machine test across a real network is
  worth doing on its own merits, and is not a reason to keep the capability
  from the platforms that cannot get it today.
