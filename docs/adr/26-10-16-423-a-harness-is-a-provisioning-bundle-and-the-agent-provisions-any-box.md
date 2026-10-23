# 26-10-16-423 — A harness is a provisioning bundle, and the sandbox agent provisions any box

- **Status**: Accepted. It supersedes:
  [0068](0068-container-images-share-one-base-image.md), whose Debian 13 and
  Docker CE base becomes the Docker driver's sandbox base image (§10);
  [0086](0086-a-harness-image-extends-the-base-and-its-manifest-is-override-only.md)
  §1's required base layer;
  [0012](0012-sandbox-config-is-three-attribute-owned-layers.md) §6's OCI
  label as the carrier of a *runnable* image's metadata;
  [0056](0056-a-repository-declares-the-sources-it-is-worked-on-with.md) §1's
  separate `sources.json`; and, for Linux sandboxes only,
  [26-10-09-106](26-10-09-106-disco-vm-is-a-pool-runtime-and-the-server-runs-its-machines.md)
  §5's disco-vm build spec. It narrows:
  [0086](0086-a-harness-image-extends-the-base-and-its-manifest-is-override-only.md)
  §2, whose label stack the server composes across two bundles;
  [0043](0043-shell-is-an-ordinary-harness-image.md)'s rejection of a
  control-plane path that resolves the base;
  [0012](0012-sandbox-config-is-three-attribute-owned-layers.md) §7, whose
  project file becomes `.discobox/settings.json`;
  [0016](0016-sandbox-image-upgrades-are-explicit-and-in-place.md) and
  [0021](0021-upgrade-is-a-re-pin-and-preserves-power-state.md), whose
  explicit re-pin now covers the harness bundle and the platform (§7);
  [0007](0007-declarative-sandbox-volumes-wired-by-the-sandbox-agent.md) §3,
  [0075](0075-the-nix-store-is-a-pool-shared-cache-seeded-on-first-use.md),
  [0107](0107-homebrew-is-image-content-on-an-overlay-handed-to-a-group.md)
  and [0114](0114-a-sandbox-pins-its-agent-version-from-a-pool-cached-store.md)
  §7, whose image content becomes provisioned content (§4);
  [0126](0126-a-sandbox-does-not-share-a-host-or-a-filesystem-with-its-pool.md)
  §3, whose bootstrap exception extends to the agent layer;
  [26-10-09-143](26-10-09-143-a-vm-sandbox-starts-its-agent-when-its-bootstrap-arrives.md)
  §§1–2, whose image no longer has the agent installed and whose backend
  places the agent layer and runs one command (§7); and
  [26-10-09-106](26-10-09-106-disco-vm-is-a-pool-runtime-and-the-server-runs-its-machines.md)
  §4, whose image kind becomes the provider's (§2); and
  [0017](0017-resource-state-is-desired-and-observed-with-no-operations.md) §§5
  and 11, whose fingerprint leaves out a delivered sandbox's image and
  platform pins (§7, as amended). It settles that ADR's
  deferred ids for a Linux machine sandbox's account (§14).
- **§7 proposed to be narrowed, for a remote driver, by**: [26-10-23-551](26-10-23-551-a-vm-pool-bootstraps-its-own-agent-from-a-bundle-and-provisions-its-machines.md)
  (Proposed) — the pool agent, not the provider, places a machine's agent
  layer and runs its one command
- **Date**: 2026-10-16
- **Relates to**: [0007](0007-declarative-sandbox-volumes-wired-by-the-sandbox-agent.md),
  [0012](0012-sandbox-config-is-three-attribute-owned-layers.md),
  [0016](0016-sandbox-image-upgrades-are-explicit-and-in-place.md),
  [0020](0020-nested-docker-trust-is-injected-by-a-runc-wrapper.md),
  [0025](0025-the-sandbox-user-is-one-contract-resolved-inside-the-sandbox.md),
  [0040](0040-discobox-images-are-reclaimed-by-label-and-local-age.md),
  [0043](0043-shell-is-an-ordinary-harness-image.md),
  [0050](0050-pool-build-state-is-not-sandbox-visible.md),
  [0056](0056-a-repository-declares-the-sources-it-is-worked-on-with.md),
  [0068](0068-container-images-share-one-base-image.md),
  [0075](0075-the-nix-store-is-a-pool-shared-cache-seeded-on-first-use.md),
  [0082](0082-a-stopped-sandbox-tracks-its-harness-image.md),
  [0086](0086-a-harness-image-extends-the-base-and-its-manifest-is-override-only.md),
  [0096](0096-the-server-reads-one-schema-checked-configuration-file.md),
  [0107](0107-homebrew-is-image-content-on-an-overlay-handed-to-a-group.md),
  [0113](0113-the-cli-stages-the-images-its-server-loads.md),
  [0114](0114-a-sandbox-pins-its-agent-version-from-a-pool-cached-store.md),
  [0126](0126-a-sandbox-does-not-share-a-host-or-a-filesystem-with-its-pool.md),
  [0140](0140-a-discobox-reaches-the-discobox-api-through-its-pool-with-a-fixed-role.md),
  [0141](0141-a-sandbox-account-is-created-with-an-id-the-guest-gives-accounts.md),
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
| **Sandbox** | The agent, every boot | Account, sudoers, sources, secrets, the intake |

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

- **The manifest stays in labels on the image config**, and within a bundle
  `ResolveImageLabels`, layering by `<NN>-<name>`, merge by identity and the
  `discobox-harness-run` convention are unchanged. What changes is where the
  base layer comes from: a harness no longer inherits it, so the server
  composes the platform bundle's `10-platform` layer under the harness
  bundle's own, resolving the platform its release pins. That is a
  control-plane path that resolves the base, which 0043 turned down when the
  base was any image a harness was built `FROM`; here the base is one release
  artifact, so there is exactly one to resolve. 0086 §1's proof of lineage
  goes with it.
- **One directory per feature** means a layer adds features rather than
  overwriting another's files. A bundle carrying a whiteout is refused.
- **A harness bundle is platform-neutral** — one artifact, no per-architecture
  index — because its steps select by OS and architecture when they run, and
  large downloads are steps with a sha256, never bundle content. It names no
  image kind: the kind 26-10-09-106 §4 places by becomes the provider's
  (`oci` for the Docker driver's containers, `discovm/<driver>` for a
  machine), and a harness places on any provider whose platform its steps
  select.
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
  sha256 per architecture), and `packages` (a list per distribution, the box's
  own os-release `ID`'s before an `ID_LIKE`'s),
  so it can do them correctly and record what it did. Anything else is `run`.
- **A script is always a file**, named by `run` or `check`, never a string in
  the YAML: shellcheck, editors and `launchertest` keep working on it, and the
  YAML holds only what a script cannot say.
- Each step is keyed by a digest of its definition and every file it names;
  applied digests are recorded under `/opt/discobox/provision/`. A step whose
  digest matches is skipped unless its `check` reports drift, so editing
  `install-cli.sh` re-runs that step alone, as Docker's layer cache did.
- **The records live on the filesystem they describe**: the root filesystem,
  never a declared volume. A container recreated from an image or snapshot
  then has the records of exactly what that filesystem holds, and a committed
  snapshot, which keeps the root filesystem and no volume, keeps both.
- Steps select on `os`, `arch`, and os-release `ID`/`ID_LIKE`. Only the Debian
  family (apt) is supported now; any other is refused by name.
- The runner is the agent's own, not a configuration-management tool: it must
  run on a bare box with nothing installed, and extend to macOS and Windows.

The name is **provision**: idempotent, re-run every boot, skipping what is
done. `boot.Provision` becomes its sandbox phase, and the
`/var/lib/discobox/provisioned` marker gives way to the step records.

### 4. Everything but the core is a feature, and every feature is on

**Core** is not optional and not a feature, and like every platform step it
is machine-scoped: the agent and its `discobox-ca-anchor` and `discobox-power`
personalities (§7), `discobox-access`, the proxy bridge and DNS stub, trusting
the egress CA, the rendered proxy env, systemd, the system bus and sudo, the
no-op `discobox-harness-run` 0086 §3 has the base ship,
`discobox-prompt-answer`, and the `discobox` and `discobox-access` skills.
The account and its sudoers drop-in are the sandbox phase's (§14), and `shell`
stays an ordinary harness (0043 §1), a bundle with no features of its own.

Everything else is a **feature** — `node`, `docker`, `browser`, `desktop`,
`nix`, `brew`, `go`, `python`, `rust`, `bun`, `mise`, `build`, `cli`,
`shells-editors`, `direnv`, `review`, `discobox-cli`, and harness bundles' own
— with declared `requires`. There are no profiles. Every feature is **on
unless opted out**, and each resolves to one outcome:

| Outcome | When | What provisioning does |
|---|---|---|
| `adopted` | The box already has a usable one | Installs nothing; wires ours on top (env, units, CA, the runc and `docker` personalities, a merged `daemon.json`) |
| `installed` | Not usable here, not opted out | Installs ours, into a prefix Discobox owns |
| `off` | Opted out, and nothing requires it | Installs nothing and wires none of its integration |

- **Usable, not present.** A feature's check names a version floor,
  reachability from a system PATH by every user, and writability where
  Discobox needs it. boxd's `/usr/local/bin/node`, a link into one user's nvm,
  fails it.
- **Probed once, then reported — never declared.** Only provisioning inspects
  the box, and its outcomes are what `provision.json` reports (§6); every
  runtime integration in the box reads that rather than probing (0145 §3
  stands). Boot steps that assume a feature today (direnv seeding, desktop
  launchers, session-bus drop-ins, the desktop API) become conditional on it.
  The manifest's `features` stays the authored declaration of what a bundle
  provides, and the document stays the pool agent's to write (0012 §§2, 8):
  an outcome the box observed is not written into it.
- **A reported feature grants nothing.** The pool and the server never decide
  access — the pool's builder or registry among it — from what a box reports
  it has; the box would be choosing its own access.
- **Image content becomes provisioned content.** What 0107 (Homebrew), 0075
  (the Nix seed), 0007 §3 (a data volume over image content becomes an
  overlay) and 0114 §7 (the agent declared in a file) say of the image is
  said of what provisioning installed. On a snapshot that content is in the
  root filesystem, under any volume wired over it, as before; on a container
  provisioned from scratch, content installed under a declared `data` path
  lands in that volume, and stays excluded from export where the path says
  so.
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
entering the box. Every string in it is written by the box, and the console
escapes it as it does all text a discobox wrote (`cli/REVIEW.md`). The
`discobox` skill names the file, the `wait` command, and that an `off` feature
was the user's choice and is not to be installed by hand.

### 7. The agent is delivered on every boot, in the platform bundle

**The platform bundle** is the release's multi-architecture `FROM scratch`
index (`linux/amd64`, `linux/arm64`; darwin and windows later). It holds:

- **the agent, multicall.** `discobox-runc`, `discobox-ca-anchor`,
  `discobox-power` and `discobox-docker` become argv[0] personalities of
  `discobox-sandbox-agent`, as `discobox-hook-publish` already is: they always
  ship at one version, and one static Go runtime is far smaller than five.
  Core lays the links of its own personalities; the `docker` feature lays
  `/opt/discobox/bin/runc` and `/usr/local/bin/docker`, and an `off` docker
  feature lays neither.
- **`discobox-access`, separately.** It stays its own binary from its own
  module: the root `DESIGN.md` and `access/DESIGN.md` keep it apart from the
  runtime so it can run outside a discobox, and an argv[0] alias would weld
  the two.
- **the platform's features**, `/features/core/` and the rest of §4.
- **the `discobox` CLI and `discobox-review`** in layers of their own, owned by
  the `discobox-cli` and `review` features. Third-party tools (`fresh`,
  neovim, nushell) are `download` steps, not bundle content.

The platform bundle is named by the **server's release**: a compiled-in
default a release overwrites at link time, the release manifest, and server
and provider overrides, as `DefaultPoolImage` is resolved. A harness bundle
is named by its harness config. A sandbox pins both, each by digest, and both
follow 0016, 0021 and 0082 as a harness image does: pinned at create, moved
by an explicit upgrade or a repair, and moved for a stopped sandbox when what
its server names moves — the platform when the release's reference resolves
to a new digest, the harness bundle when its config is re-pinned. Every box
applies the two together.

*Amended 2026-10-21, before anything shipped against it:* the platform is part
of a sandbox's pin. This section first had a box run the platform its server
ships from its next start, but a start is not a create — the server's start,
restart and ensure-running carry no spec, and a pool starts a box on use
without asking — so the pool's record of what a box may fetch (§13) could only
follow the server's platform through a pin that a create carries, as the image
pin always has.

- **The pool places only the agent layer**, and only when the box lacks its
  digest. A container gets it copied into its filesystem by the pool agent's
  Docker sandbox runtime, which owns sandbox containers (`pool-agent/DESIGN.md`;
  providers never touch them, `server/providers/DESIGN.md`), with the agent's
  units, between create and start — the pre-boot placement 26-10-08-127 §2 and
  0126 §3 already make for `sandbox.json` — so systemd stays PID 1 (26-10-10-767
  §1) and `discobox-wire.service` still wires volumes before `sysinit.target`
  (its §2), now a unit that runtime places. A machine gets it copied
  through the provider, and the provider runs one command on its first boot,
  which provisions core and installs the agent's unit (26-10-09-143 §2's
  placement, widened). Nothing the pool owns or vouches for is ever bound
  into a box — its build area (ADR 0050), its blob store, an agent layer or
  bundle tree: a sandbox's root can remount a read-only bind, and a shared
  tree it rewrote would run as root in every other box of the pool. The pool
  cache and source data stay bound as they are (ADRs 0050, 0094): shared on
  purpose, writable by every box, and trusted by nothing.
- **The agent fetches the rest into a store of its own.** The box keeps a
  content-addressed store, `/opt/discobox/bundles/`, on its root filesystem
  for the reason §3's records are: layers by digest, extracted once, and each
  bundle's tree by manifest digest. Given its pins, the agent reads each
  manifest and fetches only the layers its store lacks from
  `assets.discobox.internal` (§13), and checks every blob against its digest
  before it extracts it. An upgrade downloads what changed and nothing it
  already holds.
- **The store is the box's.** It persists across restarts and is kept by a
  snapshot, so a box booted from one fetches nothing for the plan it was taken
  with. It is never exported: every byte is refetchable by digest. The agent
  keeps the trees its current and previous pins name, for rollback, and
  removes layers no kept tree uses.
- **Run.** The agent runs from its layer through a stable
  `/opt/discobox/current`.

*Amended 2026-10-22, before anything shipped against it:* what the pool
places, and what a moved pin does to a container, are spelled out. The pool
places the agent layer, the agent's units and the box's **pins** — the
platform and harness bundles it applies at this start — before each start, as
it places `sandbox.json`; the box never writes what the pool reads back, which
keeps its own record of what it placed. A container the platform is
delivered to is the provider's sandbox base image, so neither its image pin
nor its platform pin is what it is built from: a moved one **restarts** it
into the new pins and keeps its store and what it provisioned, which narrows
[0017](0017-resource-state-is-desired-and-observed-with-no-operations.md) §§5 and
11 — the fingerprint covers the manifest without those two pins. Its harness
bundle pin stays in the fingerprint, since the harness's manifest is what the
container is built with. A sandbox with any pin that is not a content digest
is delivered nothing and runs its runnable image as before.

Server–agent version skew narrows to what a pin holds back: a box stopped
when its server's release moves follows it, one running then once it is
upgraded or repaired, and the capability labels that described an older baked-in agent are
retired. What drifts is the box, which provisioning
converges.

### 8. A snapshot is a cache, never a requirement

Any box must provision from scratch. A snapshot only makes that fast: boot a
provider image, provision the platform and harness with every step blocking,
shut down, and snapshot — on the Docker driver, a committed image carrying
the reclaim label (ADR 0040). Its key is the provider image's digest and the
digest of the resolved plan, feature set included, so one snapshot serves
every sandbox wanting the same set. A snapshot holds no sandbox:
`/etc/machine-id` is left empty, no SSH host key is kept, and the sandbox
phase has never run in it. The step records and the bundle store are on the
root filesystem (§§3, 7), so a commit keeps them with what they describe.

*Amended 2026-10-22, before anything shipped against it:* who takes a
snapshot, when, and under what. The pool takes it, lazily: a create finding
none for its plan boots the provider image and provisions from scratch, and
the snapshot is built behind it, so the next create of the plan starts from
it; the control plane never knows which a box started from. A build is
anonymous — an identity of its own, no account, source, secret or
credentials endpoint — and runs under every egress level of 26-10-01-032 but
the sandbox's own (the project's, the pool's and the harness config's), which
the server sends with each create, so nothing of the sandbox that asked for
it decides what it holds. The key adds that policy, and the project and pool,
to the provider image and the plan. A feature, or one of its steps, may say
`snapshot: false`: it is left out of every snapshot and applied on each box
at its boot, for what must be each machine's own or provisioned fresh.

### 9. Providers meet a contract and declare what they own

The platform may assume only:

1. Linux on amd64 or arm64, Debian family, systemd as PID 1.
2. Root by exec or passwordless sudo.
3. A persistent writable root disk, overlayfs, and cgroup2; `/dev/kvm` optional.
4. A way to place files and run one command, and one address reaching the
   agent's port (26-10-09-143 §2, widened by §7).
5. Outbound reach to package repositories through the pool's proxy — the only
   way out a sandbox has (0126 §6), provisioning included.

A provider **declares** its login user; whether it owns DNS, the network,
sshd and the trust store; any egress control of its own, which is an outer
allowlist and never a path around the pool's proxy; and its snapshot
capability. Provisioning reads the declaration and never displaces what a
provider owns: the DNS stub sits beside the provider's resolver, Discobox's
CA joins the trust store beside the provider's, `daemon.json` is merged, and
the runc personality resolves the real runc (Ubuntu's is `/usr/sbin/runc`)
rather than assuming `/usr/bin/runc`.

**A box is dedicated to one sandbox.** The sandbox phase writes its account
database and sudoers, and idle stop powers it off.

### 10. The Docker driver owns its sandbox base image

`base-image/Dockerfile` moves to the Docker worker driver
(`server/providers/dockerworker/sandbox-image/`) and becomes the image every
sandbox container starts from. It is resolved as the pool-agent image is:

| | Pool agent (today) | Sandbox base image |
|---|---|---|
| Compiled-in default | `DefaultPoolImage` | `DefaultSandboxBaseImage`, overwritten at link time by a release |
| Provider setting | `image` | `sandboxBaseImage` |
| Server override | `dockerPoolImage` | `dockerSandboxBaseImage` (`DISCOBOX_DOCKER_SANDBOX_BASE_IMAGE`) |
| Release manifest | `Images.PoolAgent` | `Images.SandboxBase` |

- **It is like the providers' images, and holds no special status.** Modelled
  on `exeuntu`, it holds what exe.dev and boxd have in common, not what either
  alone has: systemd; a uid-1000 login user named `discobox`
  with passwordless sudo, in `docker`, lingering; Docker, buildx
  and Compose v2, socket-activated; git, gh, build-essential, python3, pipx,
  uv, Go, curl, jq and ripgrep; exeuntu's masks (udev, getty, ssh,
  modules-load, remount-fs, sysusers, random-seed, apt-daily,
  unattended-upgrades); an empty `machine-id` and no host keys. Node, a
  browser, Nix, brew and coding agents stay out, so the platform cannot come
  to depend on them unnoticed.
- **It is Debian 13, where those images are Ubuntu 24.04.** *(Amended before
  anything shipped against it: this section first said Ubuntu 24.04 and
  Ubuntu's `docker.io`.)* Debian's mirrors are the faster to build from, and
  the features provision onto either distribution, so the platform still
  meets Ubuntu on every exe.dev and boxd machine. A feature that differs
  between them says so per distribution (a `packages` list per os-release ID,
  a step's `when`). Docker is Docker's own build, as 0068's was.
- **Every driver that runs the pool agent in Docker uses it** — local Docker,
  DigitalOcean, execvm, and the pool VMs of libkrun, vz and wslc, whose
  sandboxes are containers in the VM's daemon. `vm-image`, the pool VM's own
  guest, is not this image and is unchanged.
- **The pool-agent image builds `FROM` it**, keeping 0068's shared layers: the
  pool host pulls both.
- **The settings that named images go.** `defaultSandboxImage` and
  `harnessImages` in `server.yaml`, and the release manifest's
  `Images.SandboxAgent` and `Images.Harnesses`, become the platform and
  harness bundle references (`Bundles.Platform`, `Bundles.Harnesses`).
  `server.yaml` is a user's file (ADR 0096): the old keys are read as the new
  ones for one release, with a warning, then refused.

  *Amended 2026-10-22, before anything shipped against it:* the old keys keep
  their meaning for that release rather than being read as the new ones. An
  existing `defaultSandboxImage` names a runnable sandbox-agent image, never a
  platform bundle, so read as `platformBundle` it would fail every harness's
  composition; while the runnable images still ship, it still names the image
  a sandbox with no harness runs. The warning says it is retired and what
  replaces it. `harnessImages` was never a `server.yaml` key: a harness's
  runnable image is overridden by `DISCOBOX_HARNESS_<SLUG>_IMAGE` alone.
- No harness is an image any more. A Docker sandbox is this image, or a
  snapshot committed from it, with the agent layer placed in it (§7).

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

- 0056 §1 kept `sources.json` apart because its reader is the client, before
  the box exists, and `project.json`'s the pool, after. That still holds per
  key, and each reader takes only its own: the client `sources` and
  `features`, the pool agent the rest. What changed is that the client now
  needs `features` too, to choose a snapshot or a plan, so a separate file per
  reader would be two fetches of a remote primary source (26-10-10-545 §4)
  where one does.
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
0050 and 0126 apart from §3's bootstrap exception; 26-10-09-143's bootstrap
wait; 26-10-10-767's systemd as PID 1 and its wiring unit, now placed by
the pool agent's Docker runtime; 0043 §1's `shell` as an ordinary harness; 0141's groups-only
account, which keeps the box's account; and the development sync of the two
images that remain (the pool agent and the sandbox base image).

### 13. Bundles come from the server, by blob, cached by the pool

Neither a pool nor a box pulls a bundle from a registry. The server serves
them, addressed as OCI distribution addresses them, with only the calls a
fetch needs:

| Call | Answers |
|---|---|
| `GET /api/pools/{poolId}/assets/{name}/manifests/sha256:<digest>` | A pinned index or manifest — small JSON naming config and layer digests |
| `GET /api/pools/{poolId}/assets/{name}` | The manifest the server currently resolves for the name, for registration and re-pinning |
| `GET /api/pools/{poolId}/assets/blobs/sha256:<digest>` | One blob, immutable and cacheable forever |

*Amended 2026-10-21, before anything shipped against it:* the calls sit under
`/api/pools/{poolId}/`, as every pool route does, so the pool's signed
identity is bound to the route's (`server/internal/auth`); the table first
wrote them as `/api/assets/…`, a path no pool route can be.

- **The server keeps one blob store** and fills it from any source: the
  release's bundles, pulled from the registry once; the CLI-staged image cache
  (ADR 0113); and, in development, the watcher's output, unpacked once. The
  server's registry credentials pull the release's bundles and nothing else;
  a reference a project supplies is pulled anonymously or with registry
  credentials that project configured.
- **The asset API is a pool route, on the pool runtime's allow-list**
  (`poolRuntimeActions`), and scoped: a pool is answered only manifests pinned
  by the release or by sandboxes and harness configs of projects it serves,
  and only blobs those manifests reference.
- **The pool is a pull-through cache.** It answers from its own blob store in
  its build area (ADR 0050), fetches a blob it lacks from the server once, and
  shares one in-flight fetch among every request for the same digest. A
  sandbox's only way out is its pool (0126 §6); the cache is what lets one
  fetch serve every sandbox.
- **`assets.discobox.internal`** joins `api.` and `git.discobox.internal`
  (ADR 0140), and is how the agent fills its store (§7). The pool's proxy
  serves it read-only, and only what one sandbox's pins name: the sandbox is
  the one its mTLS client certificate names (`proxy/REVIEW.md`), its pins are
  the pool's own record of them, never anything the box reports, and a
  request for any other manifest or blob is refused there, never passed to
  the server. A bundle reference may name the host,
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
  does for images today (0082). A rebuilt agent or harness bundle reaches a
  box when its re-pin does (§7, as amended).

*Amended 2026-10-23, before anything shipped against it:* build-mode — the
development flow of a host with no Docker daemon, macOS and Windows by
default — has no host builder to export a layout with, so its bundles are
built where its images are, on a pool's daemon, through that daemon's
BuildKit, and the server saves each from that daemon as the layout it serves
from, removing it from the daemon once saved. That is the one place a bundle
enters a Docker image store, for as long as its save takes; no box runs it,
and the alternative — requiring a Docker CLI on every such host for bundles
alone — would undo what build-mode exists for. Until a pool has built a
bundle, no box is pinned to it: a create on it is refused, and the save
seeds the built-in harnesses onto it.

### 14. An account in the sandbox account's way is deleted

The client still sends the account it wants (ADR 0141), and the sandbox phase
still makes it inside the box (ADR 0025). Every provider image already has an
account at uid 1000, the uid every macOS and Windows client sends, so a
conflict is the ordinary case, and the box's account gives way entirely. This
settles the ids 26-10-09-106 deferred for a Linux machine sandbox's account:
the client's, whatever the guest already has there.

- **Any account holding the sandbox's uid or its name is deleted**, then the
  sandbox's account is created from `/etc/skel`, as if the box had never had
  one. Nothing of the old account is renamed, adopted or kept.
- **Its home goes with it, unless something is mounted there.** The deletion
  runs before volumes are wired, and it never removes a mount point or a
  directory with one beneath it: where a driver mounts the sandbox's
  persistent `data` volume at its home before boot, the old account's files
  there are the sandbox's own from an earlier boot. A plain directory at the
  path that will be the sandbox's home is removed like any other — kept, it
  would hold the files of a uid that no longer exists, and no skeleton. Nor
  is a home the account does not own removed (a system account's, such as
  daemon's `/usr/sbin`). Seeding then fills the sandbox's home only where it
  is empty, as today.
- **Its processes are ended first** (`loginctl terminate-user`, then any that
  remain), since a provider's login user has them — boxd's exec runs as
  `boxd` — and `userdel` refuses a user that is logged in (PR #180).
- **Root is never deleted.** A client account of uid 0 is root (0141), which
  needs no account; one that names `root` with another uid is refused.
- **With no conflict, the account is created**, as today.
- **It runs whenever the box has a conflicting account**: once on a machine,
  whose root filesystem persists, and on every container create, since the
  image brings its uid-1000 account back each time (26-10-10-767 §2).
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
  Go runtime of its own. `discobox-access` is the exception, for its own
  module's reason (§7).
- **The agent as a container's entrypoint, execing systemd.** It is the thin
  entrypoint 26-10-10-767 turned down; placing the agent layer before start
  keeps systemd PID 1.
- **Writing a box's observed features into its manifest.** The document is
  the pool agent's alone, and a declaration is authored, not discovered.
- **Profiles.** A feature set with requirements expresses every profile, and
  a profile would be a second vocabulary for it.
- **Opt-in features.** Every box has every feature today; opting in would
  silently take them away.
- **Uninstall on opt-out.** It ties a snapshot's contents to the last project
  that used it.
- **Block boot until provisioned.** A fresh box takes minutes (Docker,
  Chromium, Nix, Go and Homebrew dominate); most of it is not needed to start.
- **Probe for features at runtime.** It contradicts 0145 §3; probing once in
  provisioning and reporting the outcome keeps that rule.
- **Bake the agent into the snapshot instead of delivering it.** Skew between
  server and agent returns, and so do the labels that described it.
- **A sandbox image owned by the platform.** The image is the provider layer;
  exe.dev and boxd bring their own, and the Docker driver is the provider that
  brings ours.
- **Ubuntu 24.04 for the sandbox base image**, as §10 first said, so a
  self-hosted box would provision exactly as exe.dev's and boxd's do.
  *(Amended: this entry first rejected Debian 13 on that ground.)* Debian's
  mirrors build the images faster, and the features already say per
  distribution where Debian and Ubuntu differ, so the difference is one each
  feature states rather than one the base image hides.
- **Load development bundles into each Docker daemon**, as images are today.
  A bundle is never run, so a daemon's image store is the wrong place for it,
  and machine providers have no daemon to load into.
- **One OCI layout tar per bundle from the asset API.** Nothing in it can be
  shared: a release that changed only the agent would resend every layer.
- **The pool placing whole trees in every box**, or binding its store into
  one. An upgraded box would be sent what it already holds, and a bind is
  pool-shared state a sandbox's root can remount and rewrite.
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

- **One microVM per sandbox on a self-hosted pool**, booting the sandbox base
  image as its root filesystem as exe.dev and boxd run theirs. It is a future
  machine driver's; the existing VM drivers keep a pool VM of containers.
- **Other distribution families.** Revisit when a provider we target ships one.
- **Providers without systemd** (Sprites, Vercel, Daytona, Modal). Revisit
  with the agent's own supervisor, which darwin and Windows already use.
- **macOS and Windows.** The selectors and the platform index reserve them,
  and 26-10-09-106 §5's build spec stays their build path until they are
  provisioned; revisit with 0145.
- **Settings scopes** beyond the repository (`settings.local.json`, a user
  file). Feature resolution is a merge either way; revisit when an opt-out is
  wanted across repositories.
- **Skills in `settings.json`.** A separate feature.
- **Third-party features.** A feature published outside a harness bundle, and
  how it is trusted, waits for a first such feature.

## Consequences

- The harness Dockerfiles become `FROM scratch` bundles; `sandbox-agent/Dockerfile`
  becomes the platform bundle; `base-image/Dockerfile` moves to the Docker
  driver as its sandbox base image; every `boxd.yaml`, `build:boxd-images` and
  the labels directory those specs write (`/usr/local/share/discobox/labels`)
  go.
- `HarnessConfig`'s image fields and platforms-from-index, `SandboxManifest`'s
  image pin, `imagereap` for harness images, `TreeExportLabel` and
  `WorktreeGitLabel` are replaced; stored harness configs and sandboxes are
  migrated, not recreated.
- The server gains the scoped asset API, its blob store and the composition of
  the platform's manifest layer under a harness's; the pool agent gains a
  pull-through blob cache and the `assets.discobox.internal` route, and asks a
  machine's provider to place the agent layer; the pool agent's Docker runtime
  places the agent layer and `discobox-wire.service` into each container
  before it starts; the agent gains its own content store; the Docker driver
  gains `sandboxBaseImage` and its resolution chain.
- The agent becomes multicall and gains the runner, the status file,
  `discobox-provision wait`, and placeholder commands; runtime integrations
  read `provision.json`; `boot`'s account handling deletes instead of
  renaming; `resources` stops reading `/sys/fs/cgroup` as the container's own,
  and the DNS stub stops assuming `eth*` interface names.
- `release:images` publishes bundles beside the two images; the watcher and
  `devimage` build and record bundles as OCI layout tars.
- The CLI reads `.discobox/settings.json`, and the `discobox` skill teaches the
  status file.
- A fresh box is slower to finish than an image pull; a snapshot makes it as
  fast or faster, and the box is usable well before it finishes.
