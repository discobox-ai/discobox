# 26-10-15-848 — The opencode harness runs opencode 2

- **Status**: Accepted (§3 narrowed for pending and accepted local credentials by
  [26-10-26-573](26-10-26-573-a-harness-exports-local-configuration-back-to-its-config.md); §6 superseded by [26-10-27-880](26-10-27-880-access-to-a-secret-is-a-binding-held-to-static-routes-and-no-judge-decides-it.md))
- **Date**: 2026-10-15
- **Supersedes**: [0127](0127-the-opencode-harness-runs-opencode-1.md) §1
  and §2, and its rejection of opencode 2; its §3, §4's choice of judge model,
  and §5's question stand.
  [0147](0147-opencode-publishes-its-lifecycle-through-an-image-owned-plugin.md)'s
  plugin file and its event list; its decision that an image-owned plugin in
  the managed layer is opencode's hook layer stands.
  [0114](0114-a-sandbox-pins-its-agent-version-from-a-pool-cached-store.md)
  §2's "an existing pin is never rewritten", narrowed by §7 to pins of the
  package the image installs.

## Context

ADR 0127 put the harness on opencode 1 (`opencode-ai`) and rejected opencode 2
(`@opencode/cli`) for three reasons: its credential store had no supported way
to create an OAuth credential from values, its TUI's `--prompt` only filled
the input box, and it released daily. It named the condition for revisiting:
when opencode's installer installs opencode 2.

Probing `@opencode/cli` 2.0.29 against a fake model server (measured unless
marked):

- **The installer still installs opencode 1.** `opencode.ai/install` fetches
  GitHub's latest release, which is `v1.19.0` (2026-10-11). ADR 0127's
  condition is not met. opencode 2 has shipped 29 releases since 2.0.0 on
  2026-09-11, and a `dev` build most hours.
- **Credentials have a supported write path.** `opencode auth export` prints
  every stored credential as JSON and `opencode auth import` creates them
  through the server's public `credential.create` route. An entry is
  `{id, integrationID, label, active, value}`, and `value` is one of
  `{type: "key", key}`, `{type: "oauth", methodID, refresh, access, expires}`
  or `{type: "external", methodID}`. An imported OAuth credential is accepted
  as given. Import skips an `id` already present; it never updates one.
- **An imported key is sent verbatim.** A key imported for `anthropic` reached
  the fake server as `x-api-key`, so a sentinel the proxy swaps works as it
  does today.
- **`--prompt` submits.** In a tmux pane, `opencode --standalone --prompt
  "say hi"` sent the request and rendered the answer with no keypress.
- **`auth.json` is imported only into a database opencode 1 made.** With a
  v1 `auth.json` holding a key and three OAuth entries, a fresh `opencode.db`
  recorded the `import_legacy_credentials` migration as done and held no
  credential. Opening a database opencode 1.19 made beside an `auth.json` did
  import it, under ids of opencode's own, as each provider's active
  credential. Its mapping, read from the binary: `api` is `key`, `wellknown`
  is a `key` holding its token, and `oauth` keeps its tokens with method
  `chatgpt-browser` for openai, `device` for github-copilot, opencode and xai,
  and `oauth` for any other provider. An opencode 1 `opencode.json`, its old
  `plugin` key included, still loads.
- **Plugins changed shape.** The config key is `plugins`; a path entry must be
  a directory (a single file is refused: "configured plugin path must be a
  directory"); plugins are written against `@opencode/plugin`. The v1
  `tool.execute.before` hook does not exist. The bus events are new names:
  `session.execution.started|succeeded|failed|interrupted`,
  `session.tool.called|success|failed`, `session.compaction.ended`,
  `permission.asked|replied`, and more.
- **Switches the image uses are gone.** `--pure`, `OPENCODE_PERMISSION`,
  `OPENCODE_DISABLE_CLAUDE_CODE` and `OPENCODE_ENABLE_EXA` do not appear in the
  binary. `/etc/opencode` is still the managed layer,
  `OPENCODE_DISABLE_AUTOUPDATE`, `OPENCODE_CONFIG_DIR` and
  `OPENCODE_DISABLE_PROJECT_CONFIG` are still read, and the `websearch`
  permission still exists.
- **A background service is the default.** `opencode`, `run` and `auth` talk
  to a per-user background server unless given `--standalone`, which runs a
  private one for the command's life. Every probe above ran `--standalone`.
- **`run` reads stdin while it is open**: with a pipe that stays open it never
  sends the request. With stdin at `/dev/null` it answered in about a second.
- The TUI has no `--model`; the model comes from config.

## Decision

### 1. The harness installs opencode 2

`@opencode/cli`, binary `opencode`, pinned per sandbox by the agent version
store (ADR 0114). The image keeps the id `opencode`. This proceeds although
ADR 0127's revisit condition is unmet, accepting a CLI that still changes most
days; the version store is what keeps a sandbox on the release it was tested
with.

### 2. Every opencode command runs `--standalone`

The launcher, `discobox-prompt` and the configure script pass `--standalone`.
The background service is one process per user that outlives the terminal
that started it, and the hook plugin reads the terminal it publishes for from
its process's environment (ADR 0147): under a shared service, a second
terminal's events would be published as the first's (inferred from where the
plugin runs; not measured).

### 3. Credentials are opencode's own export, imported at launch

Secrets stay one per provider, `OPENCODE_<PROVIDER>_CREDENTIAL`, typed as ADR
0127 §2 types them. The configure command captures with `opencode auth
export` and returns the export, with each provider's sentinel in place of its
value, as a templated harness file. Before it starts opencode the launcher
brings the store to that file: an image-owned credential that differs is
logged out and imported again, because import never updates an id it already
has, and an image-owned credential that is not active is switched to its
provider's active credential wherever the provider has another, because
import activates one only for a provider with none and logging out a
provider's active credential promotes another (measured). Image-owned
credentials carry a fixed id per provider, so a credential the user connected
by hand inside a sandbox is never logged out or changed; it stops being its
provider's active one when a changed configuration is brought in.

### 4. A configuration made under ADR 0127 keeps working

A configuration made before this has `auth.json` as its harness file and no
export. When the export is absent the launcher builds the import from
`auth.json` with opencode's own legacy mapping (Context), so a provider is
read exactly as opencode would read it, generic `oauth` method included. On a
sandbox that ran opencode 1, opencode's own import of the same `auth.json`
sits beside the image's copy; §3's switch keeps the image's active. A
reconfigure reads the old `auth.json` the same way and returns the export.

### 5. The hook plugin is rebuilt for opencode 2

The image installs a plugin directory written against `@opencode/plugin`,
named under `plugins` in `/etc/opencode/opencode.json`. It publishes an
allowlist of opencode 2's events under their own names, with canonical names
(ADR 0146) only where Claude Code has a true counterpart, chosen from recorded
payloads during implementation.

### 6. The judge's isolation is rebuilt from what remains

`discobox-prompt --no-tools` keeps ADR 0127 §4's guarantee — an empty
directory, empty config, state and cache, no project config, no inherited
`OPENCODE_*` — using isolated XDG directories, `OPENCODE_DISABLE_PROJECT_CONFIG`,
an isolated home (opencode 2 reads `~/.claude` and `~/.agents`), and a
`"*": "deny"` permission in config in place of `--pure` and
`OPENCODE_PERMISSION`; the image's own plugin is left, silent without the hook
socket. Tools off is accepted only once the fake model is shown to receive no
tools. Its credentials are the delivered ones (§3's file, or §4's
`auth.json`), imported into a database of its own: a judge discobox never
launches the harness, so its store is empty, and a credential the agent
connected by hand no longer reaches the judge.

### 7. The version store replaces a pin for another package

A sandbox pinned under opencode 1 has `~/.local/bin/opencode` linked into the
store's `opencode-ai` directory, ahead of the image's opencode 2 on PATH.
`discobox-agent-store pin` treats a link into the store under another
package's directory as its own stale pin: it removes it and pins as for a new
sandbox. A link out of the store is still the user's, and a pin of the
image's own package is still never rewritten.

## Rejected alternatives

- **Stay on opencode 1 until the installer moves.** The condition ADR 0127
  set, and the safe choice. Rejected because ADR 0127's blockers are gone and
  opencode 2 is where opencode's development is.
- **The background service.** One server for every terminal is how opencode
  2 expects to run, and would start faster. Rejected for §2's reason: the
  plugin, and the environment it publishes from, would belong to whichever
  terminal started the service.
- **Write `opencode.db` directly.** What the first opencode 2 harness did, and
  why ADR 0127 rejected opencode 2. The import route is opencode's own.
- **Deliver keys through each provider's environment variable.** opencode 2
  still reads them, but they carry no OAuth sign-in.
- **Rely on opencode's legacy `auth.json` import** for configurations made
  under ADR 0127. It does not run for a new database (measured).
- **The judge reads the sandbox's store**, as it read `auth.json`. A judge
  discobox's store is empty, since only a launch fills it.
- **Leave an opencode 1 pin until `upgrade`.** The sandbox would run
  opencode 1 under scripts written for opencode 2 until somebody asked.
- **Require a reconfigure** after the upgrade. Simpler, but every opencode
  user's harness stops working until they notice.

## Consequences

- opencode 2's daily releases reach a sandbox only through the version store,
  so a breaking release is met by the version a sandbox pins, not by the image.
- Each `opencode auth` command starts a standalone server of its own: under a
  second on an idle machine, 10 to 40 seconds in a sandbox on a loaded one
  (measured). So the launcher keeps what it last synced and runs nothing while
  that is still what is delivered, a first launch only imports, and it
  switches a credential to active only where its provider has another.
- opencode 2 searches the web with every provider by default (measured), so
  ADR 0127 §5's question now only turns it off: `webSearch: false` denies the
  `websearch` permission, which removes the tool, and `true` is the default.
- `canonicalHookEvents` loses the opencode 1 names and gains opencode 2's.
  Audit rows recorded under the old names keep them.
