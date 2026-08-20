# 0064 — Repair rebuilds on the current image

- **Status**: Accepted
- **Date**: 2026-08-20
- **Amends**: [0035](0035-repair-is-one-rebuild-intent-plus-a-start-instruction.md) §1 — the
  repair intent now carries a re-pin as well as `RepairGeneration`
- **Narrows**: [0016](0016-sandbox-image-upgrades-are-explicit-and-in-place.md) /
  [0021](0021-upgrade-is-a-re-pin-and-preserves-power-state.md) — the re-pin is
  no longer reached only through `POST .../upgrade`

## Context

Repair (ADR 0035) tears the sandbox's runtime down and rebuilds it against the
retained durable tree. Until now it rebuilt on `Sandbox.Image`/`ImageDigest`
exactly as they stood — the pin taken at create, which ADR 0016 moves only on an
explicit upgrade.

That is the wrong pin for a rebuild. ADR 0016's own motivating case was a
sandbox wedged *because* its image was stale: the pool agent had moved on, the
image had not, and the failure surfaced four layers away as a dead terminal. A
sandbox in that state is exactly the one whose owner reaches for repair, and a
repair that rebuilds on the stale image re-creates the container that could not
work, reports success, and leaves the user to discover that the second, separate
operation — upgrade — was the one they actually needed.

The cost that made upgrade explicit does not apply either. ADR 0021 keeps
upgrades explicit because a re-pin discards everything written to the container's
filesystem outside the durable volumes. Repair has already discarded exactly
that, by definition: the teardown is the operation. Once the container is going
away, rebuilding it on the older of two images buys nothing.

## Decision

1. **Repair re-pins to the harness config's current image, in the same intent.**
   `RepairSandbox` reads the upgrade target through the same rule
   `UpgradeSandbox` and the read path use (`services.SandboxUpgradeTarget`) and
   writes `Image`/`ImageDigest` into the one `recordSandboxIntent` call that sets
   `RepairGeneration`. One generation still owns the whole repair; there is no
   second intent, no upgrade call chained behind it, and no new column.
2. **An unavailable target is not an error.** Upgrade refuses with 409 when the
   sandbox is already current or has nothing to move to, because a re-pin is the
   whole of what it was asked for. For repair the re-pin is a rider: no target
   means the rebuild uses the pin it already has, and the repair proceeds. Only a
   store failure while resolving the target fails the request.
3. **Repair adopts a harness config when the re-pin does**, on the same legacy
   path upgrade already covers (ADR 0025 §4): a sandbox predating
   `HarnessConfigID` resolves its target through the fallback `shell` config, and
   pinning that config's image without adopting the config would leave the row
   describing an image no config of its own names.
4. **Repair is offered wherever a wedged sandbox is visible.** The TUI list binds
   it on `R`, enabled for a discobox in `error` or with no container reported —
   the two shapes ADR 0035 exists for — so the recovery is reachable without
   dropping to `disco box sandbox repair`.

## Alternatives rejected

- **Leave repair on its pinned image; tell users to upgrade first.** This is the
  status quo, and it makes the stale-image wedge — the case ADR 0016 was written
  about — the one repair silently fails to fix. It also asks the user to know
  which of two rebuilds their failure needs, from an error message that by
  construction cannot say.
- **Have repair call `UpgradeSandbox` and then record the repair intent.** Two
  intents, two generations, and a window between them where the reconciler can
  converge the upgrade alone. ADR 0035 rejected client-side sequencing for the
  same reason; doing the sequencing inside the service does not make the
  intermediate generation less real.
- **A `--upgrade` / `--no-upgrade` flag on repair.** A flag whose off position
  means "rebuild on an image we already believe is wrong" is a choice with one
  defensible answer. If a genuine need for pinned-image rebuilds appears — a
  bisect, a suspected bad image — it is a different operation with its own name,
  not a modifier on the recovery path.
- **Re-pin on every reconcile instead, so no operation has to ask.** That is
  automatic upgrades, which ADR 0016 rejected deliberately: it moves a running
  sandbox's image underneath it on an unrelated converge. The re-pin belongs to
  the rebuild, and repair is a rebuild the user asked for.
