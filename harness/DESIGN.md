# Harness Design

This package owns the shared harness bundle contract (the manifest labels and
their layering, volume and env resolution, the configure-flow paths) and the
built-in harness bundles: their manifests, harness-owned hook definitions,
launchers, and configure scripts.

## Saving local configuration

The six coding harnesses declare `harness.saveBack` (ADR 26-10-26-573): watched
home-relative paths, installed export/handoff commands, settings exclusions,
credential-file schemas, and authoritative provider metadata. Each built-in coding
harness bundle carries `capture/runtime.cjs`, `capture/settings.cjs`, and the parser
licenses beside its metadata and normalization module. The generic platform
ships none of these: export/handoff are arbitrary installed commands, not a Node
API. These six built-ins use Node through their own feature dependency. Interactive configure and background capture share the
normalizers. Configure still owns onboarding and verification.

Export is noninteractive JSON over stdin/stdout. It preserves ordinary settings,
omits the harness-declared credential/local paths, and converts supported local
logins to secret values plus safe
credential documents. Unknown providers, credential commands, unsafe metadata,
and malformed inputs produce value-free diagnostics and protect local credential
files. Cleared Claude OAuth tokens represent sign-out and leave delivery
restorable; a
remaining refresh token or unsupported local login stays protected. Exporter
failures emit fixed diagnostic codes, which the agent alone allowlists into its
log; exception messages and stacks never leave the exporter. Omission never
revokes a login. `BaselineOnly` reads the delivered,
rendered baseline, not current disk settings. Current sentinels and privately supplied prior aliases become `keep`
bindings. An old delivered alias remains a reference while runtime reminting
converges; its exported document names the binding, so ordinary repair can install
the current sentinel. A sentinel belonging to another provider is never new
secret material.
Sentinels have vendor-compatible shapes, so only exact membership can identify
them; the server also rejects values matching its assignments.

Reconsideration is a pure projection of the immutable candidate and the current
server credential documents: only selected credentials replace entries, while other current
bindings remain. It never reads a newer local login. A shared credential document
is rebuilt together with its binding selection; portable file selection cannot
separately select an auth document.

Settings preserve unknown preferences, nested objects, arrays, hooks and custom
provider configuration rather than maintaining a second copy of each vendor's
preference schema. Each manifest names known credential-bearing paths to exclude
(including inline API keys and mixed environment/header maps), as well as local
state such as trust and recent-model history. Exclusion patterns match actual
object path segments, with `*` for one and `**` for any depth. This is known-shape
normalization, not a scanner for secrets embedded in arbitrary strings.
Credential files still use strict schemas and sentinel references; they are never
ordinary settings. Recognized exclusions do not themselves create diagnostics.
JSON/JSONC accepts comments and trailing commas where the CLI does. TOML and YAML
use the pinned `smol-toml` and `yaml` parsers, bundled into `settings.cjs` by
`go tool task generate:harness-settings`; its source is `settings-source.cjs`.
The checked-in parser bundle and `settings-LICENSE.txt` ship with each coding
harness, so export
requires no package installation. Parse failures retain prior state. Older
settings allowlist definitions require a harness metadata refresh; they are not
silently treated as unrestricted documents.
Portable settings are literal files. Codex excludes parsed local-project tables
and supplies workspace trust as a process configuration override in its launcher
and configure flow. Claude's initializer merges onboarding and trust into local
state while preserving other fields. Neither exports trust into shared settings.
Credential documents use exact JSON objects `{"$secret":"ENV_NAME"}` at registered
secret leaves. The installer substitutes current sentinels using JSON encoding;
there is no template language or access to arbitrary sandbox configuration.
Literal template-like text in preferences remains ordinary text.

Handoff runs only after durable acceptance. A digest of the exact captured entry
prevents overwriting a newer login; already-applied entries are idempotent. File
stores replace only acknowledged entries and retain other local accounts.
Copilot removes the acknowledged plaintext login while its environment sentinel
is delivered on restart. Pi's derived Copilot access token is not the GitHub
credential: an unchanged refresh sentinel means `keep`, even after an exchange.

OpenCode and omp read/write through their native auth export/import commands,
never SQL. OpenCode journals the acknowledged entry's digest and active choice
before retiring its native ID. The journal contains no credentials; it lets a
retry resume after logout succeeded but import failed. After import, the saved
account is switched active only when it was active at capture and the other
accounts have not changed. omp preserves account identity. Live database harness processes defer handoff
until they stop. Both launch import paths take the same
`~/.config/discobox/harness-capture.lock` as the agent, then check current exports
before importing: a new or unsupported local login blocks an older delivery.
The launcher guard also reads the agent's local `harness-sentinels.json` context
under that lock to recognize older DB aliases after reminting. This user-readable
context is never consulted by a server-bound capture export, which takes aliases
only from the private agent input. Malformed guard context fails closed.
The lock coordinates Discobox operations; it cannot serialize a vendor CLI's
OAuth refresh against the control plane, the accepted race in the ADR.

`capture/runtime.test.cjs` tests normalization, sentinel handling, generation
conflicts and native-command DB handoff. `TestBuiltInCaptureProtocol` also runs
all six actual exporter commands and validates their output using the server's
shared protocol validator.

## Image Contract

Built-in harness Dockerfiles use the repository root as their build context,
including release and remote dev builds, so each can copy the shared capture
implementation into its own feature. The watcher tracks those assets for all six
coding harnesses; shell carries no capture implementation.

- **A harness is a bundle** ([ADR 26-10-16-423](../docs/adr/26-10-16-423-a-harness-is-a-provisioning-bundle-and-the-agent-provisions-any-box.md)
  §2): a `FROM scratch` image of its features, `/features/<name>/`, applied
  and never run, whose labels carry its manifest. The platform is a bundle
  too, and a harness bundle never inherits it: the server composes the
  platform bundle's `10-platform` layer (`PlatformLayer`) under the harness
  bundle's own (`ComposeBundleLabels`). No harness is an image (§10): every
  box runs its provider's image — on the Docker driver, the sandbox base
  image — and the sandbox agent applies its platform and harness bundles at
  every start (§7, `agentlayer`).
- One bundle contains at most one harness. Its identity, seed files,
  secret declarations, optional config command, env defaults, declarative
  volumes, supplementary groups (`additionalGroups`), features, the
  destinations it needs to reach (`harness.egressHosts`, joining the harness's
  egress level and never restricting anything themselves, ADR 26-10-01-032 §3),
  and any command overrides are published in the bundle's OCI labels (`harness.ImageMetadata`) for
  server-side registration. `image.json` is the authoring source a label is
  compacted from (see `Taskfile.yml`). Save-back bundles also install that same
  document beside their capture command; configure and capture read its
  provider definitions, so refresh clients and route policy have one source.
- **A template with no image ships the same manifest as a file** (ADR 0145 §3).
  A non-Linux sandbox is a VM template assembled from a vendor's base and
  Discobox's overlay, and the overlay carries the manifest as one JSON object
  keyed by the very label keys an image would carry, each holding its layer as
  an object (`ReadManifestFile`). The file becomes that label set and resolves
  through `ResolveImageLabels`, so layering, merge by identity, the reserved
  numbers, the base layer's proof of lineage and the `discobox-harness-run`
  convention are one contract from either source; a key that is not a layer is
  refused rather than skipped, since a manifest file holds nothing else. The
  server names one by a `file://` reference
  ([`resources/harnessconfigs`](../server/internal/resources/harnessconfigs/DESIGN.md)).
- **What a platform has is declared, and what it lacks is the declaration's
  absence** — never a probe at runtime or an optional interface (ADR 0145 §3).
  A manifest file declares its `platform` (`os/arch`) and its `imageKind`
  (`vm/<driver>`, ADR 26-10-09-106 §4); a bundle declares neither,
  because its steps select the platform they run on and its kind is the
  provider's (ADR 26-10-16-423 §2). A non-Linux manifest
  names its one `account` by name alone (ADR 0145 §5) and the `shell` its
  terminals type into, as an absolute path of its platform; a Linux one names
  neither, because both come from the account the sandbox resolves from its
  own passwd database (ADR 0025). `features` says what the filesystem ships
  beyond running commands — `desktop`, `docker` (nested Docker and its runc
  wrapper) — and the platform layer declares both. A feature merges like a
  group: any layer adds one, none removes one. `ImageMetadata.ValidateFor`
  judges the merged manifest by its platform's OS: a non-Linux one with
  volumes, `additionalGroups`, a desktop or nested Docker is refused, each a
  Linux container mechanism its VM has nothing to do with. An image built on a
  base layer from before `features` existed declares none though it ships
  both; nothing reads `features` yet, and the first thing that does has to
  read such a Linux image as the base it is.
- **`egressHosts` is what the agent needs to run, not every host it can use.**
  claude-code declares the four hosts its network documentation lists for the
  API, sign-in, token refresh, and claude.ai connectors. Plugins, the
  changelog, artifacts, and Chrome are optional and left to a level a person
  sets. codex and opencode declare none until their hosts are established from
  their own documentation (ADR 26-10-01-032 §3), so a restricted project lists
  them itself.
- **claude-code sends no telemetry.** Its env layer sets `DISABLE_TELEMETRY`,
  `DISABLE_ERROR_REPORTING` and `DISABLE_FEEDBACK_COMMAND` for every discobox
  on it, restricted or not: telemetry, error reports, and `/feedback` (`/bug`)
  are off, from an unrestricted discobox too. Its other background calls are
  not held back (pull request status through `gh`, quota probes, plugin and
  MCP directory lookups); a restricted discobox's proxy refuses those its
  egress does not allow. Not `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`, which
  holds those back as well: it also stops Claude Code titling the session, and
  that title is what names a box while its agent is idle (ADR 26-10-31-618).
- **A manifest is a stack of layers** (ADR 0086 §2). A bundle's effective
  manifest is `harness.ComposeBundleLabels`: the platform's `10-platform`
  layer, then every `io.discobox.image.v1.<NN>-<name>` label the bundle
  carries in ascending order of that suffix, then its own
  `io.discobox.image.v1` label last (`ResolveImageLabels` resolves the
  bundle's part). Docker inherits a parent's labels and a `LABEL` replaces
  only the key it names, so a third party's bundle built `FROM` a harness's
  carries the layers that one contributed. Layers merge by identity — `env` per key, `volumes`/`files` by path,
  `secrets` by name, groups and egress hosts by union; a harness's scalar fields, commands, and
  `config` go to the last layer that sets them — and none of them may unset.
  A layer is a fragment: only the merged result is validated, at registration.
  `00`–`49` is reserved for layers Discobox ships (`LayerNumberReserved`).
  The same inheritance carries `ReclaimLabel` onto every image built `FROM`
  the sandbox base image, as one Discobox may reclaim (ADR 0040); no bundle
  carries it, and registration refuses a "bundle" that does.
- A declared volume says which primary volume backs it (`data` or `cache`),
  and `path` may use `%HOME%` and `uid`/`gid` `%UID%`/`%GID%`; `ResolveVolumes`
  expands them against the sandbox user and judges the path by the sandbox's
  platform (`sandboxpath`), never by the host's. Declared volumes are a Linux
  container mechanism: on any other platform there is nothing to bind, and
  `ResolveVolumes` refuses a declaration rather than ignoring it — which is
  also what keeps `%UID%`/`%GID%`, tokens only a volume carries, to the
  platform where they mean something (ADR 0145 §6). Boot chowns only the
  mountpoint as declared, never what is under it or what the image shipped
  around it, so what an image ships under home keeps the image's ownership. A
  path under `%HOME%` the user must write is a declared volume with
  `%UID%`/`%GID%`; a tree the image ships is handed to a group (setgid and
  `additionalGroups`, [ADR 0107](../docs/adr/0107-homebrew-is-image-content-on-an-overlay-handed-to-a-group.md)); a per-user file is a `harness.files` entry
  ([sandbox-agent REVIEW](../sandbox-agent/REVIEW.md), "Boot cost"). A cache path is per sandbox user unless it declares
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
- A volume's `mode` is a POSIX mode word, and `ResolveVolumes` converts it to
  `os.FileMode` rather than casting: the two agree only on the low nine bits,
  and setuid/setgid/sticky sit far higher up in Go's encoding than in POSIX's.
  A cast loses them silently — nothing errors, and the path is simply created
  without the bit. That matters because setgid on a directory is the only way an
  image can hand a tree to a *group*, which is what it must do whenever the uid
  that will use the tree is not known until the sandbox boots
  ([ADR 0107](../docs/adr/0107-homebrew-is-image-content-on-an-overlay-handed-to-a-group.md) §2).
- **A harness extends a harness, never the platform** (§2). A bundle
  carrying a `10-platform` or `10-sandbox-base` layer of its own was built
  `FROM` the platform, or a release from before bundles' sandbox agent
  image, which would pin the agent
  it was built with, and is refused. It names instead the
  `platformAPIVersion` it needs (`PlatformAPIVersion`), which must be the one
  the platform's layer provides. A bundle carrying a whiteout is refused: its
  layers add feature directories side by side and delete nothing. A
  manifest file still proves its lineage with the overlay's
  `10-sandbox-base` layer (ADR 0086 §1).
- **The harness command is a convention.** The image installs its agent as
  `/usr/local/bin/discobox-harness-run` (`harness.RunCommand`) and the runtime
  types `discobox-harness-run [--resume] '<prompt>'`. `runCommand` and
  `relaunchCommand` in a manifest are *overrides*, and nothing Discobox ships
  sets one. The convention is resolved at registration
  (`harnessconfigs.conventionCommands`), which is where the reserved `shell`
  slug is known — the sandbox knows its harness by the config's generated id
  and cannot tell a login shell from an image that declared nothing.
  The platform's core feature ships `discobox-harness-run` as a shim that
  execs nothing, so a harness installing no agent lands the user at a prompt
  rather than at a `command not found`; a harness bundle's feature overwrites
  it. See ADR 0086 §3 and ADR 26-10-16-423 §4.
- **A wrapper joins its prompt words back into one prompt.** The command is
  *typed* (ADR 0027), so the login shell splits it before the wrapper runs:
  `discobox run . fix the failing tests` reaches `discobox-harness-run` as four
  arguments, not one. A prompt given as one argument — `discobox run . 'fix the
  failing tests'`, and everything the console creates — arrives as one, which
  the same joining leaves alone. Every wrapper joins everything after the flags with
  single spaces and hands its agent that single string — an agent CLI takes its
  prompt as one positional, so a wrapper that forwards `"$@"` unchanged asks it
  to "fix". This is the wrapper's half of the convention and part of what a
  third-party harness bundle signs up for; the runtime types the words as the
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
  and re-snapshotted later — by `SeedBuiltIns` whenever a built-in's bundle
  reference, its digest or the manifest composed from it on the platform's
  layer (`ManifestDigest`) has moved, and by `RefreshHarnessConfigImage` when
  an owner re-inspects a user-registered bundle. A snapshot is a cache of a
  mutable tag's current contents, not a permanent record (ADR 0016).
- Runtime commands are never supplied by the server or pool-agent: the
  harness's feature installs its CLI when a box provisions it, as the next
  point says.
- Each harness folder owns its `Dockerfile`, `image.json`, configure script,
  and other image-specific assets. The folder is the harness's feature
  ([ADR 26-10-16-423](../docs/adr/26-10-16-423-a-harness-is-a-provisioning-bundle-and-the-agent-provisions-any-box.md)
  §4): its `provision.yaml` installs the CLI (`install-cli.sh`) and places
  each file where the harness reads it, requiring the platform features it
  runs on (`node`, and `bun` for omp).
- **A harness Dockerfile builds one artifact, its bundle** (§2): one `FROM
  scratch` stage, `bundle`, holding the feature's files under
  `/features/<feature>/` and the manifest in labels, published as
  `discobox-harness-<name>`, the name a box applies and a third party builds
  `FROM`. `harness/shell`'s bundle carries no feature, only its label. A
  published harness bundle is one `linux/amd64` manifest with no attestations
  — its content is the same on every architecture, and its steps select
  theirs when they run — where the platform bundle is a multi-architecture
  index.
- **A manifest argument is required, not optional.** `LABEL key=${ARG}` with no
  argument passed labels the image with the empty string, which is a build
  mistake that produces a working image nothing can register — so the platform
  stage of `sandbox-agent/Dockerfile` and every harness bundle stage refuse the
  build through `${ARG:?}` when their argument is missing, rather than leaving
  it for a server to report at seed time. Any build path publishing these
  (`build:*`, `release:images`, the development watcher) passes it.
- **The platform layer** (`10-platform`) is `sandbox-agent/image.json`, beside the Dockerfile that
  labels it. It carries what is true of every box the platform provisions rather than of
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
  - The four `/nix` volumes: `/nix` on `cache` with `scope: shared`, with
    `/nix/var/nix/profiles`, `/nix/var/nix/gcroots` and
    `/nix/var/nix/daemon-socket` carved back onto `data`. The store is
    pool-shared, so a closure one sandbox realizes is free for the next; the
    per-user profile state is not, because both are keyed by username and every
    sandbox in a pool runs the same user. The base image ships its own store
    aside and leaves `/nix` empty precisely so this cache bind hides nothing — a
    cache path is always a plain bind. See ADR 0075. `daemon-socket` is
    per-sandbox for a different reason: it holds the address each sandbox's
    own `nix-daemon.socket` listens on, and a socket path on a shared
    directory names one listener for the whole pool. Nothing else the image
    listens on lives under `/nix` (ADR 26-10-14-129; the wiring is in
    [`sandbox-agent/DESIGN.md`](../sandbox-agent/DESIGN.md)). All three
    carve-outs are `excludeFromExport`: `profiles` and `gcroots` point into
    `/nix`, which never travels, and a socket file is nothing to carry.
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
  - `features`: `desktop` and `docker`, because the base image's filesystem
    ships both, and a manifest that leaves a feature out has not got it.
  - The rest of the persistent and cached paths, the `brew`, `docker`, and
    `kvm` supplementary groups, the `NIX_*`/`HOMEBREW_*`/`PATH`/
    `NPM_CONFIG_PREFIX` env, and the pnpm `storeDir` seed file.
- **The agent version a sandbox runs comes from that store, not from the
  image** ([ADR 0114](../docs/adr/0114-a-sandbox-pins-its-agent-version-from-a-pool-cached-store.md)).
  A harness bundle's feature declares what it installed in
  `/usr/local/libexec/discobox/agent.conf` (`AGENT_PACKAGE`, `AGENT_BIN`, and
  optionally `AGENT_IMAGE_DIRS`), and the platform does the rest from
  `/etc/profile.d/sandbox-agent-store.sh`: `discobox-agent-store pin` points
  `~/.local/bin/<bin>` at the newest version in the store, once per sandbox and
  never again (a pin for another package of the same command, left by an
  earlier image, is replaced), and a detached `refresh` advances the store at most twice a day
  per pool user. Launchers are untouched by this — the pin is ahead of
  `/usr/bin` on PATH, so `launch.sh` still just runs its agent — and an image
  with no `agent.conf` (the `shell` harness) is unaffected. Because the store is
  the version source, each image also turns its agent's own updater off:
  `DISABLE_AUTOUPDATER` in claude-code's env layer, `check_for_update_on_startup`
  in codex's system config, `OPENCODE_DISABLE_AUTOUPDATE` in opencode's env
  layer, `COPILOT_AUTO_UPDATE=false` in copilot's, `startup.checkUpdate: false`
  in omp's settings overlay (omp only ever announces an update),
  `PI_SKIP_VERSION_CHECK=1` in pi's env layer (pi too only announces one). `discobox-harness-upgrade` is the by-hand "newest, now".
- An image says what the discobox terminal can do when its agent will not
  infer it. An exec the console opens is given `TERM=xterm-256color` and
  nothing that says its terminal takes OSC 8 hyperlinks, which Claude Code
  asks for before it writes one, so claude-code's env layer sets `FORCE_HYPERLINK=1`
  and its markdown links are written as links, which `termpane` draws. The env
  layer reaches every exec in a claude-code discobox, and what `claude` runs
  from them: anything else following the `supports-hyperlinks` convention reads
  the variable too, ahead of asking whether it writes to a terminal, and may
  put OSC 8 into a pipe. That cost is kept to the one image whose agent needs
  the hint, rather than made every exec's default.
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
  the configure terminal and, in the console, on the configure pane's header,
  where the CLI's full-screen sign-in cannot paint over it. See
  [`resources/harnessconfigs/DESIGN.md`](../server/internal/resources/harnessconfigs/DESIGN.md)
  for the snapshot and [`cli/internal/tui/DESIGN.md`](../cli/internal/tui/DESIGN.md)
  for the pane.
- **A sign-in whose callback port changes every run declares an ephemeral
  entry** (`{"ephemeral": true}`, at most one; ADR 26-10-09-867). The flow then follows the
  configure sandbox's discovered listening ports, as the workspace's forward
  does, and binds each TCP one at the same number — exact, as above, since the
  browser holds a redirect URI naming the sandbox's number. Declared services
  (the desktop) are not followed: no sign-in redirects to them. A taken port is
  reported when it is found, which is mid-flow, so the console's header cannot
  warn of it ahead of time; the CLI names the number and the entry's
  `unavailable` words say what to do. A port is found through the sandbox's
  reported listing, which a configure sandbox refreshes on a fast cadence (a
  0.5 s scan, a 1 s pool poll, a 1 s CLI poll), so a bind trails the listener
  by a few seconds at most; the harness's own instructions tell the user to
  reload a callback page the browser could not reach.

## Driver Model

- `harness.Driver` identifies one built-in harness's included bundle through
  `ID()` and `Definition()`, and nothing else. The definition catalog is a
  registration shortcut — seeding reads only a definition's `ID` (the slug),
  `Name` and `Bundle` (`harnessdefs.Seeds`); runtime metadata comes from the
  bundle's labels, composed on the platform layer. A driver holds no behavior
  a harness bundle cannot declare for itself — that is what keeps a
  third-party harness a pure registration story.
- A `Definition` names its bundle, `discobox-harness-<id>`, through
  `harness.BundleRef`, never as a literal, as `PlatformBundleRef` names the
  platform bundle. One `BundleRegistry`/`BundleTag` pair backs every one,
  unset and `local` by default and overwritten at link time by a release
  (`Taskfile.yml`'s `release:binary`), so the built-in harnesses a binary seeds
  are the bundles that shipped with it. One pair rather than a reference per
  bundle: a release publishes them together, and independent references could
  disagree about which release a sandbox is running.
- A harness's **platform** is not a definition's to declare (ADR 0145 §1). A
  bundle names none: one artifact for every architecture, whose steps select
  the platform they run on, so a pool of any platform runs it. Only a
  manifest file's template declares the one platform it runs on, which the
  control plane records and a sandbox's pool must host. See
  [`resources/harnessconfigs/DESIGN.md`](../server/internal/resources/harnessconfigs/DESIGN.md).
- Whether a harness has an interactive configure flow is the image's
  declaration (`config.command`), snapshotted as the config's config command.
  The configure process writes files and collected secret values to
  `ConfigureOutputPath`. Configure files use the
  same home-relative contract as all harness files; configure commands run from
  the sandbox workdir and must use `$HOME` when invoking one of those files.
  Every included coding harness supports config mode — see
  [Configure flows](#configure-flows).
- Provider-specific implementations live in one folder per harness:
  - `claude-code`
  - `codex-cli`
  - `opencode` — opencode 2 (`@opencode/cli`), which replaced opencode 1
    (`opencode-ai`) and keeps its credentials in a database rather than a file
    ([ADR 26-10-15-848](../docs/adr/26-10-15-848-the-opencode-harness-runs-opencode-2.md)).
  - `copilot` — GitHub Copilot CLI (`@github/copilot`). Its slug, folder,
    image and hook provider are all `copilot`.
  - `omp` — omp, "oh-my-pi" (`@oh-my-pi/pi-coding-agent`), a Bun script that
    runs on the base image's Bun. Its slug, folder, image and hook provider
    are all `omp`.
  - `pi` — pi (`@earendil-works/pi-coding-agent`), the agent omp descends
    from, a Node script that runs on the base image's Node. Its slug, folder,
    image and hook provider are all `pi`.
  - `shell` — the login shell, and the end of the resolution chain. Its
    Dockerfile installs nothing (the platform already ships the shell) and its
    `image.json` names only the platform API version it runs on: no identity
    to declare beyond its reserved slug, no secrets, no configure flow, and its
    env and volumes are the platform's layer. The reserved slug is also what withholds the harness-run
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
- Codex CLI: `/etc/codex/hooks.json`, laid by the harness's feature beside its
  `/etc/codex/config.toml` system layer. The hook definition and Codex binary
  are one image-versioned compatibility unit; the Go driver does not construct
  or merge Codex's hook format. Every configured lifecycle event invokes the
  generic publisher with its provider and event name while its stdin payload is
  stored unchanged. System hooks are treated as managed and trusted by policy.
- opencode: `/etc/opencode/opencode.json`, naming an image-owned plugin
  directory by absolute path under `plugins`
  ([ADR 0147](../docs/adr/0147-opencode-publishes-its-lifecycle-through-an-image-owned-plugin.md)).
  opencode has no command hook at any layer — its lifecycle reaches plugins
  and nothing else — so `hook-plugin/` (an opencode 2 plugin: a directory whose
  `server.js` default-exports `{id, setup}`; a single file is refused) is what
  claude-code, codex-cli and copilot declare as commands, and it invokes the same generic
  publisher. It publishes only when `DISCOBOX_HOOK_SOCKET` is set, which keeps
  it silent in a configure sandbox and outside a Discobox terminal. It hears the lifecycle
  through the bus, which a plugin subscribes to after opencode starts: the
  TUI, which is how the launcher runs a session, creates its session later and
  published every event in each run measured, while `opencode run` can create
  its session, and exit, before the subscription is up. The managed layer is
  read last of all, after the user's global config and a project's, and plugin
  entries accumulate across layers rather than replacing, so naming this one
  neither loses to the configure flow's capture nor drops a plugin the user
  added. Its policy baseline is a launch flag rather than a system layer (see
  [OpenCode](#opencode)).
- Copilot: `/etc/github-copilot/policy.d/discobox.json`. Copilot loads every
  file in that directory as **policy** hooks, beside the user's own
  (`~/.copilot/hooks`, the `hooks` setting) rather than instead of them, and
  keeps them when the user's settings say `disableAllHooks`. Its
  `/etc/github-copilot/managed-settings.json` is read too, but not for hooks.
  The file uses Copilot's own hook schema (`version: 1`, `bash` commands) and
  publishes all fifteen of its events.
- omp: **no system layer** for hooks or settings
  ([ADR 26-10-14-718](../docs/adr/26-10-14-718-the-omp-harness-imports-its-credentials-and-publishes-through-an-extension.md)).
  Its lifecycle reaches JavaScript extensions alone, as opencode's does, so
  `hook-extension.js` is the unit, installed beside the configure script; the
  image's harness files put a one-line re-export of it in
  `~/.omp/agent/extensions/discobox-hooks.js`. omp discovers that directory
  beside the user's own `extensions:` setting rather than instead of it — an
  overlay naming `extensions:` replaces the user's list, which is why the
  extension is not named there. It publishes an allowlist of events, each
  payload projected to its scalar fields and a tool event's `input`: omp's
  payloads carry the system prompt, transcript, images and compaction entries
  under many names. The home file can be removed and
  `--no-extensions` skips it, which claude-code's and Copilot's hooks survive.
  The policy baseline is the `PI_CONFIG_FILES` overlay `/etc/omp/discobox.yml`,
  scalars only, and two launch flags (see [omp](#omp)).
- pi: **no system layer** and no settings overlay either ([ADR 26-10-15-259](../docs/adr/26-10-15-259-the-pi-harness-delivers-auth-json-and-exchanges-copilot-in-the-sandbox.md)).
  The same shape as omp's: `hook-extension.js` installed beside the configure
  script, reached from `~/.pi/agent/extensions/discobox-hooks.js`. Each
  published event carries the fields named for it, not a projection by shape:
  pi's `before_agent_start` carries the whole system prompt as a plain string.
  The policy baseline is the image's env and the launcher's `--approve` (see
  [pi](#pi)).

Every hook runs `discobox-hook-publish --provider <harness> --event <name>`,
the sandbox agent's generic publisher; no Go code in this package writes or
merges a harness's settings.

The plugin publishes an **allowlist** of nine bus events plus the tool
domain's `execute.before` / `.after` hooks, published as `tool.execute.before`
/ `.after`, which carry the call's input. opencode's bus carries per-token
events — `session.text.delta` fires for every delta of every message — and a denylist
would publish each event opencode ships next by default. That makes opencode's
trail coarser than Claude Code's on purpose: it records a turn's shape and not
its content. A test in `harness/opencode` reads the allowlist out of the plugin
and fails if an event is published with no canonical name and no entry saying
it has none.

## Canonical hook event names

A harness names its lifecycle events as it likes, and a caller waiting for one
should not have to know which harness a terminal runs. `CanonicalHookEvent`
(`hookevent.go`) answers Claude Code's name for another harness's event, or the
empty string when Claude Code has no name for it
([ADR 0146](../docs/adr/0146-a-hook-keeps-the-name-its-harness-used-and-gains-a-canonical-one.md)).
The sandbox agent's store calls it as it records each hook — one owner, so no
writer can store a row whose two names disagree — and keeps both names; a wait
or an audit filter then matches either.

- **Claude Code's vocabulary is the canonical one**, so `claude-code` is absent
  from the table and passes through — including events added after the table
  was written, which is the case a table would get wrong.
- **An agreement on spelling is recorded, not assumed.** Codex adopted Claude
  Code's hook vocabulary wholesale, and each of the 11 events it shares is an
  entry mapping a name to itself: two harnesses spelling an event the same way
  is a fact about how they were built, not a contract.
- **An event with no Claude Code counterpart gets no canonical name**, never a
  copy of its own. Codex's `Interrupt` is one; omp's `before_subagent_spawn`,
  `auto_retry_start` and `credential_disabled`, and pi's
  `session_compact_failed`, are others. Copying it in
  would squat a name Claude Code has not chosen, so that when Claude Code does
  choose one, stored hooks already claim it.
- **`Stop`, `StopFailure`, `SessionStart` and `PostCompact` on opencode depend
  on a filter, not only on a name.** opencode publishes
  `session.execution.succeeded`, `session.execution.failed`, `session.created`
  and `session.compaction.ended` per *session*, and its subagent tool runs
  child sessions, so a child's success is indistinguishable by name from the
  user's turn ending. `session.execution.failed` is `StopFailure` because a
  retried model error is `session.retry.scheduled` and the execution fails
  once, after the last retry. The image's plugin
  publishes all four for the root session only, learning that a session is a
  child from the `parentID` on its `session.created` and treating an unknown
  session as the root. If that filter goes, all four entries must go with it;
  a test in `harness/opencode` runs the plugin against the filtered set it
  declares and
  fails if the two drift apart.
- **omp's session events are the main session's** for the same reason: omp
  rebinds every extension into each subagent session, so the extension
  publishes `session_start`, `session_shutdown`, `before_agent_start`, the two
  compaction events and `session_stop` only when `ctx.agent.kind` is not
  `sub`. Tool events are published for every session. `tool_result` is
  `PostToolUse` for a failed tool too, with `isError` in the payload.
- **pi's `Stop` is `agent_settled`**, not `agent_end`: `agent_end` closes one
  agent loop, and a retry, an overflow compaction or a queued message
  continues after it. pi has no subagents, so nothing is filtered by session.
- Each image's hook config and this table are kept together by a test in that
  harness's package: publishing an event without deciding what it is called
  across harnesses fails the build.

## Source-scoped memory

The runtime exposes opaque durable data for the primary source at
`/.discobox/data-per-source/primary` in every sandbox — shared by source key,
or private to the sandbox when there is no key to share it under (see
[`pool-agent/DESIGN.md`](../pool-agent/DESIGN.md)); only harnesses
interpret anything beneath it. Claude Code and Codex keep their memory there in
separate namespaces; opencode has no memory feature to point at it,
Copilot's has no setting or variable that names where it keeps memories,
omp's memory backends are off by default and are not pointed there, and pi
has no memory feature:

- **Memories are keyed by the sandbox user's uid**, as the pool cache is (ADR
  0094): each harness stores them under `.../users/<uid>/harnesses/<name>/`.
  Every sandbox on a source mounts the same tree whatever uid it runs as, so
  sandboxes that agree on a uid share memories and ones that do not neither
  share nor collide. Pool-agent chowns the mount root to the latest creator,
  so the launcher makes `users` (root-owned) and its own `users/<uid>` (0700)
  through the sandbox user's sudo grant rather than relying on who owns the
  parent. Memories written before the partition, at `.../harnesses/<name>/`,
  are copied into it once, and only for the uid that owns them. A launcher
  that cannot make the partition leaves the CLI on its local default.
- Claude Code's `autoMemoryDirectory` is
  `.../users/<uid>/harnesses/claude-code/memories`. A path naming the uid
  cannot be baked into the image's `/etc/claude-code/managed-settings.json`,
  so the launcher records it as the managed drop-in
  `/etc/claude-code/managed-settings.d/discobox-memory.json` before starting
  `claude`. That makes it the sandbox's environment rather than a launch flag:
  the harness terminal launches at sandbox start, and a `claude` typed into
  any shell afterwards finds the same memory. The policy layer also keeps it
  out of `.claude/settings.json`, which the configure flow deliberately
  replaces with the user's captured settings, and outranks anything that file
  says. Claude Code's temp tree — session scratchpads among it — is
  environmental too: the image's env sets
  `CLAUDE_CODE_TMPDIR=/var/lib/claude-code` and declares that path a `data`
  volume owned by the sandbox user, so scratchpads survive an upgrade and
  travel with an export. Claude Code adds `claude-<uid>` beneath it and
  refuses a directory another uid owns.
- Codex enables its `memories` feature in the system config and bind-mounts
  `.../users/<uid>/harnesses/codex/memories` onto `$CODEX_HOME/memories`.
  Codex rejects a symlinked memory root, so the launcher creates a real
  target directory and uses the sandbox user's passwordless mount grant. The same system layer
  enables both generation and use for new chats and sets the generation rate-
  limit threshold to zero; a sandbox is short-lived enough that silently
  skipping its only background consolidation window would otherwise defeat
  source-scoped persistence. Its auth, user
  config, sessions, and SQLite databases remain in the sandbox's ordinary
  per-sandbox home; sharing `CODEX_HOME` would cross credential and session
  ownership boundaries. A pre-existing real memories directory is preserved
  and reported rather than overwritten.

Pool-agent and sandbox-agent know only the generic source-data mount and never
a harness memory format.

The same preference decides where a harness's *policy* baseline goes when
the CLI has a system layer for it. The codex feature lays
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
binding covers it. So a configure command can *use* the old credential — that is
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

**A secret may declare routes** (`Secret.Routes`,
[ADR 26-10-02-924](../docs/adr/26-10-02-924-a-secret-may-carry-routes-and-every-request-it-is-sent-in-must-fall-on-one.md)):
where the harness itself sends the credential, the image's alone. Each route
is a `secretroute.Route` (ADR 26-10-27-880 §4) — host patterns (a host covers
itself alone; `*.h` the hosts beneath it), and optionally methods, path globs
and a GraphQL or MCP block — and whether it is blocked; one declared in the
shape before host patterns reads as it was meant (`harnesscapture.Route`,
through `secretroute.FromLegacy`) until its image is rebuilt. Configure
writes the routes onto the secret it creates. An image declaring different
ones re-syncs them, while a secret a person bound is theirs to route. Every
request carrying the secret's sentinel must fall on one, and the most specific
decides: a blocked one refuses, and anything no route covers is refused. Name
the harness's own endpoints, and block the paths beneath them that reach what
the account owns rather than what the harness needs. A route a judge used to
read is a blocked one (ADR 26-10-27-880).

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
    back **in the same field it was read from**, through a credential document
    whose `primaryApiKey` is an explicit secret reference. Startup also initializes
    this field from the current sentinel context when absent or carrying a known
    old alias; it preserves a newer local login.

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
- The launcher and configure flow run `claude-code-capture init`. It merges
  onboarding, the auto-mode nudge marker, and trust for the actual current
  directory into `.claude.json`, preserving other projects, preferences and
  newer local credentials. Malformed state is left intact and startup fails.
- `.claude/settings.json` remains literal, with the bypass-permission defaults.
  Claude refuses bypass as root, so configure runs as the sandbox account.
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

  Both credentials declare `rejectionScope`: `api.anthropic.com`, `/v1/messages`. A
  secret's `rejectionScope` (`harness.RejectionRule`: a host, and path prefixes
  on a segment boundary) names the requests that answer for the credential;
  only what an upstream makes of one of them draws or clears the refused
  credential's band. Anthropic's runner poll refuses a subscription login that
  works and its event logging accepts anything, so neither is evidence. A
  harness whose API answers the same everywhere declares none
  ([ADR 26-10-13-595](../docs/adr/26-10-13-595-a-harness-names-the-requests-whose-refusal-means-its-login-is-dead.md)).

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
  credential document with the sentinel in the credential's place.
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
  place of its secret reference, so the session opens **already signed in** and
  changing a model or theme costs no re-authentication. Detection is the same
  comparison claude-code makes: a value equal to the seeded sentinel proves
  nothing re-authenticated and comes back as `usePrevious`.
  - Codex has no `/login`; the way to change accounts from a signed-in session
    is `/logout`, which signs out **and exits**. So a session that comes back
    with no credential after being seeded is a deliberate logout, and the script
    says so and offers to start codex again rather than reporting a missed step.
  - Whatever the script does *not* seed, it removes. A configured harness
    delivers its own auth.json into the configure sandbox like any other file,
    but configure seeds its own credential from `PREV_` bindings instead of
    installing run-mode credential documents. There is no baseline auth.json.
- Verification runs `codex exec` against the auth.json as it stands, which is
  exactly what a run sandbox will do. There is no environment variable to point
  codex at one credential instead of another, so the file is the subject of the
  check and the environment is scrubbed of the variables `codex exec` would
  otherwise prefer over it.
- It returns a credential document for `auth.json` and literal user preferences
  for `~/.codex/config.toml`, excluding parsed `[projects]` tables. Both the
  launcher and configure pass `-c projects.<JSON-quoted cwd>.trust_level="trusted"`.
  Trust never needs to be appended to the user's file or persisted centrally.
- Existing stored trust suffixes and credential references are converted in
  place by the server migration. Old sandbox bootstraps and capture checkpoints
  use the same bounded conversion on read. Unknown executable templates fail
  explicitly; migration never evaluates them or discards user preferences.

### OpenCode

`opencode/configure.sh` follows the codex shape — a bare interactive
`opencode`, then an inspection of the credentials it stored — for a harness
whose credentials are not one of two fixed kinds. opencode reaches every
models.dev provider, and a user connects as many as they like with `/connect`,
so the image declares no secrets at all: their names are only known once
something is connected, and the built-in seeds `Configured`, like any image
that declares none. See
[ADR 0127](../docs/adr/0127-the-opencode-harness-runs-opencode-1.md) and
[ADR 26-10-15-848](../docs/adr/26-10-15-848-the-opencode-harness-runs-opencode-2.md).

- **Every opencode command runs `--standalone`**: the launcher, the configure
  script and the credentials helper. Without it opencode
  talks to a per-user background service, which outlives the terminal that
  started it and would run the hook plugin in that terminal's environment for
  every terminal after it.
- **Credentials are rows in opencode's database**, read and written only
  through `opencode auth export` and `auth import`, opencode's own interchange
  format: `{id, integrationID, label, active, value}`, where `value` is
  `{type: key, key}`, `{type: oauth, methodID, access, refresh, expires,
  metadata}`, or `{type: external, methodID}`. Import never updates an id it
  already holds.
- **One secret per provider**, `OPENCODE_<PROVIDER>_CREDENTIAL`, from the
  provider's active credential. The name is stable per provider, so a
  reconfigure that replaces a credential updates its secret in place. An
  `external` credential holds no secret and is not captured.
- **The secret's type follows how the credential renews.** A key is a `token`.
  An OAuth sign-in whose `refresh_token` grant the control plane can perform —
  OpenAI (ChatGPT, the client and endpoint the codex image refreshes), xAI
  (whose endpoint takes the request form-encoded, recorded as
  `tokenRequestEncoding: form`) — is an `oauth` secret. Any other OAuth sign-in
  (GitHub Copilot; DigitalOcean and Snowflake, which cannot be renewed from
  here) is a `token` holding its access token, and the script says when it
  expires.
- **Delivery is the export as a credential document**,
  `.config/discobox/opencode-credentials.json`: each entry under the image's
  own id, `cred_discobox_<provider>`, with the sentinel in place of its key or
  access token, `refresh` a placeholder that can never be spent (or the
  sentinel, where the access token is also the refresh field), and `expires`
  far future where the control plane renews the token, so opencode never
  attempts a refresh the sandbox could not complete. The launcher runs
  `opencode-credentials sync` before opencode starts. Each `opencode auth`
  command is a standalone server start (10 to 40 seconds on a loaded machine),
  so it runs nothing while what it last synced is still what is delivered;
  otherwise it exports (unless there is no database yet), logs out and
  imports only the image-owned credentials that differ, and switches an
  image-owned credential that is not active to active wherever its provider
  has another credential (import activates one only for a provider that has
  none, and logging out a provider's active credential promotes another). A
  credential the user connected by hand in a sandbox has another id and is
  never logged out or changed, though a changed configuration takes back
  which one is active. A configuration made under ADR
  0127 delivers opencode 1's `auth.json` instead; the helper reads it with
  opencode's own legacy mapping, so it keeps working until it is reconfigured,
  and on a sandbox that ran opencode 1 the switch keeps it active over
  opencode's own import of the same file. The helper is the one place the
  image's entries are made (`own`) and the one reader of what was delivered
  (`delivered`), for the launcher and configure.
- **The default model is checked, and a failure only warns.** The check runs
  on the model a sandbox starts on — the `model` setting, else the last
  `/models` pick, else opencode's own choice — named with `--model` from
  `opencode-model` (`model.sh`), since `opencode run` alone does not read the
  `/models` pick. Only that model is
  checked: whether a request succeeds depends on the model as much as the
  credential (a plan that excludes it, a region that needs opting in to), which
  opencode's model list does not show, so checking each provider on a model
  picked for it reports working keys as broken. A failure offers to start
  opencode again; declining saves everything as it is. Connecting nothing is
  allowed after a warning — opencode runs without a provider, on its free
  models or a local server.
- **Reconfigure imports the previous credentials** with each `PREV_` sentinel
  substituted, so the session opens connected. An entry whose key or access
  token is still that sentinel was not replaced, and comes back as
  `usePrevious` with its previous entry verbatim. A delivered opencode 1
  `auth.json` is removed and its entries are what is seeded and kept.
- It also returns opencode's settings as the user left them — the global
  `opencode.json`, `opencode.jsonc`, `config.json`, and `cli.json` (theme,
  keybinds; opencode 2 migrates an opencode 1 `tui.json` into it) — and
  `.local/state/opencode/model.json`, the `/models` choice, as `createOnly`: it
  seeds a sandbox's first launch and belongs to the sandbox after that.
- **The policy baseline is `--auto`** on the launch, approving every tool use a
  rule does not deny, and a launch prompt goes to `--prompt`, which the TUI
  submits. opencode's system layer, `/etc/opencode`, is its *managed*
  configuration, which outranks a project's own rules, and the user's global
  config is replaced by the captured settings — so neither is the place for it.
- **Discobox's own settings for the harness** live in
  `.config/discobox/opencode-harness.json`, a plain harness file the image
  declares and configure returns: `webSearch`. It is edited
  with `discobox admin harness edit opencode .config/discobox/opencode-harness.json`,
  and a reconfigure keeps whatever was edited into it, but for the
  `judgeModel` a harness configured before the judge was removed carried,
  which it drops.
- **Web search is asked during configure.** opencode 2 searches with every
  provider by default, through a keyless search service. The launcher reads
  `webSearch`: `false` denies the `websearch` permission through
  `OPENCODE_CONFIG_CONTENT`, which takes the tool away from the model; `true`
  and `null` — the image's baseline, before anyone configured — leave
  opencode's default.
- The configure image declares config ports 1455 (ChatGPT's browser sign-in)
  and 1456 (DigitalOcean's). A browser sign-in on a random port cannot be
  forwarded; those providers connect with a key.

### GitHub Copilot

`copilot/configure.sh` asks how to sign in before it starts Copilot, because
the two ways differ in what they hand every discobox
([ADR 26-10-02-840](../docs/adr/26-10-02-840-the-copilot-harness-takes-its-github-token-from-the-environment.md)):

- **A fine-grained PAT with only "Copilot Requests"**, the default, read by
  Copilot's own `copilot login --with-token`, which validates it.
- **`/login`** in a bare Copilot. Its browser sign-in calls back to a port
  it picks fresh each run (`listen(0)`; GitHub accepts any loopback port), so
  the image declares an ephemeral config port; device code is the fallback
  when the port it got is taken here. Its token is an OAuth App `gho_` token with `repo` scope among
  others, and Copilot sends it to both `api.github.com` and
  `githubcopilot.com`, so its secret carries no host. The script says that
  every discobox using the harness could then act on every repository the
  account can, and goes on only on an explicit yes; the warning defaults to
  no. A token pasted at the PAT prompt that is not a fine-grained PAT — gh's
  own `gho_` token, which Copilot also takes — has to clear the same warning,
  and is named for what it is rather than for the prompt it came in at. So
  does any such token the session itself stores: the choice made before
  Copilot starts binds nothing, since `/login` runs inside it from either
  path, and what is stored is what is saved.

Either way Copilot stores the token, and the script reads it back:

- **Storage is `~/.copilot/config.json`**, Copilot's own state file (JSON
  under `//` comment lines): `authTokens["<host>:<login>"].token`, for the
  account `lastLoggedInUser` names. A sandbox has no keychain, and without
  `storeTokenPlaintext` Copilot stops a sign-in at a consent question, so the
  script sets that in `settings.json` while it runs and strips it from what it
  returns. It clears `config.json` before each round so a retry never captures
  an earlier attempt.
- **One `token` secret, `COPILOT_GITHUB_TOKEN`, delivered in the
  environment.** Copilot documents that variable for headless use and it
  outranks a stored login; `gh` and git never read it, and Copilot strips it
  from the environment of the commands its agent runs. Neither sign-in yields
  anything to refresh. `config.json` is never returned: it is Copilot's state,
  rewritten as it runs.
- **Only github.com.** A login to another host is refused: an Enterprise Cloud
  account also needs `COPILOT_GH_HOST` in every sandbox, and a configure
  command returns no env.
- **Reconfigure opens signed in** with the `PREV_` sentinel as
  `COPILOT_GITHUB_TOKEN`; Enter keeps it (`usePrevious`). Copilot stores a
  `/login` even with a token in its environment, so signing in to another
  account inside the kept session replaces it, once that token has cleared
  the warning. A kept token that fails
  verification stops being offered.
- **The token is held to Copilot's own traffic.** `COPILOT_GITHUB_TOKEN`
  declares routes (ADR 26-10-02-924), which configure writes onto the secret:
  Copilot's own endpoints under `githubcopilot.com` and `api.github.com` are
  sent; its MCP server, its agents and the rest of `github.com` are blocked
  routes, which a judge used to read (ADR 26-10-27-880); anything else is
  refused. The agent needs a binding of its own to do more on GitHub.
- Every path ends in a tool-free `copilot -p` with only the chosen token, in a
  `COPILOT_HOME` of its own so the stored login cannot stand in for it.
- It returns `~/.copilot/settings.json` as the user left it (`/config model`,
  theme, status line), minus `storeTokenPlaintext`.
- **The policy baseline is two switches.** `COPILOT_ALLOW_ALL=true` in the
  image's env trusts the directory Copilot starts in — without it an
  interactive launch stops on a folder-trust dialog that `--allow-all` does
  not skip — and the launcher passes `--allow-all`, since the variable alone
  leaves an interactive session on manual approval. A launch prompt goes to
  `--interactive=`, whose `=` keeps a prompt starting with a dash a prompt;
  resume is `--continue`, which opens a new session when there is none.
- Copilot unpacks itself under `~/.cache/copilot/pkg` on first run (about
  185 MB per version), which the base layer's `%HOME%/.cache` volume already
  makes per-user pool cache.

### omp

`omp/configure.sh` follows the opencode shape — a bare interactive `omp`,
then a read of what it stored — for a harness whose credentials are any number
of providers' ([ADR 26-10-14-718](../docs/adr/26-10-14-718-the-omp-harness-imports-its-credentials-and-publishes-through-an-extension.md)).
The image declares no secrets.

- **Credentials live in SQLite** (`~/.omp/agent/agent.db`), and omp moves them
  with `omp auth export` and `omp auth import`: `{version: 1, credentials:
  [{provider, credential}]}`, where a credential is `{type: api_key, key}` or
  `{type: oauth, access, refresh, expires, …identity}`. An import replaces the
  same provider's key and the same account's login, and keeps the rest.
- **One secret per credential**, `OMP_<PROVIDER>_CREDENTIAL`, a second account
  of one provider `_2`. omp exports several accounts in no fixed order, so a
  credential is named by which secret it is: the one whose `PREV_` sentinel it
  still holds, else the previous secret of the same account (provider,
  `accountId`, `email`), else the provider's lowest free name. A key is a `token`; an OAuth login the control plane
  can renew — Anthropic and ChatGPT with the clients the claude-code and codex
  images are renewed with, xAI form-encoded — is an `oauth` secret; any other
  login is a `token` holding its access token, and the script says when it
  expires, or that it has — kept or new.
- **Delivery is the export document as a credential document**,
  `.config/discobox/omp-credentials.json`, with sentinels in place of keys and
  access tokens, `refresh` a placeholder (or the sentinel, for GitHub Copilot,
  which stores its token in both), and `expires` far future where the control
  plane renews. `launch.sh` imports it before starting omp, on every launch.
  An imported sentinel goes out as the request's credential and is never
  refreshed. omp reads ChatGPT's `chatgpt-account-id` from the access token's
  claims, which a sentinel does not carry.
- **`PI_PROXY` follows `HTTPS_PROXY`** (`proxy-env.sh`, installed as
  `/etc/profile.d/omp-proxy.sh` and sourced by the launcher and configure
  command). omp's Anthropic transport is its own client that
  reads only `PI_PROXY`, and without it connects direct, which a sandbox's
  egress does not allow: every Anthropic request hangs into "Connection
  error.". It is derived rather than written into the manifest, where a build
  in a discobox would rewrite the loopback address (ADR 26-10-01-425).
- `ONNXRUNTIME_NODE_INSTALL_CUDA=skip`, at build time and in the image's env
  (the version store installs omp in the sandbox): omp's onnxruntime-node
  dependency otherwise downloads onnxruntime's CUDA build from GitHub on
  install, which no sandbox can use and which hung a build outright.
- **Reconfigure imports the previous document** with each `PREV_` sentinel
  substituted, after removing `agent.db`, so the session opens signed in to
  exactly that. An entry whose key or access token is still that sentinel was
  not replaced, and comes back as `usePrevious` with its previous entry
  verbatim.
- The session runs **without the image's overlay**, so a first configure gets
  omp's own setup wizard (sign-in, default model, theme), and the default
  model is then checked with a tool-free `omp -p`; a failure only warns. It
  returns `.omp/agent/config.yml` as omp wrote it; not `models.yml`, which holds literal keys and `!` commands, as pi's
  `models.json` does, so custom providers are configured in the sandbox.
- **The policy baseline** is `PI_CONFIG_FILES=/etc/omp/discobox.yml` in the
  image's env — `startup.setupWizard: false`, whose wizard otherwise stalls an
  unattended launch, and `startup.checkUpdate: false` — and the launcher's
  `--auto-approve` and `--allow-home`. A launch prompt follows `--` as one
  argument: omp sends each positional argument as its own message and reads
  an `@`-led one as a file. Resume is `--continue`, which opens a new session
  when there is none.
- The configure image declares config ports 1455 (ChatGPT), 54545
  (Anthropic), 8085 (Gemini CLI), 51121 (Antigravity) and 54549 (OpenRouter).
  Every omp browser sign-in also takes the redirect URL pasted back, which is
  the fallback when a port is taken.

### pi

`pi/configure.sh` follows the opencode shape — a bare interactive `pi`, then a
read of what it stored ([ADR 26-10-15-259](../docs/adr/26-10-15-259-the-pi-harness-delivers-auth-json-and-exchanges-copilot-in-the-sandbox.md)). The image declares no secrets.

- **Credentials live in `~/.pi/agent/auth.json`**, one entry per provider:
  `{type: api_key, key, env?}` or `{type: oauth, access, refresh, expires, …}`.
  pi leaves a valid file as it is, through a run and through a 401.
- **One secret per provider**, `PI_<PROVIDER>_CREDENTIAL`. A key is a `token`;
  an Anthropic or xAI login is an `oauth` secret, renewed with the clients the
  claude-code and opencode images are renewed with; any other login is a
  `token` holding its access token, and the script says when it expires, or
  that it has — kept or new. Left out, and said: a key that is a `!` command
  (pi runs it), a ChatGPT subscription (`openai-codex`), whose account pi
  reads from the access token's claims and fails locally without, and an
  entry with no key or token at all (Bedrock's AWS profile or credential
  chain), which uses credentials kept on the machine pi runs on.
- **A Copilot login's secret is its GitHub token**, bound to `api.github.com`
  (or `api.<domain>` for GitHub Enterprise). pi exchanges it, at
  `copilot_internal/v2/token` with the token as a bearer header, for a Copilot
  token of about 30 minutes, which it keeps in `access`. That exchange runs in
  the sandbox, through the proxy, so the real Copilot token lands in the
  sandbox's `auth.json`; the GitHub token does not.
- **Delivery is auth.json as a credential document**, with sentinels in
  place of keys and access tokens, `refresh` a placeholder and `expires` far
  future where the control plane renews; a Copilot entry carries the sentinel
  in `refresh`, a placeholder `access` and `expires` zero, so pi exchanges
  before its first request.
- **Reconfigure writes the previous auth.json back** with each `PREV_`
  sentinel substituted, replacing what the configured harness delivered, so
  the session opens signed in to exactly that. An entry whose key, access
  token or (for Copilot) refresh token is still that sentinel was not
  replaced, and comes back as `usePrevious` with its previous entry verbatim.
  The default model is checked with a tool-free `pi -p`; a failure only warns.
  It returns `settings.json` and `keybindings.json` as pi wrote them; not
  `models.json` or `mcp.json`, which hold literal keys and commands.
- **The policy baseline** is the image's env — `PI_SKIP_VERSION_CHECK=1`,
  `PI_TELEMETRY=0`, since pi otherwise reports an install on a first run and
  after each upgrade and adds attribution headers to some providers'
  requests, and `PI_CLIPBOARD_OSC52=1` so a copy reaches the user's terminal as well as the
  box's X clipboard — `fd-find` installed, since pi otherwise downloads `fd`
  from GitHub on its first run, and the launcher's `--approve`: pi's
  project-trust dialog otherwise stalls a launch in any repository with
  `.agents/skills` or `.pi/`. pi asks no approval before a tool call, and
  honors `HTTPS_PROXY` for every provider. A launch prompt follows `--` as one
  argument, led by a space when it starts with `@`: pi sends each positional
  argument as its own message, and reads an `@`-led one as a file to attach
  even after `--`. Resume is `--continue`, which opens a new session when
  there is none.
- The configure image declares config ports 53692 (Anthropic), 1455 (OpenAI),
  1456 (Radius) and an ephemeral one for OpenRouter. Each browser sign-in also
  takes the redirect URL pasted back; Copilot, xAI, Kimi and Meta sign in by
  device code.
