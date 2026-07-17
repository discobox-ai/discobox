# 0002 — Harness config is the only harness concept

- **Status**: Proposed
- **Date**: 2026-07-16

## Context

Harnesses were modelled as two things: a built-in `HarnessDefinition` (a
hardcoded template: id, name, image, configure spec) and a project-scoped
`HarnessConfig` created from it. A config recorded `DefinitionID` and snapshotted
the image plus its `io.discobox.harness.v1` label metadata at create time.

Three problems converged:

1. **The split bought nothing.** A definition was only ever a template for making
   a config. Users had to `enable` a definition to get a config, and the two
   drifted: a config's snapshotted image went stale the moment the definition's
   image changed.
2. **Dev rebuilds didn't reach the server.** Harness images were built with a
   stable `:local` tag, so a rebuild never changed any recorded value and the
   server had no reason to re-read anything. The worker-agent already solved this
   by writing a hashed tag to `.env` and letting the server restart.
3. **Configure was entirely client-side.** The CLI created a `harnessMode=config`
   sandbox, attached its primary terminal, checked the exit code, `cat`-ed
   `/run/discobox/harness-configure.json`, applied the secrets and files, and
   deleted the sandbox. Every one of those steps was a client that could crash,
   be `Ctrl-C`-ed, or simply be a different client. There was also no notion of a
   harness being usable or not — an unconfigured harness could be run.

## Decision

Collapse the two into one: **a harness config is the only harness concept.** The
three included harnesses are seeded as built-in configs (`BuiltIn`), not exposed
as definitions. `HarnessDefinition`, `ConfigureSandbox`, `definitionId`, and the
`/harness-definitions` endpoints are removed.

- **`Configured` is the enable flag.** Only configured harnesses can be selected
  to run. Built-ins seed unconfigured.
- **Built-ins clobber their image.** Seeding re-points a built-in at the resolved
  image and re-snapshots its label whenever it changes, which is what carries a
  dev rebuild through. The label is still inspected exactly once per image.
- **Applying configure moves server-side, driven by the client.** `POST
  .../configure` returns the sandbox. `POST .../configure/attach` seeds the
  previous configuration into it; the client then attaches to the virtual
  `"primary"` exec, which is what launches the configure command. `POST
  .../configure/commit` verifies the command exited 0, reads the output file,
  applies it, and marks the harness configured; a non-zero exit records
  `configureError`. The sandbox is deleted either way.
- **Every sandbox-agent call runs inside a user request, with that user's
  credentials.** The control plane never contacts a sandbox on its own authority.
- **Exec gains a one-shot form**: `POST` on the exec attach route takes the
  request body as stdin and returns the exec's output, leaving `GET` as the
  websocket attach. Seeding and reading are one-shot, so they need no framing.
- **`POST .../deconfigure`** removes what configure created and clears the flag.
- **Config mode defers its primary terminal** to first attach, so the seed always
  lands before the configure command runs.

## Alternatives rejected

**Keep the definition/config split and reconcile stale configs against
definitions.** The smallest change: leave both concepts and add a startup pass
that re-points definition-backed configs at their definition's current image.
Rejected because it preserves a split whose only job is to be copied, and because
it cannot tell "this config tracks its definition" from "the user pinned a custom
image": `DefinitionID` is set either way, so the reconcile would silently clobber
a deliberate override.

**Make the config a thin pointer with `image: ""` resolved live from the
definition.** Attractive — no reconcile, no drift, and an explicit image would
naturally opt out. Rejected because the image is not the only thing snapshotted:
run/relaunch argv, files, and secret declarations all come from the same label.
Resolving the image live while freezing its metadata is incoherent, so this would
have forced label inspection into every sandbox launch and API read. Snapshot +
clobber-for-built-ins keeps inspection at write time and out of the hot path.

**Friendly built-in IDs matching the slug (`codex`).** Deferred, not taken.
`HarnessConfig.ID` is a global primary key while configs are project-scoped, so a
bare slug collides the moment built-ins are seeded into a second project. It also
breaks the prefixed-Crockford scheme in `id/id.go`. Built-ins keep generated IDs
and are selected by slug. **Revisit if** harness configs stop being project-scoped
or built-ins become global rows.

**The server polls the sandbox agent to watch the configure terminal.** Appealing
because it is self-healing: a reconciler re-derives state every tick, so no event
can be missed and no client can fail to report. Rejected because it forces the
control plane to originate requests to a sandbox with no user principal behind
them, which requires a lease acquisition that skips scope authorization — a
permanent hole kept shut only by convention. It also cuts against the established
direction, where agents report *to* the control plane
(`/api/workers/{workerId}/sandbox-removed`), not the reverse.

**The sandbox agent reports the configure result to the control plane.** Fits
that existing direction and needs no agent client at all. Rejected because it
pushes the configure contract (output path, result shape) into the agent, and
still needs a server-side backstop for an agent that never reports — so it trades
the polling loop for a distributed contract without removing the failure mode.

**A background goroutine to watch the configure terminal.** A direct port of the
CLI logic, but a server restart mid-configure would orphan the sandbox and lose
the result.

Client-commit wins because the only actor that must be present for configure to
mean anything — the human driving the terminal — is also the one holding
credentials for the sandbox. Making the client say *when* (but never *what*: the
exit status is read from the sandbox, not taken on trust) keeps the authorization
model intact and deletes the polling entirely. Its one weakness, a client that
never commits, is covered by the TTL janitor and by `/configure` clobbering any
in-flight attempt, so an abandoned run cannot wedge a harness.

**Deconfigure by unbinding only, leaving secrets.** Avoids tracking but orphans
every secret configure ever created. Deleting all secrets bound to the config was
also rejected: it would delete a secret the user shares with another harness.
Tracking `ConfiguredSecretIDs` removes exactly what was created.

**Seeding the previous configuration without secret values, or with all of
them.** Re-running configure hands the command its own prior output at
`ConfigurePreviousConfigPath`, in the identical shape it writes, so it can parse
it with the same code. Names-only was rejected: a configure command typically
*validates* the existing credential and skips re-auth, which needs the value.
Unconditional values were also rejected — that would hand plaintext to a sandbox
without consulting `SecretGrant`, the gate this system deliberately made explicit
when AutoApprove was removed. The seed therefore includes only values holding a
live `harnessConfig`-scoped grant; a revoked or expired grant simply drops out and
the command re-prompts. Correspondingly, applying configure output *mints* that
grant: the user typing a credential into a harness's configure flow is the consent
a grant records. (Bindings alone would not do — a binding is not a grant, so
without minting one the flow's own secrets would never be usable at run time.)

**A per-image `resultPath` for the configure output.** The label carried one, and
it is deleted. Nothing ever read it: the configure commands hardcode the path
themselves, so the field's only real capability was to disagree with them. The
input and output paths are fixed points of the image contract
(`harness.ConfigureOutputPath`, `harness.ConfigurePreviousConfigPath`).

**Framing the seed and read over the websocket attach, or through exec `env`.**
Both the seed (`cat > path`) and the read (`cat path`) are one-shot: bytes in,
bytes out. Driving them over the streaming attach would mean a websocket client in
the control plane speaking the frame protocol — and since `frame` lives in the
sandbox-agent module, either a third copy of a wire protocol (the CLI already
duplicates it) or moving that package. Passing the payload through the exec's
`env` avoids all that but leaves secret material in `/proc/<pid>/environ`, and
gives no path for reading output back.

Instead, `POST` on the exec attach route is the **one-shot form**: the request
body is stdin, the response body is the exec's output, and the exit status is read
from the exec record (the body cannot carry it — headers commit before the process
exits). `GET` remains the websocket attach. The frame protocol stays entirely
inside the agent, where a streaming attach is otherwise just a raw byte pipe
between client and shim. This also gives the eventual files API the primitive it
needs.

## Consequences

- `HarnessConfig` gains `BuiltIn`, `Configured`, `ConfiguredFiles`,
  `ConfiguredSecretIDs`, `ConfigureSandboxID`, `ConfigureError`; loses
  `DefinitionID`. The DB is disposable, so no migration.
- `harnessdefs` becomes a seed source rather than a definitions API.
- The server now originates sandbox-agent calls (a first), but only from within a
  user request via the existing `AcquireSandboxHTTPClient`, which authorizes the
  caller's scopes. No new authority is introduced.
- The exec one-shot form is new public surface, reusable beyond configure — it is
  the primitive a files API would otherwise have to invent.
- The reconciler exists only as a janitor: it reaps configure sandboxes that were
  never committed, and touches no agent.
- Configure is a three-call flow, so the client must drive it in order:
  `configure` → `configure/attach` → attach to `"primary"` → `configure/commit`.
- The CLI/TUI still model definitions and must be reworked onto this flow.
  `waitForPrimaryTerminal` no longer applies to configure: with the launch
  deferred, no primary exists until attach, so the client waits for the sandbox to
  be running and attaches to the virtual id instead.
