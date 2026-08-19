# 0050 — Pool build state lives outside the sandbox-visible cache

- **Status**: Accepted
- **Date**: 2026-08-18
- **Relates to**: [0044](0044-builds-run-on-a-pool-shared-buildkit.md) (the
  pool-shared BuildKit whose state this is),
  [0047](0047-local-base-images-resolve-through-a-per-sandbox-registry-namespace.md)
  (the pool registry whose blobs this is),
  [0007](0007-declarative-sandbox-volumes-wired-by-the-sandbox-agent.md)
  (the cache volume this shares a namespace with)

## Context

`layout.PoolCache(projectID, poolID)` is one directory serving two unrelated
purposes.

The pool agent puts its own runtime state there. `buildkitagent.StateRoot` is
`PoolCache/buildkit` — the content store, snapshots, and solver cache of the
pool-shared BuildKit that 0044 introduced. `buildkitagent.RegistryRoot` is
`PoolCache/registry` — the blob storage behind the per-sandbox registry
namespaces of 0047. Both are pool-scoped by construction: one daemon, one
registry, shared by every sandbox the pool runs.

The same directory is also the sandboxes' cache volume.
`prepareSandboxVolumes` binds `poolCacheRoot()` whole onto `/.discobox/cache`
in every sandbox container, and sandbox-agent's `volumeDir` mirrors each
declared cache path underneath it: a harness declaring `%HOME%/.cache` is
stored at `PoolCache/home/<user>/.cache`. The `home/` component is not a
namespace anyone chose — it is the in-sandbox target path, reflected onto the
host.

Two consequences follow, and neither is intended.

### The pool's build state is reachable from inside a sandbox

Mode `0700` on `buildkit/` and `registry/` is the only thing standing between a
sandbox and the pool's shared builder. It is not a boundary. The sandbox user
holds passwordless sudo (`writeSudoers`), so from inside a running sandbox:

```
$ sudo ls /.discobox/cache/buildkit
buildkitd.lock  cache.db  containerd-overlayfs  history.db  net  runc-overlayfs
$ sudo touch /.discobox/cache/buildkit/PROBE
WRITABLE VIA SUDO
```

Every other sandbox in the pool builds against that content store and resolves
base images from those blobs. 0044 accepted sharing a *builder* between
sandboxes and went to considerable length to bind each build's egress to its
owning sandbox's certificate; it did not intend to also share the builder's
on-disk state as writable bytes. A pool is a sharing boundary for cache *hits*,
not for cache *contents*.

### Target paths and pool state collide in one namespace

Because `volumeDir` mirrors the in-sandbox path, the set of names under
`PoolCache` is chosen by harness authors, not by the pool agent. A harness
declaring a cache volume at `/buildkit` or `/registry` lands exactly on the
pool's state. Nothing today rejects that declaration, and nothing would report
it as anything other than a corrupted builder.

This is latent rather than observed — no shipped harness declares those paths —
but it is a name collision in a namespace that two independent parties write
into, which is a bug waiting on a coincidence.

### Why this surfaced now

The create path used to assert root ownership recursively over `PoolCache`,
which meant every sandbox create walked the whole of BuildKit's store: ~4.3×10⁵
inodes for `buildkit/` and ~2.7×10⁴ for `registry/`, about 37s per create on a
cold page cache. That walk is gone — the cache root is a bind-mount source and
only needs its own ownership — but removing it made the underlying question
visible. The create path had no business touching pool build state, and neither
does the sandbox.

## Decision

Split the two purposes into sibling directories under the disposable cache
tree. `PoolCache` keeps its name and becomes exactly what its doc comment
already claims: "the cache shared by every sandbox one pool runs."

```
cache/projects/<project>/pools/<pool>/
  build/          <- pool agent only, never mounted into a sandbox
    buildkit/
    registry/
  cache/          <- PoolCache; bind-mounted at /.discobox/cache
    home/<user>/...
```

1. `layout` gains `PoolBuild(projectID, poolID)`, a sibling of `PoolCache`
   under `ProjectCachePools`. `buildkitagent.StateRoot` and `RegistryRoot`
   resolve under it.
2. `PoolCache` remains the only thing `prepareSandboxVolumes` binds to
   `/.discobox/cache`, so its entire contents are paths some harness asked for.
   Nothing under it is privileged, and mode bits stop carrying an isolation
   burden they cannot hold.
3. The namespace under `PoolCache` belongs to harness-declared target paths
   alone. No pool-agent component may write there.

Neither tree is carried across. Both are regenerable and say so in their own
doc comments, so the old directories are deleted and the new ones start empty;
the first build after the change repopulates them. Nothing under the disposable
cache tree earns a migration.

## Alternatives rejected

**Nest the sandbox-visible subtree instead (`PoolCache/sandboxes`), leaving
BuildKit and the registry where they are.** This produces the same separation
and reads about as well. It was rejected on blast radius. Since the old
locations are purged rather than migrated, whichever subtree moves is the
subtree that gets thrown away — and nesting throws away the sandboxes' cache,
the warm npm, Go module, and pnpm content users actually feel the loss of, to
spare two trees that no sandbox is entitled to notice. Discard the one whose
loss is a rebuild, not the one whose loss is a slow afternoon.

**Keep one directory and rely on mode bits.** This is the status quo. It fails
on the facts above: the sandbox user has sudo, so `0700` filters nothing, and
mode bits cannot express "this name is not yours to declare" for the collision
case at all.

**Keep one directory and mount `PoolCache` read-only into sandboxes.** A cache
volume the sandbox cannot write is not a cache. Non-starter.

**Drop the bind and give each sandbox a private cache.** This would isolate
sandboxes completely, and it is the one option that needs no namespace rules.
Rejected because sharing is the entire point of a pool cache (0007, 0013): a
per-sandbox cache means every sandbox re-downloads every dependency, which is
the cost the pool exists to amortize.

## Consequences

- BuildKit's store and the registry's blobs are unreachable from inside a
  sandbox by any means, rather than by permission bits a sudoer can step over.
- A harness may declare a cache volume at any path without the pool agent
  having reserved names in that space.
- Existing pools carry `buildkit/` and `registry/` at the old location. They
  are deleted, not relocated. Nothing under the disposable cache tree is worth
  a migration: it is cache, it rebuilds, and the first build after the change
  pays for it once. Deleting is also the only option that cannot leave the old
  directories behind — and a leftover `PoolCache/buildkit` is still mounted
  into every sandbox, which would mean this ADR bought nothing.
- `PoolCache`'s invariant becomes stateable and testable: everything under it
  is a mirrored in-sandbox target path.
