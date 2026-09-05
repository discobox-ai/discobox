# 0094 — The pool cache is partitioned by the sandbox user's uid

- **Status**: Accepted (amends
  [0007](0007-declarative-sandbox-volumes-wired-by-the-sandbox-agent.md) §1's
  cache backing)
- **Date**: 2026-09-04

## Context

`/.discobox/cache` is one directory per pool, bind-mounted whole into every
sandbox that pool runs
([0007](0007-declarative-sandbox-volumes-wired-by-the-sandbox-agent.md) §1,
[0013](0013-local-linux-pools-use-libkrun-microvms.md) §1). The sandbox agent
binds `/.discobox/cache/<target>` onto each cache path the image declares and
chowns the mountpoint to the sandbox user, so `~/.cache`, the pnpm store,
`~/go/pkg/mod`, the cargo registry, `~/.rustup` and `/nix` are one tree the
whole pool writes into.

That is safe while every sandbox in a pool runs as the same user, which is the
assumption [0075](0075-the-nix-store-is-a-pool-shared-cache-seeded-on-first-use.md)
§2 states outright. The assumption is false, and not in an exotic way. A sandbox
runs as the uid of the client that created it — the CLI sends the local
account's uid, gid, name and home so the files a sandbox writes belong to the
person who will later read them
([0025](0025-the-sandbox-user-is-one-contract-resolved-inside-the-sandbox.md)).
One server serves many clients, and two machines rarely agree on a uid: a Linux
laptop at 1000 and a second machine at 4000 are one project, one pool, one cache,
and two users.

What breaks is ownership, not naming. A cache directory filled by uid 1000 hands
uid 4000 files it cannot write; every boot re-chowns the mountpoints to whichever
user started last, taking them from a sandbox that is still running; and where
the two clients also disagree about `$HOME`, the halves that do collide are
precisely the ones with no home in their path. Nothing detects this. It surfaces
as a build that cannot write its own cache, in a sandbox that looks correctly
owned from the inside.

Every other cross-sandbox tree is already keyed by the identity that makes it
safe to share: `data-per-source` by `originkey.Of(hostID, root)`, and the
per-sandbox data, config, sources and secrets trees by sandbox ID. The cache is
the one shared tree with no key at all.

## Decision

### 1. The partition is a level in the cache, chosen where the user is resolved

`volumeDir` in `sandbox-agent/boot` backs a declared cache path with
`/.discobox/cache/.users/<uid>/<target>` instead of `/.discobox/cache/<target>`,
unless the image declared that path shared (§3). A data path is unchanged: it is
this sandbox's alone, so it has nobody to be partitioned from, and moving it
would strand every existing sandbox's home.

The pool agent still binds `layout.PoolCache` whole. It is the wrong end for
this decision. The uid a sandbox runs as can live only inside its image — the
manifest may name a user without a uid, or name nobody at all, and resolving the
account against `/etc/passwd` is the sandbox's job by
[0025](0025-the-sandbox-user-is-one-contract-resolved-inside-the-sandbox.md) §4.
Boot has already done that resolution before it wires a volume, so the partition
it picks is the uid the sandbox actually runs as, in every case, with no fallback
for the cases the host cannot answer.

`.users` is dot-prefixed because its siblings are target paths mirrored into the
cache. No image declares a cache path at `/.users`, so a target cannot shadow the
partition or be mistaken for one — and everything at the cache root that is *not*
`.users` is pre-partition content by construction.

### 2. The key is the uid, and nothing else

Boot keys on `identity.uid`. A name is not a uid, a gid does not decide who may
write a file, and a home directory is where files go rather than whose they are.
Two sandboxes that agree on a uid share a cache, which is the whole point of a
pool-shared cache; two that do not, do not.

### 3. Sharing is declared, not inferred: `"scope": "shared"`

`harness.Volume` gains `scope`, which applies to cache paths and defaults to
`user`. A path that declares `"scope": "shared"` stays at `/.discobox/cache/<target>`
— one directory for the whole pool, whoever its sandboxes run as.

Exactly one path declares it, and it is the one that would otherwise cost the
most: `/nix` is root-owned, world-readable, content-addressed and reached through
a root daemon, so N uids on one store is the same "N processes on one filesystem
on one kernel" that
[0075](0075-the-nix-store-is-a-pool-shared-cache-seeded-on-first-use.md) already
established as safe. Partitioning it would have bought nothing and cost a
multi-gigabyte store per uid.

The default is the safe one, which is the whole reason this is a field rather
than a rule about ownership. What makes sharing safe is who ends up writing
inside a directory, and a cache path that declares no owner at all is still
filled by the sandbox user — so a rule derived from `uid` would answer "shared"
for the case nobody thought about. Unstated means the user's; sharing is a claim
an image makes out loud, in a line a reviewer can see.

A `scope` on a data path is refused rather than ignored: that volume is one
sandbox's own tree, so nothing could carry the claim out. The check is
`harness.ValidateVolumeScope`, applied where the control plane reads the image
label — so a bad scope is refused naming the image, not four layers away at
boot — and again inside `ResolveVolumes`, which is what fills in the default.

The declaration is only worth as much as its transport. `scope` crosses the
image label, the harness config snapshot, the pool API's `HarnessVolume`, and
`sandbox.json` before boot reads it, and two of those hops rebuild the struct
field by field. Both OpenAPI schemas carry it, both converters forward it, and
each has a test at its own hop: a field dropped in the middle leaves the image
declaring something true and the sandbox doing something else, with nothing
failing.

## Alternatives rejected

**Partition at the mount instead: bind `PoolCache/.users/<uid>` rather than the
whole tree.** The pool agent decides the backing path, so the fix would reach
every sandbox it launches — including one pinned to an old image, which §1's
placement cannot do (see Consequences). It would also make the partition a
boundary the mount enforces rather than a layout each sandbox follows. Rejected
on what it costs to get there:

- The mount source would move two levels *inside* a tree that every pre-upgrade
  sandbox still holds read-write, with sudo. `MkdirAll`'s stat follows an
  existing symlink-to-directory and succeeds, `Chmod` follows, `Lchown`
  deliberately does not, and the daemon resolves the bind source through it. One
  surviving old sandbox can therefore point `.users/<uid>` at
  `layout.PoolBuild` — handing a fresh sandbox buildkitd's store, which
  [0050](0050-pool-build-state-is-not-sandbox-visible.md) exists to prevent — or
  at `layout.PoolIdentity`, handing over the pool's Ed25519 signing key
  ([0063](0063-a-pool-agent-keeps-its-identity-key-and-registers-once.md)).
  Putting the partition root in a sibling tree no sandbox has ever mounted closes
  that, at the cost of splitting the cache into a live tree and a dead one.
- The host would need a partition for a request that names a user without a uid,
  and that partition cannot tell `--user-name darren` from `--user-name alice` —
  two names that resolve to two uids inside the sandbox
  ([0025](0025-the-sandbox-user-is-one-contract-resolved-inside-the-sandbox.md)
  §5, §6). The fallback would reintroduce the bug in the one partition that was
  supposed to name what is known.

**Infer the exemption from the declared owner: keep `uid: 0` paths pool-wide.**
Reaches the same place as §3 for `/nix` with no new field, since `uid: 0` is
already in `image.json`. Rejected because it reads a declaration that is not
about sharing — `uid` says who owns the mountpoint — and because of where it
lands the default: a cache path that declares no uid is the common case and is
filled by the sandbox user, so an owner-derived rule would silently share exactly
the paths nobody thought about. §3 keeps the saving and inverts the default.

**Partition everything, `/nix` included, with no exemption at all.** One rule, no
schema change, and nothing a later image can get wrong. Rejected once the cost
was counted against a real image: the only path it would protect is one that
provably does not need protecting, and it charges every pool a multi-gigabyte
store and a cold seed per uid for that. An explicit `scope` is not a rule a later
image can get wrong by accident either — only by declaring something untrue,
which is visible in review.

**Purge the pre-partition trees.** Everything a pool has cached today is at the
old paths and is stranded by this change. Rejected for now, and awkwardly: with
the partition chosen inside the sandbox, nothing outside it can tell whether a
running container writes partitioned paths or unpartitioned ones — that depends
on the image's own agent, not on anything the pool agent can see — so the
"unreferenced by construction" condition a host-side sweep could have checked is
not available here. Unlinking a directory a live sandbox still has bound loses
that sandbox's writes to reclaim disposable disk. Reclamation stays where 0075 §6
and 0013 already put it: the cache is disposable storage that lives as long as
the pool — which for a long-lived pool means it is not reclaimed at all. Revisit
if a pool is observed carrying a stranded store long enough to matter, most
likely as a sandbox-side sweep that runs where the partition is decided.

## Consequences

- Two clients whose accounts differ no longer share a cache directory, and
  neither can take ownership of the other's files. This is the whole of the fix.
- **The backing layout ships in the sandbox image, so it arrives with one.** A
  sandbox pinned to a pre-0094 image keeps writing the unpartitioned paths until
  it is next created on an image that carries this. Both routes that re-pin
  deliver it once the harness config's image has moved — an explicit upgrade
  ([0016](0016-sandbox-image-upgrades-are-explicit-and-in-place.md),
  [0021](0021-upgrade-is-a-re-pin-and-preserves-power-state.md)) and a repair,
  which re-pins to that image in the same intent
  ([0064](0064-repair-rebuilds-on-the-current-image.md) §1). Replacing the
  container on the same digest does not.
- A pool running mixed image versions writes two layouts into one tree. They do
  not corrupt each other — the partitioned paths are below `.users`, which no
  older agent writes, and the shared ones are the same directory for both — but
  the sandboxes on old images keep colliding with each other over the user-scoped
  paths, exactly as they do today, with nothing to signal it.
- `/nix` does not move. One store per pool, at the path it is already at, still
  seeded once per pool under 0075 §5's `flock` — no duplication, no re-seed, and
  nothing stranded for the largest tree in the cache. It is also why a mixed-image
  pool keeps sharing one store: an older agent, which knows nothing of scopes,
  lands on the same unpartitioned `/nix` a declaring one does.
- **The user-scoped trees do move, so every existing pool refills them once.**
  `~/.cache`, the pnpm store, `~/go/pkg/mod`, the cargo registry, `~/.rustup`
  and `~/.vscode-server` are repopulated in the partition, and the old copies are
  stranded: unreferenced by any upgraded sandbox, still on disk, and still counted
  by `treeBytes(layout.PoolCache)`, so `cacheBytes` includes trees nothing
  reachable is using. Nothing reclaims them short of deleting the pool. They are
  package caches rather than a nix store, so this is a slow first build, not a
  multi-gigabyte re-seed.
- They are not migrated. The first partition to claim an old tree would inherit
  files owned by a user that may not be its own, which is the bug this fixes.
- `layout` is untouched: the cache's shape below the mount belongs to the sandbox
  agent that wires it, the same way every other in-sandbox path does under
  [0007](0007-declarative-sandbox-volumes-wired-by-the-sandbox-agent.md).
- Image metadata gains a field, so an image built before it keeps meaning what it
  meant: no `scope` is the user's, which is the answer such an image needs.
