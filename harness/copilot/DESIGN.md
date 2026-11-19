# GitHub Copilot harness

GitHub's Copilot CLI (`@github/copilot`). Its declaration is [`harness.json`](harness.json) and its vendor
operations are [`adapter.ts`](adapter.ts); what every built-in shares, and
when each part runs, is [`../DESIGN.md`](../DESIGN.md#built-in-harness-shape).
This file is what is particular to this vendor.

| Identity | |
| --- | --- |
| Directory | `copilot` |
| Harness ID and built-in config slug | `copilot` |
| Hook provider | `copilot` |
| Bundle | `discobox-harness-copilot` |
| Package and command | `@github/copilot`, `copilot` |

## What it declares

- **Launch**: `--allow-all` always; the prompt to `--interactive=`; a relaunch
  is `--continue`.
- **Environment**: `COPILOT_ALLOW_ALL=true` (folder trust) and the updater off.
- **Local state**: none: the environment and `--allow-all` are the trust.
- **Credentials**: `COPILOT_GITHUB_TOKEN`, delivered in the environment, with
  routes; a `file` store, Copilot's own `.copilot/config.json`, never
  delivered.
- **Hooks**: `command`, Copilot's format, at
  `/etc/github-copilot/policy.d/discobox.json`.

## Adapter operations

- `entries` (the token `lastLoggedInUser` names), `material`, `files` (none:
  the login is delivered in the environment), `apply`.
- `retiresLogin`: handoff removes the saved plaintext token rather than
  replacing it, and reconsideration has no document to compose.

## Credentials and settings

A box signs in inside Copilot, by `/login` or with a token
([ADR 26-10-02-840](../../docs/adr/26-10-02-840-the-copilot-harness-takes-its-github-token-from-the-environment.md)).
`/login`'s browser sign-in calls back to a port it picks fresh each run
(`listen(0)`; GitHub accepts any loopback port); device code is the way when
that port is taken on the user's machine. Its token is an OAuth App `gho_`
token with `repo` scope among others; a fine-grained PAT with only "Copilot
Requests" is the least-privilege alternative.

- **Storage is `~/.copilot/config.json`**, Copilot's own state file (JSON
  under `//` comment lines): `authTokens["<host>:<login>"].token`, for the
  account `lastLoggedInUser` names. A sandbox has no keychain, and Copilot
  stores the token in plaintext only under `storeTokenPlaintext`, which the
  export never carries into shared settings.
- **One `token` secret, `COPILOT_GITHUB_TOKEN`, delivered in the
  environment.** Copilot documents that variable for headless use and it
  outranks a stored login; `gh` and git never read it, and Copilot strips it
  from the environment of the commands its agent runs. Neither sign-in yields
  anything to refresh. `config.json` is never exported as a file: it is
  Copilot's state, rewritten as it runs. After a save the handoff removes the
  saved plaintext token; a running process keeps its previous environment
  until it restarts.
- **Only github.com.** A login to another host is refused by the export: an
  Enterprise Cloud account also needs `COPILOT_GH_HOST` in every sandbox, and
  a harness change carries no env.
- **The token is held to Copilot's own traffic.** `COPILOT_GITHUB_TOKEN`
  declares routes (ADR 26-10-02-924), which a save writes onto the secret:
  Copilot's own endpoints under `githubcopilot.com` and `api.github.com` are
  sent; its MCP server, its agents and the rest of `github.com` are blocked
  routes, which a judge used to read (ADR 26-10-27-880); anything else is
  refused. The agent needs a binding of its own to do more on GitHub.
- It exports `~/.copilot/settings.json` as the user left it (`/config model`,
  theme, status line), minus `storeTokenPlaintext`.
- **The policy baseline is two switches.** `COPILOT_ALLOW_ALL=true` in the
  image's env trusts the directory Copilot starts in — without it an
  interactive launch stops on a folder-trust dialog that `--allow-all` does
  not skip — and the launcher passes `--allow-all`, since the variable alone
  leaves an interactive session on manual approval. A launch prompt goes to
  `--interactive=`, whose `=` keeps a prompt starting with a dash a prompt;
  resume is `--continue`, which opens a new session when there is none.
- Copilot unpacks itself under `~/.cache/copilot/pkg` on first run (about
  185 MB per version), which the base layer's `%HOME%/.cache` volume already
  makes per-user pool cache.

## Hooks and policy layer

`/etc/github-copilot/policy.d/discobox.json`. Copilot loads every
  file in that directory as **policy** hooks, beside the user's own
  (`~/.copilot/hooks`, the `hooks` setting) rather than instead of them, and
  keeps them when the user's settings say `disableAllHooks`. Its
  `/etc/github-copilot/managed-settings.json` is read too, but not for hooks.
  The file uses Copilot's own hook schema (`version: 1`, `bash` commands) and
  publishes all fifteen of its events.
