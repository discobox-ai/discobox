# 0147 — opencode publishes its lifecycle through an image-owned plugin

- **Status**: Accepted
- **Date**: 2026-09-23
- **Relates to**: [ADR 0146](0146-a-hook-keeps-the-name-its-harness-used-and-gains-a-canonical-one.md),
  the canonical name a hook gains; [ADR 0137](0137-a-terminal-can-be-read-typed-into-and-waited-on-without-attaching.md),
  the wait that matches hook events; [ADR 0127](0127-the-opencode-harness-runs-opencode-1.md),
  the opencode harness and its pin.

## Context

opencode publishes no hooks, and since [ADR 0137](0137-a-terminal-can-be-read-typed-into-and-waited-on-without-attaching.md)
gave a terminal wait a hook matcher that has cost a behaviour rather than a
log: only `--quiet` and `--exit` can end a wait on an opencode terminal, so a
lead agent driving one cannot tell when a turn ended without polling the screen
and guessing. [ADR 0146](0146-a-hook-keeps-the-name-its-harness-used-and-gains-a-canonical-one.md)
raised the cost again by making a hook event a name a caller matches on across
harnesses — opencode is the harness that name cannot reach.

The reason it publishes none is structural, and was the right reading of the
CLI: Claude Code and Codex both accept `{"type": "command", "command": "…"}` in
a settings file and run it, and opencode has no such mechanism at any
configuration layer. Its lifecycle reaches JavaScript and nothing else.

What it does have, verified against the pinned 1.18.31 (ADR 0127):

- **A managed configuration layer.** `/etc/opencode/opencode.json` and
  `opencode.jsonc` are read on Linux, **last** of all layers — after the user's
  global config, after a project's, after a remote org config — so they
  outrank every one of them. The only keys a managed config strips are macOS
  MDM profile keys (`PayloadIdentifier` and friends); `plugin` is not among
  them.
- **Plugin specs that accumulate rather than replace.** Each layer's `plugin`
  entries are appended to a deduplicated `plugin_origins` list from which the
  effective list is rebuilt, so naming a plugin in the managed layer never
  drops one the user added, and the configure flow's replacement of the user's
  global config cannot drop ours.
- **A plugin spec may be an absolute file URL**, `file:///abs/path/plugin.js`,
  so a plugin can be baked into the image rather than installed from npm at
  run time.
- **An `event` hook receiving every bus event**, plus separately named hooks
  including `tool.execute.before` and `tool.execute.after`. Tool calls are
  *not* bus events, so a plugin that implements only `event` misses them — and
  they carry the two most valuable canonical names.
- **A bus that includes per-token events.** `message.part.updated` fires for
  every delta of every message.

## Decision

**The opencode image ships a plugin that publishes opencode's lifecycle through
`discobox-hook-publish`, and reaches opencode from the image's managed
configuration layer.**

### 1. The plugin is image-owned, like the other harnesses' hook definitions

`harness/opencode/hook-plugin.js`, installed at
`/usr/local/libexec/discobox/opencode-hook-plugin.js`. It is the same kind of
unit as claude-code's `managed-settings.json` and codex's `hooks.json`: owned
by the image rather than constructed by Go, and never merged into a user's
file.

It is not published to npm and not installed at run time. An image whose hook
capture depends on a registry fetch fails differently every time the registry
does.

**It is not, however, version-locked to the CLI it runs against, and must not
assume it is.** The image installs `opencode-ai` unpinned, and the agent
version store ([ADR 0114](0114-a-sandbox-pins-its-agent-version-from-a-pool-cached-store.md))
routinely puts a *newer* opencode ahead of the image's own on `PATH`. A plugin
is JavaScript loaded into that CLI's process, so it is exposed to API drift in
a way a settings file is not. What makes that survivable is opencode's own
handling, which this decision depends on:

- A plugin that fails to load is logged and swallowed — `tryPromise(...)
  .pipe(tapError(logError("failed to load plugin")), catch(() => void))` — so a
  plugin opencode cannot load never stops opencode from running.
- Hooks are dispatched by name, absent ones skipped (`let M = H[W]; if (!M)
  continue`), and the bus hook is an optional call (`V.event?.(…)`). A renamed
  or removed hook silently stops publishing rather than raising.

So CLI drift degrades hook capture to nothing and the harness still runs —
the same class of failure as a stale `hooks.json`, reached by a different
route. The plugin keeps its surface deliberately small for this reason: one
bus hook, two tool hooks, `process.env`, and the `$` it is handed.

### 2. It is reached from `/etc/opencode`, not from a plugin directory

`/etc/opencode/opencode.json` names the plugin by absolute file URL. opencode
also auto-discovers plugins in `~/.config/opencode/plugins/` and
`.opencode/plugins/`, and both are wrong here: the global one is the user's,
which the configure flow captures and replaces, and the project one is the
repository's, which is untrusted content the sandbox is holding.

### 3. It publishes through the generic publisher

`discobox-hook-publish --provider opencode --event <name>`, payload on stdin —
the same contract the other two images' hooks use, so the socket protocol has
one implementation and the plugin never learns it. The publisher already reads
the terminal ID and socket path from the environment opencode runs in.

The payload is redirected from stdin rather than interpolated into the command,
because a payload carries arbitrary text — a prompt, a diff, a command line.

### 4. The events published are an allowlist

Twelve bus events, plus the two tool hooks:

| Published | Canonical name |
| --- | --- |
| `tool.execute.before` | `PreToolUse` |
| `tool.execute.after` | `PostToolUse` |
| `session.idle` | `Stop` |
| `session.error` | `StopFailure` |
| `session.created` | `SessionStart` |
| `permission.asked` | `PermissionRequest` |
| `session.compacted` | `PostCompact` |
| `file.edited` | `FileChanged` |
| `permission.replied`, `command.executed`, `todo.updated`, `session.deleted`, `server.connected`, `installation.updated` | none |

An allowlist and not a denylist. opencode's bus carries per-token events, and
publishing one spawns a process and writes a row; a denylist would publish
every event opencode ships next by default, and a stream-rate one would flood a
sandbox's hook table until somebody noticed. An allowlist fails the other way,
by missing a new event until it is added — which is the cheaper failure and the
visible one.

`session.deleted` is deliberately **not** `SessionEnd`: Claude Code ends a
session when it terminates, and opencode deletes one on an explicit removal.
They sound alike and are not the same event, so under ADR 0146 §3 it keeps its
own name and gains none.

**The session-scoped events are published for the root session only**, and the
four canonical names above that depend on it — `Stop`, `StopFailure`,
`SessionStart`, `PostCompact` — are correct only because of it. opencode's task tool runs a
*sub-session*, and the bus publishes `session.created`, `session.idle`,
`session.error` and `session.compacted` per session rather than per turn. A wait matches by name and
the name carries no session, so an unfiltered `session.idle` would name a
subagent's finish `Stop` and end a wait at the first one — the false turn-end
ADR 0137's matcher exists to avoid.

opencode's own `run` command faces this and answers it the same way, for the
two of these it consumes: it ends on a `session.status` of idle only when the
id is the root's, and skips a `session.error` whose id is not — and to do
either it must track descendants from the `parentID` on each child's
`session.created`. It needs no rule for `session.idle` or `session.compacted`
because it reads neither. The plugin does likewise, for all four, since a
recorded hook is read by callers that have no such loop: it learns a session
is a child from the `session.created` that announced it, and treats an unknown
session as the root, which is the safe default — a resumed root's `session.created` fired
in an earlier process and will not be seen again, while a child created during
this one always announces itself first.

Claude Code distinguishes these with `SubagentStart`/`SubagentStop`. Those are
deliberately not mapped here: a subagent's idle is not published at all rather
than published under a name it might not mean, since opencode's sub-sessions
are not only the task tool's. If that filter is ever removed, all four
mappings must go with it — `harness/hookevent.go` says so beside them, and a
test in `harness/opencode` fails if the two drift apart.

### 5. A failed publish is never a failed turn

The plugin swallows publish errors and does nothing when
`DISCOBOX_HOOK_SOCKET` is unset — a configure sandbox, or opencode run outside
a Discobox terminal. Hooks are the log, never the behaviour; a harness runs
whether or not anything is listening.

A publish on the **bus** hook cannot slow a turn either: opencode dispatches
that one inside a `sync` and never awaits what it returns, so it is already
fire-and-forget from opencode's side.

The **tool** hooks are different and are awaited — that is how a plugin can
rewrite `output.args` before a tool runs — so each tool call pays one process
spawn, bounded by the publisher's own 10s ceiling. That is the cost Alternative
6 weighs and accepts; it is the same cost the other two harnesses already pay
on every `PreToolUse`.

## Consequences

- A wait can end on a hook in an opencode terminal, and `--hook Stop` now means
  the same thing on all three harnesses. This is what ADR 0146 was plumbing
  for, and the first mapping in its table that is a real translation rather
  than a recorded agreement.
- opencode's hook trail is deliberately coarser than Claude Code's. Nothing
  records streamed assistant text or per-part message updates, so
  `discobox admin audit hooks` shows a turn's shape for opencode and not its
  content.
- The plugin is a second runtime language in a harness image — the first
  JavaScript Discobox ships into a sandbox. It is small and dependency-free,
  and the alternative in *Alternatives rejected* §1 is a daemon.
- **Revisit if opencode grows a command hook.** It would make the plugin
  deletable and the image's hook definition the same shape as the other two,
  which is worth taking.

## Alternatives rejected

1. **A sidecar reading opencode's server event stream.** opencode runs a
   server and publishes the same events over it, so a small process could
   subscribe and forward. Rejected: it is a second long-lived process per
   terminal with its own lifecycle, failure mode, and reconnection logic, to
   observe a CLI running in the same sandbox; and it cannot see
   `tool.execute.before`, which is a plugin hook rather than a bus event.
2. **Installing the plugin from npm** via a bare `plugin: ["…"]` package spec.
   Rejected: it makes a sandbox's hook capture depend on a registry fetch at
   opencode start, so a sandbox's trail would go quiet whenever the registry
   did, and it puts the plugin on a release line of its own — a third moving
   version beside the image and the agent store, with nothing holding the
   three together. Baking it in at least ties the plugin to the image that
   ships it. It does not tie it to the CLI, which §1 is explicit about.
3. **Writing the plugin into the user's global config directory** at launch.
   Rejected: that directory is the user's, the configure flow captures and
   replaces what is in it, and a launcher that edits a user's config is the
   thing `harness/DESIGN.md` prefers managed layers to avoid.
4. **Publishing every bus event**, with a denylist for the per-token ones.
   Rejected in §4: the failure mode is a sandbox's hook table filling from an
   event nobody chose to publish.
5. **Publishing only the eight events that map canonically.** Rejected: ADR
   0143 §3 went out of its way to keep an event with no canonical name
   recordable and waitable under its own, and `permission.replied` and
   `command.executed` are exactly the opencode facts that rule exists for.
6. **Speaking the hook socket protocol directly from JavaScript**, instead of
   spawning the publisher. Rejected: it duplicates the wire format in a second
   language, in an image, where it would drift from the Go definition silently.
   The cost avoided is one process spawn per lifecycle event, which is what the
   other two harnesses already pay per hook.
