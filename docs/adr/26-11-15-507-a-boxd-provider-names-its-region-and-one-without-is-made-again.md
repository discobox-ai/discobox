# 26-11-15-507 — A boxd provider names its region, and one without is made again

- **Status**: Accepted (supersedes [26-11-11-080](26-11-11-080-each-disco-vm-driver-is-a-provider-type-of-its-own-name-configured-by-its-own-fields.md)
  §7's "once it is in, the pin moves and nothing else changes", and, for a
  boxd row, §8's "with neither set, no `driver` object is written, and boxd
  takes the defaults it took before" and its "a boxd pool runs on with the
  machine it has"; the rest of §§7–8 stands; §2's "disables the provider,
  so its pools delete without calling boxd" superseded by
  [26-11-22-507](26-11-22-507-a-disabled-provider-takes-no-new-pools-and-its-existing-pools-keep-working.md):
  a disabled provider's pools delete through it, and a row without a region
  fails to build, so its pools' deletes retry)
- **Date**: 2026-11-15
- **Accepted**: by the owner's decision on #1225, 2026-11-15, in the PR that
  implements it.
- **Issue**: [#1225](https://github.com/discobox-ai/discobox/issues/1225)

## Context

disco-vm's boxd driver now requires a region (discobox-ai/vm#177): a
configuration without one is refused, and the driver no longer falls back
to the home endpoint's own region. A boxd provider made before, and every
`vm` row 080 §8 moved to boxd, has none. Driver fields are immutable while
the provider has pools (080 §5), so such a row cannot be given one through
the API.

There are no deployments to keep: the owner's word on #1225.

## Decision

1. **`driver.region` is required.** A boxd provider is refused at create
   without one, with disco-vm's message, and the catalog marks
   `--driver-region` required. The pin moves to a disco-vm that requires it.
2. **A row without a region is not migrated.** Nothing writes one in. It
   fails to resolve: the server logs it at start, each reconcile of its
   pools fails on it and leaves them `pending`, and `admin provider check`
   answers `refused` with it. Its pools are neither
   re-placed nor rescued. Its owner disables the provider, so its pools
   delete without calling boxd, deletes the pools and the provider, removes
   the machines and snapshots left in boxd, and creates them again with `--driver-region`;
   the public docs say so.
3. **The region's endpoint is discovered.** `driver.addr` is the cluster's
   home endpoint; disco-vm asks it for its regions (`ListRegions`) and
   calls the one it names for `driver.region`, or the home endpoint itself
   for the home region. An explicit `driver.addr` for a custom cluster
   keeps that: discovery still runs against it.

## Alternatives rejected

- **Backfill `fra` into every boxd row without a region.** `fra` is boxd.sh's
  home region, where those rows' machines are. But a row whose `driver.addr`
  names another cluster used that cluster's home endpoint, whose region need
  not be `fra`: the backfill would point it at a region it may not have, or
  away from its machines, with no API to correct it while pools stand. With
  no deployments, a migration buys nothing.
- **Backfill only rows on boxd.sh's endpoint, and let an unset region be set
  once under pools.** Correct for both kinds of row, but it adds an
  exception to 080 §5 and a start-time migration for rows nobody runs.
- **Trust an explicit `driver.addr` as the region's endpoint**, skipping
  discovery. It needs a disco-vm change. Deferred.

## Deferred

- **An explicit endpoint taken as the region's own.** Revisited when a
  custom cluster's home endpoint does not list the region it serves.

## Consequences

- `server/go.mod` and `pool-agent/go.mod` pin a disco-vm that requires the
  region.
- A boxd provider and its pools made without a region stop working after the
  upgrade until made again.
- `scripts/boxd-pool.sh` and `scripts/live-run.sh` name a region.
