# 0075 — The nix store is a pool-shared cache, seeded from the image on first use

- **Status**: Accepted (§5's boolean stamps superseded by
  [0085](0085-the-nix-seed-stamp-names-the-store-that-seeded-it.md);
  everything else stands)
- **Date**: 2026-08-27

## Context

`/nix` is baked into the sandbox image and lives in the container's writable
layer. The Dockerfile installs nix, then `devenv` and `bashInteractive`, into a
store that is image content from that point on.

Nothing a sandbox realizes afterwards survives it. A `nix develop`, a
`devenv shell`, a `nix profile install` writes into `/nix/store` on the
container layer and is gone when the sandbox is recreated — and every other
sandbox in the same pool re-downloads the identical closure from
`cache.nixos.org`. A `devenv` closure is measured in gigabytes, so this is the
most expensive cold start the sandbox has.

The pool already has the place for it. `/.discobox/cache` is shared by every
sandbox in the pool, on its own disposable block device
([0007](0007-declarative-sandbox-volumes-wired-by-the-sandbox-agent.md),
[0013](0013-local-linux-pools-use-libkrun-microvms.md)), and
[0044](0044-builds-run-on-a-pool-shared-buildkit.md) already makes exactly this
trade for `docker build` against a pool-shared BuildKit. The nix store is the
same shape of problem with a better-behaved payload.

Two facts decide the design.

**A shared store is safe here.** Nix is built for concurrent writers: store
paths are content-addressed, built in a temp directory and `rename()`d into
place, guarded by per-path `fcntl` locks, with `db.sqlite` doing its own
locking. Two sandboxes building the same derivation both build it and one wins
the rename — wasted CPU, never corruption. `nix-daemon` exists for privilege
separation, not exclusive access. And this cache is never shared across
kernels: a pool is one host by invariant ([0006](0006-pool-is-the-runtime-host.md))
and its cache is a block device attached to that host
([0013](0013-local-linux-pools-use-libkrun-microvms.md) §1), so N sandboxes on
one store are N processes on one filesystem on one kernel.

**Seeding is expensive and usually unnecessary.** Declaring
`{"path": "/nix", "volume": "cache"}` and stopping there does not work: a plain
bind of an empty cache directory hides the store the image shipped, and the
`nix` binary is itself in that store. Something has to put the image's store
into the cache. That copy is gigabytes, and most sandboxes never run nix at
all.

## Decision

### 1. The image ships its store aside and leaves `/nix` empty

The Dockerfile installs nix exactly as it does today, then, **in the same `RUN`
layer**, moves the result aside:

```dockerfile
install -d -m 0755 /usr/local/lib/discobox
mv /nix /usr/local/lib/discobox/nix
mkdir -m 0755 /nix
```

Same layer matters: a `mv` within one layer is a rename and costs nothing, while
a move in a later layer copies the whole store up into that layer and doubles
the image.

Everything nix needs outside the store already lives outside it and is
unaffected: `/etc/nix/nix.conf`, the `nixbld` users and group, and the two unit
files, which are `install`ed into `/etc/systemd/system` as real copies at build
time — before the move.

`/usr/local/lib/discobox/nix` is a copy source and nothing else. Store paths
are absolute and baked into every binary's ELF interpreter and RPATH, so a
binary at `/usr/local/lib/discobox/nix/store/<hash>-foo/bin/foo` looks for its
loader under `/nix/store/` and cannot be executed in place. It is named for
what it is — a seed, not a second install — and sits under the `discobox`
namespace the image already uses for its own material
(`/usr/local/libexec/discobox/configure-*`). `/usr/libexec` would be the wrong
shelf: FHS reserves it for internal binaries that are executed, and this store
is read-only data that provably cannot be. `/usr/share` is wrong for the
complementary reason — it is for architecture-independent data, and this is
thousands of ELF binaries.

### 2. `/nix` is a cache volume; profiles and gcroots are not

The three harness `image.json` files gain:

```jsonc
{ "path": "/nix",                    "volume": "cache" },
{ "path": "/nix/var/nix/profiles",   "volume": "data", "uid": 0, "gid": 0, "mode": "0755" },
{ "path": "/nix/var/nix/gcroots",    "volume": "data", "uid": 0, "gid": 0, "mode": "0755" }
```

The store and its database are shared; per-user mutable state is not.

`/nix/var/nix/db` is deliberately on the shared side: a store path nix has not
registered valid in `db.sqlite` is a path nix will not use, so the database has
to travel with the store it describes.

`profiles` and `gcroots` are deliberately on the per-sandbox side. Both are
keyed by username and every sandbox in a pool runs the same user, so sharing
them would let one sandbox's `nix profile install` rewrite another's default
profile.

This needs no change to volume wiring, and it is worth being exact about why.
[0007](0007-declarative-sandbox-volumes-wired-by-the-sandbox-agent.md) picks
between a plain bind and an overlay by asking whether the image already shipped
content at the target — but only for `data` paths. `useOverlay` is
`kind == VolumeData && targetNonEmpty`, so a `cache` path is a direct bind
whether its target is empty or not.

Emptying `/nix` in the image (§1) therefore does not change which branch boot
takes; it changes what that branch costs. A cache bind over a *populated* `/nix`
would silently hide the entire store behind an empty volume directory — the
shadowing this ADR exists to avoid. With the store moved to a seed directory
there is nothing left at `/nix` to hide, so the bind is lossless and 0007's
cache rule stands exactly as written. Depth ordering already wires `/nix`
before its children.

### 3. The seed is a unit `nix-daemon` pulls in, not a boot step

`discobox-nix-seed.service` is `Type=oneshot`, `RemainAfterExit=yes`. A
`nix-daemon.service.d` drop-in adds `Requires=` and `After=` on it, alongside
the ordering `proxy.conf` already declares.

`nix-daemon` is already socket-activated, so nothing in this chain runs until
something connects to `/nix/var/nix/daemon-socket/socket`. A sandbox that never
uses nix never copies a byte and never starts the daemon.

Socket activation does not survive an empty `/nix` unassisted. Both vendored
units carry `ConditionPathIsReadWrite=/nix/var/nix/daemon-socket`, which
assumes the store the installer put at `/nix` is still there — so with the
store moved aside the socket unit's condition fails at boot and the socket is
skipped. Nothing then creates the socket, a nix client gets `cannot connect to
socket at '/nix/var/nix/daemon-socket/socket'`, and nothing can ever activate
the daemon that would pull in the seed. A `nix-daemon.socket.d` drop-in resets
the condition. Binding on an empty `/nix` is safe: systemd creates a unix
socket's parent directories itself under `DirectoryMode=`, and that mkdir is
the whole boot cost of keeping the store lazy. The condition on
`nix-daemon.service` needs no reset — it is evaluated when the service starts,
which `After=` puts after the seed has run.

`RequiresMountsFor=/nix/store` on both units needs no reset either: it resolves
to the mount containing the path, which is the `/nix` cache bind the boot flow
already made, and does not require the directory itself to exist.

### 4. A PATH shim covers the window before the client binaries exist

The daemon is not the only thing in the store — `nix`, `nix-shell`, `devenv`
are too. Before the seed, PATH's nix entries are empty directories, so the
user's `nix` never execs and nothing ever reaches the socket that would trigger
§3.

`/usr/local/bin/{nix,nix-shell,nix-build,nix-env,nix-store,devenv}` each start
`discobox-nix-seed.service` — blocking and idempotent — then exec the real
binary out of the now-populated store.

The shim disarms itself. PATH is
`.../.nix-profile/bin:/nix/var/nix/profiles/default/bin:...:/usr/local/bin`, so
it is only reachable while the store entries are empty; once seeded, the real
binary is found first and the shim is never consulted again. This is the same
injection point [0044](0044-builds-run-on-a-pool-shared-buildkit.md) uses for
`docker`, for the same reason: there is no other place to intervene.

### 5. The seed is a stamped copy under `flock`, idempotent across sandboxes

The unit takes `flock` on `/nix/.discobox-seed.lock`, and:

- if `/nix/.discobox-seeded` exists, exits 0;
- otherwise `cp -a /usr/local/lib/discobox/nix/. /nix/`, then writes the stamp.

The stamp is written last, so a copy interrupted by a crash is never mistaken
for a finished one — the next run takes the lock, finds no stamp, and redoes
it. The lock lives on the cache filesystem, so it serializes every sandbox in
the pool: the second one to ask blocks, then finds the stamp and exits.

There are two stamps, not one. The store stamp is on the cache and says nothing
about the per-sandbox volumes from §2, so a second sandbox joining an
already-seeded cache still has no `/nix/var/nix/profiles/default`. The unit
seeds `profiles` and `gcroots` from `/usr/local/lib/discobox/nix/var/nix/`
whenever they are missing, under its own stamp on the data volume. They are
symlink trees and cost nothing.

### 6. Garbage collection does not run in the sandbox

No GC timer is enabled and the shims add none. Two things make in-sandbox GC
unsafe against a shared store: `/nix/var/nix/temproots` entries are named by
pid, and each sandbox has its own PID namespace, so pids collide across
sandboxes writing one store; and with per-sandbox gcroots (§2), no sandbox can
see the roots another sandbox's live profiles hold. A `nix-collect-garbage` in
one sandbox could delete paths another is using.

Reclamation stays where [0007](0007-declarative-sandbox-volumes-wired-by-the-sandbox-agent.md)
and [0013](0013-local-linux-pools-use-libkrun-microvms.md) already put it: the
cache is per-pool disposable storage on its own block device, and deleting the
pool deletes the disk.

## Alternatives rejected

**Overlay `/nix`, with the cache as the upper layer.** Keeps the image's store
as a read-only lower and writes to the cache, so nothing needs copying at all.
Rejected because an overlayfs `upperdir`/`workdir` is private mount state, not
a shared directory: each mount caches its own view of upper, encodes deletions
as whiteout device nodes, and uses workdir as scratch for atomic copy-up. Every
sandbox in a pool would mount over the one cache upper; the kernel detects the
overlap and fails the second mount with `EBUSY`, so the second sandbox in a
pool would not boot. Per-sandbox upper dirs avoid the collision but share
nothing, which is the entire point of the change.

**Bind the pristine store at `/nix` instead of copying it.** No copy, instant
start. Rejected because it is read-only image content: nothing a sandbox builds
could persist, and the pool would still share nothing. The copy is precisely
what turns image content into a writable pool-shared store.

**Seed at boot, from the sandbox-agent init flow.** The mounting authority
already runs as PID 1 and could copy there. Rejected on cost and on ownership.
It puts a multi-gigabyte copy on the boot path of every sandbox including the
ones that never run nix; and it is a nix-specific step inside a mechanism that
is deliberately generic, where `image.json` declares paths, boot wires them, and
nothing knows what a nix store is. Socket activation gives the laziness for
free and keeps the knowledge in the image, beside the daemon it serves.

**A generic `"seed": true` flag on cache volumes.** Would let any image-shipped
cache path be copied into the shared volume on first use, wired by the boot
flow. Rejected for now: nix is the only path that wants it; the boot flow has
no way to know a user will never run nix, so the flag would be eager where the
whole value here is laziness; and a generic copy cannot express the
store-shared/profiles-per-sandbox split §2 needs. Revisit when a second path
wants the same treatment and can accept an eager copy.

**Share the store through nix's own binary cache.** A `post-build-hook` pushing
to a `file://` store on the cache volume, read back through
`extra-substituters`. Concurrency-safe, needs no image surgery, no shims.
Rejected on what it actually captures and what it costs: the hook fires only
for paths the sandbox *built*, not for paths it substituted, and in a `devenv`
workflow nearly the whole closure is substituted from `cache.nixos.org` — so
the shared cache fills with almost nothing that matters unless a periodic
`nix copy --all` runs too. Every read is then a NAR unpack into that sandbox's
own store, so each sandbox still writes the full closure to its own disk. A
shared store is read in place.

**Warm the seed in the background at boot rather than blocking the first nix
command.** Rejected because it cannot be both: nix cannot run without a store,
so whatever asks first has to wait for the copy. Starting it eagerly only moves
the cost onto the sandboxes that never use nix — the cost socket activation
exists to avoid.

## Consequences

- The first nix command in a pool with a cold cache blocks for a full store
  copy, gigabytes across a filesystem boundary (the image's writable layer to
  the cache block device). Every later sandbox in that pool pays nothing and no
  sandbox re-downloads from `cache.nixos.org`. That is the trade being made.
- A sandbox that never runs nix is unaffected: no copy, no daemon, one extra
  unit that never starts.
- The image does not grow. The store is moved within its install layer, not
  duplicated.
- `nix profile install` in one sandbox no longer reaches another, but the store
  paths it realized are visible to all of them — a second sandbox can install
  the same thing with no download.
- `nix-collect-garbage` inside a sandbox becomes unsafe and unsupported.
  Reclaiming the store means deleting the pool's cache disk.
- [0007](0007-declarative-sandbox-volumes-wired-by-the-sandbox-agent.md) is
  untouched. No new volume kind, no change to the cache bind rule, no change to
  the wiring code — the three harness `image.json` files gain three entries
  each.
- `nix-daemon.service.d/proxy.conf`'s ordering still holds. The seed unit is
  ordered before `nix-daemon` alongside `discobox-render-proxy-env.service`,
  and needs no network of its own.
- Moving the store out from under a vendored unit's `ConditionPathIsReadWrite`
  is the kind of breakage that only shows up at runtime, as a client error
  about a missing socket rather than a failed unit. The seed unit's
  `ExecStartPost` starts `nix-daemon.socket` for that reason: a sandbox that
  booted with the socket skipped still recovers the first time a shim runs.
