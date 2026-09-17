# 0127 — The opencode harness runs opencode 1

- **Status**: Accepted
- **Date**: 2026-09-17

## Context

Discobox ships an opencode harness. opencode reaches every models.dev provider,
and a user expects to connect whichever of them they have accounts with and use
them all — a different shape from the two harnesses already included, each of
which has exactly one credential of one of two fixed kinds, declared by name in
its image manifest.

Two opencodes are published, and they are different programs:

- **opencode 1** (`opencode-ai`, 1.18.x) is what opencode's installer and docs
  install, and what every GitHub release is. It keeps every provider
  credential in one file, `~/.local/share/opencode/auth.json`, keyed by
  provider — an API key, or an OAuth token set — and reads it on every use.
- **opencode 2** (`@opencode/cli`) went to 2.0.0 on 2026-09-11 and shipped five
  more releases in its first five days, on its own channel and docs. It keeps
  credentials as rows in its SQLite database, with no supported way to create
  an OAuth credential from values, and its TUI's `--prompt` only fills the
  input box.

The harness was first built on opencode 2. It worked, but only by writing
opencode's database schema directly from the launcher, starting sessions
through its server to get a prompt submitted, and waiting out a race in which a
prompt sent before providers loaded was silently lost — each a workaround
against a CLI releasing daily.

Discobox's constraints are the usual ones: a credential never enters a sandbox
(the proxy swaps a sentinel for it in request headers), a rotating token is
refreshed by the control plane (ADR 0011), and a configure command returns what
the sandbox needs as files and secrets (ADR 0009). The configure output
contract already accepts secrets the image never declared.

## Decision

### 1. The harness installs opencode 1

`opencode-ai`, pinned per sandbox by the agent version store like the other
harnesses (ADR 0114).

### 2. One secret per provider, delivered in auth.json

The configure command runs a bare interactive `opencode`, lets the user
connect providers with `/connect`, and reads `auth.json` once they exit. Each
provider becomes one secret, `OPENCODE_<PROVIDER>_CREDENTIAL`, and the command
returns `auth.json` as a templated harness file with each provider's sentinel
in place of its secret — the delivery the codex image already uses for its own
`auth.json`. A key is a `token` secret. An OAuth sign-in whose `refresh_token`
grant the control plane can perform (OpenAI, xAI) is an `oauth` secret, and its
entry carries a far-future `expires` so opencode never tries a refresh the
sandbox could not complete; any other OAuth sign-in is a `token` holding its
access token.

### 3. The control plane refreshes form-encoded as well as JSON

`SecretValue` gains `tokenRequestEncoding`: absent is JSON, as every OAuth
secret stored before it was refreshed; `form` is
`application/x-www-form-urlencoded`, which RFC 6749 §6 defines and xAI's
endpoint takes. It is recorded per secret, at capture, because it is a fact
about the authorization server. It is never negotiated at refresh time: a
refresh token rotates on use, so retrying a rejected request in the other
encoding risks spending the token on the first attempt.

### 4. The judge is the user's model, isolated from everything else

`discobox-prompt --model judge` runs on `judgeModel` from Discobox's settings
file for the harness (`.config/discobox/opencode-harness.json`, edited with
`discobox admin harnesses edit`), else the last `/models` pick, else opencode's
own pick among the connected providers. ADR 0079's wrappers pin a named judge
model; that assumes a model the harness can always reach, which a harness whose
providers the user chooses does not have, and a judge that cannot answer
refuses every command.

Both named sources are files the judged agent can write, and so is `auth.json`,
so an agent that set out to could move the judge to another model, or to a
provider it added a key of its own for. This is accepted knowingly: the agent
has sudo, so no file in the sandbox holds against a deliberate attempt (ADR
0090), and pinning would buy that only by a judge that may be unreachable.

What the wrapper does guarantee is that the judge does not run inside
configuration the agent wrote. With `--no-tools` it runs from an empty
directory, with empty config, state and cache directories, project
configuration, Claude Code files and plugins off, no inherited `OPENCODE_*`
variable, and a data directory of its own holding only the `api` and `oauth`
entries of `auth.json`. A `wellknown` entry is left out because it is a URL
opencode fetches configuration from, which neither switch stops.

### 5. Configure asks whether opencode may search the web

opencode searches with keyless Exa and Parallel, but only for models from its
own providers unless environment variables turn it on for every provider. The
configure command asks, records the answer as `webSearch` in the same settings
file, and the launcher sets those variables — or denies the `websearch`
permission, the only way to turn search off for opencode's own providers.

## Rejected alternatives

- **opencode 2.** Rejected for now, for the reasons in Context: its credential
  store has no supported write path for OAuth, its `--prompt` does not submit,
  and it changes daily. Revisit when opencode's installer installs it. opencode
  2 imports `auth.json` when it first creates its database, which is a
  migration path for a harness configured under this decision.
- **Environment variables only.** Each provider's conventional variable, bound
  to its secret, needs no file at all — and carries no OAuth sign-in. A ChatGPT
  or Copilot subscription is a common reason to reach for opencode.
- **A judge model the agent cannot write**, baked into the image: the strongest
  gate, but changing it means rebuilding the image.
- **opencode's own pick as the judge**, isolated from the `/models` choice: the
  agent could not move it, but it is not the model the user chose.
- **A fixed list of judge models**, the first one a connected provider serves.
  It keeps the judge off the user's preference, and fails for any user whose
  providers serve none of them — silently falling back to the default model
  anyway, or refusing every command.
- **`OPENCODE_AUTH_CONTENT`**, the variable that replaces `auth.json` whole. It
  would have to be one secret holding every provider's JSON, which cannot be
  refreshed per credential or delivered as sentinels.

## Consequences

- opencode 1 will be superseded. When the harness moves to opencode 2, this is
  superseded, and the workarounds in Context are what that harness must answer.
- The image declares no secrets, since the names are only known once a user
  connects something, so the built-in seeds as `Configured`. opencode runs with
  no provider connected, on its free models or a local server.
- A token-typed OAuth credential stops working when its token expires, and the
  fix is a reconfigure. The configure command says which providers that is.
- `auth.json` is a harness file, rewritten whenever a terminal launches the
  harness, so an account connected by hand inside a sandbox is replaced by the
  configured file at the next launch — as codex's is.
