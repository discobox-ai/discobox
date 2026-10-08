# 26-10-02-840 — The Copilot harness takes its GitHub token from the environment

- **Status**: Accepted
- **Date**: 2026-10-02

## Context

Discobox ships a GitHub Copilot CLI harness (`@github/copilot`, 1.0.91 when
this was written). Copilot authenticates with a GitHub token, not with a
model-provider key, and that changes three things the other included
harnesses never had to decide:

- **The token reaches more than one service.** Copilot sends it to
  `api.github.com` (the entitlement check, `/copilot_internal/user`) and to the
  Copilot API under `githubcopilot.com`. A secret's binding names one host or
  none (`guardGrantHost`). A grant may list several
  ([ADR 26-10-02-393](26-10-02-393-a-credential-request-and-its-grant-may-name-several-hosts.md)),
  but one of the two Copilot needs, `api.github.com`, is the whole REST API.
- **What `/login` yields is a GitHub OAuth App token.** The device-code and
  browser sign-ins use an OAuth App (client `Ov23ctDVkRmgkPke0Mmm`), so the
  token is a non-expiring `gho_` token with no refresh token, and it carries
  `read:user, read:org, repo, gist, codespace` and more. Given no host limit,
  as above, an agent could send its sentinel to `api.github.com` and act on
  every repository its owner can, without going through the judged
  `discobox-access` path any other GitHub use goes through. A Claude Code or
  Codex credential opens nothing beyond its model API, so this exposure is new.
- **Copilot keeps two kinds of credential storage**, and only one is a
  published interface. `COPILOT_GITHUB_TOKEN` (then `GH_TOKEN`,
  `GITHUB_TOKEN`) is documented, and it outranks a stored login. A stored login
  lives in the OS keychain or, with none, in `~/.copilot/config.json` under
  `authTokens["<host>:<login>"]` beside `lastLoggedInUser`. That file is what
  Copilot's own header calls "managed automatically", and it is not
  documented. A sandbox has no keychain, so `/login` stops at a
  plaintext-storage consent prompt unless `storeTokenPlaintext` is set.

Copilot also accepts a fine-grained personal access token that has only the
"Copilot Requests" permission. Such a token can do nothing on GitHub except
reach Copilot.

## Decision

### 1. The credential is one env-delivered `token` secret

The image declares one required secret, `COPILOT_GITHUB_TOKEN`, delivered the
default way, as an environment variable. Its sentinel takes the shape of the
real token (`secretformat`), so Copilot's token-prefix checks accept it. The
secret is a `token`, never `oauth`: `/login`'s token does not expire and comes
with nothing to refresh it. The secret carries no host, as claude-code's and
codex's do not, because Copilot must send it to two unrelated sites.

### 2. Configure offers a scoped PAT first and `/login` second, with a warning

The configure command asks how to sign in before it starts Copilot:

1. **A fine-grained PAT** with only "Copilot Requests". This is the
   recommended choice. Copilot's own `copilot login --with-token` reads it
   without echoing it, so Copilot also validates it.
2. **`/login`** in a bare interactive Copilot, as the other harnesses do. The
   command says that this token can act on every repository the account can,
   from inside every discobox that uses the harness, and goes ahead only on an
   explicit yes. A token pasted at the PAT prompt that is not a fine-grained
   PAT — Copilot also takes gh's own OAuth token — clears the same warning.

Either way the token ends up where Copilot stores it, and the command reads it
back from there once Copilot exits. To make that happen, the command sets
`storeTokenPlaintext` while it runs and strips that setting from what it
returns. A reconfigure starts already signed in, with the `PREV_` sentinel in
`COPILOT_GITHUB_TOKEN`. Keeping that sign-in comes back as `usePrevious`.
Every path is checked with a tool-free `copilot -p` before the command accepts
it.

### 3. Only github.com

A login whose `lastLoggedInUser.host` is not `https://github.com` is refused
during configure. A GitHub Enterprise Cloud (data residency) login also needs
`COPILOT_GH_HOST` set in the sandbox. Copilot has no settings key for the
host, and a configure command returns files and secrets, not env.

### 4. Policy is two switches, and lifecycle is a policy hook file

- `COPILOT_ALLOW_ALL=true` in the image's env trusts the directory Copilot
  starts in. Without it, an interactive launch stops on a folder-trust dialog
  that `--allow-all` does not skip. The launcher passes `--allow-all`, because
  the variable alone leaves an interactive session on manual approval. The
  sandbox is the isolation boundary, which is the same baseline the other
  images set.
- Hooks go in `/etc/github-copilot/policy.d/discobox.json`. Copilot loads that
  directory as **policy** hooks, which `disableAllHooks` does not turn off, and
  it reads them separately from the user's `config.json` and
  `~/.copilot/hooks`. A configure capture cannot replace them, and neither can
  the agent's own settings. (`/etc/github-copilot/managed-settings.json` exists
  too, but Copilot does not read `hooks` from it.)

### 5. The judge is a pinned model with no tools

`discobox-prompt --model judge` runs `copilot -p` on `claude-sonnet-5.5`, the
model claude-code's judge was measured on. `--no-tools` is
`--available-tools=<a name no tool has>`. An empty allowlist is ignored, and
every tool is offered. The run also gets an empty `COPILOT_HOME` and working
directory of its own, no custom instructions, no built-in MCP servers, and no
`COPILOT_ALLOW_ALL`. The token arrives through the environment, so there is
nothing to seed into that home.

## Rejected alternatives

- **The stored login, delivered as `config.json`.** A templated `authTokens`
  entry would carry the host, and with it Enterprise Cloud. It would also
  overwrite, on every launch, a file Copilot rewrites as it runs, in a format
  Copilot does not publish. The environment variable is the interface Copilot
  documents for exactly this "headless" use.
- **`/login` only, like the other harnesses.** It is the simplest flow, but it
  would make every Copilot discobox a holder of the account's full repository
  access by default.
- **A PAT only.** This is the least-privilege option. It was rejected because
  `/login` is the sign-in Copilot's own users know, and a PAT costs an extra
  trip to github.com's settings. Offering both, with the PAT first and a
  warning on `/login`, was the choice made.
- **A host on the secret** (`githubcopilot.com`), which a configure command
  can already return. Measured against Copilot 1.0.91, it fails twice over.
  Copilot sends the raw token to `api.github.com/copilot_internal/user` at
  startup, and with that refused and no cached user info it exits ("Failed to
  fetch OAuth user login (401)") before any model call. And the GitHub MCP
  server Copilot uses lives under `githubcopilot.com` (`/mcp`, `/mcp/readonly`)
  and acts on repositories with the same token, so the host would not have
  taken repository access away. A list of hosts does no better: the one
  Copilot needs, `api.github.com`, is the whole REST API.
- **Exchanging the token for a Copilot-only one.** Copilot 1.0.91 exchanges
  nothing: it sends the GitHub token as `Bearer` to the Copilot API. The legacy
  `api.github.com/copilot_internal/v2/token`, which older editor integrations
  used to mint a short-lived Copilot token, answered **403** to a
  Copilot-entitled account's token, with a Terms of Service warning. Getting
  past that would mean presenting as one of the clients GitHub admits.
- **Hiding the variable from everything but Copilot.** `gh` and git do not
  read `COPILOT_GITHUB_TOKEN`, and Copilot strips it from its own tools'
  environment, but Discobox exports env secrets into every terminal and exec,
  and the agent runs as Copilot's user with sudo: it can read any file or
  process environment the token could be put in. Placement would only hide
  the exposure.
- **`--reasoning-effort none` for the judge.** Copilot validates effort for
  each model (`supportedReasoningEfforts`). Pinning a level that the judge
  model turns out not to support would fail every judged command closed. The
  judge therefore reasons as its model does by default until
  `task eval:judge` measures the alternative.

## Consequences

- A `/login`-configured Copilot harness gives every discobox that uses it the
  account's GitHub reach. Configure says so. A PAT-configured one does not.
  Bounding a `/login` token is not this ADR's to solve. The path taken is to
  let a harness attach uses to its secret, so that every request carrying the
  sentinel goes to the request judge as an agent credential's does (a
  separate ADR), and to ask GitHub's Copilot team for a token Copilot can
  run on that reaches only Copilot.
- Enterprise Cloud data-residency accounts cannot use the included harness.
  Revisit this when a configure command can return non-secret env, or when
  someone asks for it.
- The judge model is pinned. If an account's Copilot plan or organization
  policy does not offer `claude-sonnet-5.5`, the judge cannot answer, and so
  every judged command is refused. This fails safe, but it is an outage for
  that account.
- Copilot has no setting or variable naming where it keeps memories, so this
  harness takes no part in source-scoped memory.
