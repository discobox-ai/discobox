# Codex harness

OpenAI's Codex CLI. Its declaration is [`harness.json`](harness.json) and its vendor
operations are [`adapter.ts`](adapter.ts); what every built-in shares, and
when each part runs, is [`../DESIGN.md`](../DESIGN.md#built-in-harness-shape).
This file is what is particular to this vendor.

| Identity | |
| --- | --- |
| Directory | `codex-cli` |
| Harness ID and built-in config slug | `codex` |
| Hook provider | `codex-cli` |
| Bundle | `discobox-harness-codex` |
| Package and command | `@openai/codex`, `codex` |

## What it declares

- **Launch**: the prompt as one positional; a relaunch is `resume --last`.
  `home` is `$CODEX_HOME` or `~/.codex`, and `retry` starts Codex again after
  status 1 before a session began (below).
- **Environment**: `SSH_CONNECTION` defaults to a placeholder at launch, so
  Codex copies to the terminal by OSC 52 rather than to the box's own X
  clipboard; a real session's value is kept.
- **Settings**: the `system`-layer policy `assets/system-config.toml` at
  `/etc/codex/config.toml` (approvals and the inner sandbox off, no update
  check, the thread title, memories on). See [source-scoped
  memory](../DESIGN.md#source-scoped-memory).
- **Local state**: `system-layer` trust of `/etc/codex/config.toml` under
  `/run/lock/discobox-codex-trust.lock`, from the baseline installed beside
  it; `bind-mount` memory onto `memories` in Codex's home.
- **Credentials**: `OPENAI_API_KEY` and `CODEX_OAUTH_TOKEN`, file-delivered; a
  `file` store, `.codex/auth.json`.
- **Hooks**: `command`, Codex's format, at `/etc/codex/hooks.json`, every
  documented event; the provider is `codex-cli`.

## Adapter operations

- `entries`, `material`, `login` (the ChatGPT sign-in, `oauth`, with the
  account's claims), `files`, `compose`, `apply`; `singleCredential`: one
  login at a time.
- `systemTrust`: the system layer's `projects` map, every folder trusted so
  far plus this one, ahead of the baseline.
- `oauth` and `oauthAuthFile` are also what `scripts/nested-codex-auth.sh`
  builds a nested login from.

## Credentials and settings

Codex's onboarding offers **ChatGPT in a browser**, **ChatGPT by device
code**, or an API key, and all of them write `$CODEX_HOME/auth.json`. The
browser sign-in completes against a callback server on the box's
localhost:1455; device code is the way when that port is taken on the user's
machine.

- **Both credentials are delivered as a file, never an environment variable**
  (`delivery: file`). Codex leaves no choice: the interactive TUI reads no
  credential variable at all (only `codex exec` honors `CODEX_API_KEY`), and a
  ChatGPT token has no variable to be read from. `~/.codex/auth.json` is the
  one delivery both halves of the harness agree on, so it is delivered as a
  credential document with the sentinel in the credential's place.
- An **API key** sign-in leaves `{"OPENAI_API_KEY": "sk-…"}`, saved as a plain
  `token` secret (`OPENAI_API_KEY`).
- A **ChatGPT** sign-in leaves `tokens.{id_token, access_token, refresh_token,
  account_id}`, saved as an `oauth` secret (`CODEX_OAUTH_TOKEN`) with OpenAI's
  fixed token endpoint and client id and the access token's own `exp`, so the
  control plane refreshes it as it expires. Two details of the delivered file
  follow from the credential living in the control plane:
  - `last_refresh` is far future. Codex rotates a token whose `last_refresh` is
    older than 28 days, and a rotation from inside a sandbox could not succeed —
    the refresh token is not there — nor does it need to, since a sentinel does
    not go stale.
  - The account travels as **claims in an unsigned `id_token`**, rebuilt from
    the real one, not as the signed token codex wrote. Codex needs the claims —
    it addresses the ChatGPT backend with the account id, and refuses to start
    without a plan type — and never verifies the signature, so re-signing buys
    nothing while keeping a signed identity assertion out of a harness file that
    is not a secret. The plan type is also recorded on the secret as
    `subscriptionType`, the same non-secret "what this grant is" metadata
    claude-code records scopes as.
- There is no baseline auth.json: an empty one reads as signed in.
- Literal user preferences in `~/.codex/config.toml` are exported, excluding
  parsed `[projects]` tables. Before every start the launcher records its
  actual cwd in `/etc/codex/config.toml`, under a system-layer lock. The
  system file combines the image's defaults with an inline TOML `projects`
  map, retaining previously trusted folders. Restarts, source-less boxes and
  alternate working directories use the same path. Later direct `codex`
  starts in those folders see the same trust, without launch flags or edits
  to portable user preferences. Trusting `/` does not trust child repositories.
  The pinned-binary PTY test (`task test:codex-trust`) runs the old override
  as an untrusted control, then launch, restart and direct starts. It removes
  explicit approval/sandbox defaults from its fixture because those can hide
  a broken trust entry. Its Codex version is pinned in
  `test/codex-trust/version`; a vendor upgrade updates that contract test.
- Existing stored trust suffixes and credential references are converted in
  place by the server migration. Old sandbox bootstraps and capture checkpoints
  use the same bounded conversion on read. Unknown executable templates fail
  explicitly; migration never evaluates them or discards user preferences.
- **The launcher starts Codex again when it failed before its session
  began.** A signed-in Codex TUI exits with status 1 at bootstrap when its
  first requests cannot connect (`account/read failed ... workspace routing
  discovery failed`), with no retry of its own, and that ends the box's job. So
  the launcher's declared `launch.retry` runs `codex` rather than exec'ing it, and on status 1 with no
  rollout under `$CODEX_HOME/sessions` new or grown (a resume appends to its
  rollout) it starts it again, five attempts in all, waiting 2, 5, 10 and 20
  seconds. A new or grown rollout means the session began and holds the
  prompt, so it is never started again; a clean exit (how a user quits
  sign-in), a death by signal, and any other status (a usage error, a missing
  binary) are left alone. Rollouts are compared by name and size, not by time,
  so no clock has to agree with another. Claude Code's launcher has none: a
  failed model request at its start leaves it on its screen, retrying the
  request itself.

## Hooks and policy layer

`/etc/codex/hooks.json`, laid by the harness's feature beside its
  `/etc/codex/config.toml` system layer. The hook definition and Codex binary
  are one image-versioned compatibility unit, generated from the declared
  events. Every configured lifecycle event invokes the
  generic publisher with its provider and event name while its stdin payload is
  stored unchanged. System hooks are treated as managed and trusted by policy.
