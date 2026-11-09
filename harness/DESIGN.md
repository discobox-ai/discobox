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
Parents emptied by exclusions are omitted; explicitly empty user objects remain.
Codex's model-availability tooltip state is local bookkeeping and is excluded
from both current settings and delivered baselines.
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
and records workspace trust in its sandbox-local system configuration before
launching the CLI, so direct starts see the same trust. Claude's initializer takes the capture lock while merging onboarding and trust
into local state, preserving other fields and serializing with credential handoff. Neither exports trust into shared settings.
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
- Each harness folder owns its `Dockerfile`, `image.json`, launcher, exporter,
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
  [Credentials per harness](#credentials-per-harness).
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
    to declare beyond its reserved slug, no secrets, no save-back, and its
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
  it silent in a build and outside a Discobox terminal. It hears the lifecycle
  through the bus, which a plugin subscribes to after opencode starts: the
  TUI, which is how the launcher runs a session, creates its session later and
  published every event in each run measured, while `opencode run` can create
  its session, and exit, before the subscription is up. The managed layer is
  read last of all, after the user's global config and a project's, and plugin
  entries accumulate across layers rather than replacing, so naming this one
  neither loses to a saved harness change nor drops a plugin the user
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
  `hook-extension.js` is the unit, installed beside the launcher; the
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
  The same shape as omp's: `hook-extension.js` installed beside the
  launcher, reached from `~/.pi/agent/extensions/discobox-hooks.js`. Each
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
  out of `.claude/settings.json`, which a saved harness change replaces with
  the user's own settings, and outranks anything that file says. Claude Code's temp tree — session scratchpads among it — is
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
that file is the user's: save-back exports whatever the user left in it, and
a baseline living there would be replaced by the first saved change. The same layer selects Codex's `activity` and `thread-title`
terminal-title items. Codex 0.150 gives an unnamed thread a provisional title
from its first prompt, replaces it asynchronously with a generated title, and
preserves manual `/rename` values. Codex emits that thread title as OSC 0; the
sandbox agent observes it and Discobox uses it as the sandbox display name.

## Credentials per harness

A harness has no setup command (ADR 26-11-02-152). A box on a harness config
with no login saved starts its CLI signed out; the person signs in there with
the CLI's own sign-in, and the box's save-back exports what the CLI wrote as a
harness change. Saving it stores each credential as a secret bound to the
harness config and its settings in the config's overlay, and every later box
on the config receives them: credentials as sentinels in the document the CLI
reads, settings as literal files. This section is what each CLI writes, what
the export makes of it, and how it is delivered.

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

A browser sign-in's callback reaches a box through the console's workspace
forward, at its own number when that is free (see [Image Contract](#image-contract));
every built-in also offers a device-code, paste-back or key sign-in for when
it is not.

### Claude Code

`/login` inside a box writes one of two credentials, both read by the export:

- `~/.claude/.credentials.json` — a subscription login's rotating OAuth blob
  (access token + **refresh token** + expiry), saved as an `oauth` secret
  (`CLAUDE_CODE_OAUTH_TOKEN`) with the fixed Anthropic `tokenUrl`/`clientId`
  from the provider definition, so the control plane refreshes the access
  token as it expires (see `resources/harnessconfigs/DESIGN.md` → OAuth
  secrets). Only `/login` yields a refresh token; `claude setup-token` does
  not. The blob's `scopes` and `subscriptionType` travel with it. They are not
  credentials — they say what the login may do — and Claude Code gates Remote
  Control on finding `user:profile` recorded beside the token, so they are
  kept rather than assumed.
- `primaryApiKey` in `~/.claude.json` — an Anthropic Console account's
  long-lived managed key, saved as a plain `token` secret
  (`ANTHROPIC_API_KEY`). A later sandbox gets its sentinel back **in the same
  field**, through a credential document whose `primaryApiKey` is an explicit
  secret reference. Startup also initializes this field from the current
  sentinel context when absent or carrying a known old alias; it preserves a
  newer local login.

Neither credential is exported as an environment variable. Both are declared
`delivery: file` (`harness.SecretDeliveryFile`), so the sentinel is minted and
rendered into the file the CLI reads, and the variable is withheld — a CLI
that finds both prefers the variable, and the variable carries none of the
metadata the file does. The delivered credentials file's expiry is far out,
so Claude Code never tries to rotate a sentinel.

- The launcher runs `claude-code-capture init`. It merges onboarding, the
  auto-mode nudge marker, and trust for the actual current directory into
  `.claude.json`, preserving other projects, preferences and newer local
  credentials. Malformed state is left intact and startup fails.
- `.claude/settings.json` is exported as the user left it, and remains
  literal, with the bypass-permission defaults. `~/.claude.json` is never
  exported as a file: besides the credential, it carries the box's own
  per-workspace trust map.
- Both credentials declare `rejectionScope`: `api.anthropic.com`, `/v1/messages`. A
  secret's `rejectionScope` (`harness.RejectionRule`: a host, and path prefixes
  on a segment boundary) names the requests that answer for the credential;
  only what an upstream makes of one of them draws or clears the refused
  credential's band. Anthropic's runner poll refuses a subscription login that
  works and its event logging accepts anything, so neither is evidence. A
  harness whose API answers the same everywhere declares none
  ([ADR 26-10-13-595](../docs/adr/26-10-13-595-a-harness-names-the-requests-whose-refusal-means-its-login-is-dead.md)).

### Codex CLI

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
  parsed `[projects]` tables. Before every start the launcher records its
  actual cwd in `/etc/codex/config.toml`, under a system-layer lock. The
  system file combines the image's defaults with an inline TOML `projects`
  map, retaining previously trusted folders. Restarts, source-less boxes and
  alternate working directories use the same path. Later direct `codex`
  starts in those folders see the same trust, without launch flags or edits
  to portable user preferences. Trusting `/` does not trust child repositories.
  The pinned-binary PTY test (`task test:codex-trust`) runs the old override
  as an untrusted control, then launch, restart and direct starts. It removes
  explicit approval/sandbox defaults from its fixture because those can hide
  a broken trust entry. Its Codex version is pinned in
  `test/codex-trust/version`; a vendor upgrade updates that contract test.
- Existing stored trust suffixes and credential references are converted in
  place by the server migration. Old sandbox bootstraps and capture checkpoints
  use the same bounded conversion on read. Unknown executable templates fail
  explicitly; migration never evaluates them or discards user preferences.
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
  so no clock has to agree with another. Claude Code's launcher has none: a
  failed model request at its start leaves it on its screen, retrying the
  request itself.

### OpenCode

A user connects as many models.dev providers as they like with `/connect`, so
the image declares no secrets at all: their names are only known once
something is connected. See
[ADR 0127](../docs/adr/0127-the-opencode-harness-runs-opencode-1.md) and
[ADR 26-10-15-848](../docs/adr/26-10-15-848-the-opencode-harness-runs-opencode-2.md).
ChatGPT's browser sign-in calls back to 1455 and DigitalOcean's to 1456;
ChatGPT also has a headless sign-in, and the rest connect with a key.

- **Every opencode command runs `--standalone`**: the launcher and the
  credentials helper. Without it opencode talks to a per-user background
  service, which outlives the terminal that started it and would run the hook
  plugin in that terminal's environment for every terminal after it.
- **Credentials are rows in opencode's database**, read and written only
  through `opencode auth export` and `auth import`, opencode's own interchange
  format: `{id, integrationID, label, active, value}`, where `value` is
  `{type: key, key}`, `{type: oauth, methodID, access, refresh, expires,
  metadata}`, or `{type: external, methodID}`. Import never updates an id it
  already holds.
- **One secret per provider**, `OPENCODE_<PROVIDER>_CREDENTIAL`, from the
  provider's active credential. The name is stable per provider, so a save
  that replaces a credential updates its secret in place. An `external`
  credential holds no secret and is not captured.
- **The secret's type follows how the credential renews.** A key is a `token`.
  An OAuth sign-in whose `refresh_token` grant the control plane can perform —
  OpenAI (ChatGPT, the client and endpoint the codex image refreshes), xAI
  (whose endpoint takes the request form-encoded, recorded as
  `tokenRequestEncoding: form`) — is an `oauth` secret. Any other OAuth sign-in
  (GitHub Copilot; DigitalOcean and Snowflake, which cannot be renewed from
  here) is a `token` holding its access token, and the export says when it
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
  opencode's own legacy mapping, so it keeps working until a new sign-in is
  saved, and on a sandbox that ran opencode 1 the switch keeps it active over
  opencode's own import of the same file. The helper is the one place the
  image's entries are made (`own`).
- It also exports opencode's settings as the user left them — the global
  `opencode.json`, `opencode.jsonc`, `config.json`, and `cli.json` (theme,
  keybinds; opencode 2 migrates an opencode 1 `tui.json` into it) — and
  `.local/state/opencode/model.json`, the `/models` choice.
- **The policy baseline is `--auto`** on the launch, approving every tool use a
  rule does not deny, and a launch prompt goes to `--prompt`, which the TUI
  submits. opencode's system layer, `/etc/opencode`, is its *managed*
  configuration, which outranks a project's own rules, and the user's global
  config is replaced by a saved change — so neither is the place for it.
- **Discobox's own settings for the harness** live in
  `.config/discobox/opencode-harness.json`, a plain harness file the image
  declares: `webSearch`. It is edited in a box and saved as a harness change,
  or with `discobox admin harness edit opencode
  .config/discobox/opencode-harness.json`. opencode 2 searches with every
  provider by default, through a keyless search service. The launcher reads
  `webSearch`: `false` denies the `websearch` permission through
  `OPENCODE_CONFIG_CONTENT`, which takes the tool away from the model; `true`
  and `null` — the image's baseline — leave opencode's default.

### GitHub Copilot

A box signs in inside Copilot, by `/login` or with a token
([ADR 26-10-02-840](../docs/adr/26-10-02-840-the-copilot-harness-takes-its-github-token-from-the-environment.md)).
`/login`'s browser sign-in calls back to a port it picks fresh each run
(`listen(0)`; GitHub accepts any loopback port); device code is the way when
that port is taken on the user's machine. Its token is an OAuth App `gho_`
token with `repo` scope among others; a fine-grained PAT with only "Copilot
Requests" is the least-privilege alternative.

- **Storage is `~/.copilot/config.json`**, Copilot's own state file (JSON
  under `//` comment lines): `authTokens["<host>:<login>"].token`, for the
  account `lastLoggedInUser` names. A sandbox has no keychain, and Copilot
  stores the token in plaintext only under `storeTokenPlaintext`, which the
  export never carries into shared settings.
- **One `token` secret, `COPILOT_GITHUB_TOKEN`, delivered in the
  environment.** Copilot documents that variable for headless use and it
  outranks a stored login; `gh` and git never read it, and Copilot strips it
  from the environment of the commands its agent runs. Neither sign-in yields
  anything to refresh. `config.json` is never exported as a file: it is
  Copilot's state, rewritten as it runs. After a save the handoff removes the
  saved plaintext token; a running process keeps its previous environment
  until it restarts.
- **Only github.com.** A login to another host is refused by the export: an
  Enterprise Cloud account also needs `COPILOT_GH_HOST` in every sandbox, and
  a harness change carries no env.
- **The token is held to Copilot's own traffic.** `COPILOT_GITHUB_TOKEN`
  declares routes (ADR 26-10-02-924), which a save writes onto the secret:
  Copilot's own endpoints under `githubcopilot.com` and `api.github.com` are
  sent; its MCP server, its agents and the rest of `github.com` are blocked
  routes, which a judge used to read (ADR 26-10-27-880); anything else is
  refused. The agent needs a binding of its own to do more on GitHub.
- It exports `~/.copilot/settings.json` as the user left it (`/config model`,
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

A harness whose credentials are any number of providers'
([ADR 26-10-14-718](../docs/adr/26-10-14-718-the-omp-harness-imports-its-credentials-and-publishes-through-an-extension.md)).
The image declares no secrets. A box signs in with `/login`; every omp
browser sign-in (ChatGPT 1455, Anthropic 54545, Gemini CLI 8085, Antigravity
51121, OpenRouter 54549) also takes the redirect URL pasted back, which is the
way when its port is taken on the user's machine.

- **Credentials live in SQLite** (`~/.omp/agent/agent.db`), and omp moves them
  with `omp auth export` and `omp auth import`: `{version: 1, credentials:
  [{provider, credential}]}`, where a credential is `{type: api_key, key}` or
  `{type: oauth, access, refresh, expires, …identity}`. An import replaces the
  same provider's key and the same account's login, and keeps the rest.
- **One secret per credential**, `OMP_<PROVIDER>_CREDENTIAL`, a second account
  of one provider `_2`. omp exports several accounts in no fixed order, so a
  credential is named by which secret it is: the one whose sentinel it still
  holds, else the previous secret of the same account (provider, `accountId`,
  `email`), else the provider's lowest free name. A key is a `token`; an OAuth
  login the control plane can renew — Anthropic and ChatGPT with the clients
  the claude-code and codex images are renewed with, xAI form-encoded — is an
  `oauth` secret; any other login is a `token` holding its access token, and
  the export says when it expires, or that it has.
- **Delivery is the export document as a credential document**,
  `.config/discobox/omp-credentials.json`, with sentinels in place of keys and
  access tokens, `refresh` a placeholder (or the sentinel, for GitHub Copilot,
  which stores its token in both), and `expires` far future where the control
  plane renews. `launch.sh` imports it before starting omp, on every launch.
  An imported sentinel goes out as the request's credential and is never
  refreshed. omp reads ChatGPT's `chatgpt-account-id` from the access token's
  claims, which a sentinel does not carry.
- **`PI_PROXY` follows `HTTPS_PROXY`** (`proxy-env.sh`, installed as
  `/etc/profile.d/omp-proxy.sh` and sourced by the launcher). omp's Anthropic
  transport is its own client that reads only `PI_PROXY`, and without it
  connects direct, which a sandbox's egress does not allow: every Anthropic
  request hangs into "Connection error.". It is derived rather than written
  into the manifest, where a build in a discobox would rewrite the loopback
  address (ADR 26-10-01-425).
- `ONNXRUNTIME_NODE_INSTALL_CUDA=skip`, at build time and in the image's env
  (the version store installs omp in the sandbox): omp's onnxruntime-node
  dependency otherwise downloads onnxruntime's CUDA build from GitHub on
  install, which no sandbox can use and which hung a build outright.
- It exports `.omp/agent/config.yml` as omp wrote it; not `models.yml`, which
  holds literal keys and `!` commands, as pi's `models.json` does, so custom
  providers are configured in each sandbox.
- **The policy baseline** is `PI_CONFIG_FILES=/etc/omp/discobox.yml` in the
  image's env — `startup.setupWizard: false`, whose wizard otherwise stalls an
  unattended launch, and `startup.checkUpdate: false` — and the launcher's
  `--auto-approve` and `--allow-home`. A launch prompt follows `--` as one
  argument: omp sends each positional argument as its own message and reads
  an `@`-led one as a file. Resume is `--continue`, which opens a new session
  when there is none.

### pi

The agent omp descends from, with the same any-providers credentials
([ADR 26-10-15-259](../docs/adr/26-10-15-259-the-pi-harness-delivers-auth-json-and-exchanges-copilot-in-the-sandbox.md)).
The image declares no secrets. A box signs in with `/login`; each browser
sign-in (Anthropic 53692, OpenAI 1455, Radius 1456, OpenRouter on a port of
its own) also takes the redirect URL pasted back, and Copilot, xAI, Kimi and
Meta sign in by device code.

- **Credentials live in `~/.pi/agent/auth.json`**, one entry per provider:
  `{type: api_key, key, env?}` or `{type: oauth, access, refresh, expires, …}`.
  pi leaves a valid file as it is, through a run and through a 401.
- **One secret per provider**, `PI_<PROVIDER>_CREDENTIAL`. A key is a `token`;
  an Anthropic or xAI login is an `oauth` secret, renewed with the clients the
  claude-code and opencode images are renewed with; any other login is a
  `token` holding its access token, and the export says when it expires, or
  that it has. Left out, and said: a key that is a `!` command (pi runs it), a
  ChatGPT subscription (`openai-codex`), whose account pi reads from the
  access token's claims and fails locally without, and an entry with no key or
  token at all (Bedrock's AWS profile or credential chain), which uses
  credentials kept on the machine pi runs on.
- **A Copilot login's secret is its GitHub token**, bound to `api.github.com`
  (or `api.<domain>` for GitHub Enterprise). pi exchanges it, at
  `copilot_internal/v2/token` with the token as a bearer header, for a Copilot
  token of about 30 minutes, which it keeps in `access`. That exchange runs in
  the sandbox, through the proxy, so the real Copilot token lands in the
  sandbox's `auth.json`; the GitHub token does not. An unchanged refresh
  sentinel with a changed `access` is that exchange, not a new login.
- **Delivery is auth.json as a credential document**, with sentinels in
  place of keys and access tokens, `refresh` a placeholder and `expires` far
  future where the control plane renews; a Copilot entry carries the sentinel
  in `refresh`, a placeholder `access` and `expires` zero, so pi exchanges
  before its first request.
- It exports `settings.json` and `keybindings.json` as pi wrote them; not
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

Literal settings and explicit credential references follow [ADR 26-10-30-457](../docs/adr/26-10-30-457-harness-settings-are-literal-and-credentials-use-json-bindings.md).

## Credential destination policy

Built-in static secrets and captured providers declare destination policy in their
manifests (ADR 26-11-04-772). Per-type `typeRoutes` overrides common `routes`;
`manualRoutes` refuses automatic capture of account-specific endpoints, with a
fixed diagnostic and local-file protection. See [the inventory and evidence](credential-destinations.md).
Shell collects no credentials. Server-side refresh endpoints are separate from
sandbox access-token spending.
