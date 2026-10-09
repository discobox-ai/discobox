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
| Where does a sign-in land, in what shape? | the configure capture | claude/codex/opencode: a JSON file; copilot: a "managed automatically" `config.json` it rewrites |
| Is there a documented env var for the credential? Does it outrank the stored one? | env vs file delivery | copilot: `COPILOT_GITHUB_TOKEN` outranks a stored login |
| Which hosts receive the credential, and what else can it do there? | secret `host`, and whether to escalate to the user | copilot's token goes to `api.github.com` *and* `githubcopilot.com`, and carries `repo` |
| Does the token expire? Is there a refresh token, and a standard refresh endpoint? | `token` vs `oauth` secret | codex: OAuth refresh; copilot: OAuth App token, never expires |
| Is there a system/managed config layer the user cannot override? Does it carry hooks? | `Managed Layers` | `/etc/claude-code/managed-settings.json`, `/etc/codex/hooks.json`, `/etc/opencode/opencode.json` plugin, `/etc/github-copilot/policy.d/*.json` |
| What are the hook events, and their payloads? | `hooks.json`, canonical names, audit summary | read them off the CLI's own schema; record payloads with a recorder hook |
| What blocks an unattended interactive launch? | policy baseline | trust dialogs survive `--allow-all`-style flags more often than not — test in tmux |
| How is auto-update turned off? | the agent version store owns versions (ADR 0114) | env var or system config |
| What does it write on first run, and where? | volumes, caches | copilot unpacks 185 MB under `~/.cache` (already pool cache) |
| What is its tools-off switch, really? | `--no-tools` in the judge | an empty allowlist meant "no filter" in copilot; count the tools the fake model is sent |
| Where does it copy to? | OSC 52 vs the box's own X clipboard | codex needed `SSH_CONNECTION`; copilot writes OSC 52 always |
| Does it keep memories in a directory you can point elsewhere? | source-scoped memory | claude: managed setting; codex: bind mount; opencode/copilot: none |

## 2. Decide

Write these down before writing scripts; most become ADR sections.

1. **Slug, folder, image, hook provider.** Make them one name. `codex` vs
   `codex-cli` costs a special case in the Taskfile and the watcher forever.
2. **Credential secrets.** Name, `required`, `oneOfGroup` for alternatives,
   `type` (`token`, or `oauth` only when the control plane can run a standard
   refresh), and `host` (one host or none; a configure output may set it).
3. **Delivery.** `delivery: file` when the CLI reads metadata beside the token
   or prefers an env var that would defeat the file (claude-code, codex); the
   default env delivery when the CLI documents an env var for headless use
   and the stored-login file is its own churned state (copilot). Never ship a
   baseline credential file: an empty one reads as "signed in".
4. **Policy baseline**: auto-approve and trust, in a layer the configure
   capture cannot replace — a system config, an image env var, or a launcher
   flag. Not the user's settings file: configure replaces that wholesale.
5. **Hooks**: which system layer, and which events. Publish every event with
   `discobox-hook-publish --provider <id> --event <name>` (silent on stdout,
   always exits 0, so it is safe in decision hooks).
6. **Judge model** for `--model judge`: pinned (claude-code, codex, copilot)
   or the user's choice (opencode, where providers vary). A pinned id the
   account cannot reach refuses every judged command — say so in the ADR.

## 3. Build `harness/<id>/`

| File | Contract |
| --- | --- |
| `Dockerfile` | `FROM ${SANDBOX_AGENT_IMAGE}`; refuse an empty `HARNESS_METADATA`; `npm install -g` then `rm -rf /root/.npm`; install `agent.conf`, hook/system config, `configure.sh` → `/usr/local/libexec/discobox/configure-<id>`, `launch.sh` → `/usr/local/bin/discobox-harness-run`, `prompt.sh` → `/usr/local/bin/discobox-prompt` |
| `image.json` | `apiVersion`, `env` (auto-update off, policy env), `harness.{id,name,description,secrets,files,config.{command,reminder,ports}}`. No `runCommand` — the convention is the command. Every localhost callback port a sign-in uses goes in `config.ports` — its number when fixed, `{"ephemeral": true}` when it changes each run — with an `unavailable` message naming the fallback (device code, an API key) |
| `agent.conf` | `AGENT_PACKAGE`, `AGENT_BIN` for the pool-cached version store |
| `driver.go` | `ID()` and `Definition()` naming `harness.ImageRef("discobox-harness-<id>")`, `Configure: &harness.Configure{}` |
| `launch.sh` | join prompt words into one prompt; `--resume` → the CLI's resume; `exec` the CLI with the policy flags |
| `prompt.sh` | the `discobox-prompt` contract (copy a sibling's arg parsing); map roles; `--no-tools` = verified tools-off + an isolated home and empty cwd; capture, don't pipe; schema'd answers through `discobox-prompt-answer` |
| `configure.sh` | see below |
| hook config | in the CLI's own schema, at its system layer |

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
- Template harness files with dotted access (`{{ .secrets.NAME }}`), never
  `index .secrets "NAME"` inside JSON.
- Check what was *entered*, not which prompt it came in at. Copilot's PAT
  prompt takes any token Copilot accepts, gh's repo-scoped `gho_` included;
  a least-privilege prompt has to look at the token it got, warn on a broad
  one, and name the secret for what the token is.

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
  end of input. Check each test fails when the behavior it names is broken.

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
   point where the credential is entered (the CLI's own prompt). Waiting on a
   pane's text is enough; save every pane to `/dev/shm` for the report.
2. **run, under one `discobox-access run`**: type the credential into that
   prompt, finish the flow (the CLI's TUI, `/exit`, the script's own
   verification), then `discobox new --no-source -d -H <id> -p <one-line
   question with a checkable answer>` and wait on its screen for the answer.
3. Check the audit: `discobox admin audit hooks --discobox-id <box>` shows the
   image's hooks under its provider, and `--event Stop` (a canonical name)
   finds them.

What this needs, learned the hard way:

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
