# Harness Design

This package owns the shared harness bundle contract (the manifest labels and
their layering, volume and env resolution) and the built-in harness bundles:
their manifests, harness-owned hook definitions, launchers, and save-back
exporters.

A harness has no setup step (ADR 26-11-02-152). Every registered harness runs;
a box on one with no login saved starts it signed out, the person signs in
there, and that box's save-back offers the login and settings as a harness
change to save for every box on the harness config.

## Saving local configuration

The six coding harnesses declare `harness.saveBack` (ADR 26-10-26-573): watched
home-relative paths, installed export/handoff commands, settings exclusions,
credential-file schemas, and authoritative provider metadata. Each built-in coding
harness bundle carries `capture/runtime.cjs`, `capture/settings.cjs`, and the parser
licenses beside its metadata and normalization module. The generic platform
ships none of these: export/handoff are arbitrary installed commands, not a Node
API. These six built-ins use Node through their own feature dependency. Save-back
is the only way a login or setting reaches a harness config from a box;
onboarding and workspace trust are each launcher's, never something a save
carries.

Export is noninteractive JSON over stdin/stdout. It preserves ordinary settings,
omits the harness-declared credential/local paths, and converts supported local
logins to secret values plus safe
credential documents. Unknown providers, credential commands, unsafe metadata,
and malformed inputs produce value-free diagnostics and protect local credential
files. A login a harness's export reads as signed out (Claude Code's cleared
OAuth tokens) leaves delivery restorable; an unsupported local login stays
protected. Exporter
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
Parents emptied by exclusions are omitted; explicitly empty user objects remain.
Bookkeeping a CLI writes on its own first run is local and is excluded from
both current settings and delivered baselines.
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
Portable settings are literal files. Workspace trust is never exported into
shared settings. A launcher whose CLI keeps trust in a file (claude-code,
codex-cli) records it in box-local state before starting the CLI, taking the
capture lock when that state shares a file with credentials; the others trust
through the image's env or a launch flag, or have no trust step (each
harness's `DESIGN.md` says which).
Credential documents use exact JSON objects `{"$secret":"ENV_NAME"}` at registered
secret leaves. The installer substitutes current sentinels using JSON encoding;
there is no template language or access to arbitrary sandbox configuration.
Literal template-like text in preferences remains ordinary text.

Handoff runs only after durable acceptance. A digest of the exact captured entry
prevents overwriting a newer login; already-applied entries are idempotent. File
stores replace only acknowledged entries and retain other local accounts.
What a handoff does beyond that is each harness's (its `DESIGN.md`).

A harness whose credentials live in a database (opencode, omp) reads and
writes them through the CLI's native auth export/import commands, never SQL.
Live database harness processes defer handoff
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
  secret declarations, save-back declaration, env defaults, declarative
  volumes, supplementary groups (`additionalGroups`), features, the
  destinations it needs to reach (`harness.egressHosts`, joining the harness's
  egress level and never restricting anything themselves, ADR 26-10-01-032 §3),
  and any command overrides are published in the bundle's OCI labels (`harness.ImageMetadata`) for
  server-side registration. `image.json` is the authoring source a label is
  compacted from (see `Taskfile.yml`). Save-back bundles also install that same
  document beside their capture command, which reads its provider
  definitions, so refresh clients and route policy have one source.
- **A configure declaration is refused** (ADR 26-11-02-152 §2): a `config`
  key under `harness` — the configure command, its reminder, its callback
  ports — fails to parse (`Image.UnmarshalJSON`), naming the retired key, so a
  bundle written for the removed flow is not registered without the setup
  step its author expected.
- **What a platform has is declared, and what it lacks is the declaration's
  absence** — never a probe at runtime or an optional interface (ADR 0145 §3).
  A bundle declares no `platform` and no `imageKind`, because its steps
  select the platform they run on and the kind of box is its pool's (ADR
  26-10-16-423 §2, ADR 26-11-01-122); a label naming either is refused at
  registration. `ValidateFor` keeps the rules 0145 §§3, 5 set for a
  non-Linux manifest — its one `account` by name alone and the absolute
  `shell` its terminals type into, and no Linux container mechanism — as the
  contract a non-Linux bundle would be judged by. No such bundle is
  registered today: every manifest is a bundle's, judged as Linux, which names
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
  A harness declares the hosts its CLI's own network documentation lists for
  the API, sign-in and token refresh, and leaves optional features to a level
  a person sets. One whose hosts are not yet established from its own
  documentation declares none (ADR 26-10-01-032 §3), so a restricted project
  lists them itself. Each harness's `DESIGN.md` says which it declares.
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
  A layer that names an `apiVersion` names `discobox.ai/image/v1`
  (`ImageAPIVersion`), which every manifest Discobox ships declares, or
  `discobox.dev/image/v1` (`LegacyImageAPIVersion`), the namespace it was
  first published in, which bundles already in registries carry; any other is
  refused. A legacy layer is read as it stands, never rewritten, so a config
  snapshotted from such a bundle keeps its manifest digest.
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
  layers add feature directories side by side and delete nothing.
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
- **A primary source can gate launch on Linux.** The terminal layer's
  platform-owned `discobox-primary-run` runs the source's
  `.discobox/scripts/pre-harness` before the resolved command, including
  overrides and resume commands. Failure leaves the login shell for recovery;
  auxiliary terminals bypass it. Harness bundles keep their existing launcher
  contract. See [the sandbox launch gate](../sandbox-agent/DESIGN.md#primary-source-launch-gate)
  and ADR 26-11-05-719.
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
  wrapper resuming a session ignores it — each included launcher replaces it
  with its own resume flag, except where one finding nothing to resume starts
  a fresh session with it (each harness's `DESIGN.md` says which) — but
  because the command is *typed* (ADR 0027),
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
- Each harness folder owns its `Dockerfile`, `image.json`, launcher, exporter,
  and other image-specific assets. The folder is the harness's feature
  ([ADR 26-10-16-423](../docs/adr/26-10-16-423-a-harness-is-a-provisioning-bundle-and-the-agent-provisions-any-box.md)
  §4): its `provision.yaml` installs the CLI (`install-cli.sh`) and places
  each file where the harness reads it, requiring the platform features it
  runs on (`node`, and `bun` for omp). An npm CLI is installed by the node
  feature's `npm-global`: with the npm the node feature chose (adopted or
  its own), into Discobox's prefix `/usr/local/lib/discobox/npm`, its
  commands linked into `/usr/local/bin` — never `npm install -g`, whose npm
  and prefix are whatever the box's PATH finds, on boxd one user's nvm. The
  `cli` step's `check-cli.sh` (`npm-global check`) puts it back when it
  left the prefix or was shadowed. Launchers, configure and capture scripts
  run `node` by name: the node feature links the chosen one into
  `/usr/local/bin`. See
  [`provision`](../sandbox-agent/provision/DESIGN.md#where-a-feature-installs).
- **A harness Dockerfile builds one artifact, its bundle** (§2): one `FROM
  scratch` stage, `bundle`, holding the feature's files under
  `/features/<feature>/` and the manifest in labels, published as
  `discobox-harness-<name>`, the name a box applies and a third party builds
  `FROM`. `harness/shell`'s bundle carries no feature, only its label. A
  published harness bundle is one `linux/amd64` manifest with no attestations
  — its content is the same on every architecture, and its steps select
  theirs when they run — where the platform bundle is a multi-platform
  index: Linux on amd64 and arm64, and darwin on arm64 (ADR 26-11-11-963 §9),
  each entry with its own platform layer.
- **A manifest argument is required, not optional.** `LABEL key=${ARG}` with no
  argument passed labels the image with the empty string, which is a build
  mistake that produces a working image nothing can register — so the platform
  stage of `sandbox-agent/Dockerfile` and every harness bundle stage refuse the
  build through `${ARG:?}` when their argument is missing, rather than leaving
  it for a server to report at seed time. Any build path publishing these
  (`build:*`, `release:images`, the development watcher) passes it.
- **The platform layer** (`10-platform`) is `sandbox-agent/image.json`, beside the Dockerfile that
  labels it, on the platform bundle's Linux entries; its darwin entry's is
  `sandbox-agent/image.darwin.json` (the env and PATH of a macOS sandbox, and
  no volumes, groups, desktop or Docker). `image.json` carries what is true of every Linux box the platform provisions
  rather than of any harness:
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
  - No container runtime's state (`/var/lib/docker`, `/var/lib/containerd`,
    and Kubernetes'): it stays on the box's root, a container's anonymous
    volume or a machine's root disk, never a declared volume, since a
    machine's data root can be FUSE, which no overlay upper directory takes
    ([ADR 26-11-23-993](../docs/adr/26-11-23-993-container-runtime-state-is-on-the-boxs-root-never-on-a-declared-volume.md)).
    It is not exported. `/var/lib/discobox` travels — it is the sandbox
    agent's own state.
  - `features`: `desktop` and `docker`, because the base image's filesystem
    ships both, and a manifest that leaves a feature out has not got it.
  - The rest of the persistent and cached paths, the `brew`, `docker`, and
    `kvm` supplementary groups, the `NIX_*`/`HOMEBREW_*`/`PATH`/
    `NPM_CONFIG_PREFIX` env, and the pnpm `storeDir` seed file.
- **The agent version a sandbox runs comes from that store, not from the
  image** ([ADR 0114](../docs/adr/0114-a-sandbox-pins-its-agent-version-from-a-pool-cached-store.md)).
  A harness bundle's feature declares what it installed in
  `/usr/local/libexec/discobox/agent.conf` (`AGENT_PACKAGE`, `AGENT_BIN`, and
  optionally `AGENT_IMAGE_DIRS`, where its own copy is read from when not
  Discobox's npm prefix), and the platform does the rest from
  `/etc/profile.d/sandbox-agent-store.sh`: `discobox-agent-store pin` points
  `~/.local/bin/<bin>` at the newest version in the store, once per sandbox and
  never again (a pin for another package of the same command, left by an
  earlier image, is replaced), and a detached `refresh` advances the store at most twice a day
  per pool user. Launchers are untouched by this — the pin is ahead of
  `/usr/bin` on PATH, so `launch.sh` still just runs its agent — and an image
  with no `agent.conf` (the `shell` harness) is unaffected. Because the store is
  the version source, each image also turns its agent's own updater off, in
  its env layer or its system config (each harness's `DESIGN.md` says how).
  `discobox-harness-upgrade` is the by-hand "newest, now".
- An image says what the discobox terminal can do when its agent will not
  infer it, in its own env layer, never every exec's default: an env layer
  reaches every exec in the box, and other programs read the same variables
  (claude-code's `FORCE_HYPERLINK=1`).
- **A sign-in's callback port needs no declaration.** A browser sign-in ends
  at a callback server on the CLI's own localhost, at a port the identity
  provider registered or one picked each run. The console's workspace forward
  binds each port a box listens on at its own number when that number is free
  on the user's machine, which is where the redirect sends the browser; when
  it is taken, the CLI's device-code or paste-back sign-in is the way. The
  configure flow's declared, exactly-bound ports are retired with it
  (ADR 26-11-02-152 §2, superseding ADR 26-10-09-867).

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
  the platform they run on, so a pool of any platform runs it, and the
  control plane records no harness platform: a sandbox's is its pool's (ADR
  26-11-01-122). See
  [`resources/harnessconfigs/DESIGN.md`](../server/internal/resources/harnessconfigs/DESIGN.md).
- A harness has no setup command. Every included coding harness starts
  signed out in a box with nothing saved, and declares `saveBack` so a sign-in
  there can be saved for the harness config — see
  [Credentials](#credentials).
- Provider-specific implementations live in one folder per harness, each with
  its own `DESIGN.md` (and `REVIEW.md` where it has pitfalls of its own) for
  what is particular to it — its image, hooks, memory, credentials, launcher
  and save-back:
  - [`claude-code`](claude-code/DESIGN.md) — Claude Code.
  - [`codex-cli`](codex-cli/DESIGN.md) — OpenAI's Codex CLI.
  - [`opencode`](opencode/DESIGN.md) — opencode 2 (`@opencode/cli`).
  - [`copilot`](copilot/DESIGN.md) — GitHub Copilot CLI (`@github/copilot`).
  - [`omp`](omp/DESIGN.md) — omp, "oh-my-pi" (`@oh-my-pi/pi-coding-agent`),
    on the base image's Bun.
  - [`pi`](pi/DESIGN.md) — pi (`@earendil-works/pi-coding-agent`), the agent
    omp descends from, on the base image's Node.
  - [`shell`](shell/DESIGN.md) — the login shell, and the end of the
    resolution chain.
- `registry` is the list of harnesses a release ships, exposed as
  `Definitions()` for the control plane to seed built-in harness configs. It is
  a manifest, not a dispatch table: nothing looks a driver up by harness type at
  runtime.

## Managed Layers

Prefer managed or system-owned configuration layers so hook capture is not
subject to repo trust prompts or user/project override. The hook definition
is an image-owned compatibility unit, versioned with the CLI it is written
for; no Go code in this package constructs or merges a harness's hook format,
or writes or merges its settings.
Where a CLI has no system layer for hooks, its lifecycle reaches an
image-owned plugin or extension that the CLI discovers beside the user's own,
never instead of them, and which publishes an allowlist of events rather than
a denylist, so an event the CLI ships next is not published by default.

| Harness | Hook layer |
|---|---|
| [claude-code](claude-code/DESIGN.md#hooks) | `/etc/claude-code/managed-settings.json` |
| [codex-cli](codex-cli/DESIGN.md#hooks) | `/etc/codex/hooks.json`, beside the `/etc/codex/config.toml` system layer |
| [opencode](opencode/DESIGN.md#hooks) | plugin named by `/etc/opencode/opencode.json` ([ADR 0147](../docs/adr/0147-opencode-publishes-its-lifecycle-through-an-image-owned-plugin.md)) |
| [copilot](copilot/DESIGN.md#hooks) | `/etc/github-copilot/policy.d/discobox.json` |
| [omp](omp/DESIGN.md#hooks) | extension in `~/.omp/agent/extensions/` (no system layer, [ADR 26-10-14-718](../docs/adr/26-10-14-718-the-omp-harness-imports-its-credentials-and-publishes-through-an-extension.md)) |
| [pi](pi/DESIGN.md#hooks) | extension in `~/.pi/agent/extensions/` (no system layer, [ADR 26-10-15-259](../docs/adr/26-10-15-259-the-pi-harness-delivers-auth-json-and-exchanges-copilot-in-the-sandbox.md)) |

Every hook runs `discobox-hook-publish --provider <harness> --event <name>`,
the sandbox agent's generic publisher.

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
  copy of its own. Codex's `Interrupt` is one; each harness's `DESIGN.md`
  names its others. Copying it in
  would squat a name Claude Code has not chosen, so that when Claude Code does
  choose one, stored hooks already claim it.
- **A session event is the user's session's.** A harness whose subagents run
  as sessions of their own publishes session-scoped events (turn end, start,
  compaction) for the root session only, or a child's turn ending would be
  indistinguishable from the user's; the filter is part of what the table
  entry means, and goes with it (opencode's and omp's `DESIGN.md` say how
  each filters).
- Each image's hook config and this table are kept together by a test in that
  harness's package: publishing an event without deciding what it is called
  across harnesses fails the build.

## Source-scoped memory

The runtime exposes opaque durable data for the primary source at
`/.discobox/data-per-source/primary` in every sandbox — shared by source key,
or private to the sandbox when there is no key to share it under (see
[`pool-agent/DESIGN.md`](../pool-agent/DESIGN.md)); only harnesses
interpret anything beneath it. A harness whose CLI has a memory feature that
can be pointed at a directory keeps its memory there in its own namespace;
one whose CLI has none, or none it can be told where to keep, leaves it
unused (each harness's `DESIGN.md` says which):

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
- The path is the CLI's environment, not a launch flag, wherever the CLI
  allows: the harness terminal launches at sandbox start, and the CLI typed
  into any shell afterwards must find the same memory. It lives in a layer
  a saved harness change does not replace (claude-code: a managed drop-in;
  codex-cli: its system layer and a bind mount).

Pool-agent and sandbox-agent know only the generic source-data mount and never
a harness memory format.

A harness's *policy* baseline turns its CLI's own approval prompts and inner
sandbox off, since the sandbox is the isolation boundary. Where it lives
decides whether a saved harness change can override it, so it is chosen per
harness: a system layer the user's settings file cannot replace (codex-cli),
the image's env or a launch flag (opencode, copilot, omp, pi), or a bundle
default in the user's own settings file that a saved or configured file
deliberately takes precedence over (claude-code). Each harness's `DESIGN.md`
says where its baseline is and why.

## Credentials

A harness has no setup command (ADR 26-11-02-152). A box on a harness config
with no login saved starts its CLI signed out; the person signs in there with
the CLI's own sign-in, and the box's save-back exports what the CLI wrote as a
harness change. Saving it stores each credential as a secret bound to the
harness config and its settings in the config's overlay, and every later box
on the config receives them: credentials as sentinels in the document the CLI
reads, settings as literal files. What each CLI writes, what its export makes
of it, and how it is delivered is in that harness's `DESIGN.md`.

**A credential never reaches another sandbox.** Delivery is a sentinel the
proxy swaps for the real value on an outbound request, only while a binding
covers it. The box that signed in holds the real credential until the save's
handoff replaces it with the accepted sentinel (ADR 26-10-26-573 §4).

**A secret may declare routes** (`Secret.Routes`,
[ADR 26-10-02-924](../docs/adr/26-10-02-924-a-secret-may-carry-routes-and-every-request-it-is-sent-in-must-fall-on-one.md)):
where the harness itself sends the credential, the image's alone. Each route
is a `secretroute.Route` (ADR 26-10-27-880 §4) — host patterns (a host covers
itself alone; `*.h` the hosts beneath it), and optionally methods, path globs
and a GraphQL or MCP block — and whether it is blocked; one declared in the
shape before host patterns reads as it was meant (`harnesscapture.Route`,
through `secretroute.FromLegacy`) until its image is rebuilt. A save writes
the routes onto the secret it creates. Policy changes append independently
enforced restrictions for owned secrets (ADR 26-11-04-772). An image declaring different ones
re-syncs them, while a secret a person bound is theirs to route. Every
request carrying the secret's sentinel must fall on one, and the most specific
decides: a blocked one refuses, and anything no route covers is refused. Name
the harness's own endpoints, and block the paths beneath them that reach what
the account owns rather than what the harness needs. A route a judge used to
read is a blocked one (ADR 26-10-27-880).

**A secret may declare a `rejectionScope`** (`harness.RejectionRule`: a host,
and path prefixes on a segment boundary), naming the requests that answer for
the credential; only what an upstream makes of one of them draws or clears
the refused credential's band. A harness whose API answers the same
everywhere declares none
([ADR 26-10-13-595](../docs/adr/26-10-13-595-a-harness-names-the-requests-whose-refusal-means-its-login-is-dead.md)).
Each harness's `DESIGN.md` names its scope, if it has one.

A browser sign-in's callback reaches a box through the console's workspace
forward, at its own number when that is free (see [Image Contract](#image-contract));
every built-in also offers a device-code, paste-back or key sign-in for when
it is not.

Literal settings and explicit credential references follow [ADR 26-10-30-457](../docs/adr/26-10-30-457-harness-settings-are-literal-and-credentials-use-json-bindings.md).

## Credential destination policy

Built-in static secrets and captured providers declare destination policy in their
manifests (ADR 26-11-04-772). Per-type `typeRoutes` overrides common `routes`;
`manualRoutes` refuses automatic capture of account-specific endpoints, with a
fixed diagnostic and local-file protection. See [the inventory and evidence](credential-destinations.md).
The shell harness collects no credentials. Server-side refresh endpoints are separate from
sandbox access-token spending.
