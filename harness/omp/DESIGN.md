# omp harness

omp, "oh-my-pi" (`@oh-my-pi/pi-coding-agent`), a Bun script that runs on the platform's Bun. Its declaration is [`harness.json`](harness.json) and its vendor
operations are [`adapter.ts`](adapter.ts); what every built-in shares, and
when each part runs, is [`../DESIGN.md`](../DESIGN.md#built-in-harness-shape).
This file is what is particular to this vendor.

| Identity | |
| --- | --- |
| Directory | `omp` |
| Harness ID and built-in config slug | `omp` |
| Hook provider | `omp` |
| Bundle | `discobox-harness-omp` |
| Package and command | `@oh-my-pi/pi-coding-agent`, `omp` |

## What it declares

- **Install**: requires `bun` too, and installs with
  `ONNXRUNTIME_NODE_INSTALL_CUDA=skip`, which the manifest's env carries for
  the version store's installs.
- **Launch**: `--auto-approve --allow-home` always; the prompt after `--`; a
  relaunch is `--continue`.
- **Environment**: `PI_CONFIG_FILES` names the overlay; `PI_PROXY` aliases
  `HTTPS_PROXY`, in the launcher and every login shell
  (`/etc/profile.d/omp-proxy.sh`).
- **Settings**: the `overlay` policy `assets/discobox.yml` at
  `/etc/omp/discobox.yml`, scalars only.
- **Credentials**: no declared secrets; a `native` store,
  `.omp/agent/agent.db`, delivered as `.config/discobox/omp-credentials.json`.
- **Hooks**: `extension`, `assets/hook-extension.js` at
  `/usr/local/libexec/discobox/omp-hook-extension.js`, loaded from
  `~/.omp/agent/extensions/discobox-hooks.js`.

## Adapter operations

- `exportNative` (`omp auth export`), `entries`, `plan` (`normalize`: one
  secret per account), `files`, `compose`, `wanted` (the same account),
  `apply` (`omp auth import` of the one replaced login).
- `importWanted` and `importNative`: a delivered document is imported at
  launch, behind the runtime's guard.

## Credentials and settings

A harness whose credentials are any number of providers'
([ADR 26-10-14-718](../../docs/adr/26-10-14-718-the-omp-harness-imports-its-credentials-and-publishes-through-an-extension.md)).
The image declares no secrets. A box signs in with `/login`; every omp
browser sign-in (ChatGPT 1455, Anthropic 54545, Gemini CLI 8085, Antigravity
51121, OpenRouter 54549) also takes the redirect URL pasted back, which is the
way when its port is taken on the user's machine.

- **Credentials live in SQLite** (`~/.omp/agent/agent.db`), and omp moves them
  with `omp auth export` and `omp auth import`: `{version: 1, credentials:
  [{provider, credential}]}`, where a credential is `{type: api_key, key}` or
  `{type: oauth, access, refresh, expires, …identity}`. An import replaces the
  same provider's key and the same account's login, and keeps the rest.
- **One secret per credential**, `OMP_<PROVIDER>_CREDENTIAL`, a second account
  of one provider `_2`. omp exports several accounts in no fixed order, so a
  credential is named by which secret it is: the one whose sentinel it still
  holds, else the previous secret of the same account (provider, `accountId`,
  `email`), else the provider's lowest free name. A key is a `token`; an OAuth
  login the control plane can renew — Anthropic and ChatGPT with the clients
  the claude-code and codex images are renewed with, xAI form-encoded — is an
  `oauth` secret; any other login is a `token` holding its access token, and
  the export says when it expires, or that it has.
- **Delivery is the export document as a credential document**,
  `.config/discobox/omp-credentials.json`, with sentinels in place of keys and
  access tokens, `refresh` a placeholder (or the sentinel, for GitHub Copilot,
  which stores its token in both), and `expires` far future where the control
  plane renews. The launcher's preparation imports it before starting omp, on every launch
  (the adapter's `importNative`, behind the runtime's guard).
  An imported sentinel goes out as the request's credential and is never
  refreshed. omp reads ChatGPT's `chatgpt-account-id` from the access token's
  claims, which a sentinel does not carry.
- **`PI_PROXY` follows `HTTPS_PROXY`** (the declaration's
  `environment.aliases`: generated as
  `/etc/profile.d/omp-proxy.sh` for login shells, and set by the launcher itself). omp's Anthropic
  transport is its own client that reads only `PI_PROXY`, and without it
  connects direct, which a sandbox's egress does not allow: every Anthropic
  request hangs into "Connection error.". It is derived rather than written
  into the manifest, where a build in a discobox would rewrite the loopback
  address (ADR 26-10-01-425).
- `ONNXRUNTIME_NODE_INSTALL_CUDA=skip`, at build time and in the image's env
  (the version store installs omp in the sandbox): omp's onnxruntime-node
  dependency otherwise downloads onnxruntime's CUDA build from GitHub on
  install, which no sandbox can use and which hung a build outright.
- It exports `.omp/agent/config.yml` as omp wrote it; not `models.yml`, which
  holds literal keys and `!` commands, as pi's `models.json` does, so custom
  providers are configured in each sandbox.
- **The policy baseline** is `PI_CONFIG_FILES=/etc/omp/discobox.yml` in the
  image's env — `startup.setupWizard: false`, whose wizard otherwise stalls an
  unattended launch, and `startup.checkUpdate: false` — and the launcher's
  `--auto-approve` and `--allow-home`. A launch prompt follows `--` as one
  argument: omp sends each positional argument as its own message and reads
  an `@`-led one as a file. Resume is `--continue`, which opens a new session
  when there is none.

## Hooks and policy layer

**no system layer** for hooks or settings
  ([ADR 26-10-14-718](../../docs/adr/26-10-14-718-the-omp-harness-imports-its-credentials-and-publishes-through-an-extension.md)).
  Its lifecycle reaches JavaScript extensions alone, as opencode's does, so
  `assets/hook-extension.js` is the unit, installed beside the launcher; the
  image's harness files put a one-line re-export of it in
  `~/.omp/agent/extensions/discobox-hooks.js`. omp discovers that directory
  beside the user's own `extensions:` setting rather than instead of it — an
  overlay naming `extensions:` replaces the user's list, which is why the
  extension is not named there. It publishes an allowlist of events, each
  payload projected to its scalar fields and a tool event's `input`: omp's
  payloads carry the system prompt, transcript, images and compaction entries
  under many names. The home file can be removed and
  `--no-extensions` skips it, which claude-code's and Copilot's hooks survive.
  The policy baseline is the `PI_CONFIG_FILES` overlay `/etc/omp/discobox.yml`,
  scalars only, and two launch flags (see [Credentials and settings](#credentials-and-settings)).
