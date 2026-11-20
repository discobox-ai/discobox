# pi harness

pi (`@earendil-works/pi-coding-agent`), the agent omp descends from, a Node script. Its declaration is [`harness.json`](harness.json) and its vendor
operations are [`adapter.ts`](adapter.ts); what every built-in shares, and
when each part runs, is [`../DESIGN.md`](../DESIGN.md#built-in-harness-shape).
This file is what is particular to this vendor.

| Identity | |
| --- | --- |
| Directory | `pi` |
| Harness ID and built-in config slug | `pi` |
| Hook provider | `pi` |
| Bundle | `discobox-harness-pi` |
| Package and command | `@earendil-works/pi-coding-agent`, `pi` |

## What it declares

- **Install**: also `fd-find`, which pi otherwise downloads on its first run.
- **Launch**: `--approve` always; the prompt after `--`, led by a space when
  it starts with `@`; a relaunch is `--continue`.
- **Environment**: no version check, no telemetry, OSC 52 copies.
- **Credentials**: no declared secrets; a `file` store, `.pi/agent/auth.json`.
- **Hooks**: `extension`, `assets/hook-extension.js` at
  `/usr/local/libexec/discobox/pi-hook-extension.js`, loaded from
  `~/.pi/agent/extensions/discobox-hooks.js`.

## Adapter operations

- `entries`, `material` (a Copilot login's GitHub token is its refresh field),
  `plan` (`normalize`: one secret per provider), `files`, `compose`, `apply`.
- `unsupported`: a ChatGPT subscription; `settings`: pi's changelog marker is
  not a preference.

## Credentials and settings

The agent omp descends from, with the same any-providers credentials
([ADR 26-10-15-259](../../docs/adr/26-10-15-259-the-pi-harness-delivers-auth-json-and-exchanges-copilot-in-the-sandbox.md)).
The image declares no secrets. A box signs in with `/login`; each browser
sign-in (Anthropic 53692, OpenAI 1455, Radius 1456, OpenRouter on a port of
its own) also takes the redirect URL pasted back, and Copilot, xAI, Kimi and
Meta sign in by device code.

- **Credentials live in `~/.pi/agent/auth.json`**, one entry per provider:
  `{type: api_key, key, env?}` or `{type: oauth, access, refresh, expires, …}`.
  pi leaves a valid file as it is, through a run and through a 401.
- **One secret per provider**, `PI_<PROVIDER>_CREDENTIAL`. A key is a `token`;
  an Anthropic or xAI login is an `oauth` secret, renewed with the clients the
  claude-code and opencode images are renewed with; any other login is a
  `token` holding its access token, and the export says when it expires, or
  that it has. Left out, and said: a key that is a `!` command (pi runs it), a
  ChatGPT subscription (`openai-codex`), whose account pi reads from the
  access token's claims and fails locally without, and an entry with no key or
  token at all (Bedrock's AWS profile or credential chain), which uses
  credentials kept on the machine pi runs on.
- **A Copilot login's secret is its GitHub token**, bound to `api.github.com`
  (or `api.<domain>` for GitHub Enterprise). pi exchanges it, at
  `copilot_internal/v2/token` with the token as a bearer header, for a Copilot
  token of about 30 minutes, which it keeps in `access`. That exchange runs in
  the sandbox, through the proxy, so the real Copilot token lands in the
  sandbox's `auth.json`; the GitHub token does not. An unchanged refresh
  sentinel with a changed `access` is that exchange, not a new login.
- **Delivery is auth.json as a credential document**, with sentinels in
  place of keys and access tokens, `refresh` a placeholder and `expires` far
  future where the control plane renews; a Copilot entry carries the sentinel
  in `refresh`, a placeholder `access` and `expires` zero, so pi exchanges
  before its first request.
- It exports `settings.json` and `keybindings.json` as pi wrote them; not
  `models.json` or `mcp.json`, which hold literal keys and commands.
- **The policy baseline** is the image's env — `PI_SKIP_VERSION_CHECK=1`,
  `PI_TELEMETRY=0`, since pi otherwise reports an install on a first run and
  after each upgrade and adds attribution headers to some providers'
  requests, and `PI_CLIPBOARD_OSC52=1` so a copy reaches the user's terminal as well as the
  box's X clipboard — `fd-find` installed, since pi otherwise downloads `fd`
  from GitHub on its first run, and the launcher's `--approve`: pi's
  project-trust dialog otherwise stalls a launch in any repository with
  `.agents/skills` or `.pi/`. pi asks no approval before a tool call, and
  honors `HTTPS_PROXY` for every provider. A launch prompt follows `--` as one
  argument, led by a space when it starts with `@`: pi sends each positional
  argument as its own message, and reads an `@`-led one as a file to attach
  even after `--`. Resume is `--continue`, which opens a new session when
  there is none.

## Hooks and policy layer

**no system layer** and no settings overlay either ([ADR 26-10-15-259](../../docs/adr/26-10-15-259-the-pi-harness-delivers-auth-json-and-exchanges-copilot-in-the-sandbox.md)).
  The same shape as omp's: `assets/hook-extension.js` installed beside the
  launcher, reached from `~/.pi/agent/extensions/discobox-hooks.js`. Each
  published event carries the fields named for it, not a projection by shape:
  pi's `before_agent_start` carries the whole system prompt as a plain string.
  The policy baseline is the image's env and the launcher's `--approve` (see [Credentials and settings](#credentials-and-settings)).
