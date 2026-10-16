# 26-10-16-423 — A harness is a provisioning bundle, and the sandbox agent provisions any box

- **Status**: Proposed (would supersede
  [0086](0086-a-harness-image-extends-the-base-and-its-manifest-is-override-only.md)
  §1, whose base-layer proof becomes a bundle applied beside the platform's,
  and narrow its §2, whose label stack now describes a bundle;
  [0012](0012-sandbox-config-is-three-attribute-owned-layers.md) §6's OCI
  label as the carrier of a *runnable* image's metadata;
  [26-10-10-767](26-10-10-767-a-container-sandbox-boots-systemd-and-a-unit-wires-its-volumes.md)'s
  agent installed at build time and never placed into a running machine; and
  [26-10-09-106](26-10-09-106-disco-vm-is-a-pool-runtime-and-the-server-runs-its-machines.md)
  §5's disco-vm build spec for a sandbox image. Would narrow
  [0068](0068-container-images-share-one-base-image.md), whose shared base
  becomes the Docker driver's sandbox image, and
  [0012](0012-sandbox-config-is-three-attribute-owned-layers.md) §7, whose
  project file becomes `.discobox/settings.json`)
- **Date**: 2026-10-16
- **Relates to**: [0012](0012-sandbox-config-is-three-attribute-owned-layers.md),
  [0020](0020-nested-docker-trust-is-injected-by-a-runc-wrapper.md),
  [0040](0040-discobox-images-are-reclaimed-by-label-and-local-age.md),
  [0050](0050-pool-build-state-is-not-sandbox-visible.md),
  [0068](0068-container-images-share-one-base-image.md),
  [0082](0082-a-stopped-sandbox-tracks-its-harness-image.md),
  [0086](0086-a-harness-image-extends-the-base-and-its-manifest-is-override-only.md),
  [0096](0096-the-server-reads-one-schema-checked-configuration-file.md),
  [0113](0113-the-cli-stages-the-images-its-server-loads.md),
  [0114](0114-a-sandbox-pins-its-agent-version-from-a-pool-cached-store.md),
  [0126](0126-a-sandbox-does-not-share-a-host-or-a-filesystem-with-its-pool.md),
  [0140](0140-a-discobox-reaches-the-discobox-api-through-its-pool-with-a-fixed-role.md),
  [0145](0145-a-sandbox-declares-its-platform-and-a-non-linux-one-is-a-vm-template.md),
  [26-10-09-106](26-10-09-106-disco-vm-is-a-pool-runtime-and-the-server-runs-its-machines.md),
  [26-10-09-143](26-10-09-143-a-vm-sandbox-starts-its-agent-when-its-bootstrap-arrives.md),
  [26-10-10-767](26-10-10-767-a-container-sandbox-boots-systemd-and-a-unit-wires-its-volumes.md).

## Context

A harness today is an OCI image three builds deep: `base-image` (Debian 13,
Docker CE, about 70 masked units), `sandbox-agent` (750 lines: the build and
desktop toolchains, Chromium, Node, Go, Nix, Homebrew, bun, uv, about 30 units
and drop-ins, eight Go binaries from this repository), and `harness/<id>` (one
`npm install -g` and four or five copied files). Its manifest rides in OCI
labels, snapshotted onto the harness config at registration. The sandbox agent
is baked into the image, so it is upgraded only by replacing the container,
and labels (`TreeExportLabel`, `WorktreeGitLabel`) tell the pool what an older
baked-in agent can do. Every provider ends in a privileged Docker container
started from that image, and development loads each rebuilt image into every
pool's Docker daemon.

Providers that run machines do not take our image. exe.dev's `exeuntu` and
boxd's default are both Ubuntu 24.04, with systemd behind the provider's own
init and kernel, a uid-1000 user with passwordless sudo in the `docker` group,
Ubuntu's `docker.io`, Go, Python with uv, gh, build-essential, a Chrome-family
browser, and the major coding agents. The wider field (Freestyle, Runloop,
Codespaces, E2B, Vercel) varies only around that. The `boxd.yaml` twins of
our Dockerfiles tried to rebuild the image as machine steps and became a
second implementation of it that drifted; they are not a design source.

The harness half is already small. What makes a box a discobox is the agent
and what it needs, plus a large set of conveniences, and most of either is
neither needed at boot nor tied to a container.

## Decision

### 1. A box is four layers, and only the last is per sandbox

| Layer | Owner | What it is |
|---|---|---|
| **Provider** | exe.dev, boxd, … or Discobox's Docker driver | The OS a box boots: distro, kernel, init, login user, network, DNS, SSH |
| **Platform** | The platform bundle | The agent, its core, and Discobox's features |
| **Harness** | A harness bundle | The agent CLI, its wrappers, policy files, and manifest |
| **Sandbox** | The agent, every boot | Account, sources, secrets, the intake |

The platform and harness layers are **machine-scoped**: no step may depend on
a sandbox's identity, account, uid, secrets or sources, and none may need
`sandbox.json`. That is what lets a provisioned box be snapshotted and reused
(§8). The sandbox layer is what `boot.Provision` and `boot.Wire` already do,
and it never runs before a snapshot.

### 2. A bundle is a `FROM scratch` OCI image that is applied, never run

A bundle is built by a Dockerfile `FROM scratch` and holds one directory per
feature it provides, `/features/<name>/`. It is never a container's root
filesystem: it is fetched, extracted, and applied by the agent.

```dockerfile
# harness/claude-code/Dockerfile
FROM scratch
COPY . /features/claude-code/
LABEL io.discobox.image.v1=${HARNESS_METADATA}
```

- **The manifest stays in labels on the image config.** `ResolveImageLabels`,
  layering by `<NN>-<name>`, merge by identity, the `discobox-harness-run`
  convention, and registration reading only the config are unchanged — the
  label stack now describes a bundle rather than a runnable image.
- **One directory per feature** means a layer adds features rather than
  overwriting another's files. A bundle carrying a whiteout is refused.
- **A harness bundle is platform-neutral** — one artifact, no per-architecture
  index — because its steps select by OS and architecture when they run, and
  large downloads are steps with a sha256, never bundle content.
- **A harness extends a harness, never the platform.** A third party builds
  `FROM ghcr.io/discobox-ai/discobox-harness-claude-code` and adds
  `/features/acme-claude/`, as harness images extend today. Building `FROM` the
  platform would pin the agent a harness was built with, which is the skew §7
  removes; a harness instead names the platform API version it needs.
- `HarnessConfig.Image`/`ImageDigest` become a bundle reference and digest
  (an OCI reference, or an asset URL, §13). Stored configs are migrated, not
  recreated.

### 3. Provisioning is idempotent typed steps the agent runs

A feature directory holds `provision.yaml` (`apiVersion:
discobox.ai/provision/v1`) and the files it names:

```
/features/claude-code/
  provision.yaml        # order, kind, requires, async, check, selectors
  install-cli.sh        # run: install-cli.sh
  launch.sh             # file: /usr/local/bin/discobox-harness-run, source: launch.sh
  prompt.sh  configure.sh  agent.conf  managed-settings.json
```

- The agent provides the step kinds `file`, `systemd` (enable, mask,
  drop-in), `group`, `env` (profile and manifest env), `download` (a URL and a
  sha256 per architecture), and `packages` (one list per distribution family),
  so it can do them correctly and record what it did. Anything else is `run`.
- **A script is always a file**, named by `run` or `check`, never a string in
  the YAML: shellcheck, editors and `launchertest` keep working on it, and the
  YAML holds only what a script cannot say.
- Each step is keyed by a digest of its definition and every file it names;
  applied digests are recorded under `/var/lib/discobox/provision/`. A step
  whose digest matches is skipped unless its `check` reports drift, so editing
  `install-cli.sh` re-runs that step alone, as Docker's layer cache did.
- Steps select on `os`, `arch`, and os-release `ID`/`ID_LIKE`. Only the Debian
  family (apt) is supported now; any other is refused by name.
- The runner is the agent's own, not a configuration-management tool: it must
  run on a bare box with nothing installed, and extend to macOS and Windows.

The name is **provision**: idempotent, re-run every boot, skipping what is
done. `boot.Provision` becomes its sandbox phase, and the
`/var/lib/discobox/provisioned` marker gives way to the step records.

### 4. Everything but the core is a feature, and every feature is on

**Core** is not optional and not a feature: the agent and what it serves
through argv[0] (§7), the proxy bridge and DNS stub, trusting the egress CA,
the rendered proxy env, the account and sudoers, systemd and the system bus,
the shell harness, `discobox-prompt-answer`, and the `discobox` and
`discobox-access` skills.

Everything else is a **feature** — `node`, `docker`, `browser`, `desktop`,
`nix`, `brew`, `go`, `python`, `rust`, `bun`, `mise`, `build`, `cli`,
`shells-editors`, `direnv`, `review`, `discobox-cli`, and harness bundles' own
— with declared `requires`. There are no profiles. Every feature is **on
unless opted out**, and each resolves to one outcome:

| Outcome | When | What provisioning does |
|---|---|---|
| `adopted` | The box already has a usable one | Installs nothing; wires ours on top (env, units, CA, a merged `daemon.json`) |
| `installed` | Not usable here, not opted out | Installs ours, into a prefix Discobox owns |
| `off` | Opted out, and nothing requires it | Installs nothing and wires none of its integration |

- **Usable, not present.** A feature's check names a version floor,
  reachability from a system PATH by every user, and writability where
  Discobox needs it. boxd's `/usr/local/bin/node`, a link into one user's nvm,
  fails it.
- **Probed once, then declared.** Only provisioning inspects the box. Its
  outcome is the box's declared feature set — what the manifest's `features`
  becomes — and every runtime integration reads that set rather than probing
  (0145 §3 stands). Boot steps that assume a feature today (direnv seeding,
  desktop launchers, session-bus drop-ins, the desktop API) become conditional
  on it.
- **Opting out never uninstalls.** A snapshot that has brew, used by a project
  that turns it off, leaves brew on disk and unwired. Uninstalling would make a
  snapshot depend on the last project to use it.
- **A requirement outranks an opt-out.** A harness that requires `node` in a
  project that turns it off is refused, naming both — unless the provider's
  Node is usable, which satisfies it.

### 5. Boot never waits on provisioning; readiness waits on what it needs

The agent serves as soon as it starts and provisions in the background.
Blocking means the existing readiness gates (the `ready` file,
`awaitSources`, the primary terminal's launch) wait for a step, never that the
box or the agent does.

- **Blocking:** core; a feature the harness requires; and every on feature's
  cheap wiring — its groups, its env and PATH, and its placeholder commands.
  Groups and env go first, before the account exists, because a group joined
  later reaches no running session.
- **Async** — everything else, and anything marked `async: true` on a feature
  or step. A harness requirement overrides the mark.
- Async features run in parallel in dependency order. Steps using apt or dpkg
  take one shared lock.
- **Waiting is built in.** `discobox-provision wait <feature>` blocks until the
  feature is ready or failed. Each async feature's commands (`docker`, `brew`,
  `nix`, `go`, …) are placeholders until it is installed: run early, one says
  on stderr that the feature is still provisioning, waits, and runs the real
  command. This is `nix-shim`'s and `discobox-docker`'s existing pattern,
  generalized.
- A failed async step fails its feature and nothing else. A failed blocking
  step fails readiness, and the primary terminal shows the error and its log.

### 6. Provisioning state is a file, reported and taught

`/run/discobox/provision.json` is written by the agent, readable by everyone,
and replaced atomically on each change. It describes this boot; what has been
applied is the records of §3.

```json
{
  "state": "provisioning",
  "ready": true,
  "updatedAt": "2026-10-16T19:52:03Z",
  "features": {
    "node":    { "state": "ready", "how": "adopted", "version": "24.9.0" },
    "docker":  { "state": "ready", "how": "snapshot" },
    "desktop": { "state": "provisioning", "async": true, "step": "chromium",
                 "log": "/var/log/discobox/provision/desktop.log" },
    "brew":    { "state": "off", "reason": "\"brew\": false in .discobox/settings.json" }
  }
}
```

`state` is `pending`, `provisioning`, `ready`, `failed` or `off`; `how` is
`installed`, `adopted` or `snapshot`. The same document rides the agent's
status report, so the console shows a feature still provisioning without
entering the box. The `discobox` skill names the file, the `wait` command, and
that an `off` feature was the user's choice and is not to be installed by hand.

### 7. The agent is delivered on every boot, in the platform bundle

**The platform bundle** is the release's multi-architecture `FROM scratch`
index (`linux/amd64`, `linux/arm64`; darwin and windows later). It holds:

- **one agent binary, multicall.** `discobox-runc`, `discobox-ca-anchor`,
  `discobox-power`, `discobox-docker` and `discobox-access` become argv[0]
  personalities of `discobox-sandbox-agent`, as `discobox-hook-publish`
  already is; core lays their links (`/opt/discobox/bin/runc`,
  `/usr/local/bin/docker`, …). They always ship at one version, and one static
  Go runtime is far smaller than six. `access` keeps its own `main` for use
  outside a discobox.
- **the platform's features**, `/features/core/` and the rest of §4.
- **the `discobox` CLI and `discobox-review`** in layers of their own, owned by
  the `discobox-cli` and `review` features. Third-party tools (`fresh`,
  neovim, nushell) are `download` steps, not bundle content.

The platform bundle is pinned by the **server's release**: a compiled-in
default a release overwrites at link time, the release manifest, and server
and provider overrides, as `DefaultPoolImage` is resolved. A harness bundle is
pinned by its harness config. Every box applies the two together.

- **The pool places only the agent.** On every start it makes sure the box
  holds the agent layer the release pins — bound into a container, copied onto
  a machine through the provider (26-10-09-143 §2) — and skips it when the box
  already holds that digest. Nothing else is the pool's to place.
- **The agent fetches the rest into a store of its own.** The box keeps a
  content-addressed store, `/var/lib/discobox/bundles/`: layers by digest,
  extracted once, and each bundle's tree by manifest digest. Given its pins,
  the agent reads each manifest and fetches only the layers its store lacks
  (§13), so an upgrade downloads what changed and nothing it already holds.
- **Layers are looked up before they are fetched**: the box's own store, then
  a read-only store its runtime offers — a container's pool binds its blob
  store, so a container fetches nothing its pool already has — then
  `assets.discobox.internal`.
- **The store is the box's.** It persists across restarts and is kept by a
  snapshot, so a box booted from one fetches nothing for the plan it was taken
  with. It is `excludeFromExport`: every byte is refetchable by digest. The
  agent keeps the trees its current and previous pins name, for rollback,
  and removes layers no kept tree uses.
- **Run.** The agent runs from its layer through a stable
  `/opt/discobox/current`. In a container it is the entrypoint, provisions
  core, then execs `/sbin/init`; on a machine one exec on first boot provisions
  core, which installs the agent's unit on that stable path.

Server–agent version skew goes away with it: the agent is always the release
the server shipped with, and the capability labels that described an older
baked-in agent are retired. What drifts is the box, which provisioning
converges.

### 8. A snapshot is a cache, never a requirement

Any box must provision from scratch. A snapshot only makes that fast: boot a
provider image, provision the platform and harness with every step blocking,
shut down, and snapshot — on the Docker driver, a committed image carrying
the reclaim label (ADR 0040). Its key is the provider image's digest and the
digest of the resolved plan, feature set included, so one snapshot serves
every sandbox wanting the same set. A snapshot holds no sandbox:
`/etc/machine-id` is left empty, no SSH host key is kept, and the sandbox
phase has never run in it.

### 9. Providers meet a contract and declare what they own

The platform may assume only:

1. Linux on amd64 or arm64, Debian family, systemd as PID 1.
2. Root by exec or passwordless sudo.
3. A persistent writable root disk, overlayfs, and cgroup2; `/dev/kvm` optional.
4. A way to place files and run one command, and one address reaching the
   agent's port (26-10-09-143 §2).
5. Outbound reach to package repositories, directly or through the pool.

A provider **declares** its login user; whether it owns DNS, the network,
sshd, the trust store and egress; and its snapshot capability. Provisioning
reads the declaration and never displaces what a provider owns: the DNS stub
sits beside the provider's resolver, Discobox's CA joins the trust store
beside the provider's, `daemon.json` is merged, and the runc personality
resolves the real runc (Ubuntu's is `/usr/sbin/runc`) rather than assuming
`/usr/bin/runc`.

**A box is dedicated to one sandbox.** Provisioning writes its account
database and sudoers, and idle stop powers it off.

### 10. The Docker driver owns its sandbox image

`base-image/Dockerfile` moves to the Docker worker driver
(`server/providers/dockerworker/sandbox-image/`) and becomes the image every
sandbox container starts from. It is resolved as the pool-agent image is:

| | Pool agent (today) | Sandbox image |
|---|---|---|
| Compiled-in default | `DefaultPoolImage` | `DefaultSandboxImage`, overwritten at link time by a release |
| Provider setting | `image` | `sandboxImage` |
| Server override | `dockerPoolImage` | `dockerSandboxImage` (`DISCOBOX_DOCKER_SANDBOX_IMAGE`) |
| Release manifest | `Images.PoolAgent` | `Images.SandboxBase` |

- **It is like the providers' images, and holds no special status.** Modelled
  on `exeuntu`, it holds what exe.dev and boxd have in common, not what either
  alone has: Ubuntu 24.04; systemd; a uid-1000 login user with passwordless
  sudo, in `docker`, lingering; Ubuntu's `docker.io`, buildx and compose-v2,
  socket-activated; git, gh, build-essential, python3, pipx, uv, Go, curl, jq
  and ripgrep; exeuntu's masks (udev, getty, ssh, modules-load, remount-fs,
  sysusers, random-seed, apt-daily, unattended-upgrades); an empty
  `machine-id` and no host keys. Node, a browser, Nix, brew and coding agents
  stay out, so the platform cannot come to depend on them unnoticed.
- **Every driver that runs the pool agent in Docker uses it** — local Docker,
  DigitalOcean, execvm, and the pool VMs of libkrun, vz and wslc, whose
  sandboxes are containers in the VM's daemon.
- **The pool-agent image builds `FROM` it**, keeping 0068's shared layers: the
  pool host pulls both.
- No harness is an image any more. A Docker sandbox is this image, or a
  snapshot committed from it, with the platform and harness trees bound in.

### 11. A repository's settings are one file

`.discobox/settings.json` holds what `.discobox/sources.json` and
`.discobox/project.json` hold today, and `features`:

```json
{
  "$schema": "https://raw.githubusercontent.com/discobox-ai/discobox/main/settings.schema.json",
  "sources": { "x": "https://github.com/discobox-ai/x" },
  "features": { "brew": false, "nix": { "enabled": true } },
  "runCommand": [],
  "workingDirectorySubpath": "",
  "filesAdd": []
}
```

- The split was by reader, not by content: the client resolves `sources`
  against its own disk, and the pool agent read the rest from the clone.
  Readers can take their own keys from one file, and the client now needs
  `features` too, before the box exists, to choose a snapshot or a plan. A
  remote primary source then costs one fetch at its pinned commit, not two.
- `features` is an object, so `false` opts out and a value can later carry a
  feature's settings without a schema change.
- The schema is generated by `configfile` (ADR 0096), committed, and named by
  its raw GitHub URL, as `server.yaml`'s and `client.yaml`'s are.
- The old files live in users' repositories and cannot be migrated: they are
  read when `settings.json` is absent, with a warning, and a repository
  holding both is refused rather than merged.
- `.discobox/hooks`, `services` and `review` stay directories: they are
  content, not settings.

### 12. What stays

0145 §3's manifest contract; 0114's agent version store, as part of the
`node` feature; 0020's runc wrapper, as a personality in the `docker` feature;
0126 whole; 26-10-09-143's bootstrap wait and VM contract; `boot.Wire`, as the
Docker driver's volume wiring rather than the platform's; and the development
sync of the two images that remain (the pool agent and the sandbox image).

### 13. Bundles come from the server, by blob, cached by the pool

Neither a pool nor a box pulls a bundle from a registry. The server serves
them, addressed by blob as OCI distribution addresses them, with only the two
calls a fetch needs:

| Call | Answers |
|---|---|
| `GET /api/assets/{name}` | The bundle's index or manifest — small JSON naming config and layer digests |
| `GET /api/assets/blobs/sha256:<digest>` | One blob, immutable and cacheable forever |

- **The server keeps one blob store** and fills it from any source: the
  release's bundles, pulled from the registry once; the CLI-staged image cache
  (ADR 0113); and, in development, the watcher's output, unpacked once.
  Registry credentials stay with the server.
- **The pool is a pull-through cache.** It answers from its own blob store in
  its build area (ADR 0050), fetches a blob it lacks from the server once, and
  shares one in-flight fetch among every request for the same digest. A
  sandbox's only way out is its pool (0126 §6); the cache is what lets one
  fetch serve every sandbox.
- **`assets.discobox.internal`** joins `api.` and `git.discobox.internal`
  (ADR 0140). The pool's proxy serves it to sandboxes read-only, for exactly
  the manifests that sandbox's pins name and the blobs they reference, and it
  is how the agent fills its store (§7). A bundle reference may name it,
  `https://assets.discobox.internal/<name>`, pinned by digest like any other.
- **Layers stay disjoint**, since a layer adds feature directories and carries
  no whiteout (§2), so a tree is its layers side by side — no overlay merge in
  the pool or the box.
- **Removal is by reference**: a blob leaves a store when no manifest pinned
  there — by a sandbox, a snapshot, or the server's harness configs and
  release — names it.
- **Development builds bundles without a Docker image store.** The watcher
  builds the same Dockerfiles with `docker buildx build --output
  type=oci,dest=build/assets/<name>.tar`, records each digest in the
  development manifest (`devimage`) beside the two images it still syncs, and
  the server re-pins built-in harnesses when a digest moves, as `SeedBuiltIns`
  does for images today. A box picks up a rebuilt agent or bundle on its next
  start, since both are delivered on every boot.

### 14. An account in the sandbox account's way is deleted

The client still sends the account it wants (ADR 0141), and the sandbox phase
still makes it inside the box (ADR 0025). Every provider image already has an
account at uid 1000, the uid every macOS and Windows client sends, so a
conflict is the ordinary case, and the box's account gives way entirely:

- **Any account holding the sandbox's uid or its name is deleted, home and
  all**, then the sandbox's account is created fresh from `/etc/skel`, as if
  the box had never had one. Nothing of the old account is renamed, adopted
  or kept.
- **Its processes are ended first** (`loginctl terminate-user`, then any that
  remain), since a provider's login user has them — boxd's exec runs as
  `boxd` — and `userdel` refuses a user that is logged in (PR #180).
- **With no conflict, the account is created**, as today.
- **The deletion runs once per box**, in the sandbox phase, before anything
  runs as the account; a later boot finds the account it made and leaves it.
- **What it costs is the provider's, and it is recorded.** Tooling that names
  its user (exe.dev's `login-user`, boxd's `boxd connect`) finds no such user,
  and whatever the provider installed in that home (boxd's nvm) is gone, which
  is also why §4 never adopts a tool that lives in a user's home. The provider
  declares its login user (§9), so provisioning knows what it deleted and
  records it in `provision.json`.

The uid, which the pool cache (ADR 0094) and source-scoped memories partition
by, is the client's in every case.

## Alternatives rejected

- **Keep images, and add a machine build beside them** (the `boxd.yaml`
  twins). Two implementations of one image drift, and neither runs on a
  provider's image.
- **A configuration-management tool** (Ansible, pyinfra, cloud-init, Nix).
  Each needs a runtime on a bare box or runs only before first boot, none
  reaches macOS and Windows on our terms, and Nix covers neither the desktop
  nor systemd units the way the box needs. The steps are already shell.
- **Scripts inline in the YAML.** They escape shellcheck, editors and the
  launcher tests, and a step's digest is no clearer for it.
- **A custom artifact media type, or a bare tarball, for a bundle.** A
  `FROM scratch` image keeps the label machinery, buildx's multi-architecture
  builds, layer sharing and registries with no new format.
- **A harness built `FROM` the platform**, as a harness image is built `FROM`
  the sandbox agent today. It pins the agent the harness was built with.
- **Separate helper binaries.** They always ship together, and each carries a
  Go runtime of its own.
- **Profiles.** A feature set with requirements expresses every profile, and
  a profile would be a second vocabulary for it.
- **Opt-in features.** Every box has every feature today; opting in would
  silently take them away.
- **Uninstall on opt-out.** It ties a snapshot's contents to the last project
  that used it.
- **Block boot until provisioned.** A fresh box takes minutes (Docker,
  Chromium, Nix, Go and Homebrew dominate); most of it is not needed to start.
- **Probe for features at runtime.** It contradicts 0145 §3; probing once in
  provisioning and declaring the outcome keeps that rule.
- **Bake the agent into the snapshot instead of delivering it.** Skew between
  server and agent returns, and so do the labels that described it.
- **A sandbox image owned by the platform.** The image is the provider layer;
  exe.dev and boxd bring their own, and the Docker driver is the provider that
  brings ours.
- **Debian 13 for the sandbox image.** Neither target provider runs it; a
  self-hosted box would provision differently from theirs.
- **Load development bundles into each Docker daemon**, as images are today.
  A bundle is never run, so a daemon's image store is the wrong place for it,
  and machine providers have no daemon to load into.
- **One OCI layout tar per bundle from the asset API.** Nothing in it can be
  shared: a release that changed only the agent would resend every layer.
- **The pool placing whole trees in every box.** An upgraded box would be
  sent what it already holds; with a store of its own, it fetches only what
  changed.
- **Pools pulling bundles from registries, or the server serving a registry
  API.** The first spreads registry credentials and a second fetch path to
  every pool; the second is a large surface for a fetch by name and digest.
- **`discobox.local` for the asset host.** `.local` is mDNS's (RFC 6762) and
  resolvers treat it specially; `.internal` is reserved for private use and is
  the convention the pool already serves.
- **Rename the account holding the uid to the client's name**, as boot does
  today. It fails on a provider's logged-in user, and the renamed account
  keeps a home built for someone else: the provider's dotfiles, its tools,
  and files owned under the name it had.
- **Adopt the account already holding the uid**, running under its name and
  home. The sandbox would be named by whichever provider it landed on, rather
  than by the person who made it.
- **Always run as the provider's login user.** A Linux client whose uid is
  free would lose its own account for no gain.
- **Always create our own account beside the provider's.** At uid 1000 there
  is nothing free to create, and a second human account on every box confuses
  ownership, sudo and processes.
- **Keep `sources.json` beside a settings file.** Two fetches for a remote
  primary source, and two files whose split no longer follows a reader.
- **Host the schema on discobox.ai.** It changes too fast for a sync to the
  site; the raw GitHub URL is the existing precedent.

## Deferred

- **One microVM per sandbox on a self-hosted pool**, booting the sandbox image
  as its root filesystem as exe.dev and boxd run theirs. It is a future
  machine driver's; the existing VM drivers keep a pool VM of containers.
- **Other distribution families.** Revisit when a provider we target ships one.
- **Providers without systemd** (Sprites, Vercel, Daytona, Modal). Revisit
  with the agent's own supervisor, which darwin and Windows already use.
- **macOS and Windows steps.** The selectors and the platform index reserve
  them; revisit with 0145.
- **Settings scopes** beyond the repository (`settings.local.json`, a user
  file). Feature resolution is a merge either way; revisit when an opt-out is
  wanted across repositories.
- **Skills in `settings.json`.** A separate feature.
- **Third-party features.** A feature published outside a harness bundle, and
  how it is trusted, waits for a first such feature.

## Consequences

- The harness Dockerfiles become `FROM scratch` bundles; `sandbox-agent/Dockerfile`
  becomes the platform bundle; `base-image/Dockerfile` moves to the Docker
  driver as its sandbox image; every `boxd.yaml`, `build:boxd-images` and the
  labels directory go.
- `HarnessConfig`'s image fields and platforms-from-index, `SandboxManifest`'s
  image pin, `imagereap` for harness images, `TreeExportLabel` and
  `WorktreeGitLabel` are replaced; stored harness configs and sandboxes are
  migrated, not recreated.
- The server gains the blob-addressed asset API and its blob store; the pool
  agent gains a pull-through blob cache, placing the agent layer on every
  start, and the `assets.discobox.internal` route; the agent gains its own
  content store; the Docker driver gains `sandboxImage` and its resolution
  chain.
- The agent becomes multicall and gains the runner, the status file,
  `discobox-provision wait`, and placeholder commands; runtime integrations
  read the declared feature set; `resources` stops reading `/sys/fs/cgroup` as
  the container's own, and the DNS stub stops assuming `eth*` interface names.
- `release:images` publishes bundles beside the two images; the watcher and
  `devimage` build and record bundles as OCI layout tars.
- The CLI reads `.discobox/settings.json`, and the `discobox` skill teaches the
  status file.
- A fresh box is slower to finish than an image pull; a snapshot makes it as
  fast or faster, and the box is usable well before it finishes.
