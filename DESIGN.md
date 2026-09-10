# Design Overview

## System Pattern

The server stores desired resource intent and reconciles actual sandbox runtime
state through sandbox providers. Providers own runtime-specific mechanics and
report or expose observed runtime state back to the server.

Accepted intent changes are persisted with their durable reconcile jobs in one
transaction. Reconcile jobs target one resource generation and are
canceled when superseded by newer intent.

## High-Level System Design

At the system boundary, Discobot is three cooperating concepts:

- **Server**: the control plane and API surface. It stores which sandboxes
  should exist and with what spec, and coordinates reconciliation toward that.
  It holds no opinion about whether a sandbox is running: power state is
  observed and reported by the pool agent, and start/stop/restart are
  instructions forwarded to it (ADR 0017 §9).
- **Pool**: the user-visible sharing boundary sandboxes are scheduled into,
  and its own runtime host (ADR-0003, ADR-0006). Sandboxes in one pool share a
  cache volume, a resource envelope, and a kernel/host; a pool binds immutably
  to one provider instance, and its host runtime (container/VM/pod) is
  replaceable in place under the pool's identity.
- **Sandbox provider**: the Go-level runtime integration interface implemented
  by provider backends. A provider instance is backend identity only —
  capacity and sharing policy live on pools.
- **Worker-local sandbox operations API**: the REST/OpenAPI runtime-operation
  interface exposed by a pool agent for sandboxes hosted on that pool.
- **Sandbox agent API**: the future in-sandbox REST/OpenAPI interface exposed
  from inside an individual sandbox environment.

```mermaid
flowchart LR
    cli["CLI"] -->|"generated client"| server["Server / control plane"]
    clients["API clients"] --> server
    server -->|"Go interface"| provider["Sandbox provider"]
    provider -->|"delegates access"| sandbox["Worker-local sandbox operations API"]
    server -->|"REST/OpenAPI through provider"| sandbox
    provider -->|"observed runtime state"| server
```

The root design intentionally stops at this integration view. Interface details
belong to the owning component docs.

## API Contracts

Use contract-first REST API development for public and provider-delegated REST
surfaces:

- Server REST API: control plane API consumed by the CLI and external clients.
- Pool-local sandbox operations API: runtime operations exposed by pool
  harnesses and reached through provider-delegated access.
- Sandbox agent API: in-sandbox API exposed by the sandbox-agent runtime.

The OpenAPI contract is the canonical API definition. Generate server handlers,
client types, validators, and documentation from the contract instead of deriving
the contract from Go handler code. Current contracts are intentionally split by
surface:

- `api/openapi/server.yaml` is the canonical control-plane REST API contract.
- `pool-agent/api/openapi/pool.yaml` is the canonical pool-local sandbox
  operations API contract. `pool-agent/generate.go` generates combined
  client/server transport code into `pool-agent/api/gen` and stable schema
  aliases into `pool-agent/api/model`; `pool-agent/server` adapts the
  generated server scaffold to local runtime operations.
- Sandbox-agent terminal routes are canonical in `api/openapi/server.yaml` and
  marked for sandbox-agent subset generation. `api/openapi/sandbox.yaml` is
  generated from that server contract and must not be edited directly.
- `/etc/discobox/sandbox.json` (the sandbox's effective runtime config) is
  not a REST contract and is not OpenAPI-generated. It is the hand-written
  `sandboxconfig` package — see `sandboxconfig/DESIGN.md` and
  `docs/adr/0012-sandbox-config-is-three-attribute-owned-layers.md`.

## Target Module Boundaries

Make the repository root the stable contracts/API module. Server-owned
persistence, provider contracts, and provider implementations live in the server
module so provider adapters can use server-internal control-plane contracts.

```mermaid
flowchart TD
    cli["github.com/discobox-ai/discobox/cli"] --> root["github.com/discobox-ai/discobox"]
    server["github.com/discobox-ai/discobox/server"] --> root
    server --> providers["github.com/discobox-ai/discobox/server/providers"]
    server --> orchestration["github.com/discobox-ai/discobox/orchestration"]
    server --> x["github.com/discobox-ai/x"]
    providers --> serverInternal["github.com/discobox-ai/discobox/server/internal"]
    providers --> poolAgent["github.com/discobox-ai/discobox/pool-agent"]
    poolAgent --> root
    sandboxAgent["github.com/discobox-ai/discobox/sandbox-agent"] --> root
    agentCred["github.com/discobox-ai/discobox/access"] --> root
```

- Root module: public API definitions, control-plane OpenAPI documents,
  generated API clients/scaffolds, cross-module sentinel errors, IDs, pool
  boot metadata contracts, client-facing stream DTOs, and the exec attach
  stream: the wire protocol (`execstream/frame`), the duplex `execstream.Conn`
  seam, resumable positioned delivery (`execstream/resume`), and both roles:
  `execstream/host` serves a process's output to attached clients, and
  `execstream/client` attaches a caller's stdio to a remote process.
  `execstream.Prober` is the optional physical-transport timing capability;
  `resume` combines its heartbeat RTT with positioned-action acknowledgement
  RTT and emits transport-neutral timing events for frontends.
  `execstream.Delivery` is the optional capability reporting how far the host
  has caught up with what the client wrote, which only a positioned stream can
  answer; `execstream/client` uses it to tell an interrupt the remote applied
  from one a stalled stream swallowed. See
  [`execstream/resume/DESIGN.md`](execstream/resume/DESIGN.md) for the consumer
  contract and status interpretation. The platform halves stay with their
  platform — the PTY and
  screen emulator in `sandbox-agent`, terminal control in the CLI — so the
  shared module never grows a terminal dependency. See
  [ADR 0008](docs/adr/0008-attach-stream-packages.md).
- CLI module: `discobox` command implementation; depends on root generated
  clients/contracts for normal user commands and talks to the control plane
  through the Server REST API. It does **not** depend on the server module:
  `discobox admin server` resolves a `discobox-server` binary — one beside the
  executable, or one staged from the manifest the release linked in — and runs
  it as a child, which is also what local auto-launch starts
  ([ADR 0099](docs/adr/0099-the-cli-downloads-the-server-it-starts.md)).
- Server module: control plane implementation, persistence models, sandbox
  provider Go interfaces, provider manager, and Docker/VM/cloud/pool-backed
  provider implementations.
- Shared generic libraries live in [`discobox-ai/x`](https://github.com/discobox-ai/x)
  — `frontmatter`, `gormdb`, `gitutil`, `id`, `selection`, `shorttmp` — consumed
  at the latest commit on its `main`. Nothing there knows about Discobox; a
  helper that would have to is not generic and belongs in the module that needs
  it.
- Pool-agent module: in-guest pool host process, pool-local runtime DTOs, and
  generated pool-local sandbox operations API server adapter; depends on root
  pool boot contracts and OpenAPI contracts.
- Root module: local Docker development image watcher for the shared base,
  pool-agent, and sandbox-agent images, plus the versioned development-image
  manifest contract shared with the server.
- Sandbox-agent module: in-sandbox agent REST API runtime environment and harness
  implementation; depends on root contracts and generated API types.
- Agent-credential CLI module: `discobox-access`, the in-sandbox client of
  the agent credentials protocol. It is its own module and its own binary — not
  an `argv[0]` alias of the sandbox agent — because it is meant to be liftable
  into another repository, and it depends on nothing but the stdlib-only
  `agentcreds` package. See [`access/DESIGN.md`](access/DESIGN.md).

Worker-agent and sandbox-agent implementations cannot depend on packages under
Go `internal/` outside their module. Provider implementations are part of the
server module and may depend on `server/internal`.

Root module package map:

| Package/path | Ownership |
| --- | --- |
| [`api/openapi`](api/openapi) | Canonical OpenAPI source contracts owned by the root module: the server REST API, plus generated sandbox-agent subset output. Pool-agent-owned contracts live under `pool-agent/api/openapi`. |
| [`api/gen`](api/gen) | Generated client/server API scaffold from `api/openapi/server.yaml`, plus handwritten client helpers for transports OpenAPI generation cannot own. |
| [`api/sandboxgen`](api/sandboxgen) | Generated client/server API scaffold from generated `api/openapi/sandbox.yaml`, the sandbox-agent subset of the server contract. |
| [`api/model`](api/model) | Generated stable aliases for server REST API schema types. |
| [`devimage`](devimage) | Versioned watcher/server contract for content-addressed development Docker image sets and their opt-in environment keys. |
| [`agentcreds`](agentcreds) | The agent credentials protocol: the portable list/request/get contract, its client, an `http.Handler` over a `Service` interface, and the stable error codes and client configuration both halves share. It knows nothing about Discobox, which is what lets one in-sandbox CLI work against sandbox-agent and against any other implementation. See [`docs/agent-credentials-protocol.md`](docs/agent-credentials-protocol.md) and [ADR 0031](docs/adr/0031-agent-credentials-are-a-portable-protocol-with-ephemeral-sentinels.md). |
| [`endpoint`](endpoint) | How a client reaches the control plane and how the control plane listens, resolved from a URL scheme. Shared because the CLI and the server must agree on what an endpoint means, and because `git`, websockets, and the generated client all reach the server through the one client it builds. The pool-agent hop is resolved separately by [`pool-agent/wire`](pool-agent/wire). It also owns `Diagnose`, which reports reaching a server layer by layer rather than as the one error the top of the stack produces, and the transport logging behind `discobox --iroh-log` and the server's `iroh.logLevel` — both live here because the layers they describe are this package's, and nothing above it can see them. |
| [`harness`](harness) | Harness hook registration drivers for sandbox terminals. |
| [`serverstage`](serverstage) | The description of a released server — its assets, where each is published, and what each one's SHA-256 is — and the staging that turns one into verified files on disk, one directory per version. In the root module because it is a release-format contract with two ends: the CLI decodes the manifest a release linked into it, and `internal/cmd/discobox-server-manifest` is what encodes one at build time. See [ADR 0099](docs/adr/0099-the-cli-downloads-the-server-it-starts.md). |
| [`id`](id) | Shared identifier helpers. |
| [`hostscope`](hostscope) | What a credential's host scope covers: a scope covers itself and everything beneath it, never its parent. Shared because three places compare a scope against the destination the proxy observed — the control plane's grant lookup, the pool agent's activation check, and the guard on what a grant may point a secret at — and a rule that differs in one of them is either a credential that stops working for no visible reason or one that travels somewhere nobody approved. |
| [`secretformat`](secretformat) | The shape of credential values: a generative template that mints a sentinel byte-identical to a real provider key, and inference of a template from a real value. Shared because both ends mint sentinels — the control plane the stable one bound to a sandbox, the pool agent the ephemeral one per use — and a sentinel shaped by different rules at each end would be distinguishable from the real thing. |
| [`internal/hostid`](internal/hostid) | This machine's generated, persisted Discobox identity. Shared because a CLI and a control plane on one machine must resolve the same value: that agreement is how the server knows a request came from its own filesystem. |
| [`internal/originkey`](internal/originkey) | Derives the key identifying a sandbox origin. Shared so client and server cannot drift on it. |

Submodule package docs belong in their owning module trees and are intentionally
not listed here.

## Build, Check, and Release

Three layers, and the top one holds no logic (ADR 0066):

```mermaid
flowchart TD
    workflows[".github/workflows/<br/>when, on which runner, how artifacts move"]
    action[".github/actions/task<br/>checkout done, install Nix, restore caches, run one target"]
    taskfile["Taskfile.yml<br/>every step of check, test, build, release"]
    flake["flake.nix<br/>system tools and environment"]

    workflows --> action --> taskfile --> flake
```

- `flake.nix` owns the toolchain. `nix develop` (or direnv via `.envrc`) is the
  entry point; its `shellHook` exports the `DISCOBOX_*` environment and
  regenerates CLI completions. `GOTOOLCHAIN=auto` means Nix supplies a bootstrap
  Go and `go.mod` names the one that compiles. `devShells.libkrun` adds the two
  things a machine running libkrun pools needs installed — libkrun and passt —
  and stays out of the default shell because that libkrun build is not in
  cache.nixos.org; `nix build .#libkrun-runtime` is the same closure for a
  machine that is not in a dev shell.
- `Taskfile.yml` owns every step. `go tool task --list` is the index; `task`,
  `golangci-lint`, and `ogen` are `go tool` dependencies pinned by `go.mod`, not
  by Nix.
- `.github/workflows/` decides only *when* a target runs and on which runner.
  Every job body is `nix develop -c go tool task <target>`, carried by the
  `.github/actions/task` composite action.

Release binaries are built on the platform they target — darwin has to be, since
`vz` is cgo against Virtualization.framework and its entitlement is applied by
`codesign` at build time (see
[`server/providers/vz/DESIGN.md`](server/providers/vz/DESIGN.md)). Windows is the
exception in both directions: no Nix, so its tests run under `actions/setup-go`,
and no cgo, so its binary is cross-compiled from the Linux job. Agent images are
built once for both architectures by `depot build`, falling back to emulated
`docker buildx`.

A release passes through three states, and only the first is decided by the tag
(ADR 0105). Pushing a tag publishes a GitHub **prerelease** and nothing more:
whether a build turned out to be good is not knowable at push time, so nothing
at push time claims it.

```mermaid
flowchart LR
    tag["push vX.Y.Z<br/>release.yml"] --> pre["GitHub prerelease"]
    pre --> latest["latest channel<br/>ghcr :latest<br/>brew discobox-dev"]
    pre -.->|"a human unticks<br/>“Set as a pre-release”"| stable
    stable["stable channel<br/>brew discobox<br/>winget"]
    rc["push vX.Y.Z-rc1"] --> only["that tag only"]
```

- **`DOT_RELEASE`** — exactly `vMAJOR.MINOR.PATCH` — is the *latest* channel's
  rule, and the only thing the tag itself decides. A dot release moves ghcr's
  `:latest` (`dockerworker.DefaultPoolImage`, what a pool boots when nothing
  named a version) and the `discobox-dev` formula the moment it is cut. An
  explicit prerelease tag (`-alpha`, `-beta`, `-rc`) moves neither, deliberately:
  it is the lowest confidence level — a real release build, signed and pushed,
  that reaches no brew channel at all and is had by pinning the version.
- **Stable** is a human clearing the prerelease box on a GitHub release, which
  runs `promote.yml`. That is the only thing that moves `brew install discobox`
  and winget, and `release:require-dot` refuses to promote anything that is not
  a dot release. It triggers on `released`, `edited`, and `published` and gates
  on `release.prerelease == false`, because which activity type a checkbox-only
  edit emits is not something GitHub documents plainly. There is no un-promote —
  both stable channels are recomputed from the newest blessed release, so
  backing one out means blessing a different one.

The two package channels are fed in opposite directions, and the difference is
ownership.

The Homebrew tap is ours, so it **pulls**: `discobox-ai/homebrew-tap` generates
both formulae from this repository's public releases with
`scripts/brew-formula.sh` — `discobox` from the newest blessed release,
`discobox-dev` (`--dev`) from the newest dot release — needing no credential of
ours to read them and its own `GITHUB_TOKEN` to commit. They are two formulae
rather than one because Homebrew will not link two that install the same file,
and holding both at once is the point; `discobox-dev` installs its command under
that name, and the CLI reads which one it is off `argv[0]`. `brew:refresh` is
one dispatch that recomputes both, and is the only thing that starts the tap —
it polled on a cron once, and that was removed deliberately: a backstop that
covers for a broken release step is a backstop that stops anyone noticing the
step is broken. So a release that cannot reach the tap fails rather than quietly
leaving `brew install` a version behind. `brew:publish -- [--dev] TAG` remains
the by-hand override for a tag the tap's own rule will not take.

winget cannot be inverted, because `microsoft/winget-pkgs` is not ours and
nothing there can pull from us. So `winget:publish` **pushes**: it opens a pull
request from a fork, which is not something `GITHUB_TOKEN` can do, and ends
there — a validation pipeline and a moderator decide the rest. It is by hand
only at the moment: promotion's `winget` job is removed while the first
submission waits on a CLA signature and a moderator, since resubmitting per
release re-queues the package rather than queueing the versions. Both cross-repo
steps therefore hold a token, and both are scoped to the least each needs: the
tap's may only start a workflow already defined there, and winget's may only
write public repositories as the submitting account.

Release assets are also mirrored at `assets.discobox.ai`, a Cloudflare Worker
caching them in R2 on first request — a short, CORS-enabled, range-capable URL
for programmatic downloads. The Worker is generic
(`discobox-ai/release-asset-mirror`); the deployment, its scheduled smoke
tests, and the secrets it needs belong to `discobox-ai/infra`. It fills from
published releases and is never something a release pushes to, so it cannot
break one. No consumer here names it — the server manifest, both formulae and
the winget manifest all resolve `github.com` directly — and ADR 0106 holds the
rule for when that may change.

Its aliases read the same prerelease bit the channels above do, which makes
`/{namespace}/latest/` the newest blessed release and `/{namespace}/prerelease/`
the newest unblessed one. Neither is a channel: `prerelease` is not
`discobox-dev`, which follows the newest dot release whether or not it has been
blessed.

Windows is the one platform whose asset is an archive rather than a bare binary.
winget resolves a portable package's command from the file name whenever it
cannot create a symlink, so `release:windows-zip` repackages the same binary as
`discobox.exe` and uploads it alongside the rest.

Container images form one chain, so that a pool host pulling all of them pulls
the shared surface once (ADR 0068):

```mermaid
flowchart TD
    base["base-image/<br/>discobox-base<br/>Debian, Docker, systemd unit mask"]
    pool["pool-agent/<br/>discobox-pool-agent"]
    sandbox["sandbox-agent/<br/>discobox-sandbox-agent"]
    harness["harness/&lt;type&gt;/<br/>discobox-harness-*"]

    base -->|BASE_IMAGE| pool
    base -->|BASE_IMAGE| sandbox
    sandbox -->|SANDBOX_AGENT_IMAGE| harness
```

`discobox-base` is released like the rest even though nothing runs it: its
children must resolve one already-built reference for their layers to be the
same blobs. `release:images` builds them in that order, and the development
image watcher orders its own builds from the same parent links. After any
partial rebuild, the watcher derives both the complete development manifest and
every image setting in `.env` from one inspection of the full local image set;
the two publications must never name different pool or sandbox images.
Dockerfile verification reuses the Taskfile build recipes with test-only tags,
so checking a Dockerfile cannot move the watcher-owned `:local` tags underneath
a running development server.

The pool VM guest — the kernel, initrd, and root filesystem a VM-backed pool
boots — is built from [`vm-image/`](vm-image/DESIGN.md) and released on its own
`vm/v*` line with its own workflow (ADR 0062 §3); the server pins its digest. It
is one image for every VM backend: `vz` boots its `linux/arm64` variant and
`libkrun` the `linux/amd64` one (ADR 0101). The libkrunfw-patched kernel libkrun
needs is the one artifact it cannot take from there, so it has a third line of
its own, `vm-kernel/v*`.
