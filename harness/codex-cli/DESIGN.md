# Codex CLI Harness Design

The `codex-cli` bundle: OpenAI's Codex CLI on the shared harness contract.
What every harness shares is in [`harness/DESIGN.md`](../DESIGN.md); this is
what is particular to Codex.

## Image

- **`egressHosts`** is empty until Codex's hosts are established from its own
  documentation (ADR 26-10-01-032 §3), so a restricted project lists them
  itself.
- **The updater is off**: `check_for_update_on_startup` in the system config,
  since the agent version store is the version source.

## System layer

The codex feature lays `/etc/codex/config.toml` (`system-config.toml`), Codex's
system layer, and keeps there what Discobox decides for every box rather than
in the user's `.codex/config.toml`, which is the user's: save-back exports
whatever the user left in it, and a baseline living there would be replaced
by the first saved change.

- **The policy baseline** is `approval_policy = "never"` and
  `sandbox_mode = "danger-full-access"` — the sandbox is the isolation
  boundary, so Codex's own approval prompts and inner sandbox would only guard
  a machine that exists to be written to.
- **Terminal title.** The same layer selects Codex's `activity` and
  `thread-title` terminal-title items. Codex 0.150 gives an unnamed thread a
  provisional title from its first prompt, replaces it asynchronously with a
  generated title, and preserves manual `/rename` values. Codex emits that
  thread title as OSC 0; the sandbox agent observes it and Discobox uses it as
  the sandbox display name.
- **Memory.** The layer enables Codex's `memories` feature, both generation
  and use for new chats, and sets the generation rate-limit threshold to zero;
  a sandbox is short-lived enough that silently skipping its only background
  consolidation window would otherwise defeat source-scoped persistence (see
  [Source-scoped memory](../DESIGN.md#source-scoped-memory)). The launcher
  bind-mounts `.../users/<uid>/harnesses/codex/memories` onto
  `$CODEX_HOME/memories`. Codex rejects a symlinked memory root, so the
  launcher creates a real target directory and uses the sandbox user's
  passwordless mount grant. Its auth, user config, sessions, and SQLite
  databases remain in the sandbox's ordinary per-sandbox home; sharing
  `CODEX_HOME` would cross credential and session ownership boundaries. A
  pre-existing real memories directory is preserved and reported rather than
  overwritten.
- **Workspace trust** is recorded here too (see Credentials and settings).

## Hooks

The managed layer is `/etc/codex/hooks.json`, laid by the harness's feature
beside its `/etc/codex/config.toml` system layer. The hook definition and
Codex binary are one image-versioned compatibility unit; the Go driver does
not construct or merge Codex's hook format. Every configured lifecycle event
invokes the generic publisher with its provider and event name while its
stdin payload is stored unchanged. System hooks are treated as managed and
trusted by policy.

Codex adopted Claude Code's hook vocabulary wholesale, and each of the 11
events it shares is an entry in `CanonicalHookEvent`'s table mapping a name to
itself. `Interrupt` has no Claude Code counterpart and gets no canonical name.

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
  parsed `[projects]` tables. Codex's first-run bookkeeping, its
  model-availability tooltip state and its `tui.screen_reader_detection_done`
  flag, is local and is excluded from both current settings and delivered
  baselines.
- **Workspace trust.** Before every start the launcher records its actual cwd
  in `/etc/codex/config.toml`, under a system-layer lock. The system file
  combines the image's defaults with an inline TOML `projects` map, retaining
  previously trusted folders. Restarts, source-less boxes and alternate
  working directories use the same path. Later direct `codex` starts in those
  folders see the same trust, without launch flags or edits to portable user
  preferences. Trusting `/` does not trust child repositories. Trust is never
  exported into shared settings.
  The pinned-binary PTY test (`task test:codex-trust`) runs the old override
  as an untrusted control, then launch, restart and direct starts. It removes
  explicit approval/sandbox defaults from its fixture because those can hide
  a broken trust entry. Its Codex version is pinned in
  `test/codex-trust/version`; a vendor upgrade updates that contract test.
- Existing stored trust suffixes and credential references are converted in
  place by the server migration. Old sandbox bootstraps and capture checkpoints
  use the same bounded conversion on read. Unknown executable templates fail
  explicitly; migration never evaluates them or discards user preferences.

## Launcher

- **The launcher starts Codex again when it failed before its session
  began.** A signed-in Codex TUI exits with status 1 at bootstrap when its
  first requests cannot connect (`account/read failed ... workspace routing
  discovery failed`), with no retry of its own, and that ends the box's job. So
  the launcher runs `codex` rather than exec'ing it, and on status 1 with no
  rollout under `$CODEX_HOME/sessions` new or grown (a resume appends to its
  rollout) it starts it again, five attempts in all, waiting 2, 5, 10 and 20
  seconds. A new or grown rollout means the session began and holds the
  prompt, so it is never started again; a clean exit (how a user quits
  sign-in), a death by signal, and any other status (a usage error, a missing
  binary) are left alone. Rollouts are compared by name and size, not by time,
  so no clock has to agree with another.
- Resume is `resume --last`, which starts a new session when there is none
  (Codex 0.172.0).
