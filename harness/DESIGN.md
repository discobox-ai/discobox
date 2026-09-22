# Harness Design

This package owns the shared harness image contract (the manifest labels and
their layering, volume and env resolution, the configure-flow paths) and the
built-in harness images: their manifests, image-owned hook definitions,
launchers, and configure scripts.

## Image Contract

- One sandbox image contains at most one harness. Its identity, seed files,
  secret declarations, optional config command, env defaults, declarative
  volumes, supplementary groups (`additionalGroups`), and any command overrides
  are published in OCI image labels (`harness.ImageMetadata`) for server-side
  registration. There is no baked-in
  file inside the image carrying this data — `image.json` is the build-time
  authoring source a label is compacted from (see `Taskfile.yml`), not a
  runtime artifact.
- **A manifest is a stack of layers** (ADR 0086 §2). An image's effective
  manifest is `harness.ResolveImageLabels`: every
  `io.discobox.image.v1.<NN>-<name>` label in ascending order of that suffix,
  then the image's own `io.discobox.image.v1` label last. Docker inherits a
  parent's labels and a `LABEL` replaces only the key it names, so a layer set
  by `sandbox-agent/Dockerfile` is present on every image built from it at any
  depth. Layers merge by identity — `env` per key, `volumes`/`files` by path,
  `secrets` by name, groups by union; a harness's scalar fields, commands, and
  `config` go to the last layer that sets them — and none of them may unset.
  A layer is a fragment: only the merged result is validated, at registration.
  `00`–`49` is reserved for layers Discobox ships (`LayerNumberReserved`).
  The same inheritance carries `ReclaimLabel`, which marks every image built
  from the base as one Discobox may reclaim (ADR 0040).
- A declared volume says which primary volume backs it (`data` or `cache`),
  and `path` may use `%HOME%` and `uid`/`gid` `%UID%`/`%GID%`; `ResolveVolumes`
  expands them against the sandbox user and judges the path as a Linux path on
  every host. A cache path is per sandbox user unless it declares
  `scope: shared`, which is refused on a `data` path (`ValidateVolumeScope`,
  [ADR 0094](../docs/adr/0094-the-pool-cache-is-partitioned-by-the-sandbox-users-uid.md)).
  An env value's `%HOME%` is expanded by `ExpandEnvHomeTokens`, and left in
  place when the home is not yet known.
- A `data` volume may declare `excludeFromExport`: its backing directory, and
  every declared path beneath it, stays out of an export
  ([ADR 0129](../docs/adr/0129-the-sandbox-agent-reads-the-tree-an-export-carries.md) §2).
  The rule for setting it is that an export carries the discobox's work, not
  what was installed into it — a nested daemon's store or a package manager's
  prefix is excluded, the user's home and the agent's own state are not. It is
  refused on a `cache` path, which never travels (`ValidateVolume`, which also
  judges kind and scope, and which both registration and `ResolveVolumes`
  call). Absent means the path travels.
- **`TreeExportLabel`** says an image's sandbox agent has the export mode that
  reads a stopped sandbox's tree. `sandbox-agent/Dockerfile` sets it and every
  image built from it inherits it; the pool agent refuses to export a sandbox
  whose image lacks it, because an older agent reads the mode's argument as an
  ordinary start (ADR 0129 §3).
- A volume's `mode` is a POSIX mode word, and `ResolveVolumes` converts it to
  `os.FileMode` rather than casting: the two agree only on the low nine bits,
  and setuid/setgid/sticky sit far higher up in Go's encoding than in POSIX's.
  A cast loses them silently — nothing errors, and the path is simply created
  without the bit. That matters because setgid on a directory is the only way an
  image can hand a tree to a *group*, which is what it must do whenever the uid
  that will use the tree is not known until the sandbox boots
  ([ADR 0107](../docs/adr/0107-homebrew-is-image-content-on-an-overlay-handed-to-a-group.md) §2).
- **Extending `discobox-sandbox-agent` is required**, and the base layer proves
  it. Registration rejects an image carrying no `10-sandbox-base` layer, because
  the runtime contract — PID 1, systemd units, the runc wrapper — lives in the
  base image's filesystem, and an image that did not come from it cannot run a
  sandbox whatever it declares (ADR 0086 §1).
- **The harness command is a convention.** The image installs its agent as
  `/usr/local/bin/discobox-harness-run` (`harness.RunCommand`) and the runtime
  types `discobox-harness-run [--resume] '<prompt>'`. `runCommand` and
  `relaunchCommand` in a manifest are *overrides*, and nothing Discobox ships
  sets one. The convention is resolved at registration
  (`harnessconfigs.conventionCommands`), which is where the reserved `shell`
  slug is known — the sandbox knows its harness by the config's generated id
  and cannot tell a login shell from an image that declared nothing.
  The base image ships `discobox-harness-run` as a shim that execs nothing, so
  an image installing no agent lands the user at a prompt rather than at a
  `command not found`; a harness image overwrites it. See ADR 0086 §3.
- **A wrapper joins its prompt words back into one prompt.** The command is
  *typed* (ADR 0027), so the login shell splits it before the wrapper runs:
  `discobox new fix the failing tests` reaches `discobox-harness-run` as four
  arguments, not one. A prompt given as one argument — `discobox -p 'fix the
  failing tests'`, and everything the launcher creates — arrives as one, which
  the same joining leaves alone. Every wrapper joins everything after the flags with
  single spaces and hands its agent that single string — an agent CLI takes its
  prompt as one positional, so a wrapper that forwards `"$@"` unchanged asks it
  to "fix". This is the wrapper's half of the convention and part of what a
  third-party harness image signs up for; the runtime types the words as the
  user's shell split them and does not rejoin them, because what is on screen
  is an editable command line the user may extend. Each included `launch.sh` is
  tested by running it under a POSIX shell with a stubbed agent
  (`internal/launchertest`).
- **The prompt trails every launch**, relaunch included (ADR 0086 §4). A
  wrapper resuming a session ignores it — both included launchers replace it
  with their own resume flags — but because the command is *typed* (ADR 0027),
  a sandbox whose first launch failed still shows what it was asked to do, as
  an editable command line.
- The resolved manifest is snapshotted onto the harness config at registration
  and re-snapshotted later — by `SeedBuiltIns` whenever a built-in's image
  reference or digest has moved, and by `RefreshHarnessConfigImage` when an
  owner re-inspects a user-registered image. A snapshot is a cache of a mutable
  tag's current contents, not a permanent record (ADR 0016).
- Harness CLIs are installed at image build time. Runtime commands are never
  supplied by the server or pool-agent.
- Each harness folder owns its `Dockerfile`, `image.json` (when it needs one),
  configure script, and other image-specific assets. `harness/shell` needs
  none: it installs nothing, declares nothing, and *is* its inherited base
  layer.
- **A manifest argument is required, not optional.** `LABEL key=${ARG}` with no
  argument passed labels the image with the empty string, which is a build
  mistake that produces a working image nothing can register — so
  `sandbox-agent/Dockerfile` and every harness Dockerfile that declares a
  manifest fail the build when their argument is missing, rather than leaving it
  for a server to report at seed time. Any build path publishing these images
  (`build:*-image`, `release:images`, the development watcher) passes it.
- **The base layer** is `sandbox-agent/image.json`, beside the Dockerfile that
  labels it. It carries what is true of that image's filesystem rather than of
  any harness:
  - `DISPLAY=:0`, because the base ships the socket-activated desktop (Xorg
    dummy on `:0`, Xfce, x11vnc, websockify — see
    [`sandbox-agent/DESIGN.md`](../sandbox-agent/DESIGN.md)) unconditionally.
    Without it nothing in a sandbox can open a window: `DISPLAY` reaches an exec
    only through `sandbox.json`'s env, which is where the image layer lands. It
    is safe to declare always because nothing runs until something connects —
    `xvfb.service` is `static`, pulled up on demand by the proxy service
    `x11-display.socket` activates — so
    an unused `DISPLAY` starts no X server.
  - The three `/nix` volumes: `/nix` on `cache` with `scope: shared`, with
    `/nix/var/nix/profiles` and `/nix/var/nix/gcroots` carved back onto
    `data`. The store is pool-shared, so a closure one sandbox realizes is
    free for the next; the
    per-user profile state is not, because both are keyed by username and every
    sandbox in a pool runs the same user. The base image ships its own store
    aside and leaves `/nix` empty precisely so this cache bind hides nothing — a
    cache path is always a plain bind. See ADR 0075. `profiles` and `gcroots`
    are `excludeFromExport`: they point into `/nix`, which never travels.
    `nix-seed`'s per-sandbox stamp lives inside `profiles`
    (`.discobox-seeded`), so a restore that left the profiles behind re-seeds
    them rather than trusting a stamp that travelled without them.
  - The Homebrew prefix, `/home/linuxbrew/.linuxbrew`, as a `data` volume:
    the image ships content there, so boot wires it as an overlay and a
    sandbox's own installs persist. The tree is handed to the `brew` group
    rather than to a uid
    ([ADR 0107](../docs/adr/0107-homebrew-is-image-content-on-an-overlay-handed-to-a-group.md)).
    It is `excludeFromExport`: `brew install`'s results are the same kind of
    thing as `apt-get install`'s, and the image's own Homebrew is still the
    overlay's lower layer after a restore.
  - The **agent version store**, `%HOME%/.local/share/discobox/agents` on
    `cache`, and npm's download cache `%HOME%/.npm` beside it
    ([ADR 0114](../docs/adr/0114-a-sandbox-pins-its-agent-version-from-a-pool-cached-store.md)).
    The store holds one directory per agent version, so a sandbox can be
    pinned to one while the pool moves on; `~/.npm-global` is deliberately
    *not* cached, because npm's global tree is unversioned and is ahead of
    `/usr/local/bin`'s shims on PATH.
  - `/var/lib/docker` and `/var/lib/containerd` on `data`, `excludeFromExport`:
    the nested daemon's images, containers and volumes are rebuildable, and
    its snapshot store cannot be carried faithfully by a plain tar.
    `/var/lib/discobox` travels — it is the sandbox agent's own state.
  - The rest of the persistent and cached paths, the `brew`, `docker`, and
    `kvm` supplementary groups, the `NIX_*`/`HOMEBREW_*`/`PATH`/
    `NPM_CONFIG_PREFIX` env, and the pnpm `storeDir` seed file.
- **The agent version a sandbox runs comes from that store, not from the
  image** ([ADR 0114](../docs/adr/0114-a-sandbox-pins-its-agent-version-from-a-pool-cached-store.md)).
  A harness image declares what it installed in
  `/usr/local/libexec/discobox/agent.conf` (`AGENT_PACKAGE`, `AGENT_BIN`, and
  optionally `AGENT_IMAGE_DIRS`), and the base image does the rest from
  `/etc/profile.d/sandbox-agent-store.sh`: `discobox-agent-store pin` points
  `~/.local/bin/<bin>` at the newest version in the store, once per sandbox and
  never again, and a detached `refresh` advances the store at most twice a day
  per pool user. Launchers are untouched by this — the pin is ahead of
  `/usr/bin` on PATH, so `launch.sh` still just runs its agent — and an image
  with no `agent.conf` (the `shell` harness) is unaffected. Because the store is
  the version source, each image also turns its agent's own updater off:
  `DISABLE_AUTOUPDATER` in claude-code's env layer, `check_for_update_on_startup`
  in codex's system config, `OPENCODE_DISABLE_AUTOUPDATE` in opencode's env
  layer. `discobox-harness-upgrade` is the by-hand "newest, now".
- Every harness image provides **`/usr/local/bin/discobox-prompt`**, a one-shot
  prompting interface in-sandbox tools ask for a model through
  ([ADR 0079](../docs/adr/0079-a-local-judge-gates-every-wrapped-credential-use.md)):
  `discobox-prompt --model ROLE --system TEXT --prompt TEXT --output-schema JSON [--no-tools]`,
  answering on stdout and exiting 0 only when the model answered. `--model`
  names a *role* (`judge`, which pins a named model — except in the opencode
  image, whose providers are the user's choice; see [OpenCode](#opencode) — or
  `fast`), never a model id — the caller does not know
  what the image installed, so mapping the role is the wrapper's job, and it is
  version-coupled to a CLI the image pins the way the hook and launch wrappers
  are. `--no-tools` means the model answers from its prompt and executes
  nothing — no command, no file read, no network fetch
  ([ADR 0090](../docs/adr/0090-the-judge-is-handed-facts-and-given-no-tools.md))
  — and mapping it onto whatever the image's CLI calls the same thing is the
  wrapper's job too; a CLI with no tools-off switch maps it onto the strictest
  sandboxing it has instead. It goes on PATH rather than in `libexec` because
  its callers resolve it by name. Its first consumer is the credential CLI's
  judge, which will not run a wrapped command until a model agrees the command
  is the approved use, and which never omits `--no-tools`. The `shell` image
  runs no model, so its wrapper answers only the `judge` role, with a fixed
  verdict. It refuses, as the judge fails closed, unless a person set
  `DISCOBOX_SHELL_JUDGE=allow` for the discobox, which allows every judged
  command. It is a stand-in until the pool judge decides these requests; the
  host a credential may go to is enforced either way.
- `harnessMode: config` selects the image-owned interactive config command;
  normal or omitted mode selects the image-owned run/relaunch commands.
- **A config command may declare the ports its sign-in needs** (`config.ports`,
  `harness.ConfigPort`). A browser sign-in ends at a callback server on the
  CLI's own localhost, at a port the identity provider registered in advance —
  Codex's ChatGPT login redirects to `localhost:1455` and nowhere else — and
  inside a sandbox that server is one the user's browser cannot reach. The
  configure flow forwards each declared port from the user's machine into the
  configure sandbox **at the same number or not at all**
  (`portforward.Options.Exact`): the redirect URI is fixed, so a forward that
  landed on the next free port would look bound and answer no browser. A port
  the machine cannot give is reported in the image's own `unavailable` words —
  only the image knows what its harness can still do without the callback — in
  the configure terminal and, in the launcher, on the configure pane's header,
  where the CLI's full-screen sign-in cannot paint over it. See
  [`resources/harnessconfigs/DESIGN.md`](../server/internal/resources/harnessconfigs/DESIGN.md)
  for the snapshot and [`cli/internal/tui/DESIGN.md`](../cli/internal/tui/DESIGN.md)
  for the pane.

## Driver Model

- `harness.Driver` identifies one built-in harness's included image through
  `ID()` and `Definition()`, and nothing else. The definition catalog is an
  image shortcut — seeding reads only a definition's `ID` (the slug), `Name`,
  and `Image` (`harnessdefs.Seeds`); runtime metadata comes from the
  registered image label. A driver
  holds no behavior a harness image cannot declare for itself — that is what
  keeps a third-party harness a pure image-registration story.
- A `Definition` names its image through `harness.ImageRef`, never as a
  literal. One `ImageRegistry`/`ImageTag` pair backs every one, unset and
  `local` by default and overwritten at link time by a release
  (`Taskfile.yml`'s `release:binary`), so the built-in harnesses a binary seeds
  are the images that shipped with it. One pair rather than a reference per
  harness: a release publishes them together, and independent references
  could disagree about which release a sandbox is running.
- Whether a harness has an interactive configure flow is the image's
  declaration (`config.command`), snapshotted as the config's config command;
  a `Definition`'s `Configure` field (set by `claude-code`, `codex-cli`, and `opencode`, nil
  for `shell`) is read by nothing. The configure process writes files and
  collected secret values to `ConfigureOutputPath`. Configure files use the
  same home-relative contract as all harness files; configure commands run from
  the sandbox workdir and must use `$HOME` when invoking one of those files.
  Every included coding harness supports config mode — see
  [Configure flows](#configure-flows).
- Provider-specific implementations live in one folder per harness:
  - `claude-code`
  - `codex-cli`
  - `opencode` — opencode 1 (`opencode-ai`), the release its installer and docs
    install; opencode 2 (`@opencode/cli`) is a different program.
  - `shell` — the login shell, and the end of the resolution chain. Its
    Dockerfile installs nothing (the base image already ships the shell) and it
    has no `image.json` at all: no identity to declare beyond its reserved slug,
    no secrets, no configure flow, and its env and volumes are the base layer it
    inherits. The reserved slug is also what withholds the harness-run
    convention from it — a command typed into this terminal would be a second
    shell inside the first. It is otherwise an ordinary harness in every
    mechanism that touches it (ADR 0043, ADR 0086 §3).
- `registry` is the list of harnesses a release ships, exposed as
  `Definitions()` for the control plane to seed built-in harness configs. It is
  a manifest, not a dispatch table: nothing looks a driver up by harness type at
  runtime.

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

Every hook runs `discobox-hook-publish --provider <harness> --event <name>`,
the sandbox agent's generic publisher; no Go code in this package writes or
merges a harness's settings.

opencode publishes no hooks. Its lifecycle events reach JavaScript plugins
rather than commands, and hooks are recorded and read by nothing that derives
state from them (`sandbox-agent/agentstatus`), so the harness loses a log rather
than a behavior. Its policy baseline is a launch flag rather than a system layer
(see [OpenCode](#opencode)).

## Source-scoped memory

The runtime exposes opaque durable data for the primary source at
`/.discobox/data-per-source/primary`; only harness images interpret anything
beneath it. Claude Code and Codex launch through small image-owned wrappers and
keep their memory in separate namespaces; opencode has no memory feature to
point at it:

- Claude Code passes its supported `autoMemoryDirectory` launch setting as
  `.../harnesses/claude-code/memories`. Supplying it at launch keeps this
  storage invariant out of `.claude/settings.json`, which the configure flow
  deliberately replaces with the user's captured settings.
- Codex enables its `memories` feature in the system config and bind-mounts
  `.../harnesses/codex/memories` onto `$CODEX_HOME/memories`. Codex rejects a
  symlinked memory root, so the launcher creates a real target directory and
  uses the sandbox user's passwordless mount grant. The same system layer
  enables both generation and use for new chats and sets the generation rate-
  limit threshold to zero; a sandbox is short-lived enough that silently
  skipping its only background consolidation window would otherwise defeat
  source-scoped persistence. Its auth, user
  config, sessions, and SQLite databases remain in the sandbox's ordinary
  per-sandbox home; sharing `CODEX_HOME` would cross credential and session
  ownership boundaries. A pre-existing real memories directory is preserved
  and reported rather than overwritten.

When the primary source-data mount is absent — configure sandboxes and
source-less sandboxes — each wrapper launches the CLI unchanged. Pool-agent and
sandbox-agent know only the generic source-data mount and never a harness memory
format.

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
  and changing one should not cost a fresh login.
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
- Every round prints an instruction banner and waits for Enter
  (`confirm_launch`) before starting anything. Two things the banner has to do,
  because the failure mode is a confused user rather than a broken script:
  - Say **this is configuration, not a session**. The user is dropped into a CLI
    they know, in a sandbox that is deleted the moment they leave, so the
    default reading — "my working session has started" — is the wrong one.
  - Separate the **required** steps (`/login`, then `/exit`; setup captures
    nothing without the first and cannot finish before the second) from the
    optional ones (`/model`, `/config`). A seeded session says it is already
    signed in instead, and lists `/login` as optional, for switching accounts
    only. Color carries that split — the heading
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
    `token` secret (`ANTHROPIC_API_KEY`), and a later sandbox gets its sentinel
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
- Directory trust is the image's `.claude.json` template and nothing else. It
  trusts `.workingDir` — the directory the sandbox's terminals start in — so the
  configure sandbox, which has no source, opens on login rather than on the
  trust dialog for the workspace it is already sitting in. The script writes no
  trust map of its own: what it wrote would belong to a sandbox that is deleted
  minutes later, and `.claude.json` is not returned as a harness file. That is
  what makes this harness the simple case — nothing a configure run captures
  can overlay the template, unlike codex's `config.toml` below.
- `.claude.json` is `createOnly`, and home is a persistent data volume, so the
  template settles trust for a sandbox's **first** launch only. A sandbox that
  already has a `.claude.json` keeps it; upgrading its image does not rewrite
  it, and a change to the template reaches it only as one trust dialog, which
  Claude Code then records itself. Nothing repairs the file in place:
  `createOnly` says the harness owns the file after the first write, and
  reaching back into it is what that flag exists to forbid.
- The image's baseline `.claude/settings.json` sets
  `permissions.defaultMode: bypassPermissions`, which Claude Code refuses to
  honor as root. That is why the configure sandbox runs as a non-root account
  (`harness.ConfigureUserName`, uid `ConfigureUserUID`) rather than the image's
  root — see `resources/harnessconfigs/DESIGN.md` → Configured lifecycle.
- Every path ends in a `claude -p` check with only the chosen variable in the
  environment (and the credentials file moved aside), so a credential that
  cannot actually talk to the API never reaches a `HarnessConfig`. The script
  exits non-zero rather than looping when stdin is closed — at
  `confirm_launch` or at `confirm_retry` — which fails the configure flow.
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
already offers every sign-in this flow would otherwise reimplement — **ChatGPT
in a browser**, **ChatGPT by device code**, or an API key — and all of them
write `$CODEX_HOME/auth.json`, so the script only has to read what the session
left behind. The sandbox has no browser, so the banner says how each of the two
ChatGPT sign-ins gets there: the browser flow prints a link to open on the
user's own machine and completes against a callback server on the sandbox's
localhost:1455, which the image declares as a config port so the configure flow
forwards it in from the user's machine (see [Image Contract](#image-contract));
device code is the fallback the flow names when that port was taken, in the
image's `unavailable` message.

- **Both credentials are delivered as a file, never an environment variable**
  (`delivery: file`). This is not the tidiness argument claude-code makes; codex
  leaves no choice. The interactive TUI reads no credential variable at all
  (only `codex exec` honors `CODEX_API_KEY`), and a ChatGPT token has no
  variable to be read from in the first place. `~/.codex/auth.json` is the one
  delivery both halves of the harness agree on, so the flow returns it as a
  templated harness file with the sentinel in the credential's place.
- An **API key** sign-in leaves `{"OPENAI_API_KEY": "sk-…"}`, stored as a plain
  `token` secret (`OPENAI_API_KEY`).
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
  back — the same one the image declares, trusting `.workingDir`, the directory
  the sandbox's terminals start in.
  - The stanza is **guarded** on `.workingDir` rather than rendered bare. Unlike
    claude's, this file is persisted in the harness config and delivered to
    whatever sandbox uses it, which may run an image whose agent does not set
    that key; bare, `missingkey=zero` renders `[projects.null]` — valid TOML
    that silently trusts a project named `null`. Guarded, it degrades to no
    trust, a visible failure.
  - The script still trusts the workspace before launching (`ensure_workspace_trusted`).
    A configured harness delivers its captured `config.toml` into the configure
    sandbox, and a captured copy whose stanza does not follow `.workingDir`
    shadows the image's fixed template — in the very run that would refresh it.
    Whatever the script writes is stripped back out by `write_output`, so the
    returned file is the fixed one either way.
  - `ConfiguredFiles` overlay the image's `Files` by path, so fixing the image
    fixes nothing for a config already captured. The stanza this script used to
    write — trusting the primary source's target, so a source-less sandbox
    trusted nothing — is rewritten onto `.workingDir` in stored configs at
    server start (`server/internal/database` → `retrustConfiguredCodexWorkingDir`),
    keeping the old stanza as the fallback where `.workingDir` is unset: a
    stored config can name an image whose agent predates the key, and must not
    lose the trust it had. Changing the stanza again needs the same: a
    migration for each stanza it replaces, including that one, or every config
    captured before keeps it.

### OpenCode

`opencode/configure.sh` follows the codex shape — a bare interactive
`opencode`, then an inspection of the credentials file it wrote — for a harness
whose credentials are not one of two fixed kinds. opencode reaches every
models.dev provider, and a user connects as many as they like with `/connect`,
so the image declares no secrets at all: their names are only known once
something is connected, and the built-in seeds `Configured`, like any image
that declares none. See
[ADR 0127](../docs/adr/0127-the-opencode-harness-runs-opencode-1.md).

- **Credentials are one file**, `~/.local/share/opencode/auth.json`, keyed by
  provider: `{type: api, key}`, `{type: oauth, access, refresh, expires, …}`,
  or `{type: wellknown, key, token}`. opencode reads it on every use.
- **One secret per provider**, `OPENCODE_<PROVIDER>_CREDENTIAL`. The name is
  stable per provider, so a reconfigure that replaces a credential updates its
  secret in place.
- **The secret's type follows how the credential renews.** A key is a `token`.
  An OAuth sign-in whose `refresh_token` grant the control plane can perform —
  OpenAI (ChatGPT, the client and endpoint the codex image refreshes), xAI
  (whose endpoint takes the request form-encoded, recorded as
  `tokenRequestEncoding: form`) — is an `oauth` secret. Any other OAuth sign-in
  (GitHub Copilot, which never expires; DigitalOcean and Snowflake, which cannot
  be renewed from here) is a `token` holding its access token, and the script
  says when it expires.
- **Delivery is auth.json as a templated harness file**, as the codex image
  delivers its own: each entry with the sentinel in place of its key or access
  token, `refresh` a placeholder that can never be spent (or the sentinel, for
  Copilot, which authenticates with that field), and `expires` far future where
  the control plane renews the token, so opencode never attempts a refresh the
  sandbox could not complete. The image declares no baseline auth.json: an empty
  one would authenticate nothing while reading as configured.
- **The default model is checked, and a failure only warns.** `opencode run`
  with no `--model` runs on the model a sandbox starts on — the `model` setting,
  else the last `/models` pick, else opencode's own choice. Only that model is
  checked: whether a request succeeds depends on the model as much as the
  credential (a plan that excludes it, a region that needs opting in to), which
  opencode's model list does not show, so checking each provider on a model
  picked for it reports working keys as broken. A failure offers to start
  opencode again; declining saves everything as it is. Connecting nothing is
  allowed after a warning — opencode runs without a provider, on its free
  models or a local server.
- **Reconfigure seeds auth.json** from the previous one with each `PREV_`
  sentinel substituted, so the session opens connected. An entry whose key or
  access token is still that sentinel was not replaced, and comes back as
  `usePrevious` with its previous entry verbatim. Whatever is not seeded is
  removed, as codex removes an auth.json rendered against secrets the configure
  sandbox does not have.
- It also returns opencode's settings as the user left them — the global
  `opencode.json`, `opencode.jsonc`, `config.json`, and `tui.json` (theme,
  keybinds) — and `.local/state/opencode/model.json`, the `/models` choice, as
  `createOnly`: it seeds a sandbox's first launch and belongs to the sandbox
  after that.
- **The policy baseline is `--auto`** on the launch, approving every tool use a
  rule does not deny, and a launch prompt goes to `--prompt`, which the TUI
  submits. opencode's system layer, `/etc/opencode`, is its *managed*
  configuration, which outranks a project's own rules, and the user's global
  config is replaced by the captured settings — so neither is the place for it.
- **Discobox's own settings for the harness** live in
  `.config/discobox/opencode-harness.json`, a plain harness file the image
  declares and configure returns: `judgeModel` and `webSearch`. It is edited
  with `discobox admin harnesses edit opencode .config/discobox/opencode-harness.json`,
  and a reconfigure keeps whatever was edited into it.
- **Web search is asked during configure.** opencode searches with keyless Exa
  and Parallel, but only for models from its own providers (OpenCode Zen and Go)
  unless `OPENCODE_ENABLE_EXA`/`OPENCODE_ENABLE_PARALLEL` turn it on for every
  provider. The launcher reads `webSearch`: `true` sets both, `false` denies the
  `websearch` permission (the only way to turn it off for opencode's own
  providers; fetching a known URL is a separate permission and stays), and
  `null` — the image's baseline, before anyone configured — leaves opencode's
  default.
- `discobox-prompt` runs `opencode run`. **Tools-off is isolation**, not a
  permission rule layered over whatever is there: the caller may be the agent
  being judged, running as the same user in the same directory, so every source
  opencode would read is one it could have written — a config file's permission
  rules (merged before any override, and the last matching rule wins), a plugin
  (code in the process, whatever the permissions say), an `AGENTS.md` found
  from the working directory, a cached model catalog, any `OPENCODE_*`
  variable. With `--no-tools` the wrapper runs opencode from an empty directory
  with empty config, state and cache directories, project configuration and
  Claude Code files off, `--pure`, no inherited `OPENCODE_*` variable, one rule
  denying every permission, and a data directory of its own holding only the
  `api` and `oauth` entries of `auth.json` — a `wellknown` entry is a URL
  opencode fetches configuration from, past both switches. claude-code's wrapper
  reaches the same place with `--restricted`. As there, this keeps the judge out
  of configuration the agent wrote; it is not a boundary against an agent that
  sets out to defeat it, which has sudo (ADR 0090).
- **`judge` is not pinned.** It is `judgeModel` from the settings file, else the
  model opencode itself starts with: the `model` setting in its configuration
  (`opencode.jsonc`, `opencode.json`, then `config.json`), else the last
  `/models` pick, else opencode's own pick among the connected providers. The
  configuration is read as JSONC, as opencode reads it (comments and trailing
  commas, walked rather than pattern-matched). The named ones are read before
  isolation and passed as one `--model=NAME` word, so only the model's name
  crosses into the isolated run, and no value can become a flag of its own. The user chooses the providers
  here, so no fixed model is one the harness can be sure to reach, and a judge
  that cannot answer refuses every command — which is also why it follows
  opencode's default rather than a `/models` pick the default overrides. The
  named sources are files the judged agent can write, and so is `auth.json`,
  so an agent that set out to could move the judge to another model, or to a
  provider it added a key for. That trade is recorded in ADR 0127 §4.
- The configure image declares config ports 1455 (ChatGPT's browser sign-in)
  and 1456 (DigitalOcean's). A browser sign-in on a random port cannot be
  forwarded; those providers connect with a key.
