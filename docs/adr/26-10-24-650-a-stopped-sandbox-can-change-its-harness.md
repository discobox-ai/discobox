# 26-10-24-650 — A stopped sandbox can change its harness

- **Status**: Accepted (narrows [0021](0021-upgrade-is-a-re-pin-and-preserves-power-state.md)
  §§3–4 for a change of harness config: replacement requires a stopped sandbox
  and cannot inherit a concurrent start; clarifies
  [0038](0038-terminal-identity-is-the-exec-id-terminals-revive-in-place.md)
  Decision items 1, 4–6: terminal continuity across a harness change does not
  promise conversation continuity, and creation metadata is historical;
  narrows [26-10-11-657](26-10-11-657-a-stopped-discobox-is-read-from-what-it-last-posted.md)
  §4 for the new `currentHarnessId`: selection is composed from the sandbox's
  current config even when the terminal observation is read from inventory)
- **Date**: 2026-10-24
- **Relates to**: [0002](0002-harness-config-is-the-only-harness-concept.md),
  [0032](0032-every-sandbox-has-a-harness-config-and-shell-is-the-built-in.md),
  [26-10-24-254](26-10-24-254-a-discobox-is-one-positional-and-flags-take-one-type-each.md)
  §§1–2,
  [26-10-08-127](26-10-08-127-a-sandboxs-bootstrap-is-static-and-the-intake-carries-the-rest.md)
  §1, [26-10-16-423](26-10-16-423-a-harness-is-a-provisioning-bundle-and-the-agent-provisions-any-box.md)
  §§3–7, [PR #357](https://github.com/discobox-ai/discobox/pull/357).

## Context

A sandbox selects a harness config at creation. Its workspace can outlive the
choice of agent: a user may want Codex to continue work started with Claude,
without exporting the sandbox or creating another workspace.

PR #357 makes a revived terminal resolve the sandbox's current harness rather
than the harness ID in its durable record. That fixes transfers between servers,
whose config IDs differ, and permits terminal identity to survive a later harness
change. It neither provides a way to change the sandbox's harness nor makes its
historical terminal metadata describe the current harness.

A harness is more than a launch command. It brings a provisioning bundle,
manifest defaults, configured files and secret bindings. Changing only the
command or config ID could run a harness that was never provisioned. Conversely,
rebuilding a running sandbox would interrupt work the user has not stopped.

## Decision

### 1. Changing harness is an explicit sandbox configuration update

The existing update command gains `--harness HARNESS`:
`discobox admin box update DISCOBOX --harness HARNESS`. This adds no root
`update` command or separate `switch` verb. `DISCOBOX` uses the shared sandbox
selector: a listed name, ID, short ID or address, with the selected sandbox's
server and project determining where the harness is resolved.

As required by 26-10-24-254 §§1–2, `HARNESS` is a slug, name or ID, resolved
server-side by the same selector as `run -H` and import, with harness completion.
The sandbox update API accepts `config.harness` for that selector and
`config.harnessConfigId` for an explicit ID, following create's existing shape;
the two are mutually exclusive. The CLI sends the selector rather than treating
a name as an ID or resolving against a different server's project.

The target is a usable harness config in the sandbox's project,
including the ordinary `shell` config. An omitted field leaves the selection
unchanged; clearing it does not create a harnessless sandbox.
An explicitly empty selector is refused. Omitting `--harness` does not select the
project default, and there is no `--clear-harness`: selecting `shell` is the
ordinary way to request a shell. The stopped precondition applies to a harness
change, not to unrelated name or egress updates. A refusal tells the caller to
run `discobox stop DISCOBOX`; the update does not stop or start it implicitly.

The target must be compatible with the sandbox's platform and pool's image kind.
Switching does not move the sandbox to another pool or change its platform.
An update selecting the existing config is a no-op for harness selection;
upgrading that config's pins remains the upgrade operation.

### 2. Only a stopped sandbox may change harness

A switch requires an existing, settled sandbox that is stopped. Running,
suspended, archived, deleting, and not-yet-created sandboxes are refused with a
conflict explaining what must change first. A suspended sandbox must be stopped;
freezing its processes is not ending them. An unreachable pool is not evidence
that a sandbox is stopped.

The pool's runtime observation is authoritative. Checking only the server's last
reported status is insufficient. The stopped precondition and application of the
new spec must be serialized against every start path, including start-on-use,
restart, thaw and scheduled wake. A start that wins first makes the switch
ineligible; a switch that wins first prevents an old-spec start until the new
spec has been applied. A later start uses the new spec.

This exclusion must survive retries and process restarts. A refused switch must
not leave accepted new harness intent that can later replace a running sandbox.
It is not a durable desired power state: switching never means "keep stopped"
after the change has settled.

### 3. The whole harness configuration changes through reconciliation

Accepted intent records the selected config and its resolved bundle or template
pins and manifest digest together, with the sandbox generation and reconcile
dirty mark in the same transaction. A different config is a spec change even
when its bundle digest is identical: its commands, configured files or secret
bindings may differ.

The ordinary spec reconciliation replaces the runtime configuration and applies
the target's provisioning contract. The bootstrap remains static for each
runtime instance (26-10-08-127 §1); switching does not add a second channel for
mutating a running agent's harness. Target configured files and secret delivery
are resolved from the new selection under the existing authorization rules.

Switching does not launch the sandbox or its harness. Provisioning that requires
boot runs on the next start, and readiness waits for the target's required steps
before launching its harness. A successful configuration update means intent was
accepted, not that boot-time provisioning has completed. Provisioning failures
are reported through the existing readiness and failure paths and remain
recoverable through the ordinary reconciliation and repair mechanisms.

### 4. Preserve sandbox data; promise no conversation transfer

The sandbox retains its identity, workspace and durable data under the existing
spec-replacement rules. Switching does not erase old harness data or migrate it
into the new harness's format. It does not promise to preserve disposable runtime
filesystem changes or uninstall everything the old harness installed.

Claude to Codex carries no promise of conversation continuity. The user should
expect to explain the task again, ask the new agent to learn from local changes,
or direct it to old data. What a particular harness can recover from existing
files, including when switching back, remains harness-defined. Provisioning the
selected harness is required regardless of whether it can recover a conversation.

Existing terminal IDs, transcripts and immutable creation metadata remain.
Revival uses the sandbox's selected harness, without replaying the original
initial prompt as a migration mechanism. Terminal and session reporting must
distinguish the current harness from the historical harness ID recorded at
creation; clients must not label a Codex run as Claude because of that record.
The new `currentHarnessId` describes the selected harness for the current or next
revival. The control plane composes it with cached terminal and session
observations too, so a stopped sandbox shows its new selection before it next
boots. Historical `harnessId`, transcripts, unknown fields, and `observedAt`
remain as posted; this derives selection without rewriting stored inventory.

## Alternatives rejected

- **Switch a running sandbox and restart it.** Ordinary upgrades may do this,
  but changing agents must not terminate live work. The caller stops it first.
- **Check stopped status once in the server.** Status can lag, and pool-local
  start-on-use can race the check. The runtime must enforce the exclusion.
- **Only change the launch command.** It bypasses provisioning, configured files
  and secret selection, leaving the new agent incompletely installed.
- **Translate or reset all conversation state.** Harness state has no common
  format. Translation is not needed to provision another agent; deletion would
  destroy data the user may want the new agent to inspect.
- **Create a new sandbox or terminal identity for each harness.** It makes
  changing tools require another workspace or breaks stable terminal handles.

## Consequences

- A switch is a disruptive spec change with an explicit stopped precondition,
  rather than an implicit restart or a live command substitution.
- The pool must coordinate switching with all ways a sandbox can start. A
  server-only update field cannot provide the required behavior.
- Selecting another config on the same bundle still changes effective config.
- Retained transcripts may span different harnesses. They are history, not a
  portable agent conversation, and the UI must describe the current harness
  independently of immutable creation records.
- Conversation recovery remains unspecified until a concrete harness contract
  supplies it; this does not defer installation or readiness of the new harness.
