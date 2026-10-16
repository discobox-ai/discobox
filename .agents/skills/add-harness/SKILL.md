---
name: add-harness
description: Add a new built-in coding harness (a terminal agent CLI such as Claude Code, Codex, OpenCode or GitHub Copilot) to Discobox — probe the real CLI, decide credential delivery, hooks, trust, and the judge wrapper, build the harness/<id> image folder, wire it into the registry, Taskfile, watcher and docs, test it end to end on the nested dev server, and fold what it taught back into this skill. Use when the user asks to add, support, or package another agent CLI as a harness, or to change how an existing harness's image, configure flow, launcher, or discobox-prompt wrapper works.
allowed-tools: Bash, Read, Glob, Grep, Edit, Write, Agent, AskUserQuestion, Skill
metadata:
  argument-hint: "<cli name or npm package>"
---

# Add a Harness

A harness is an image, not Go code. A driver only names the image; everything
the harness *does* is declared in its manifest and done by shell scripts baked
into it ([`harness/DESIGN.md`](../../../harness/DESIGN.md), ADR 0086). So most
of the work is finding out, **from the CLI itself**, how it authenticates,
where it keeps state, how it is told to trust and auto-approve, and where a
system-owned hook layer lives — and then writing four scripts against what
you found.

Read `harness/DESIGN.md` first, and the closest existing harness end to end.
`harness/copilot` and `harness/opencode` are the most recent and the most
commented; `codex-cli` is the reference for an OAuth credential the control
plane refreshes.

## 0. Ground rules

- **This skill grows with every harness.** Each harness so far taught
  something this file did not say — a CLI quirk, a layer that looked right and
  was not, a test that passed vacuously, a step of the end-to-end run that
  needed the user. Keep a running list as you go, and fold it in before you
  finish (§7). A harness that ships without updating this skill leaves the
  next one to relearn it.

- **Verify every CLI fact against the installed binary**, and keep what you
  measured apart from what you remember. Vendor docs lag, model memory is
  about older versions, and a recalled endpoint stated as found is the most
  expensive mistake in this work. Write "measured" or "inferred" next to each
  fact in your notes, and carry the distinction into the ADR and the report.
- **Probe with fakes, not credentials.** A fake model server and a fake
  identity host answer almost every question. Ask for a real credential
  (`discobox-access`) only for what a fake cannot show, scoped to the literal
  calls. See [probing-a-cli.md](probing-a-cli.md).
- **Decisions about security posture are the user's.** If the CLI's
  credential reaches more than its model API (a GitHub token with `repo`,
  a cloud key), stop and ask with the trade-offs before designing around it.
- **An ADR is needed** whenever a plausible alternative was rejected for a
  non-obvious reason — and a new harness always has some (where the credential
  lives, which config layer, what the judge runs on). Probe first (§1), so
  the ADR records measured facts; then draft it `Proposed`
  (`docs/adr/README.md` for the ID scheme) and stop for the user to accept
  it. Accepting is the gate: build against an accepted ADR, and land the ADR
  as a commit of its own ahead of the harness. Amend it while nothing has
  shipped against it, as probing the real CLI keeps turning up facts.

## 1. Probe the CLI

Install it into the scratchpad (`npm install --prefix <scratch> <pkg>`), and
answer each of these with evidence. [probing-a-cli.md](probing-a-cli.md) has
the techniques.

| Question | Why it matters | How it went before |
| --- | --- | --- |
| How does it run a prompt interactively, and non-interactively? | `launch.sh` and `discobox-prompt` | claude/codex: positional; opencode: `--prompt`; copilot: `--interactive=` (the `=` keeps a dash-led prompt a prompt) |
| How does it resume, and what happens with nothing to resume? | `--resume` in the convention | `--continue`, `resume --last`; copilot opens fresh |
| Does a browser sign-in call back to a localhost port? Fixed, or a new one each run? | `config.ports` | codex: fixed 1455; opencode: fixed 1455 and 1456; copilot: a new one each run (`listen(0)`), so `{"ephemeral": true}` |
| Where does a sign-in land, in what shape? | the configure capture | claude/codex: a JSON file; opencode 2: rows in SQLite, reached only through `auth export`/`auth import`; copilot: a "managed automatically" `config.json` it rewrites |
| Does it run a shared background server? | launch flags, hook attribution | opencode 2: a per-user service by default; `--standalone` on every command, or one terminal's environment publishes every terminal's hooks |
| Is there a documented env var for the credential? Does it outrank the stored one? | env vs file delivery | copilot: `COPILOT_GITHUB_TOKEN` outranks a stored login |
| Which hosts receive the credential, and what else can it do there? | secret `host`, and whether to escalate to the user | copilot's token goes to `api.github.com` *and* `githubcopilot.com`, and carries `repo` |
| Does the token expire? Is there a refresh token, and a standard refresh endpoint? | `token` vs `oauth` secret | codex: OAuth refresh; copilot: OAuth App token, never expires |
| Is there a system/managed config layer the user cannot override? Does it carry hooks? | `Managed Layers` | `/etc/claude-code/managed-settings.json`, `/etc/codex/hooks.json`, `/etc/opencode/opencode.json` plugin, `/etc/github-copilot/policy.d/*.json` |
| What are the hook events, and their payloads? | `hooks.json`, canonical names, audit summary | read them off the CLI's own schema; record payloads with a recorder hook |
| What blocks an unattended interactive launch? | policy baseline | trust dialogs survive `--allow-all`-style flags more often than not — test in tmux, in a directory shaped like a real source: pi asks about any repository holding `.agents/skills` (this one does) until `--approve`, and reads a discobox's `~/.agents/skills` as the user's own |
| How is auto-update turned off? | the agent version store owns versions (ADR 0114) | env var or system config |
| What does it write on first run, and where? | volumes, caches | copilot unpacks 185 MB under `~/.cache` (already pool cache); pi downloads `fd` from GitHub into its home unless `fd` or `fdfind` is on PATH — install the distribution's package in the image |
| What is its tools-off switch, really? | `--no-tools` in the judge | an empty allowlist meant "no filter" in copilot; count the tools the fake model is sent |
| Where does it copy to? | OSC 52 vs the box's own X clipboard | codex needed `SSH_CONNECTION`; copilot writes OSC 52 always |
| Does it keep memories in a directory you can point elsewhere? | source-scoped memory | claude: managed setting; codex: bind mount; opencode/copilot: none |
| Is the credential store a file at all? Is there an export/import? | delivery | omp: SQLite, but `omp auth export`/`import` is a published document — template that, import at launch |
| Is there a system layer at all? Do settings layers merge lists or replace them? | where hooks and policy can live | omp: none; its `PI_CONFIG_FILES` overlay outranks the user but *replaces* any list it sets |
| What does one-shot mode do with a non-terminal stdin? With several words? With `@word`, after `--` too? | `prompt.sh`, `launch.sh` | omp and pi `-p` wait for stdin to close; each word is its own message; `@x` attaches a file — `</dev/null`, and the prompt after `--`. pi reads `@x` as a file even after `--`: a prompt that starts with `@` gets a leading space, which pi sends as it is |
| Does it take `--flag=value`? Does a flag consume a dash-led next word? | how the judge passes a model name the agent can write | copilot and omp take `--model=NAME`; pi has no `=` form (it reads one as an extension's flag) but its `--model` takes the next word whatever it starts with — read the argument parser, it decides which form keeps a value a value |
| Does every provider's client honour `HTTPS_PROXY`? | a sandbox's egress is the proxy | pi installs undici's `EnvHttpProxyAgent` globally and every provider answered through the proxy; omp's Anthropic client reads only `PI_PROXY` and otherwise connects direct, hanging every request in a discobox; fakes on `127.0.0.1` never see this, so make one real request through the sandbox's proxy before §6. Derive such a variable from `HTTPS_PROXY` in the image (a `/etc/profile.d` file the scripts also source): a loopback proxy address written into `image.json` is rewritten by a build in a discobox |
| What do its dependencies' install scripts fetch? | image build, version store | omp's onnxruntime-node downloads a CUDA build from GitHub on linux-x64 and hung the build: `ONNXRUNTIME_NODE_INSTALL_CUDA=skip` at build time and in the image's env. A scratchpad install that takes minutes is the warning |
| Does a first interactive run open a wizard? | unattended launch | omp's five-step setup wizard stalls a launch until `startup.setupWizard: false` |
| Where does it collect context files (`AGENTS.md`) from? What stops the walk? | judge isolation | omp walks up to a repository root, or `/` without one, so `/tmp/AGENTS.md` reached a judge in `/tmp`, and an empty `.git` stops it; pi walks to `/` past any `.git`, and only `--no-context-files` stops it — measure with a marker file above the judge's directory |
| Can a stored credential name its own endpoint, or run something? | judge isolation | omp: `apiEndpoint`, `enterpriseUrl`, a JSON key with `baseUrl`; pi: `enterpriseUrl`, a Copilot token's own `proxy-ep`, an API key's `env` object, and a key starting `!`, which pi runs as a command — the judge rebuilds credentials from an allowlist of fields, each of token characters (an empty refresh token too: OpenRouter's) |
| Does it take a thinking level of `off` for every model? What does it do with a model that must reason? | whether the judge can answer without reasoning | omp's ADR assumed `off` would fail models with no `off`; omp's source raises it to the model's lowest level instead, and its default with no flag is `high` — read the CLI's level handling before leaving the judge at its default, and have configure ask the judge one question with the level it will run at |
| Does a provider read anything out of the token itself? | whether a sentinel can stand in for it | a sentinel copies a token's shape, not its contents: pi's ChatGPT provider reads the account from the access token's JWT claims and fails locally on a sentinel, so configure leaves that login out and says why |
| Is a "refresh" a standard OAuth refresh, or an exchange? | `oauth` vs a sentinel the CLI exchanges itself | pi's Copilot login keeps a GitHub token as `refresh` and swaps it at `copilot_internal/v2/token` for a 30-minute Copilot token. The control plane cannot; read where the exchange sends the token (a header the proxy can swap, or a body it cannot) through a TLS-terminating proxy, and take the in-sandbox exchange to the user |

## 2. Decide

Write these down before writing scripts; most become ADR sections.

1. **Slug, folder, image, hook provider.** Make them one name. `codex` vs
   `codex-cli` costs a special case in the Taskfile and the watcher forever.
2. **Credential secrets.** Name, `required`, `oneOfGroup` for alternatives,
   `type` (`token`, or `oauth` only when the control plane can run a standard
   refresh), and `host` (one host or none; a configure output may set it).
   `rejectionScope` (`[{host, paths}]`) only when the provider refuses or
   accepts the login at endpoints that do not answer for it — a `401` for what
   the login may do, beside inference that works; name the endpoints that do
   (ADR 26-10-13-595). Otherwise leave it out.
3. **Delivery.** `delivery: file` when the CLI reads metadata beside the token
   or prefers an env var that would defeat the file (claude-code, codex); the
   default env delivery when the CLI documents an env var for headless use
   and the stored-login file is its own churned state (copilot). A store that
   is not a file is delivered as the CLI's own export document, templated, and
   imported by the launcher — never write the database yourself. omp imports
   the whole document on every launch, since each import replaces that
   provider's key and that account's login; opencode 2's import never updates
   an id it has, so its launcher imports under ids the image owns and changes
   only what differs. Never ship a baseline credential file: an empty one reads as
   "signed in". A login the CLI exchanges for a short-lived token (pi's
   Copilot) is delivered as the long-lived token's sentinel, bound to the one
   host it is exchanged at, with the short-lived field a placeholder already
   expired — only when the user accepts the short-lived token landing in the
   sandbox. A login nothing in a sandbox can use (one read from its claims,
   one with no key at all, like Bedrock's AWS profile, a key that is a
   command) is left out, and configure says so: a silent drop reads as "No
   provider is signed in".
4. **Policy baseline**: auto-approve and trust, in a layer the configure
   capture cannot replace — a system config, an image env var, or a launcher
   flag. Not the user's settings file: configure replaces that wholesale.
5. **Hooks**: which system layer, and which events. With none (omp), a
   harness file in the CLI's user extension directory that re-exports a
   module the image installs keeps the code the image's; never a settings
   list in an overlay that would replace the user's. Publish every event with
   `discobox-hook-publish --provider <id> --event <name>` (silent on stdout,
   always exits 0, so it is safe in decision hooks).
6. **Judge model** for `--model judge`: pinned (claude-code, codex, copilot)
   or the user's choice (opencode, omp, where providers vary). A pinned id the
   account cannot reach refuses every judged command — say so in the ADR.
   With the user's choice, configure asks the judge one question through the
   image's own `discobox-prompt`, in a home holding what a sandbox's would,
   so a judge that cannot answer is found before it is saved (omp).

## 3. Build `harness/<id>/`

| File | Contract |
| --- | --- |
| `Dockerfile` | `FROM ${SANDBOX_AGENT_IMAGE}`; refuse an empty `HARNESS_METADATA`; `npm install -g` then `rm -rf /root/.npm`; install `agent.conf`, hook/system config, `configure.sh` → `/usr/local/libexec/discobox/configure-<id>`, `launch.sh` → `/usr/local/bin/discobox-harness-run`, `prompt.sh` → `/usr/local/bin/discobox-prompt` |
| `image.json` | `apiVersion`, `env` (auto-update off, policy env), `harness.{id,name,description,secrets,files,config.{command,reminder,ports}}`. No `runCommand` — the convention is the command. Every localhost callback port a sign-in uses goes in `config.ports` — its number when fixed, `{"ephemeral": true}` when it changes each run — with an `unavailable` message naming the fallback (device code, an API key) |
| `boxd.yaml` | the Dockerfile's twin for disco-vm: the same install steps and paths; change the two together, and add it to `build:boxd-images` |
| `agent.conf` | `AGENT_PACKAGE`, `AGENT_BIN` for the pool-cached version store |
| `driver.go` | `ID()` and `Definition()` naming `harness.ImageRef("discobox-harness-<id>")` |
| `launch.sh` | join prompt words into one prompt, passed so the CLI cannot read it as flags or files (omp: after `--`); `--resume` → the CLI's resume; `exec` the CLI with the policy flags |
| `prompt.sh` | the `discobox-prompt` contract (copy a sibling's arg parsing); map roles; `--no-tools` = verified tools-off + an isolated home and an empty cwd that is its own repository root (an empty `.git`, where that is what stops the CLI's context walk; otherwise its own switch, pi's `--no-context-files`); stdin from `/dev/null`; credentials for the isolated home rebuilt from an allowlist of fields, never copied from a file the agent can write; capture, don't pipe; schema'd answers through `discobox-prompt-answer` |
| `configure.sh` | see below |
| hook config | in the CLI's own schema, at its system layer. A JavaScript hook (opencode, omp, pi) publishes an allowlist of events, and each payload is an allowlist too, never a list of field names to drop: the transcript travels under many names. Projecting by shape (scalars, a tool's input) holds only while nothing large is a scalar — pi's `before_agent_start.systemPrompt` is a string, so pi's extension names the fields it keeps per event; check every scalar of every published event's type |

**`configure.sh`** — copy the shape of `codex-cli` or `copilot`, which encode
lessons that each cost a bug:

- Banner says *this is configuration, not a session*, separates required from
  optional steps, honors `NO_COLOR`, and waits for Enter (a full-screen TUI
  erases it otherwise).
- Every `continue` in the collection loop is preceded by `confirm_retry`; end
  of input at any prompt exits non-zero rather than spinning.
- Reconfigure opens already signed in with the `PREV_` sentinel; finding the
  sentinel unchanged afterwards means `usePrevious`, never a value. A kept
  credential that fails verification stops being offered.
- Clear the CLI's stored credential before each round so a retry cannot
  capture an earlier attempt.
- Verify with a tool-free one-shot run using only the chosen credential, in
  an isolated home so a stored login cannot stand in for it.
- Return the user's settings file, minus anything the script set for its own
  sake; never return the CLI's state files or trust maps.
- With several accounts of one provider (omp), name each secret by which
  secret it is — its `PREV_` sentinel, then its account — never by its
  position in what the CLI exported; a re-login moves it.
- Template harness files with dotted access (`{{ .secrets.NAME }}`), never
  `index .secrets "NAME"` inside JSON.
- Check what was *entered*, not which prompt it came in at. Copilot's PAT
  prompt takes any token Copilot accepts, gh's repo-scoped `gho_` included;
  a least-privilege prompt has to look at the token it got, warn on a broad
  one, and name the secret for what the token is.

Moving a harness to a new major of its CLI is this whole skill again, against
the existing folder: probe, an ADR superseding the old one, and every
configuration captured under the old CLI keeps working without a reconfigure
(opencode 2 reads opencode 1's `auth.json` in its launcher). When the npm
package changes too (`opencode-ai` → `@opencode/cli`), `AGENT_PACKAGE`
changes, and the agent store replaces a pin into the old package's directory.

## 4. Wire it in

Every place the last harness touched (find them: `git grep -il <last-id> -- ':!harness/<last-id>' ':!docs/adr'`):

- `harness/registry/registry.go` and its test's ID list.
- `harness/hookevent.go`: a `canonicalHookEvents` entry per event with a true
  Claude Code counterpart, and none for the rest (ADR 0146 §3 — never copy a
  name Claude Code has not chosen; an event that fires on a retried error is
  not `StopFailure`).
- `internal/cmd/discobox-docker-image-watch/main.go` `harnessImages`, and its
  test's counts, names, env keys, parents and metadata args.
- `Taskfile.yml`: `build:harness-images`, the `build:harness-image` and
  `release:harness-image` descriptions, the Dockerfile COPY-path check list,
  and `release:images`.
- `.env.example` (`DISCOBOX_HARNESS_<ID>_IMAGE`).
- `cli/internal/cli/audit_list.go` `hookPayload`/`hookSummary` if the CLI's
  tool-hook payload has a new shape, with a case in `audit_test.go`.
- Docs: `harness/DESIGN.md` (driver list, managed layers, updater list, judge
  notes, memory, a configure-flow section), `README.md`,
  `sandbox-agent/DESIGN.md` (watcher's image list),
  `sandbox-agent/image/skills/discobox/SKILL.md` (harness row).

## 5. Test

In `harness/<id>/`, mirroring the siblings:

- **Launcher**: `launchertest.RunLauncher` — split words, quoted prompt, a
  dash-led prompt, no prompt, resume.
- **Hooks**: every expected event has exactly one publisher with a 1–3 s
  timeout; every published event has a canonical name or is listed as having
  none.
- **Manifest**: secrets, delivery, policy env, no command override, the
  Dockerfile installs each script where the contract says.
- **`prompt.sh`**: against a stub CLI that records argv, cwd, env and home —
  tools off, isolated home removed afterwards, empty cwd, the answer alone
  (fenced and unfenced), a failing CLI is a failing wrapper.
- **`configure.sh`, behaviorally**: rewrite `/run/discobox/configure/` to a
  temp dir, stub the CLI with the storage shape you measured and `script` as
  `exec sh -c "$4"`, drive stdin. Cover first sign-in, keep, replace inside a
  kept session, revoked keep, refused credential retried only when asked,
  end of input, and each login it leaves out saying why. Put the delivered
  credentials file in the home rendered with sentinels the sandbox was *not*
  given: rendered with none, its fields are empty and get skipped anyway, so a
  missing reset passes. Check each test fails when the behavior it names is
  broken — break it and run the test; most first drafts pass something.
- **Synthetic events in their real shapes.** A driver that fires a hook event
  with `{}` cannot see a payload that leaks the transcript; take the shapes
  from a recorded session or the CLI's own types. A stub store mimics the
  CLI's ordering too (omp moves a replaced login to the end).

Then: `go test ./harness/... ./internal/cmd/discobox-docker-image-watch/`,
the CLI audit test, `go tool task build:harness-image HARNESS=<id>`,
`go tool task ci:check`, and `go tool task check-hooks`.

## 6. Prove it end to end

Against the nested `task dev` server (`.agents/skills/test-fix/driving-task-dev.md`
— it is already running; always `--server http://127.0.0.1:8080`), with a real
credential the user grants through `discobox-access`. Copilot's run, which
passed, is the template:

1. **prep, no credential**: in a detached tmux pane, run the user's own
   command, `discobox admin harnesses configure <id>`, and drive it to the
   point where the CLI's TUI is up, ready for a sign-in. Waiting on a pane's
   text is enough; save every pane to `/dev/shm` for the report.
2. **run, under one `discobox-access run`** with a driver script on stdin:
   write the credential into the CLI's store in the configure sandbox on
   stdin (`discobox shell <configure-box> -- sh -c 'umask 077; cat > …'`,
   below), never typed into the TUI; finish the flow (the CLI's quit key, the
   script's own verification, its fixed status lines only), then `discobox
   new --no-source -d -H <id> -p <one-line question with a checkable answer>`
   and wait on its screen for the answer.
3. Check the audit: `discobox admin audit hooks --discobox-id <box>` shows the
   image's hooks under its provider, and `--event Stop` (a canonical name)
   finds them.

What this needs, learned the hard way:

- **The running image watcher predates a new harness.** Its list is compiled
  in, so it builds nothing for one it has never heard of, and the server
  falls back to the definition's `discobox-harness-<id>:local`. Build that
  with `go tool task build:harness-image HARNESS=<id>
  SANDBOX_AGENT_IMAGE=<the dev sandbox-agent tag from
  .tmp/discobox-dev-images.json>`: the `:local` base can be far older.
  Then tag it by content (`discobox-harness-<id>:dev-<12 hex of its image
  id>`) and set `DISCOBOX_HARNESS_<ID>_IMAGE` in `.env` to that: the dev
  server seeds only what `.env` names, and re-reads a manifest only when the
  reference changes — a rebuilt `:local` keeps the label it first read.
- **The box's own Claude login can stand in for a real one** — as far as
  the request. When the user says so, the sentinel in
  `~/.claude/.credentials.json` works one level down
  (`scripts/nested-claude-auth.sh` explains why). Put it where the CLI
  stores a login from inside the configure session (omp: its `!` shell
  escape, `omp auth import`), as a credential nothing will refresh, and the
  rest of the flow runs for real. Anthropic may still refuse the answer to a
  third-party harness ("Third-party apps now draw from your extra usage",
  a 400 — pi warns of it in its TUI first): then everything up to the model
  is verified, and an answer needs an API key. Ask for a workspace-scoped
  one: an organization-level key is refused without an
  `anthropic-workspace-id` header most CLIs never send.
- **A granted key goes into the configure sandbox on stdin**, never typed
  into the CLI's TUI: typing echoes it into the pane and the configure log,
  and the command judge refuses the run for it. `discobox --server … shell
  <configure-box> -- sh -c 'umask 077; cat > <the CLI's credential file>'`
  under `discobox-access run`, then the CLI's quit key in the pane; print
  only the configure log's fixed status lines (`grep -o`), never the log.
- **The grant's use is judged per request too.** The outer proxy judges each
  request the nested discobox's CLI sends with the key against the use, and
  refused pi's first model request (a 403 "blocked by proxy") under a use
  worded as "one short question". Word the use for what the CLI sends: its
  model requests from nested discoboxes, carrying its own system prompt and
  tools.
- **A model chosen in the configure session may not persist.** pi's
  `/model NAME` sets the session's model; only its picker saves a default.
  Read `settings.json` (or the CLI's equivalent) in the returned files, and
  say in the banner which gesture saves.
- **`terminal input` prints a resume point; pass it to `terminal wait
  --after`.** A one-line answer's `Stop` lands before a wait started after
  it, which then times out.

- **One run, five minutes.** `discobox-access run` mints a sentinel that
  lives about five minutes (`activationTTL`); the nested server stores *that*
  sentinel, so configure and the first model answer must both happen inside
  one run. Do everything slow (dev loop up, images built, pool ready, configure
  sandbox booted) before it.
- **Every host the CLI sends the credential to, in one request** (`hosts`,
  ADR 26-10-02-393) — Copilot needed `api.github.com` and
  `githubcopilot.com`. Missing one shows up as the CLI's own "authorization
  error", not as anything naming the proxy.
- **Pass a driver script on stdin** (`discobox-access run --use <id> -- sh -s
  run < script`). The command judge sees argv, stdin and the working
  directory, never a script file's contents, so `sh <file>` is opaque code to
  it and is refused; word the use as what the script does.
- **Ask for the use once, broadly enough** to cover a diagnostic re-run (the
  CLI with debug logging in the nested discoboxes). A use worded only for the
  happy path gets the diagnosis refused. After two refusals, stop and ask.
- **A failed nested pool is often a stale image tag** from a rebuild in
  flight; `discobox admin pool ls` shows it recover on its own.
- `discobox new` against the nested server enrolls an SSH key and adds an
  `Include` to the box's `~/.ssh/config`; say so in the report.

`go tool task eval:judge WRAPPER=harness/<id>/prompt.sh` measures the judge
model; report it, and anything else only a real account can confirm, as
unverified until it has run.

## 7. Fold what you learned back into this skill

Before you hand the harness back, go through the list from §0 and put each
item where the next harness will meet it: a probing question in §1's table, a
decision in §2, a contract in §3, a wiring site in §4, a test in §5, an
end-to-end step in §6, a technique or pitfall in
[probing-a-cli.md](probing-a-cli.md). Write the lesson, not the story — what
to do, and the one fact that makes it necessary. Correct anything here the
new harness proved wrong; do not leave an exception beside it. Then name the
harness among the precedents if it is now the clearest example of something.
