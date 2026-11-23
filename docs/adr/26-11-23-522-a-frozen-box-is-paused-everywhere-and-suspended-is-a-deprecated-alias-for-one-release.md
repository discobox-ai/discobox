# 26-11-23-522 — A frozen box is `paused` everywhere, and `suspended` is a deprecated alias for one release

- **Status**: Proposed
- **Date**: 2026-11-23
- **Issue**: #1555
- **Supersedes in part**:
  - [26-10-16-420](26-10-16-420-a-docker-pool-freezes-a-sleeping-sandbox-and-a-person-can-suspend-one.md)
    §1's state name `suspended` and §3's route, provider method and pool
    route names. What a freeze does, and when, is unchanged.
  - [26-10-20-317](26-10-20-317-a-person-pauses-a-discobox-from-the-root-cli-and-the-console.md)'s
    "the API, the pool, the provider contract and the stored state keep
    `suspend` / `suspended`, and the CLI translates", §1's route, and §4's
    rule that `-o json`, `-o yaml` and `discobox events` print `suspended`.
    317's commands, console action and sandbox-role rule (§§1–3, 5) stand.
- **Renames in**:
  [26-11-21-395](26-11-21-395-a-machine-pool-freezes-a-sleeping-sandbox-where-its-driver-can-and-stops-it-where-it-cannot.md),
  where `suspended`, `SuspendSandbox` and `StatusSuspended` name the same
  state and calls. Its decisions are unchanged.
- **Relates to**:
  - [0118](0118-whoever-serves-an-api-is-strict-and-whoever-reads-it-is-tolerant.md),
    still deferred: every generated reader rejects an enum value it does not
    know, which is why §3 is needed;
  - [26-10-23-600](26-10-23-600-the-cli-is-spelled-one-way-run-takes-its-source-and-only-commits-go-in.md)
    §1, a shipped CLI spelling kept as an untaught alias;
  - [26-11-14-624](26-11-14-624-after-an-upgrade-the-server-is-not-ready-until-its-pools-are.md),
    the upgrade window in which a new server talks to old pool agents;
  - disco-vm ADR 26-11-21-438 (vm#201), whose `machine.StatePaused` and
    `Machine.Pause` already use the word.

## Context

One state has two names. A person freezes a box with `discobox pause`, and
`discobox ls` lists it as `paused` (317). disco-vm calls it a pause
(`machine.StatePaused`, `Machine.Pause`). Underneath, discobox still says
`suspend`:

- **Server API** (`api/openapi/server.yaml`): the route `POST
  /projects/{p}/sandboxes/{s}/suspend` (`suspend-sandbox`,
  `SuspendSandboxBody`); `suspended` in the `runtimeState` and `displayState`
  enums of a sandbox, a listing and a sandbox event; and the pool's
  `/pool-agent/{poolId}/sandbox-suspend-completion` route, with its legacy
  `/api/pools/...` alias (`server/internal/server/legacy_server_routes.go`).
- **Stored state**: `sandboxes.runtime_state = 'suspended'`
  (`model.SandboxRuntimeStateSuspended`, `server/internal/model/model.go`),
  and the `runtimeState`/`displayState` held in `sandbox_events.data`. The
  newest event for each sandbox is never pruned
  (`PruneSandboxEvents`, `server/internal/store/sandbox_events.go`), so an old
  value can outlive any retention.
- **Provider contract**: `sandbox.Runtime.Suspend` and
  `SandboxRef.SuspendID`/`SuspendEpoch` (`server/internal/sandbox/runtime.go`),
  implemented by `poolruntime.Provider` and its pool-agent client.
- **Pool API** (`pool-agent/api/openapi/pool.yaml`): the route
  `.../sandboxes/{s}/suspend` (`pool-suspend-sandbox`), its `suspendId` and
  `suspendEpoch` fields, `suspended` in the state report and the runtime
  status, and the `X-Discobox-Sandbox-Not-Running` header's value
  (`pool-agent/api/model/errors.go`).
- **Pool agent internals** (`pool-agent/sandboxruntime/runtime.go`): two
  statuses that both report `suspended`. `StatusSuspended` is a freeze the
  pool made. `StatusPaused` is a pause the provider made (Archil at its
  session limit), added by #1558. The pool must keep telling them apart
  (619 §5, 395 §4). The status is derived at run time and never persisted.
- **CLI and console**: `sandboxDisplayState` (`cli/internal/cli/output.go`)
  and `tui.StatePaused` (`cli/internal/tui/data.go`) map `suspended` to
  `paused` for a person, and `-o json` and `discobox events` print
  `suspended`.

disco-vm needs no rename. The only `suspend` left in it is boxd's own API
(`SuspendVm`, `SetAutoSuspendTimeout`, the `suspended` status in
`pkg/machine/boxd/boxd.go`). Those are boxd's names for boxd's calls.

317 rejected the rename because it touches every layer and needs a
migration and a period in which both words are accepted. That period is now
wanted. The mapping has spread to the pool agent (#1558), PR 1561 and 395.
The owner, 2026-11-22: "we can put in an issue to normalize the state to
paused throughout discobox and vm."

Two readers make the rename more than a find-and-replace:

- **An older CLI.** The CLI decodes responses with the generated client
  (`cli/internal/cli/server_api_client.go`), which validates every enum. An
  older CLI that reads `runtimeState: paused` fails to decode the whole
  sandbox, and so the whole listing, as soon as any box in it is paused. A
  deprecated `/suspend` route alone does not help it.
- **An older pool agent.** After a server upgrade, the server talks to each
  pool's old agent until the pool is upgraded (624). That agent serves only
  `/suspend` and reports only `suspended`. A pool that never upgrades stays
  that way.

## Decision

**The action is `pause` and the state is `paused`, in the API, the stored
state, the provider contract, the pool API and the CLI. `suspended` and the
`suspend` routes stay for one release as deprecated aliases. A server serves
`suspended` to a client that has not said it reads `paused`. It falls back to
a pool agent's `/suspend` when `/pause` is not found.**

### 1. The server API says `paused`

- `POST /projects/{p}/sandboxes/{s}/pause` (`pause-sandbox`,
  `PauseSandboxBody`) does what `/suspend` does today (420 §3).
- `runtimeState` and `displayState`, on a sandbox, in a listing and in a
  sandbox event, use `paused`.
- The pool's completion route is `/pool-agent/{poolId}/sandbox-pause-completion`.
- The sandbox role still has no pause route (317 §5).

### 2. Deprecated aliases, for one release

Each alias is marked `deprecated: true` in its OpenAPI document, and its
description names the release that removes it:

- `POST .../sandboxes/{s}/suspend`, the same handler as `/pause`.
- `suspended` in each state enum, served only as §3 says.
- `/pool-agent/{poolId}/sandbox-suspend-completion` and its legacy
  `/api/pools/...` path, for an old pool agent.
- The pool API's `/suspend` route and `suspendId`/`suspendEpoch` fields, kept
  in `pool.yaml` so the server's generated client can still call an old pool
  agent (§5). A new pool agent serves both routes.

The aliases ship in the first release that has this change. They are removed
in the next minor release after it. Implementation names both releases in
the deprecation notes and in `docs/public`.

The CLI's hidden `admin box suspend` spelling is not one of these aliases.
It is a CLI spelling under 600 §1 and stays.

### 3. A client says it reads `paused`, or it is served `suspended`

The CLI and the console send a request header saying they read `paused`.
For a request without it, the server writes `suspended` wherever it would
write `paused`: `runtimeState` and `displayState` in every response, and in
the event stream that request opened. This is one function at the response
boundary. Nothing below it knows about old clients.

So a CLI from before this change still lists, pauses (through `/suspend`)
and resumes boxes against a new server. When the aliases are removed, the
server ignores the header and serves `paused` to everyone.

A new CLI against an old server reads `suspended` as `paused` and falls back
from `/pause` to `/suspend` on a 404. Both go away with the aliases.

The header's name and value are left to implementation (Open).

### 4. Stored state is migrated once, and readers accept both until it has run

A post-`AutoMigrate` data migration (`server/internal/database`'s value
rewrites) does two things:

- it rewrites `sandboxes.runtime_state = 'suspended'` to `paused`;
- it rewrites `runtimeState` and `displayState` `suspended` to `paused` in
  `sandbox_events.data`.

It is guarded and idempotent, as every step there is. It runs at startup,
before the server serves.

A state report from an old pool agent saying `suspended` is stored as
`paused` (§5). Readers of the stored state (`model.SandboxRuntimeStates`,
`DisplayState`, the inventory's and transfer's checks) accept both values
until the aliases are removed. Nothing writes `suspended` after the
migration has run, so the second value only covers a row read mid-upgrade.

### 5. The provider contract and the pool agent say pause, and the server falls back to an old agent

- **Provider contract.** `sandbox.Runtime.Suspend` becomes `Pause`, and
  `SandboxRef.SuspendID`/`SuspendEpoch` become `PauseID`/`PauseEpoch`. Every
  implementation changes with it, and no alias is kept. The contract is
  internal to the server binary.
- **Pool API.** A new pool agent serves `.../sandboxes/{s}/pause` with
  `pauseId`/`pauseEpoch`. It reports `paused` on the state channel, in its
  runtime status and in the `X-Discobox-Sandbox-Not-Running` header. It calls
  `sandbox-pause-completion`.
- **Fallback.** The server's pool-agent client posts to `/pause`. A 404 means
  an agent from before this change, so it posts the same request to
  `/suspend` with `suspendId`/`suspendEpoch`. This is the 404 that
  `Suspend` already reads as `sandbox.ErrPoolAgentUnsupported`. The fallback
  is tried only on a 404. Any other refusal (`sandbox-not-running`, a pool
  that cannot freeze) is that agent's answer, and is returned as it is. If
  the old route also answers 404, the agent predates freezing, and that is
  `ErrPoolAgentUnsupported`, as today.
- **What the server accepts from a pool.** It accepts `suspended` and
  `paused` on the state channel, in a pool agent's runtime status and in
  the not-running header, and records `paused` for either. A new pool agent
  never talks to an older server, because the server ships the agent (420
  Consequences), so the reverse needs nothing.

### 6. The pool agent's two kinds of pause get their own names

Both report `paused`. Neither keeps the name `paused` for itself, so the
code never has to ask which "paused" a line means:

- `StatusSuspended`, a freeze the pool made, becomes `StatusFrozen`. This
  matches 395's `Frozen` record (PR 1561).
- `StatusPaused`, a pause the provider made, becomes
  `StatusProviderPaused`.

The rule that tells them apart (395 §4, 619 §5) is unchanged. Names that are
persisted on a pool's disk (the completion receipt in
`suspend_completion.go`) are not renamed by this ADR (Open).

### 7. The CLI and console read `paused` directly

`sandboxDisplayState` and the console's state map drop the
`suspended`→`paused` translation. They keep reading `suspended` as `paused`
only until the aliases are removed (§3). `-o json`, `-o yaml` and `discobox
events` print the API's record, which is now `paused`. This replaces 317
§4's reason for printing `suspended`.

### 8. disco-vm

disco-vm already says pause. boxd's own call and status names stay as boxd
spells them. If a disco-vm doc or comment calls disco-vm's own state
"suspended", it is reworded in a disco-vm change. No discobox code waits on
that change.

### 9. Error messages and docs

Server and pool error text says "paused" (for example, a read refused
because the box is paused). `docs/public` and the `DESIGN.md`/`REVIEW.md`
files of each package touched say pause and paused. They mention
`suspended` only to document the deprecated aliases, with the release that
removes them. The sandbox agent's `autostop/resumed` route keeps its name.
Only its description's "thawed from a suspend" is reworded.

## Alternatives rejected

- **Keep `suspended` underneath, and translate in the CLI (317's answer).**
  Rejected: the translation now lives in the CLI, the console, the pool
  agent's `stateFromStatus`, its errors and three ADRs. Every reader of
  `-o json` meets a word that no command uses.
- **Serve `paused` to every client from the first release.** Rejected: every
  older CLI would fail to decode any listing that holds a paused box (§3's
  Context), not just its pause command. 0118's tolerant readers would not
  help: they skip unknown fields, not unknown enum values, and they are not
  built yet.
- **Two releases: readers accept `paused` first, and the server flips a
  release later.** No header is needed. Rejected: the API keeps saying
  `suspended` for a whole release, which is what the issue asks to end. A
  CLI from before the first release breaks at the flip anyway, just as it
  breaks when §3's aliases go.
- **Tell old clients apart by `User-Agent` or a version string.** Rejected:
  ogen's client sets no version, and a version comparison is a rule about
  releases. A header that says what the client reads is a rule about the
  client.
- **Rewrite only `sandboxes.runtime_state`, and map events when they are
  read.** Rejected: the newest event for each sandbox is never pruned, so the
  read-side mapping could never be removed.
- **Ask an old pool agent which routes it has, rather than falling back on a
  404.** Rejected: the 404 is already how the client tells an old agent
  apart (`ErrPoolAgentUnsupported`), and a capability report would add a
  field for one release.
- **Keep `StatusPaused` as the provider's pause, and report it as
  `paused`.** Rejected: the name would then mean one kind of pause in the
  pool and both kinds on the wire.

## Consequences

- `discobox ls`, `-o json`, `discobox events`, the API reference, the
  database and the pool wire all say `paused`. `suspended` appears only on
  the deprecated aliases, and only until the next minor release.
- **Server API** (`server.yaml`): it gains `pause-sandbox` and
  `sandbox-pause-completion`. `suspend-sandbox`, `suspended` and
  `sandbox-suspend-completion` are deprecated. The generated packages and
  `api/model` are regenerated.
- **Pool API** (`pool.yaml`): it gains `pool-pause-sandbox`.
  `pool-suspend-sandbox` and `suspended` are deprecated.
- **Server.** It gains the response-boundary downgrade (§3), the data
  migration (§4), the dual-value acceptance from pools (§5), and the pool
  client's fallback (§5).
- **Tests** cover each mixed pairing:
  - a request without the header pauses through `/suspend` and reads
    `suspended`;
  - a server with an old pool agent pauses and resumes through the
    fallback;
  - a database holding `suspended` rows and events migrates to `paused`,
    and a box keeps its state.
- **Removal.** In the next minor release, the aliases, the header handling,
  the fallback and the dual-value readers are removed in one change.
- **ADR status lines.** 420 and 317 are marked superseded in part by this
  ADR. 395, if still unshipped then, is amended to the one word. Otherwise it
  is noted as renamed by this ADR.

## Open, for implementation

- The header's name and value (§3), and whether the console's own requests
  share the CLI's client and so send it.
- The exact releases in the deprecation notes (§2).
- Whether the pool agent's persisted completion receipts and their paths
  are renamed. A rename needs a read of the old name for one release.
  Leaving them is invisible outside the pool's disk.
- The SQL for the `sandbox_events.data` rewrite on SQLite and Postgres.
  Go's JSON encoding is stable, so a string replace of
  `"runtimeState":"suspended"` and `"displayState":"suspended"` is likely
  enough. A decode and re-encode is the fallback.
