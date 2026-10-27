# 26-10-15-259 — The pi harness delivers auth.json, refuses ChatGPT subscription logins, and lets pi exchange Copilot tokens in the sandbox

- **Status**: Accepted (§5 superseded by [26-10-27-880](26-10-27-880-access-to-a-secret-is-a-binding-held-to-static-routes-and-no-judge-decides-it.md))
- **Date**: 2026-10-15

## Context

Discobox adds a harness for pi (`@earendil-works/pi-coding-agent`, 1.2.0
when this was written; formerly `@mariozechner/pi-coding-agent`, now
deprecated), the terminal coding agent omp descends from. Every fact below
was measured against that version, with a fake OpenAI-compatible model server,
a header-logging fake, a TLS-terminating proxy and throwaway homes, unless it
says it was read from pi's shipped source or docs.

pi is the opencode shape — many providers, signed in with `/login` — and
differs from omp, the closest harness, in ways that each change a decision:

- **It runs on Node** (`#!/usr/bin/env node`, Node ≥ 22.19); the base image's
  Node 25 serves. An install takes seconds and runs no download.
- **Its credentials are a file**, `~/.pi/agent/auth.json`: one entry per
  provider, `{type: api_key, key, env?}` or `{type: oauth, access, refresh,
  expires, …}`. A valid file is left byte-for-byte unchanged by a run, and by a
  run whose request was refused with a 401. An Anthropic login with a far-future
  `expires` sends its access token as `Authorization: Bearer` and never
  refreshes; an API key goes out as the provider's header.
- **A stored key can be a command**: a key beginning with `!` is run, and its
  output used (read from the docs). An API-key entry's `env` object outranks
  the process environment for that provider.
- **No system layer and no overlay.** pi reads `~/.pi/agent` (or
  `PI_CODING_AGENT_DIR`) and a trusted project's `.pi/`, and nothing under
  `/etc`; there is no `PI_CONFIG_FILES` equivalent (read from the source).
- **Its hooks are extensions**, as omp's are. A one-line `.js` file in
  `~/.pi/agent/extensions/` re-exporting an absolute path loads (measured).
  pi has no subagents and no `ctx.agent`. Event names and payload shapes match
  omp's for tools (`tool_call`/`tool_result` with `toolName` and `input`), but
  `before_agent_start.systemPrompt` is a **string** — the whole system prompt
  — where omp's is an array; a projection to scalar fields would publish it.
  `agent_settled` fires once a run will not continue (after retries,
  compaction and queued messages); `agent_end` fires per loop.
- **A project-trust dialog** blocks an interactive launch in any directory
  holding `.pi/` settings or resources, or `.agents/skills` in it or an
  ancestor (measured with `.agents/skills`). `--approve` trusts for one
  process. `~/.agents/skills`, where every discobox installs its skills, is
  read as the user's own skills, with no dialog. pi asks no approval before
  any tool call.
- **No first-run wizard** unless `PI_EXPERIMENTAL=1`; an update check only
  prints a notice (`PI_SKIP_VERSION_CHECK`).
- **It downloads `fd` from GitHub on its first interactive run** when neither
  `fd` nor `fdfind` is on PATH (measured; the sandbox image has `rg` but no
  `fd`).
- **Every provider honors `HTTPS_PROXY`**: pi installs undici's
  `EnvHttpProxyAgent` as the global dispatcher, and Anthropic, OpenAI and xAI
  each answered a real 401 through a discobox's egress proxy (measured). omp's
  `PI_PROXY` workaround has no counterpart here.
- **One-shot mode** (`-p`) waits for a non-terminal stdin to close and
  prepends it to the prompt. Each positional word is **a separate message**.
  `--` keeps a dash-led word a prompt but **not an `@`-led one**: any argument
  beginning with `@` is a file to attach, after `--` too, and a missing file
  fails the launch. A prompt is sent as given, leading space included.
- `--continue` with no session opens a new one.
- `--no-tools` offers the model zero tools. Context files (`AGENTS.md`,
  `CLAUDE.md`) are collected from the working directory **up to `/`**; an empty
  `.git` beside the run does not stop the walk, `--no-context-files` does.
- `--system-prompt` reads a file when its value names an existing path.
  pi has no `--flag=value` form: `--model=x` is taken for an extension's flag
  `model`. `--model` and `--system-prompt` take the next argument as their
  value whatever it starts with (read from the source).
- **ChatGPT subscription logins cannot run on a sentinel.** pi's
  `openai-codex` provider reads the account id from the access token's JWT
  claims and fails locally — "Failed to extract accountId from token" —
  before sending anything; a sentinel copies a token's shape, not its claims
  (measured). A stored `accountId` is not consulted.
- **GitHub Copilot logins are a token exchange.** pi signs in with VS Code's
  Copilot client, stores the GitHub token (`read:user`) as `refresh`, and
  exchanges it for a Copilot token of about 30 minutes, stored as `access`.
  The exchange is `GET https://api.github.com/copilot_internal/v2/token` with
  `Authorization: Bearer <GitHub token>` and no body; pi then sends the
  Copilot token to `api.individual.githubcopilot.com` (the host its
  `proxy-ep` names) and writes the new `access` and `expires` back to
  `auth.json`, `refresh` unchanged (measured through a TLS-terminating proxy).
  omp does not exchange: it signs in with another client and sends the GitHub
  token itself.
- The OpenAI "Sign in with ChatGPT" token-sharing login (`openai`) refreshes
  with a per-login client id and a `resource` parameter the control plane's
  refresh does not send (read from the source).
- Browser sign-ins call back to `localhost`: Anthropic 53692 (else any free
  port), OpenAI 1455, Radius 1456, OpenRouter any free port; each also accepts
  the redirect pasted back into pi. GitHub Copilot, xAI, Kimi and Meta sign in
  with a device code (read from the source).
- pi copies to the X clipboard, and also writes OSC 52 only over SSH or with
  `PI_CLIPBOARD_OSC52=1` (read from the source). A discobox has an X display,
  and the user's terminal is reached through the harness terminal, not SSH.

## Decision

### 1. Slug, image and runtime

The slug, folder, image (`discobox-harness-pi`) and hook provider are all
`pi`. The image installs `@earendil-works/pi-coding-agent` with npm and runs it
on the base image's Node, and installs `fd-find` so pi finds `fdfind` rather
than downloading `fd`. `agent.conf` names that package and `pi`.

### 2. Credentials are auth.json, delivered as a templated harness file

The configure flow runs a bare `pi`, where the user signs in with `/login`,
then reads `~/.pi/agent/auth.json`. Each provider becomes one secret,
`PI_<PROVIDER>_CREDENTIAL`, and the file comes back as a templated harness file
at `.pi/agent/auth.json` with each sentinel in place of its key or token — the
opencode delivery. pi keeps one entry per provider, so a secret is named by
its provider alone.

- An API key is a `token` secret, its entry kept with whatever else pi
  stored beside it (`env`). A key beginning with `!` is a command, not a
  secret: it is not captured, and configure says so.
- An Anthropic or xAI login is an `oauth` secret with its refresh material:
  the client and endpoint the claude-code image (and omp's) refresh for
  Anthropic, and the form-encoded client opencode's image refreshes for xAI.
  Its entry carries `expires` far in the future and a placeholder `refresh`,
  so pi never attempts a refresh the sandbox could not complete.
- A ChatGPT subscription login (`openai-codex`) is **not captured**: no
  sentinel can carry the claims pi reads from it. Configure says so and
  points at an OpenAI API key.
- A GitHub Copilot login's secret is its **GitHub token**, a `token` secret
  bound to `api.<domain>` (`api.github.com`, or a GitHub Enterprise domain's),
  delivered in `refresh` with `access` a placeholder and `expires` zero. **pi
  exchanges it inside the sandbox**: the sentinel travels in the exchange's
  `Authorization` header, the proxy swaps it there, and the Copilot token
  GitHub answers with — real, and good for about 30 minutes — lands in the
  sandbox's `auth.json` and goes to Copilot without the proxy's help. The
  GitHub token itself never enters the sandbox.
- Any other OAuth login is a `token` secret holding its access token, and
  configure says when it expires, because nothing will renew it.

The image declares no baseline `auth.json`.

### 3. Lifecycle is an image-owned extension, with a field list per event

The image installs `pi-hook-extension.js` beside its configure script, and
declares a one-line harness file, `.pi/agent/extensions/discobox-hooks.js`,
that re-exports it, as omp's image does. It publishes `session_start`,
`session_shutdown`, `before_agent_start`, `tool_call`, `tool_result`,
`session_before_compact`, `session_compact`, `session_compact_failed` and
`agent_settled` through `discobox-hook-publish --provider pi`. Each payload
is the fields named for its event — the prompt and its kind, a tool's name,
call ids, input and `isError`, a reason, `aborted` — and nothing else. A
projection by shape is not enough here: pi's system prompt is a scalar
string. `agent_settled` is `Stop`. pi has no subagents, so nothing is
filtered by session.

### 4. The policy baseline is the image's env and one launch flag

The image's env sets `PI_SKIP_VERSION_CHECK=1` (versions come from the
pool-cached store) and `PI_CLIPBOARD_OSC52=1` (a copy reaches the user's
terminal as well as the box's X clipboard). The launcher passes `--approve`:
the sandbox is the isolation boundary, as for every other image, and without
it pi's trust dialog stalls every launch in a repository that keeps skills in
`.agents/skills`. A launch prompt is the words joined into one, after `--`,
and given a leading space when it begins with `@`, which pi would otherwise
read as a file to attach.

### 5. The judge follows the user's model, isolated from what the agent wrote

`discobox-prompt --model judge` (and `fast`; pi has no model roles) uses
`judgeModel` from Discobox's settings for the harness
(`.config/discobox/pi-harness.json`), else `defaultProvider`/`defaultModel`
from pi's settings, else pi's own choice among the signed-in providers. The
providers are the user's choice, so no pinned model is one the harness can be
sure to reach (ADR 0127 §4, ADR 26-10-14-718 §5). Thinking is left at pi's
default.

With `--no-tools`, pi runs `-p --no-session --no-tools` with extensions,
skills, prompt templates, themes, context files and project trust off and
`--offline`, in a home of its own holding only the harness's credentials —
rebuilt from the delivered `auth.json` with each entry's type and key, or its
access token, refresh token and expiry, and nothing else: no `env`, no
`enterpriseUrl`, no key that is not made of token characters (so no `!`
command) — from an empty working directory, with no inherited `PI_*`, `XDG_*`
or `*_BASE_URL` variable, `--system-prompt` set to the caller's system text,
and stdin from `/dev/null`. The model's name is the one value read from the
user's configuration. pi has no `--model=NAME` form — it reads one as an
extension's flag — but takes the word after `--model` as its value whatever it
starts with, so the name crosses as that one word and never as a flag of its
own; the system text likewise follows `--system-prompt`.

## Rejected alternatives

- **Provider environment variables instead of auth.json.** pi documents one
  per provider for API keys, but OAuth logins mostly have none, and the
  configure script would carry pi's table of providers. The file pi writes is
  the one it reads.
- **Capturing ChatGPT subscription logins anyway**, as omp's harness does
  while it waits on a real account. Here the failure is measured and local:
  every request would fail before it left the sandbox.
- **Leaving Copilot out**, so no real token ever enters the sandbox. Rejected
  by the user: Copilot is worth having in pi, the real token in the sandbox
  lasts about 30 minutes, and the long-lived GitHub token stays out.
- **The control plane exchanging Copilot tokens.** Its refresh is a standard
  OAuth `refresh_token` grant; the Copilot exchange is a GET with the GitHub
  token as a bearer, answering a token of another kind.
- **Answering the trust question from the hook extension** (`project_trust`).
  It would trust every `pi` in the sandbox, not just the harness terminal's,
  and put policy in the file whose job is recording.
- **`defaultProjectTrust: "always"` in pi's settings.** That file is the
  user's, and configure returns it as they left it.
- **A projection to scalars, as omp's extension does.** pi's
  `before_agent_start` carries the whole system prompt as a string.
- **Returning `models.json` and `mcp.json` from configure.** pi keeps literal
  API keys and `!` commands in both, which would become plaintext harness
  files. Custom endpoints are configured in the sandbox instead.

## Consequences

- A Copilot login puts a real Copilot token, good for about 30 minutes, in
  the sandbox's `auth.json`, and pi renews it there through the proxy while
  the GitHub token's secret stands. A judge with a Copilot model exchanges
  its own in its isolated home. GitHub Enterprise Copilot is out of the
  judge's reach, since its endpoint is dropped.
- ChatGPT subscriptions are not reachable from pi in a discobox until a
  sentinel can carry the claims pi reads; the `openai` provider with an API
  key is.
- `models.json`, `mcp.json` and custom providers are not carried by the
  harness; a `judgeModel` naming a custom provider refuses every judged
  command.
- The hook file sits in the user's home, so the agent can remove it, and
  `--no-extensions` turns it off for one run. Revisit if pi gains a system
  layer.
- A `pi` started outside the harness terminal shows the trust dialog in a
  repository with `.agents/skills`.
- pi has no memory feature, so nothing is pointed at source-scoped memory.
