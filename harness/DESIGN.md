# Harness Design

This package owns the shared harness image contract and hook registration for
sandbox terminals.

## Image Contract

- One sandbox image contains at most one harness. Its immutable identity,
  run/relaunch commands (`runCommand` optional — omitting it declares that the
  sandbox resolves the user's login shell, ADR 0043 §2), seed files, secret
  declarations, optional config command, env defaults, and declarative volumes
  are published in the
  `io.discobox.image.v1` OCI image label (`harness.ImageMetadata`) for
  server-side registration. There is no baked-in file inside the image
  carrying this data — `image.json` is the build-time authoring source the
  label is compacted from (see `Taskfile.yml`'s `build:harness-image`), not a
  runtime artifact.
- The label is snapshotted onto the harness config at registration and
  re-snapshotted when the image's config digest changes — by `SeedBuiltIns` for
  built-ins, by `RefreshHarnessConfigImage` for user-registered images. A
  snapshot is a cache of a mutable tag's current contents, not a permanent
  record (ADR 0016).
- Harness CLIs are installed at image build time. Runtime commands are never
  supplied by the server or pool-agent.
- Each provider folder owns its `Dockerfile`, `image.json`, configure script,
  and other image-specific assets. Harness images extend the sandbox-agent base
  selected by the `SANDBOX_AGENT_IMAGE` build argument, and must repeat any
  base env/volumes they still need (the harness's own `image.json` is the
  final image's sole metadata source; nothing merges with the base image's).
- `harnessMode: config` selects the image-owned interactive config command;
  normal or omitted mode selects the image-owned run/relaunch commands.

## Driver Model

- `harness.Driver` wires one harness provider's hook integration via
  `InstallHooks` and identifies its included image through `Definition()`. The
  public definition catalog is an image shortcut; runtime metadata comes from
  the registered image label and the copy inside that image.
- A `Definition` sets `Configure` to enable an ephemeral sandbox the CLI
  runs interactively after registering a `HarnessConfig`. The configure process
  writes files and collected secret values to `ConfigureOutputPath`; definitions
  without interactive setup leave it nil. Configure files use the same
  home-relative contract as all harness files; configure commands run from the
  sandbox workdir and must use `$HOME` when invoking one of those files. Both
  included harnesses support config mode — see
  [Configure flows](#configure-flows).
- Provider-specific implementations live in one folder per harness:
  - `claude-code`
  - `codex-cli`
  - `shell` — the login shell, and the end of the resolution chain. Its
    Dockerfile installs nothing (the base image already ships the shell) and it
    declares no `runCommand`, no secrets, and no configure flow. It is otherwise
    an ordinary harness in every mechanism that touches it (ADR 0043).
- `registry` selects the driver from the image harness type ID, can install all
  drivers for hook/bootstrap workflows, and exposes
  `Definitions()` for the control plane to surface built-in harness configs.

## Managed Layers

Prefer managed or system-owned configuration layers so hook capture is not
subject to repo trust prompts or user/project override:

- Claude Code: `/etc/claude-code/managed-settings.json` on Linux/WSL.
- Codex CLI: `/.codex/hooks.json` as a system hook layer. System hooks are
  treated as managed and trusted by policy.

Drivers must be idempotent and preserve unrelated settings where the harness uses
a single shared JSON object.

## Configure flows

A configure command's contract is one fixed directory, two fixed paths in it,
and one env prefix:

- Both paths live under `ConfigureDir` (`/run/discobox/configure`), created by
  sandbox-agent in config mode, **owned by the sandbox user, mode 0700**. It is
  not `/run/discobox` itself: that is root-owned and holds the resolved secrets
  file, the proxy's CA bundles and trust env, and the control-plane and buildkit
  sockets, so a configure command running as a non-root user gets this one
  writable subdirectory rather than write access to all of them.
- It **writes** its result to `ConfigureOutputPath`
  (`<ConfigureDir>/harness-configure.json`).
- It **may read** the previous run's output from `ConfigurePreviousConfigPath`
  (`<ConfigureDir>/harness-previous-config.json`), seeded before the command
  starts. Same shape, but **no secret values** — it says which secrets exist, not
  what they are.
- Each of those secrets is offered as `ConfigurePreviousEnvPrefix` + its env
  name: a secret bound to `ANTHROPIC_API_KEY` arrives as
  `PREV_ANTHROPIC_API_KEY`.

**A credential never enters the sandbox.** `PREV_*` holds a sentinel that the
proxy swaps for the real value on an outbound request, and only while a live
grant covers it. So a configure command can *use* the old credential — that is
how it verifies one — without being able to read, print, or log it. Nothing
along this path decrypts a secret, and a revoked credential simply fails the
command's own verification.

The prefix is not decoration. Seeding `ANTHROPIC_API_KEY` itself would let the
harness CLI silently authenticate with the old credential: the flow would offer
a choice it had already made, and its verification would prove nothing about the
credential being configured.

Output is authoritative and replaces the previous configuration wholesale, so a
command that means to keep a file must re-emit it. To keep a *secret*, a command
returns it with `usePrevious: true` and no value; handing the `PREV_` sentinel
back as the value (`X=$PREV_X`) means the same thing, since storing a sentinel
as a credential would only configure the harness with something that resolves to
nothing.

The seed is therefore itself a valid output: writing it back unchanged is an
exact no-op reconfigure, which is the baseline a command edits rather than
rebuilds.

Every configure command should offer to keep an existing credential rather than
force a re-login, and should verify it first — it may have been revoked since.

The CLI applies returned files and encrypted secret bindings only after the
configure terminal exits successfully. No configure flow stores credentials in a
public harness file.

### Claude Code

`claude-code/configure.sh` launches a bare, interactive `claude` and lets the
user sign in and configure it the way they normally would, rather than
reimplementing an auth menu: Claude Code's own onboarding already offers the
same choice (Claude subscription vs. Anthropic Console account), and both
choices write their result to disk, so the script only needs to inspect what's
there once the user leaves the session (`/exit` or Ctrl-D).

- When the seed lists a secret whose `PREV_` variable is set, the session opens
  **already signed in**: `seed_previous_credential` writes that sentinel where
  Claude Code reads a credential, replaying the previously captured file so the
  seeded session carries the same scopes the real login had. There is no
  keep-or-replace question — the answer is whatever the user does in the
  session. Reconfigure is usually about a setting (model, theme, statusline),
  and a flow that either kept the credential without launching `claude` or
  launched it signed out made changing one cost a fresh login.
- Afterwards `detect_credential` decides what changed by comparing what it finds
  against the sentinel it seeded. The sentinel is a value the script chose, so
  finding it still in place proves nothing re-authenticated, and the credential
  is reported back as `usePrevious` rather than as a value. A *changed*
  credential wins over an unchanged one in either shape: signing in to a Console
  account leaves the subscription file untouched, so stopping at the first
  credential found would report "unchanged" and discard the account the user
  just switched to.
- A seeded credential that fails verification stops being seeded. Another round
  would sign the session back in with it and detect "unchanged" again, offering
  a retry that cannot succeed until the user signs in afresh.
- Otherwise the script prints an instruction banner and waits for Enter
  (`confirm_launch`) before starting anything. Two things the banner has to do,
  because the failure mode is a confused user rather than a broken script:
  - Say **this is configuration, not a session**. The user is dropped into a CLI
    they know, in a sandbox that is deleted the moment they leave, so the
    default reading — "my working session has started" — is the wrong one.
  - Separate the **required** steps (`/login`, then `/exit`; setup captures
    nothing without the first and cannot finish before the second) from the
    optional ones (`/model`, `/config`). Color carries that split — the heading
    and the required steps are emphasized, commands are cyan — degrading to
    identical wording when `NO_COLOR` is set or either stream is not a terminal,
    since this also lands in logs.

  The wait is the point: `claude` repaints the terminal as it starts, and
  increasingly runs full-screen, so a banner printed straight into a launch is
  gone before it can be read. It then runs `claude` under `script` (it needs a
  real TTY), and once the user exits, checks two locations in a fixed order:
  - `~/.claude/.credentials.json` — a subscription `/login` writes the
    rotating OAuth blob (access token + **refresh token** + expiry) here. The
    script returns that whole blob plus the fixed Anthropic
    `tokenUrl`/`clientId` as an `oauth`-typed secret
    (`CLAUDE_CODE_OAUTH_TOKEN`), so the control plane can refresh the access
    token as it expires (see `resources/harnessconfigs/DESIGN.md` → OAuth
    secrets). This is why the subscription path is `/login` and not `claude
    setup-token`: only `/login` yields a refresh token.
    The blob's `scopes` and `subscriptionType` are copied out with it. They are
    not credentials — they say what the login may do — and `/login` is the only
    moment they can be read. Claude Code gates Remote Control on finding
    `user:profile` recorded beside the token, so they are captured rather than
    assumed; guessing a scope the token lacks turns a clear refusal into a 401.
  - `primaryApiKey` in `~/.claude.json` — an Anthropic Console account login
    writes its long-lived managed key here. The script returns it as a plain
    `bearer` secret (`ANTHROPIC_API_KEY`), and a later sandbox gets its sentinel
    back **in the same field it was read from**. That rendering lives in the
    image's own `.claude.json` template rather than coming back from configure:
    unlike the subscription credential there is no captured metadata to replay,
    only the sentinel, so nothing needs to be carried across.

  Neither credential is exported as an environment variable. Both are declared
  `delivery: file` (`harness.SecretDeliveryFile`), so the sentinel is minted and
  rendered into the file the CLI reads, and the variable is withheld — a CLI
  that finds both prefers the variable, and the variable carries none of the
  metadata the file does.
  If neither is present, the script reports whether `claude` exited non-zero
  (it would not run) or cleanly (the user didn't sign in) and offers to relaunch
  it. Every retry goes through `confirm_retry`, so the loop only turns when a
  person asks it to: an attempt that fails before reaching the user fails again
  the instant it is retried, and looping on that is a busy loop rather than a
  retry. A candidate that fails verification is cleared
  (`clear_captured_credential`) before the retry, so a stale artifact from an
  earlier attempt in the same sandbox can't be mistaken for a fresh one.
- The configure sandbox has no source, so the image's `.claude.json` template
  trusts no directory. The script first merges `hasTrustDialogAccepted` for the
  workspace into `~/.claude.json`, so the interactive session (and the
  `claude -p` verification) run without stopping at the trust dialog. This
  touches only trust/onboarding, never a credential, and `.claude.json` is not
  returned as a harness file.
- The image's baseline `.claude/settings.json` sets
  `permissions.defaultMode: bypassPermissions`, which Claude Code refuses to
  honor as root. That is why the configure sandbox runs as a non-root account
  (`harness.ConfigureUserName`, uid `ConfigureUserUID`) rather than the image's
  root — see `resources/harnessconfigs/DESIGN.md` → configure flow.
- Every path ends in a `claude -p` check with only the chosen variable in the
  environment (and the credentials file moved aside), so a credential that
  cannot actually talk to the API never reaches a `HarnessConfig`. The script
  exits non-zero rather than looping when stdin is closed — at the keep/replace
  prompt or at `confirm_retry` — which fails the configure flow.
- It returns **one file**: a snapshot of `~/.claude/settings.json`, exactly as
  the user left it (theme, model, statusline, ... — whatever they touched
  during the session, or nothing, if they touched nothing). This is
  deliberately narrower than "return everything the sandbox has" —
  `~/.claude.json` is still never returned, since besides the credential
  already extracted above it carries this sandbox's own per-workspace trust
  map, which must not override a real sandbox's trust state. See
  `resources/harnessconfigs/DESIGN.md` for how a returned file actually
  reaches a later sandbox.

### Codex CLI

`codex-cli/configure.sh` collects one OpenAI API key without echoing it. When the
seed lists `OPENAI_API_KEY` and its `PREV_` variable is set, keeping the existing
key is the default choice. Every path ends in a `codex exec` check with the
chosen key in the environment; a failed check returns to the prompt. Nothing in
this flow performs a ChatGPT login, so there is no auth file to move aside as the
claude-code flow does.
