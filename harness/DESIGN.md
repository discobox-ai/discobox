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
  The `Dockerfile` and the `image.json` beside it therefore go together: what
  the image installs and what it declares are one authoring unit, and every
  base-provided fact is restated by hand until there is a composition pattern
  for these manifests. `DISPLAY` and the `NIX_*`/`PATH` block are the current
  cost of that.
- Every `image.json` declares `DISPLAY=:0`, because the base image ships the
  socket-activated desktop (Xorg dummy on `:0`, openbox, x11vnc, websockify —
  see [`sandbox-agent/DESIGN.md`](../sandbox-agent/DESIGN.md)) unconditionally,
  and every harness extends that base. Without it nothing in a sandbox can open
  a window: `DISPLAY` reaches an exec only through `sandbox.json`'s env, which
  is where the image layer lands. It is safe to declare always because nothing
  runs until something connects — `xvfb.service` is `static`, pulled up on
  demand by `x11-display.socket` — so an unused `DISPLAY` starts no X server.
- `harnessMode: config` selects the image-owned interactive config command;
  normal or omitted mode selects the image-owned run/relaunch commands.

## Driver Model

- `harness.Driver` wires one harness provider's hook integration via
  `InstallHooks` and identifies its included image through `Definition()`. The
  public definition catalog is an image shortcut; runtime metadata comes from
  the registered image label and the copy inside that image.
- A `Definition` names its image through `harness.ImageRef`, never as a
  literal. One `ImageRegistry`/`ImageTag` pair backs all three, unset and
  `local` by default and overwritten at link time by a release
  (`Taskfile.yml`'s `release:binary`), so the built-in harnesses a binary seeds
  are the images that shipped with it. One pair rather than a reference per
  harness: a release publishes them together, and three independent references
  could disagree about which release a sandbox is running.
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
  The image bakes this file beside the Claude Code binary, making the hook
  definition an image-owned compatibility unit; the Go driver does not
  construct or merge Claude Code's hook format.
- Codex CLI: `/etc/codex/hooks.json`, baked into the harness image beside its
  `/etc/codex/config.toml` system layer. The hook definition and Codex binary
  are one image-versioned compatibility unit; the Go driver does not construct
  or merge Codex's hook format. Every configured lifecycle event invokes the
  generic publisher with its provider and event name while its stdin payload is
  stored unchanged. System hooks are treated as managed and trusted by policy.
  Codex's hook definition is likewise an image-owned compatibility unit.

Drivers must be idempotent and preserve unrelated settings where the harness uses
a single shared JSON object.

The same preference decides where a harness image's *policy* baseline goes when
the CLI has a system layer for it. The codex image bakes
`/etc/codex/config.toml` (`codex-cli/system-config.toml`) with
`approval_policy = "never"` and `sandbox_mode = "danger-full-access"` — the
sandbox is the isolation boundary, so Codex's own approval prompts and inner
sandbox would only guard a machine that exists to be written to. It is the
*system* layer and not the harness's `.codex/config.toml` file precisely because
that file is the user's: the configure flow captures whatever the user left in
it, and a baseline living there would be replaced by that capture on the first
reconfigure. The same layer selects Codex's `activity` and `thread-title`
terminal-title items. Codex 0.150 gives an unnamed thread a provisional title
from its first prompt, replaces it asynchronously with a generated title, and
preserves manual `/rename` values. Codex emits that thread title as OSC 0; the
sandbox agent observes it and Discobox uses it as the sandbox display name.

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

`codex-cli/configure.sh` follows the claude-code shape: it launches a bare,
interactive `codex` and lets the user sign in and configure it the way they
normally would, then inspects what codex itself wrote. Codex's onboarding
already offers every sign-in this flow would otherwise reimplement — ChatGPT in
a browser, **ChatGPT by device code**, or an API key — and all of them write
`$CODEX_HOME/auth.json`, so the script only has to read what the session left
behind. Device code is the one that works here and the banner says so: the
sandbox has no browser, and codex's browser flow completes against a callback
server on the sandbox's own localhost.

- **Both credentials are delivered as a file, never an environment variable**
  (`delivery: file`). This is not the tidiness argument claude-code makes; codex
  leaves no choice. The interactive TUI reads no credential variable at all
  (only `codex exec` honors `CODEX_API_KEY`), and a ChatGPT token has no
  variable to be read from in the first place. `~/.codex/auth.json` is the one
  delivery both halves of the harness agree on, so the flow returns it as a
  templated harness file with the sentinel in the credential's place.
- An **API key** sign-in leaves `{"OPENAI_API_KEY": "sk-…"}`, stored as a plain
  `bearer` secret (`OPENAI_API_KEY`).
- A **ChatGPT** sign-in leaves `tokens.{id_token, access_token, refresh_token,
  account_id}`, stored as an `oauth` secret (`CODEX_OAUTH_TOKEN`) with OpenAI's
  fixed token endpoint and client id and the access token's own `exp`, so the
  control plane refreshes it as it expires (see
  `resources/harnessconfigs/DESIGN.md` → OAuth secrets). Two details of the
  returned file follow from the credential living in the control plane:
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
- Reconfigure seeds the previous auth.json back with the `PREV_` sentinel in
  place of its template action, so the session opens **already signed in** and
  changing a model or theme costs no re-authentication. Detection is the same
  comparison claude-code makes: a value equal to the seeded sentinel proves
  nothing re-authenticated and comes back as `usePrevious`.
  - Codex has no `/login`; the way to change accounts from a signed-in session
    is `/logout`, which signs out **and exits**. So a session that comes back
    with no credential after being seeded is a deliberate logout, and the script
    says so and offers to start codex again rather than reporting a missed step.
  - Whatever the script does *not* seed, it removes. A configured harness
    delivers its own auth.json into the configure sandbox like any other file,
    but its template renders against secrets this sandbox does not have (they
    arrive `PREV_`-prefixed), so what lands is a credential-shaped file with
    nothing behind it — which codex reads as "signed in", skipping the sign-in
    screen the user came for. For the same reason the image declares **no**
    baseline auth.json: an empty one authenticates every sandbox with nothing.
- Verification runs `codex exec` against the auth.json as it stands, which is
  exactly what a run sandbox will do. There is no environment variable to point
  codex at one credential instead of another, so the file is the subject of the
  check and the environment is scrubbed of the variables `codex exec` would
  otherwise prefer over it.
- It returns **two files**: that auth.json, and `~/.codex/config.toml` as the
  user left it. Codex keeps settings and directory trust in one file, so
  returning it verbatim would make this throwaway sandbox's trust map the
  harness's. The `[projects]` tables are stripped and one templated stanza put
  back — the same one the image declares, trusting the sandbox's primary source
  wherever it lands. That is also why the script trusts the workspace before
  launching: without it the session opens on the trust screen instead of the
  sign-in screen.
