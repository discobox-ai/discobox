# OpenCode harness

opencode 2 (`@opencode/cli`), which replaced opencode 1 (`opencode-ai`) and keeps its credentials in a database rather than a file ([ADR 26-10-15-848](../../docs/adr/26-10-15-848-the-opencode-harness-runs-opencode-2.md)). Its declaration is [`harness.json`](harness.json) and its vendor
operations are [`adapter.ts`](adapter.ts); what every built-in shares, and
when each part runs, is [`../DESIGN.md`](../DESIGN.md#built-in-harness-shape).
This file is what is particular to this vendor.

| Identity | |
| --- | --- |
| Directory | `opencode` |
| Harness ID and built-in config slug | `opencode` |
| Hook provider | `opencode` |
| Bundle | `discobox-harness-opencode` |
| Package and command | `@opencode/cli`, `opencode` |

## What it declares

- **Launch**: `--standalone --auto` always; the prompt as
  `--prompt=<prompt>`, which keeps a dash-led prompt a prompt; a relaunch is
  `--continue`.
- **Environment**: the updater off; `fromSettings` derives
  `OPENCODE_CONFIG_CONTENT` from Discobox's own settings file (web search).
- **Settings**: the seed `.config/discobox/opencode-harness.json`; the
  `managed`-layer policy `assets/managed-config.json` at
  `/etc/opencode/opencode.json`, which names the hook plugin.
- **Local state**: none; `--auto` approves, and opencode asks no folder trust.
- **Credentials**: no declared secrets (they are known once a provider is
  connected); a `native` store, opencode's database, delivered as
  `.config/discobox/opencode-credentials.json`.
- **Hooks**: `plugin`, `assets/hook-plugin/` at
  `/usr/local/libexec/discobox/opencode-hook-plugin`.

## Adapter operations

- `exportNative` (`opencode auth export`), `entries`, `plan` (`normalize`: one
  secret per provider), `files`, `compose`.
- `importNative`: the launch-time sync of the delivered document into the
  database (below), and `handoffNative`: the journaled logout/import/switch
  that replaces a saved login.
- `launchEnvironment`: `webSearch: false` denies the `websearch` permission.

## Credentials and settings

A user connects as many models.dev providers as they like with `/connect`, so
the image declares no secrets at all: their names are only known once
something is connected. See
[ADR 0127](../../docs/adr/0127-the-opencode-harness-runs-opencode-1.md) and
[ADR 26-10-15-848](../../docs/adr/26-10-15-848-the-opencode-harness-runs-opencode-2.md).
ChatGPT's browser sign-in calls back to 1455 and DigitalOcean's to 1456;
ChatGPT also has a headless sign-in, and the rest connect with a key.

- **Every opencode command runs `--standalone`**: the launcher and every
  command the adapter runs. Without it opencode talks to a per-user background
  service, which outlives the terminal that started it and would run the hook
  plugin in that terminal's environment for every terminal after it.
- **Credentials are rows in opencode's database**, read and written only
  through `opencode auth export` and `auth import`, opencode's own interchange
  format: `{id, integrationID, label, active, value}`, where `value` is
  `{type: key, key}`, `{type: oauth, methodID, access, refresh, expires,
  metadata}`, or `{type: external, methodID}`. Import never updates an id it
  already holds.
- **One secret per provider**, `OPENCODE_<PROVIDER>_CREDENTIAL`, from the
  provider's active credential. The name is stable per provider, so a save
  that replaces a credential updates its secret in place. An `external`
  credential holds no secret and is not captured.
- **The secret's type follows how the credential renews.** A key is a `token`.
  An OAuth sign-in whose `refresh_token` grant the control plane can perform —
  OpenAI (ChatGPT, the client and endpoint the codex image refreshes), xAI
  (whose endpoint takes the request form-encoded, recorded as
  `tokenRequestEncoding: form`) — is an `oauth` secret. Any other OAuth sign-in
  (GitHub Copilot; DigitalOcean and Snowflake, which cannot be renewed from
  here) is a `token` holding its access token, and the export says when it
  expires.
- **Delivery is the export as a credential document**,
  `.config/discobox/opencode-credentials.json`: each entry under the image's
  own id, `cred_discobox_<provider>`, with the sentinel in place of its key or
  access token, `refresh` a placeholder that can never be spent (or the
  sentinel, where the access token is also the refresh field), and `expires`
  far future where the control plane renews the token, so opencode never
  attempts a refresh the sandbox could not complete. The launcher's preparation
  imports it (the adapter's `importNative`) before opencode starts. Each `opencode auth`
  command is a standalone server start (10 to 40 seconds on a loaded machine),
  so it runs nothing while what it last synced is still what is delivered;
  otherwise it exports (unless there is no database yet), logs out and
  imports only the image-owned credentials that differ, and switches an
  image-owned credential that is not active to active wherever its provider
  has another credential (import activates one only for a provider that has
  none, and logging out a provider's active credential promotes another). A
  credential the user connected by hand in a sandbox has another id and is
  never logged out or changed, though a changed configuration takes back
  which one is active. A configuration made under ADR
  0127 delivers opencode 1's `auth.json` instead; the adapter reads it with
  opencode's own legacy mapping, so it keeps working until a new sign-in is
  saved, and on a sandbox that ran opencode 1 the switch keeps it active over
  opencode's own import of the same file. The adapter is the one place the
  image's entries are made (`own`).
- It also exports opencode's settings as the user left them — the global
  `opencode.json`, `opencode.jsonc`, `config.json`, and `cli.json` (theme,
  keybinds; opencode 2 migrates an opencode 1 `tui.json` into it) — and
  `.local/state/opencode/model.json`, the `/models` choice.
- **The policy baseline is `--auto`** on the launch, approving every tool use a
  rule does not deny. The TUI submits the launch prompt from
  `--prompt=<prompt>`. The joined form preserves a leading dash in the prompt.
  opencode's system layer, `/etc/opencode`, is its *managed*
  configuration, which outranks a project's own rules, and the user's global
  config is replaced by a saved change — so neither is the place for it.
- **Discobox's own settings for the harness** live in
  `.config/discobox/opencode-harness.json`, a plain harness file the image
  declares: `webSearch`. It is edited in a box and saved as a harness change,
  or with `discobox admin harness edit opencode
  .config/discobox/opencode-harness.json`. opencode 2 searches with every
  provider by default, through a keyless search service. The launcher's preparation reads
  `webSearch` (`environment.fromSettings`, the adapter's `launchEnvironment`): `false` denies the `websearch` permission through
  `OPENCODE_CONFIG_CONTENT`, which takes the tool away from the model; `true`
  and `null` — the image's baseline — leave opencode's default.

## Hooks and policy layer

`/etc/opencode/opencode.json`, naming an image-owned plugin
  directory by absolute path under `plugins`
  ([ADR 0147](../../docs/adr/0147-opencode-publishes-its-lifecycle-through-an-image-owned-plugin.md)).
  opencode has no command hook at any layer — its lifecycle reaches plugins
  and nothing else — so `assets/hook-plugin/` (an opencode 2 plugin: a directory whose
  `server.js` default-exports `{id, setup}`; a single file is refused) is what
  claude-code, codex-cli and copilot declare as commands, and it invokes the same generic
  publisher. It publishes only when `DISCOBOX_HOOK_SOCKET` is set, which keeps
  it silent in a build and outside a Discobox terminal. It hears the lifecycle
  through the bus, which a plugin subscribes to after opencode starts: the
  TUI, which is how the launcher runs a session, creates its session later and
  published every event in each run measured, while `opencode run` can create
  its session, and exit, before the subscription is up. The managed layer is
  read last of all, after the user's global config and a project's, and plugin
  entries accumulate across layers rather than replacing, so naming this one
  neither loses to a saved harness change nor drops a plugin the user
  added. Its policy baseline is a launch flag rather than a system layer (see [Credentials and settings](#credentials-and-settings)).
