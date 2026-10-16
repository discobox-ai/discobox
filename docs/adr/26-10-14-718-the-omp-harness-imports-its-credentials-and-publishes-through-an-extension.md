# 26-10-14-718 — The omp harness imports its credentials at launch and publishes its lifecycle through an extension

- **Status**: Accepted (§5's judge thinking at its model's default superseded by
  [26-10-16-965](26-10-16-965-the-omp-judge-answers-with-thinking-off-and-configure-proves-it-can.md))
- **Date**: 2026-10-14

## Context

Discobox adds a harness for omp ("oh-my-pi", `@oh-my-pi/pi-coding-agent`,
18.11.0 when this was written), a terminal coding agent descended from
pi-mono. Every fact below was measured against that version, with a fake
OpenAI-compatible model server and throwaway homes, unless it says it was
read from omp's shipped source or docs.

omp is the opencode shape in most respects — many providers, any number of
them signed in at once — and differs from every harness so far in four ways
that each force a decision:

- **It runs on Bun.** The npm package's bin is a bundled script with
  `#!/usr/bin/env bun`, and the package requires Bun ≥ 1.3.14. The base image
  already installs Bun, and its native addon ships for linux-x64 and
  linux-arm64.
- **Its credentials live in SQLite**, `~/.omp/agent/agent.db`, not in a file
  a harness can template. A stored OAuth login outranks a provider's
  environment variable. omp has a published interface for moving them,
  `omp auth export` and `omp auth import <file>`: a versioned JSON document of
  `{provider, credential}` entries, where a credential is
  `{type: api_key, key}` or `{type: oauth, access, refresh, expires, …identity}`.
  Import merges: an API key replaces the provider's API key, and an OAuth login
  replaces the same account's login. Importing the same document twice leaves
  one entry per provider (measured).
- **Its hooks are JavaScript extensions**, not commands, as opencode's are
  plugins. An extension is a module whose default export calls
  `pi.on(event, handler)`. omp has **no system layer** for extensions or
  settings: it reads its user directory (`~/.omp/agent`), the project's
  `.omp/`, the user and project directories of Claude Code, Codex and Gemini,
  and the settings overlays named by `PI_CONFIG_FILES` and `--config`. The
  overlays outrank the user's and the project's settings, but a setting holding
  a list is **replaced** by a higher layer, not merged: an overlay naming
  `extensions:` silently dropped the user's own `extensions:` list (measured).
  The extension *directory*, `~/.omp/agent/extensions/`, is discovered
  separately and adds to whatever the settings name.
- **Its first interactive launch is a five-step setup wizard** (sign in, the
  default model, glyphs, prompt layout, theme) that waits for keys, so an
  unattended launch stalls on it. `startup.setupWizard: false` skips it
  (measured). Update checks only print a notice (`startup.checkUpdate`); omp
  never installs an update on its own.

Other measured facts the decisions lean on:

- Tools are auto-approved by default (`tools.approvalMode: yolo`), and
  `--auto-approve` forces it. No trust dialog appears for a new directory.
- `omp -p` with a non-terminal stdin waits for that stdin to close, even when
  given a prompt, and a launch given several words sends **each word as a
  separate message**. An argument beginning with `@` is read as a file to
  attach, and a missing file fails the launch. After `--`, every argument is
  literal text, dash- and `@`-led alike.
- `--continue` resumes the directory's last session and opens a new one when
  there is none.
- `--no-tools` offers the model no tools at all (zero `tools` in the request).
  It does not stop context files: `AGENTS.md`, `CLAUDE.md` and their kin are
  loaded from the working directory and every directory above it up to a
  repository root, so a judge run in an empty `/tmp` directory read
  `/tmp/AGENTS.md`. An empty `.git` directory beside the run stops the walk.
- With an empty agent directory, omp fetches its providers' model catalogs
  before the first request.
- Extension factories are rebound into every subagent session, and
  `ctx.agent.kind` says `main` or `sub`: a session's extension runner is
  given `sub` when it runs at a task depth above zero or under a parent task
  (read from the source; not measured at runtime). `session_stop` is the main
  session's turn ending, and omp says it never fires for a subagent.
- A stored credential is more than a secret. Some name the endpoint their
  requests go to: github-copilot's `apiEndpoint`, snowflake's and
  alibaba-coding-plan's `enterpriseUrl`, and alibaba-token-plan's key, which
  is a JSON document carrying a `baseUrl` (read from the source).
- Event payloads carry the conversation under many names: the system prompt
  and pasted images on `before_agent_start`, the transcript on
  `session_stop`, the entries being summarized on `session_before_compact`,
  the tool's output on `tool_result`.
- Browser sign-ins call back to fixed loopback ports: ChatGPT 1455 (no
  fallback port), Anthropic 54545, Gemini CLI 8085, Antigravity 51121,
  OpenRouter 54549, among others. Every one also accepts the redirect URL
  pasted back into omp instead (read from the source).
- omp's Anthropic transport is its own HTTP client, not Bun's `fetch`, and
  takes a proxy from `PI_PROXY` (or `PI_PROXY_<PROVIDER>`) alone: with only
  `HTTPS_PROXY` set it connects direct, which a sandbox's egress does not
  allow, and every Anthropic request hung until it failed as "Connection
  error." (measured in a discobox; with `PI_PROXY` set it answered).
- omp derives ChatGPT's `chatgpt-account-id` header from the access token's
  JWT claims and nowhere else. A sentinel is JWT-shaped but carries no claims,
  so with one in place no such header was sent (measured against the fake).

## Decision

### 1. Slug, image and runtime

The slug, folder, image (`discobox-harness-omp`) and hook provider are all
`omp`. The image installs `@oh-my-pi/pi-coding-agent` with npm and runs it on
the base image's Bun. `agent.conf` names that package and `omp`, so the
pool-cached version store manages its versions as it does every other agent's.

### 2. Credentials are an `omp auth export` document, imported at launch

The configure flow runs a bare `omp`, where the user signs in with `/login`
to any providers they like, then reads them back with `omp auth export`. Each
credential becomes one secret, `OMP_<PROVIDER>_CREDENTIAL`, so a reconfigure
updates it in place, as opencode's are. omp keeps several accounts of one
provider side by side and exports them in no fixed order, so a credential's
name is decided by which secret it is, not where it sits: one still holding a
previous secret's sentinel is that secret, kept; a new one for an account a
previous secret held (provider, `accountId`, `email`) replaces it; anything
else takes the provider's lowest free name, the second account `_2`.

- An API key is a `token` secret.
- An OAuth login whose refresh the control plane can perform is an `oauth`
  secret with its refresh material: Anthropic (the client and endpoint the
  claude-code image refreshes), ChatGPT (the codex image's), and xAI (the
  client opencode's image refreshes, whose endpoint takes the request
  form-encoded). A login the control plane cannot renew is a `token` holding
  its access token, and configure says when it expires, or that it has —
  for a kept login as much as a new one.

They reach a sandbox as one templated harness file,
`.config/discobox/omp-credentials.json`: the export document with each
sentinel in place of its key or access token, `refresh` a placeholder that
can never be spent — or the sentinel, where a provider stores the one token
in both fields (GitHub Copilot, whose token does not expire) — and `expires`
far in the future wherever the control plane renews the token, so omp never attempts a refresh the sandbox could
not complete. The launcher runs `omp auth import` on that file before it
starts omp. A sentinel imported this way goes out as omp's `Authorization`
header and is never refreshed (measured for an API key and for Anthropic and
ChatGPT logins). The image declares no baseline credential file.

### 3. Lifecycle is an image-owned extension, reached from the extension directory

The image installs `omp-hook-extension.js` beside its configure script, and
declares a one-line harness file, `.omp/agent/extensions/discobox-hooks.js`,
that re-exports it (measured: omp loads it). The logic stays version-coupled
to the image, as opencode's plugin is, and the file in the home only names it.
The extension publishes each event through `discobox-hook-publish --provider
omp --event <name>`, with a payload projected down to its scalar fields and
a tool event's `input`: the conversation is carried under too many names, and
gains new ones, for a list of what to drop to hold. Lifecycle events are published for the main session only; tool
events for every session.

### 4. The policy baseline is one overlay and two launch flags

`PI_PROXY`, the one proxy variable omp's Anthropic transport reads, follows
`HTTPS_PROXY`: `/etc/profile.d/omp-proxy.sh` sets it for a login shell, and the
launcher, configure command and `discobox-prompt` source the same file, since
not all of them run from one; a `PI_PROXY` the user set is left alone. The
image's env sets `PI_CONFIG_FILES=/etc/omp/discobox.yml`, an overlay
holding only scalar settings: `startup.setupWizard: false` and
`startup.checkUpdate: false`. Neither is a list, so the overlay replaces
nothing of the user's beyond those two keys. The launcher passes
`--auto-approve` (the sandbox is the isolation boundary, as for every other
image) and `--allow-home`, so omp starts where Discobox started it rather
than moving itself to a temporary directory. A launch prompt follows `--`
as one argument.

### 5. The judge follows the user's model, isolated from what the agent wrote

`discobox-prompt --model judge` uses `judgeModel` from Discobox's settings for
the harness (`.config/discobox/omp-harness.json`), else omp's `default` model
role (`omp config get modelRoles`), else omp's own choice among the signed-in
providers. `fast` uses the `smol` role. The providers are the user's choice
here, so no pinned model is one the harness can be sure to reach, which is
opencode's reason too (ADR 0127 §4). The judge reasons as its model does by
default: omp's thinking levels are per model, some offer no `off`, and a
level the model does not take would fail every judged command, as Copilot's
would (ADR 26-10-02-840).

With `--no-tools`, omp runs `-p --no-tools` in a home of its own holding only
the harness's credentials — rebuilt from the delivered file with each
credential's tokens, expiry, account, email and project and nothing else, and
no API key that is itself a document, since the agent can write that file and
an endpoint in it would point the judge at a server the agent runs — from an empty directory with an empty `.git`
beside it, with extensions, skills, rules, LSP, sessions and titles off,
`--system-prompt` set to the caller's system text, no inherited `PI_*`,
`OMP_*`, `XDG_*` or `*_BASE_URL` variable but a `PI_PROXY` set to its
`HTTPS_PROXY`, and stdin from `/dev/null`. The model's name is
the one value read from the user's configuration, and it crosses as one
`--model=NAME` word.

## Rejected alternatives

- **Credentials as provider environment variables.** API keys would work
  (each provider documents one), but OAuth logins mostly have none, a stored
  login outranks the variable, and the configure script would carry a table
  of forty-odd provider-to-variable names that omp already knows. One
  document, in omp's own export format, covers both kinds.
- **Writing `agent.db` directly.** It is omp's internal schema, and SQLite is
  not a file a harness can template. `omp auth import` is the published way in.
- **The hook extension named by the `PI_CONFIG_FILES` overlay** (`extensions:`).
  It is environmental and outranks the user, but it would replace the user's
  own `extensions:` list wholesale (measured). The extension directory adds to
  the list rather than replacing it.
- **The hook extension as a launch flag** (`--extension`). It adds without
  replacing, but only for the launcher: an `omp` typed into another shell
  would publish nothing. The directory reaches every `omp` in the sandbox.
- **Forcing `tools.approvalMode: yolo` in the overlay.** It would outrank the
  user's own `tools.approval` rules as well as the mode. The launch flag forces
  the mode for the harness terminal and leaves the rules standing.
- **`PI_PROXY` as a literal in the image's env, or in the pool's proxy env.**
  The egress proxy is the pool's to name, and a copy in the manifest would
  drift from it — and was rewritten: a build in a discobox retargets a
  loopback proxy address in the build arguments it passes on, the manifest's
  label among them (ADR 26-10-01-425), so the image recorded the build
  network's address. The pool's env holds no variable that only one harness
  reads; pool code knows no harness.
- **omp's own `judge` model role.** That role selects judgment-API models
  (TypeSafe and the like), not chat models, so it is a different job that
  happens to share the name.
- **Copying `models.yml` or the catalog cache into the judge's home.** Both are
  files the judged agent can write, and either can point a provider at
  another endpoint, so the agent could answer for the judge. The judge sees
  the built-in providers alone, and pays a catalog fetch on each run for it.

## Consequences

- A credential reaches omp only through the launcher's import. An `omp`
  started before the harness terminal's first launch has nothing signed in.
  An import runs on every launch, so a login a sandbox replaced in place is
  restored on the next launch.
- The hook file sits in the user's home, so the agent (or a user) can remove
  it, and `--no-extensions` turns it off for one run. claude-code's and
  Copilot's hooks survive both. Revisit if omp gains a system layer.
- Custom providers in `models.yml` are not reachable by the judge, nor are
  providers that need an endpoint from their credential (GitHub Copilot
  Enterprise, Snowflake, the Alibaba plans), and a `judgeModel` naming one
  refuses every judged command.
- **ChatGPT sign-ins are unverified.** With a sentinel in place of the access
  token, omp sends no `chatgpt-account-id`. Whether ChatGPT's backend answers
  without it needs a real account; if it does not, ChatGPT is reachable here
  with an API key only, until the control plane can deliver an access token
  whose claims survive.
- omp has no setting this harness points at source-scoped memory; its memory
  backends are off by default and live in the sandbox's own home.
