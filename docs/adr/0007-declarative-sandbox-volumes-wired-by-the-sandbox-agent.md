# 0007 — Declarative sandbox volumes wired by the sandbox-agent

- **Status**: Proposed
- **Date**: 2026-07-20

## Context

Today the pool-agent decides every sandbox mount. `prepareSandboxVolumes`
(`pool-agent/sandboxruntime/runtime.go`) bind-mounts a per-sandbox `home`
directory onto the resolved user's home, a read-only `config` directory onto
`/etc/discobox`, and one directory per git source onto its target. The proxy
material is bind-mounted at `/etc/discobox/proxy`. Everything else a running
sandbox needs to persist — `/var/lib/docker`, `/var/lib/discobox`, `/workspace`,
package caches — is left to anonymous Docker `VOLUME` directives baked into the
image (`sandbox-agent/Dockerfile`, `pool-agent/Dockerfile`).

Three problems converge:

1. **The pool-agent hardcodes image-specific knowledge.** Which paths a given
   image needs persisted (its docker root, its home, its language caches) is a
   property of the image, but the pool host decides them. A new image that stores
   state somewhere new requires a pool-agent change.
2. **There is no shared cache.** Every path is per-sandbox. Two sandboxes on the
   same pool re-download the same npm/nix/pnpm/go artifacts. The host layout
   (`projects/{project}/sandboxes/{sandbox}/volumes/...`) has no pool-scoped
   level to hang a shared cache on.
3. **Anonymous `VOLUME`s are throwaway and invisible.** They are not persisted
   across sandbox recreation, cannot be shared, and cannot be reasoned about —
   the pool host never sees them, so nothing manages their lifecycle.

`image.json` (`sandbox-agent/config/image.go`) already carries image-owned,
runtime-resolved configuration (`env`, `harness`) with a `%HOME%` token expanded
at load, and the sandbox-agent already reads the per-sandbox manifest
(`/etc/discobox/sandbox.json`) for runtime paths. The declarative surface and the
in-sandbox reader both already exist.

## Decision

**The image declares its persistent and cached paths; the sandbox-agent wires
them as PID 1 before systemd starts. The pool-agent only provisions four
host-backed volume roots and materializes sources.**

### 1. Four container mounts, provisioned by the pool-agent

The pool-agent stops mounting home/docker/individual paths. It mounts exactly:

| Mount in sandbox | Scope | Host backing |
| --- | --- | --- |
| `/.discobox/data` | per-sandbox, persistent | `/var/lib/discobox/projects/{project_id}/pools/{pool_id}/sandboxes/{sandbox_id}/data` |
| `/.discobox/cache` | shared across the pool's sandboxes in this project | `/var/lib/discobox/cache/projects/{project_id}/pools/{pool_id}/cache` |
| `/.discobox/config` | per-sandbox | `/var/lib/discobox/projects/{project_id}/pools/{pool_id}/sandboxes/{sandbox_id}/config` |
| `/.discobox/sources` | per-sandbox | `/var/lib/discobox/projects/{project_id}/pools/{pool_id}/sandboxes/{sandbox_id}/sources` |

The host layout gains a `pools/{pool_id}` level so the cache is
pool-and-project scoped. `config` holds `sandbox.json` and the proxy material,
exactly as `/etc/discobox` does today; `sources` holds git-materialized
checkouts. The cache uses its own top-level root so providers may mount
disposable storage at `/var/lib/discobox/cache` without moving durable sandbox
or proxy state.

### 2. The image declares volumes; `%HOME%`/`%UID%`/`%GID%` resolve at runtime

`image.json` gains a `volumes` array. Each entry names a container `path`, the
backing `volume` (`data` or `cache`), and optional `uid`/`gid`/`mode` for the
mount:

```jsonc
"volumes": [
  { "path": "%HOME%",                 "volume": "data",  "uid": "%UID%", "gid": "%GID%", "mode": "0755" },
  { "path": "/var/lib/docker",        "volume": "data",  "uid": 0, "gid": 0, "mode": "0711" },
  { "path": "/var/lib/discobox",      "volume": "data" },
  { "path": "/nix",                   "volume": "cache" },
  { "path": "/var/lib/discobox/pnpm", "volume": "cache" }
]
```

The user identity (`%HOME%`, `%UID%`, `%GID%`) comes from the manifest's resolved
`config.user`, reusing the existing token-expansion mechanism.

### 3. The sandbox-agent wires everything as PID 1, then `exec`s systemd

A new `discobox-sandbox-agent init` subcommand becomes the container entrypoint
and runs as PID 1. It absorbs everything `entrypoint.sh` does today — user/group
creation, sudoers, `/etc/skel` home seeding, systemd desktop drop-ins — plus the
new mounting, then `exec`s `/sbin/init` in place. `entrypoint.sh` is retired.

Mounting rules for a declared path `/x/y/z` backed by `data`:

- Create `/.discobox/data/x/y/z`, creating parent dirs of the target as needed.
- **Target absent or empty → bind mount** the volume dir onto the target.
- **Target exists and is non-empty → overlay mount**: `lowerdir` is the target
  (the image content), `upperdir` is `/.discobox/data/x/y/z/upper`, `workdir` is
  `/.discobox/data/x/y/z/work`, merged back onto the target. Image content shows
  through; writes persist to the volume.
- Apply `uid`/`gid`/`mode`.

For `cache`-backed paths the volume is **always a direct shared bind** — never an
overlay (see alternatives). Then: bind `/.discobox/config` onto `/etc/discobox`,
and for each source in `sandbox.json`, bind `/.discobox/sources/<slug>` onto its
target.

### 4. `sandbox.json` carries source bind targets

The manifest gains a `sources` list (slug → target, with ownership) so the init
flow knows where to bind each source. The pool host materializes the bytes; the
sandbox-agent places them.

## Alternatives rejected

**Keep the pool-agent deciding mounts, just add a shared cache dir.** The
smallest change: leave `prepareSandboxVolumes` in charge and teach it a
pool-level cache path. Rejected because it leaves image-specific path knowledge
(where docker/home/caches live) in the pool host, so every new image storage
location is still a pool-agent change. The point of the ADR is to move that
knowledge into the image, where it belongs.

**Plain bind for every path, no overlay.** Simpler and uniform. Rejected because
some declared paths ship content in the image (a pre-populated home skeleton, a
seeded `/etc` subtree, tool defaults under a cache path). A plain bind hides that
content behind the empty volume dir. The overlay-when-non-empty rule preserves
image content as the lower layer while persisting writes to the volume — the only
scheme that satisfies both "persist writes" and "don't lose what the image
shipped." The empty-target fast path avoids overlay overhead where there is
nothing to preserve.

**Overlay for cache paths too, with a shared upperdir.** Uniform with data.
Rejected as unsafe: the cache volume is bind-mounted into multiple concurrently
running sandboxes. An overlayfs `upperdir`/`workdir` may not be shared by
concurrent overlay mounts — doing so corrupts the workdir. Cache paths therefore
get a direct shared bind only; an image that ships content at a cache path
accepts that the shared cache hides it. Data paths are per-sandbox, so their
overlay dirs are never shared and the rule is safe there.

**Pool-agent binds sources directly onto their targets (as today).** Rejected
for consistency: once the sandbox-agent owns in-container mounting, splitting
sources out — the pool host binds these, sandbox-agent binds those — reintroduces two
mount authorities. The pool host materializes bytes into `/.discobox/sources`; the
sandbox-agent does all binding, driven by the manifest.

**Do the mounting from a systemd unit instead of PID 1.** A `discobox-mounts`
oneshot ordered before everything. Rejected because the mounts must exist before
`dockerd`, the harness, and the proxy bridge start, and several of those paths
(`/var/lib/docker`, `/var/lib/discobox`) are read by early-boot units. Owning PID
1 and mounting before `exec`ing systemd guarantees the namespace is fully wired
before *any* unit runs, with no ordering edges to maintain. It also gives one
coherent init that replaces the shell entrypoint rather than layering a unit on
top of it.

**Sources as a subdir of `/.discobox/data` rather than a dedicated mount.** One
fewer mount. Rejected to keep lifecycle boundaries crisp: sources are
pool-materialized and may be reset/re-cloned independently of the sandbox's
persistent data; a dedicated mount keeps that boundary explicit and avoids a
source reset touching the data volume tree.

## Consequences

- `pool-agent/sandboxruntime/runtime.go` loses the home/config/source bind
  construction in `prepareSandboxVolumes` and the `sandboxVolumesRoot` layout
  gains a `pools/{pool_id}` level; it provisions four roots and mounts them.
- `image.json` gains `volumes`; `config.ImageConfig` gains the parsed shape and
  `%UID%`/`%GID%` join `%HOME%` in token expansion. The manifest
  (`SandboxManifest`) gains a `sources` list.
- The sandbox-agent gains an `init` PID-1 subcommand and the mounting engine;
  `entrypoint.sh` is deleted and its user/sudoers/skel/desktop logic moves into
  Go. The container `ENTRYPOINT` becomes `discobox-sandbox-agent init`.
- The `VOLUME` directives in the sandbox image Dockerfile are removed — the
  paths are now managed binds, and anonymous volumes would only shadow them and
  waste storage. The pool host image's `VOLUME`s are its own host storage and are
  unaffected.
- `/var/lib/discobox` (holding the sandbox-agent SQLite DB) becomes a declared
  `data` path, so sandbox-agent state persists per sandbox instead of living in a
  throwaway anonymous volume.
- Path constants for `/etc/discobox` (`config.DefaultPath`,
  `proxyagent.SandboxProxyMount`) are unchanged in value — `/etc/discobox` still
  exists, now via the config bind the sandbox-agent performs.
- The sandbox runs `Privileged: true` already, so the in-container `mount(2)`
  calls need no new capability.
- The DB is disposable, so the host-layout change needs no migration; existing
  per-sandbox volume trees are abandoned, not upgraded.
- Existing pool caches at
  `/var/lib/discobox/projects/{project_id}/pools/{pool_id}/cache` are
  disposable and are reclaimed rather than migrated.
