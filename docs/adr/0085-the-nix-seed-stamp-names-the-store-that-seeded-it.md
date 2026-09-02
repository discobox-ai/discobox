# 0085 — The nix seed stamp names the store that seeded it

- **Status**: Accepted
- **Date**: 2026-09-02
- **Supersedes**: [0075](0075-the-nix-store-is-a-pool-shared-cache-seeded-on-first-use.md) §5's
  boolean stamps. §1–§4 and §6 stand unchanged.

## Context

[0075](0075-the-nix-store-is-a-pool-shared-cache-seeded-on-first-use.md) §5
guards the seed with two stamps that answer one question: *has this been
seeded?* `/nix/.discobox-seeded` on the pool cache, `/var/lib/discobox/nix-seeded`
on the sandbox's data volume, both empty files, both meaning yes-or-no.

Neither says *from which store*. A pool cache outlives the image build that
seeded it — the cache is a disposable block device that lives as long as the
pool ([0013](0013-local-linux-pools-use-libkrun-microvms.md) §1), while the
sandbox image is rebuilt whenever anything in it changes — so the two drift
apart, and the guard cannot see it.

That drift is not hypothetical. A pool seeded on 2026-08-27 met a sandbox
booted on the 2026-09-01 image:

- `seed_store` found `/nix/.discobox-seeded` and returned, copying no store
  paths — the cache kept the August closure.
- `seed_sandbox` found no stamp on this sandbox's fresh data volume and copied
  the September image's `profiles` and `gcroots` into it — a default profile
  whose generation link points at `/nix/store/4rg4…-profile`, a path that
  exists only in the September image's seed.

The result is a default profile that dangles: `/nix/var/nix/profiles/default/bin/nix`
does not resolve, the PATH shim (§4) starts the seed unit, the unit exits 0
having decided there was nothing to do, and the shim prints
`nix store seeding did not produce …`. `RemainAfterExit=yes` then makes every
later `systemctl start` an instant no-op, so the sandbox cannot recover on its
own — and neither can any other sandbox that later joins that pool on the new
image.

The stamps are also why the obvious repair is unsafe. Re-running the copy with
the cache already populated would copy the seed's `var/nix/db/db.sqlite` over a
database that has been recording every path the pool's sandboxes built and
substituted since. Those paths would become invalid in one step, and the swap
races the `nix-daemon` processes of every other sandbox on that cache.

## Decision

### 1. Each stamp is the list of seed stores merged into that scope

Both stamps become newline-delimited lists of *seed ids*. A run appends its id
after its copy succeeds; a run whose id is already listed does nothing.

The seed id is `sha256(sort(basenames of the seed's /nix/store))`, computed at
image build time and shipped beside the seed in
`/usr/local/lib/discobox/nix-seed.env`. Store path names are content hashes, so
the listing identifies the closure exactly: two images built from the same
inputs produce the same id and no needless copy, and any change to what the
image installs produces a different one.

Writing the id last preserves 0075 §5's crash safety unchanged — an interrupted
copy leaves no id, and the next run under the `flock` redoes it. The list, not
a single value, is what makes the merge honest: the cache genuinely holds the
union of every seed poured into it, including ones from images older than the
sandbox reading it.

An empty stamp — every pool seeded before this ADR — matches no id, so those
pools re-seed once, on the first nix command after a sandbox boots on a newer
image. That is the self-heal, and it is the same code path as any other image
change.

### 2. A warm cache is merged, never overwritten; the database is loaded

`seed_store` picks its path from the cache, not from the stamp:

- **Cold** (`/nix/var/nix/db/db.sqlite` absent): the copy of 0075 §5, store and
  `db` together. Unchanged, and still the only path a fresh pool takes.
- **Warm** (a database is already there): copy `./store` only, then register
  the new paths with `nix-store --load-db`, reading a dump of the seed's
  database that the image ships at `/usr/local/lib/discobox/nix-db.dump`.

Store paths are content-addressed, so the store copy is additive by
construction. The database is not: it is mutable pool state that outranks the
seed's. `--load-db` adds the seed's registrations to it and leaves every other
row alone, which is exactly the difference between merging a store and
replacing one.

The dump is generated at build time, next to the id, from the store the image
is about to move aside. Generating it at runtime instead would mean running
`nix-store` against the seed's own state directory — writing to what is
supposed to be a pristine copy source, on a path where nix has not been proven
to run at all.

`nix-store` is itself in the store being seeded, so the binary that loads the
database is the one the copy just delivered: `nix-seed.env` records its
absolute `/nix/store/…` path at build time. Nothing in the seed directory is
executed in place — 0075 §1's rule holds.

### 3. The seed unit is retryable

`RemainAfterExit=yes` is dropped from `discobox-nix-seed.service`. A `Type=oneshot`
that does not remain active re-runs on the next `systemctl start`, which is
what the PATH shim (0075 §4) has always assumed it was asking for.

This costs nothing now that §1 makes an up-to-date run a stamp read: two small
file reads under a lock. Concurrent starts still collapse — systemd joins a
caller to the running job rather than starting a second — and the `flock`
still serializes across sandboxes.

## Alternatives rejected

**Use the image digest or a build timestamp as the id.** Both are available at
build time and neither describes the store. Two images that differ only in a Go
binary would carry different digests and force a multi-gigabyte re-copy of an
identical closure; a timestamp would do the same on every rebuild. The store's
own content hashes are already the right identity and cost one `ls` to read.

**Wipe the cache when the id does not match.** One rule, no merge logic, and
the store ends up exactly matching the current image. Rejected because the
cache's value *is* its history: a pool's accumulated `devenv` closures are what
0075 exists to keep, and discarding them because the image gained a package
would make every image rebuild a cold start for every sandbox in every pool —
the cost this design is here to avoid.

**Copy the seed's `db.sqlite` over the cache's on a re-seed.** Simplest
possible merge, and wrong in the one way that matters: it invalidates every
path the pool built since it was seeded, and swaps a file that other sandboxes'
daemons hold open.

**Have the shim `systemctl restart` the unit instead of dropping
`RemainAfterExit`.** Recovers the same no-op case, but a restart issued while
another shim's first-use copy is running stops that copy mid-flight. The state
survives (§1's stamp discipline), the user's wait does not.

**Verify the profile resolves instead of comparing ids.** Checking that
`/nix/var/nix/profiles/default/bin/nix` exists would catch this exact symptom
and re-seed on it. Rejected as a narrower guard for the same cost: it detects
one consequence of a store/profile mismatch rather than the mismatch, and says
nothing about a cache that is merely missing paths the new image ships.

## Consequences

- An image rebuild that changes the store makes the first nix command in each
  existing pool pay one merge copy — the new closure only, since store paths
  the cache already holds are copied over themselves. Later sandboxes on that
  image find their id in the stamp and pay nothing.
- Pools stamped before this change re-seed once. There is no migration step and
  nothing to run by hand; the empty stamp matches no id.
- The image gains two build-time artifacts beside the seed:
  `/usr/local/lib/discobox/nix-seed.env` and `nix-db.dump` — a few megabytes of
  registration data, not a second store.
- A sandbox whose data volume predates its image gets the image's default
  profile back. Generations under `per-user/root` that the image also ships are
  overwritten; a user's own `nix profile install`, which lands under
  `per-user/<sandbox user>`, is not in the seed and is untouched.
- `discobox-nix-seed.service` shows as inactive after a successful run rather
  than active. `systemctl status` no longer distinguishes "seeded" from "never
  ran"; the stamps do, and they now say from what.
