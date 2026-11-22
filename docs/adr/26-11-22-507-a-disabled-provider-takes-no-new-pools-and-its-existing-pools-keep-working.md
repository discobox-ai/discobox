# 26-11-22-507 — A disabled provider takes no new pools, and its existing pools keep working

- **Status**: Proposed
- **Date**: 2026-11-22
- **Issue**: #1556
- **Supersedes in part**
  [26-11-14-624](26-11-14-624-after-an-upgrade-the-server-is-not-ready-until-its-pools-are.md)
  §1: a pool on a disabled provider no longer drops out of the upgrade wait.
  Only one on a removed provider, or on one that cannot be built here (§2),
  does.
- **Settles**
  [26-11-11-080](26-11-11-080-each-disco-vm-driver-is-a-provider-type-of-its-own-name-configured-by-its-own-fields.md)
  §8's "how a pool of an unregistered type is deleted is implementation's".

## Context

Today `Disabled` switches off everything the server does with a provider
instance:

- `convergedProvider` (`server/internal/resources/pools/reconciler.go`)
  returns nil for a disabled instance, so its pools are neither reconciled
  nor upgraded. `ProviderManager.ResolveInstance`
  (`server/internal/sandbox/manager.go`) refuses it, so its pools take no
  sandboxes and no transfers.
- `CreatePool` (`server/internal/resources/pools/service.go`) does not check
  `Disabled`, so it accepts a pool that is then never brought up.
- `reconcileDeleted` calls `RemovePool` only when the provider resolves. A
  pool deleted on a disabled remote provider (Archil, boxd) loses its row,
  but its machine and its `pool-<id>` scope keep running and keep being
  billed. Nothing finds them again: several servers share one provider
  account, so no server sweeps a scope against its own rows.

The server also disables two kinds of instance itself. It builds neither,
and neither can have pools that ever ran:

- the built-in default on an OS with no provider, type `unsupported`
  (`server/internal/service/defaults.go`);
- a `vm` row left with no matching driver by the driver migration
  (`server/internal/store/vmproviders.go`, ADR 26-11-11-080 §8).

## Decision

### 1. Disabled closes a provider to new pools, and nothing else

- Creating a pool on a disabled provider is refused with 409.
- Everything else treats a disabled provider like an enabled one: pool
  reconcile, the upgrade watch, `EnsureExistingSandboxProviderInstances`,
  sandbox create and transfer onto its existing pools, and pool delete.
  Deleting a pool calls `RemovePool`, which removes its machines and its
  scope.
- `ResolveInstance` stops refusing disabled instances. The new-pool refusal
  lives in the pool service, the one place a pool is created.
- `CheckInstance` no longer needs a separate path for a disabled instance:
  it is resolved and cached like any other.

### 2. "Cannot be built here" is the type, not a stored state

An instance whose type has no factory and no registered provider on this
server cannot be resolved. `unsupported`, `vm`, and a `vz` or `wslc` row on
another OS are all like this. For such an instance:

- its pools are not reconciled and drop out of the upgrade wait, as on a
  removed provider;
- deleting one of its pools drops the row and calls no `RemovePool`, since
  nothing of it can exist and nothing could be built to reach it.

The server still marks both kinds of instance it creates itself as disabled,
so they take no new pools. But the row-only delete follows from the type,
not from `Disabled`.

A provider whose type is registered but fails to build or reach its service
is still a failure. Its reconcile and its delete retry, exactly as on an
enabled provider today. The row-only delete is never a fallback for a build
error.

## Alternatives rejected

- **Keep disabled as "off" and only fix delete.** That keeps reconcile off
  while letting `RemovePool` through, which leaves pools half-supported:
  they run but are never upgraded or repaired. The owner's meaning (2026-11-22)
  is that existing pools keep working.
- **A stored state for "cannot be built", such as a `disabledReason` or an
  `orphaned` flag.** It would duplicate what the type registry already
  knows, and it can go stale: a later server that registers the type would
  still read the flag. Deriving it from the registry leaves nothing to
  migrate.
- **A server-side sweep of a provider's scopes for machines with no row.**
  This is ruled out because servers share accounts (owner, 2026-11-21).
  Removal stays tied to deleting a pool or sandbox. Orphans left by other
  paths are accepted.

## Consequences

- An operator who disables a provider to stop its cost has to delete its
  pools as well. Disabling only stops growth. CLI help for
  `--disabled` and the API documentation say this.
- A database moved to a server that cannot build a local type (a `vz` row on
  Linux) deletes that type's pools as rows alone. Whatever those pools ran
  stays on the host that ran them, which this server never reached anyway.
- The pools, providers and server `DESIGN.md` files change with the code.
  The `Disabled` field's API description changes with them.
