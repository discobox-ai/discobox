# 26-11-23-522 — A frozen box is `paused` everywhere, with no deprecated alias

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
  - [0118](0118-whoever-serves-an-api-is-strict-and-whoever-reads-it-is-tolerant.md):
    compatibility across versions is not maintained, and a client that
    breaks is the signal to upgrade it;
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
  enums of a sandbox, a listing, a sandbox event and a pool's state report;
  and the pool's `/pool-agent/{poolId}/sandbox-suspend-completion` route,
  with its legacy `/api/pools/...` path
  (`server/internal/server/legacy_server_routes.go`).
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
  `suspendEpoch` fields, `suspended` in the runtime status, and the
  `X-Discobox-Sandbox-Not-Running` header's value
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

317 rejected the rename because it touches every layer. The mapping has
since spread to the pool agent (#1558), PR 1561 and 395. The owner,
2026-11-22: "we can put in an issue to normalize the state to paused
throughout discobox and vm." On this ADR's first draft, which kept
`suspended` as a deprecated alias for a release, the owner asked
(2026-11-24): "why does this need to be deprecated for a release? Can we
just get rid of it? just hard switch to paused everywhere?"

## Decision

**The action is `pause` and the state is `paused`, in the API, the stored
state, the provider contract, the pool API and the CLI, from one release
on, with no alias. The one exception is internal: the server accepts
`suspended` from a pool agent and records it as `paused`, so an old agent
does not stall the server mid-upgrade.**

### 1. The server API says `paused`, and nothing else

- `POST /projects/{p}/sandboxes/{s}/pause` (`pause-sandbox`,
  `PauseSandboxBody`) does what `/suspend` does today (420 §3). The
  `/suspend` route is removed.
- `runtimeState` and `displayState`, on a sandbox, in a listing and in a
  sandbox event, use `paused`. `suspended` is removed from those enums.
- The pool's completion route is `/pool-agent/{poolId}/sandbox-pause-completion`.
  The `suspend` path and its legacy `/api/pools/...` path are removed.
- The sandbox role still has no pause route (317 §5).

### 2. Clients older than the switch break, and are upgraded

An older CLI cannot list boxes against a new server while any box is paused:
its generated client (`cli/internal/cli/server_api_client.go`) validates
every enum, and `paused` is not in its copy. Its pause command gets a 404.
This is accepted. It is 0118's standing rule: compatibility across versions
is not maintained, and a breaking client is the signal to upgrade. The CLI
also replaces an older local server it starts, so the common pairing is
never mismatched.

A new CLI against an old server is the same case the other way round. It
gets no fallback route and no translation of `suspended`.

### 3. Stored state is migrated once

A post-`AutoMigrate` data migration (`server/internal/database`'s value
rewrites) does two things:

- it rewrites `sandboxes.runtime_state = 'suspended'` to `paused`;
- it rewrites `runtimeState` and `displayState` `suspended` to `paused` in
  `sandbox_events.data`.

It is guarded and idempotent, as every step there is, and it runs at
startup, before the server serves. Afterwards no stored value says
`suspended`, so no reader accepts it.

### 4. The provider contract and the pool agent say pause

- **Provider contract.** `sandbox.Runtime.Suspend` becomes `Pause`, and
  `SandboxRef.SuspendID`/`SuspendEpoch` become `PauseID`/`PauseEpoch`. Every
  implementation changes with it.
- **Pool API.** The pool agent serves `.../sandboxes/{s}/pause` with
  `pauseId`/`pauseEpoch`, and no `/suspend`. It reports `paused` on the state
  channel, in its runtime status and in the `X-Discobox-Sandbox-Not-Running`
  header, and calls `sandbox-pause-completion`.
- **No fallback to an old agent's route.** A pause sent to a pool whose
  agent is not yet upgraded gets a 404. That is
  `sandbox.ErrPoolAgentUnsupported`, which the person sees as "this pool
  cannot pause". The server holds readiness until its pools are upgraded
  (624), so this lasts as long as the upgrade, or for good on a pool that
  never upgrades.

### 5. The one exception: the server accepts `suspended` from a pool agent

After a server upgrade, the new server talks to each pool's old agent until
that pool is upgraded (624). An old agent with a frozen box reports
`suspended`. If the server's state-report schema refused it, the batch that
carries it would be refused, and every box in it would go unreported until
the pool upgraded.

So the server accepts `suspended` as well as `paused` where a pool agent
writes a state to it: the state channel's report, and an agent's runtime
status and not-running header. It records `paused` for either. This value is
inbound only. The server never writes `suspended`, no client is ever served
it, and nothing outside the pool wire documents it as a state. It goes once
no supported upgrade starts from a release whose agent writes `suspended`
(Open).

### 6. The pool agent's two kinds of pause get their own names

Both report `paused`. Neither keeps the name `paused` for itself, so the
code never has to ask which "paused" a line means:

- `StatusSuspended`, a freeze the pool made, becomes `StatusFrozen`. This
  matches 395's `Frozen` record (PR 1561).
- `StatusPaused`, a pause the provider made, becomes
  `StatusProviderPaused`.

The rule that tells them apart (395 §4, 619 §5) is unchanged. Names
persisted on a pool's disk (the completion receipt in
`suspend_completion.go`) are not renamed by this ADR (Open).

### 7. The CLI and console read `paused` directly

`sandboxDisplayState` and the console's state map drop the
`suspended`→`paused` translation. `-o json`, `-o yaml` and `discobox events`
print the API's record, which is now `paused`. That replaces 317 §4's
reason for printing `suspended`.

The hidden `admin box suspend` spelling is a CLI spelling, not an API alias.
It stays under 600 §1, and like `admin box pause` it now posts to `/pause`.

### 8. disco-vm

disco-vm already says pause. boxd's own call and status names stay as boxd
spells them. If a disco-vm doc or comment calls disco-vm's own state
"suspended", it is reworded in a disco-vm change. No discobox code waits on
that change.

### 9. Error messages and docs

Server and pool error text says "paused" (for example, a read refused
because the box is paused). `docs/public` and the `DESIGN.md`/`REVIEW.md`
files of each package touched say pause and paused. The release notes say
that the route and the state value changed, and that older CLIs must be
upgraded. The sandbox agent's `autostop/resumed` route keeps its name. Only
its description's "thawed from a suspend" is reworded.

## Alternatives rejected

- **Keep `suspended` underneath, and translate in the CLI (317's answer).**
  Rejected: the translation now lives in the CLI, the console, the pool
  agent's `stateFromStatus`, its errors and three ADRs. Every reader of
  `-o json` meets a word that no command uses.
- **Keep `suspended` and `/suspend` as deprecated aliases for one release,
  serving `suspended` to a client that does not send a header saying it
  reads `paused`, and falling back to an old agent's `/suspend` on a 404.**
  This was the first draft. Rejected by the owner: it adds a header, a
  response-boundary downgrade, two fallbacks, dual-value readers and a later
  removal change. All of that only keeps clients working that 0118 already
  says to upgrade.
- **Two releases: readers accept `paused` first, and the server flips a
  release later.** Rejected for the same reason, and it keeps `suspended`
  in the API for a whole release.
- **Hard switch on the pool wire too, with no inbound `suspended`.**
  Rejected: a frozen box on a pool that is not yet upgraded would make the
  server refuse that pool's state reports. The server would see none of
  that pool's boxes change until the pool upgraded, or ever, for a pool that
  never upgrades. Accepting one inbound value costs a few lines and is never
  visible outside the server.
- **Rewrite only `sandboxes.runtime_state`, and map events when they are
  read.** Rejected: the newest event for each sandbox is never pruned, so the
  mapping on read could never be removed.
- **Keep `StatusPaused` as the provider's pause, and report it as
  `paused`.** Rejected: the name would then mean one kind of pause in the
  pool and both kinds on the wire.

## Consequences

- `discobox ls`, `-o json`, `discobox events`, the API reference, the
  database and the pool's routes all say `paused`. `suspended` survives only
  as an inbound value the server accepts from an old pool agent.
- **Server API** (`server.yaml`): it gains `pause-sandbox` and
  `sandbox-pause-completion`, and loses `suspend-sandbox`,
  `sandbox-suspend-completion` and `suspended` in every served state. The
  generated packages and `api/model` are regenerated.
- **Pool API** (`pool.yaml`): `pool-suspend-sandbox` becomes
  `pool-pause-sandbox`.
- **An older CLI** fails against a new server while any box is paused, until
  it is upgraded. The release notes say so.
- **During an upgrade**, a pause on a pool whose agent is not yet upgraded
  is refused as unsupported. A box that pool froze before the upgrade is
  still reported, as `paused`.
- **Tests** cover:
  - a database holding `suspended` rows and events migrates to `paused`,
    and a box keeps its state;
  - a state report saying `suspended` is recorded as `paused`;
  - pause and resume through `/pause`, end to end.
- **ADR status lines.** 420 and 317 are marked superseded in part by this
  ADR. 395, if still unshipped then, is amended to the one word. Otherwise it
  is noted as renamed by this ADR.

## Open, for implementation

- When the inbound `suspended` (§5) is removed. That is when no supported
  upgrade starts from an agent that writes it, and it is its own change.
- Whether the pool agent's persisted completion receipts and their paths
  are renamed. A rename needs a read of the old name. Leaving them is
  invisible outside the pool's disk.
- The SQL for the `sandbox_events.data` rewrite on SQLite and Postgres.
  Go's JSON encoding is stable, so a string replace of
  `"runtimeState":"suspended"` and `"displayState":"suspended"` is likely
  enough. A decode and re-encode is the fallback.
