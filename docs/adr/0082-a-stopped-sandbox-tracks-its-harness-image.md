# 0082 — A stopped sandbox tracks its harness image

- **Status**: Accepted
- **Date**: 2026-09-02
- **Supersedes**: [0021](0021-upgrade-is-a-re-pin-and-preserves-power-state.md) §2
  ("No implicit re-pin, ever"). §§1, 3–5 stand, and this ADR is built on them.
- **Restores, with a different trigger**:
  [0016](0016-sandbox-image-upgrades-are-explicit-and-in-place.md) §5
- **Closes**: 0016's Deferred *"a project- or pool-level `upgradePolicy`
  field"*

## Context

A sandbox runs the image it is pinned to until somebody upgrades it. In
development, where `discobox-docker-image-watch` rebuilds the harness images
continuously, every discobox anyone stops is behind within the hour and stays
behind forever. ADR 0021's own Consequences record this and wave it through —
*"This is the intended behavior, not friction to design around."* This ADR
revisits that.

### What was tried, and why it failed

ADR 0016 §5 said "a stopped sandbox upgrades itself on its next start" and
implemented it as a re-pin inside the reconciler's `ensure`. ADR 0021 §2
withdrew it. The stated flaw was the **trigger**, not the idea:

> the pin advanced on a reconcile of a non-live sandbox — an event a user does
> not issue and cannot see. Whether the sandbox they come back to is the image
> they left depends on whether something marked it dirty while it was stopped.

And the common way a stopped sandbox comes up marks nothing dirty at all: ADR
0017 §12's auto-start latch brings a sandbox up from sandbox-directed traffic
without consulting the control plane. So a rule phrased as "on its next start"
fired on some starts and not others, according to whether an unrelated write
happened to be in flight. A rule that upgrades a sandbox sometimes is worse
than either answer.

### What has changed since

**The operation already exists and already does the right thing.** ADR 0021 §1
made `UpgradeSandbox` a pure desired-state re-pin, and §3 made the pool agent
record whether the container it replaced was running and start the replacement
only if it was. So an upgrade of a stopped sandbox *already* rebuilds its
container on the new image and leaves it stopped. Nothing about that behavior
needs inventing here.

**The question "is it stopped?" is now askable.** ADR 0034 split `state`
(existence, written only by `SandboxReconciler`) from `runtime_state` (power,
written only by `Store.ApplySandboxStateReports`). Under the single column ADR
0021 was written against, "converged, and observed stopped" was not expressible
— the two answers overwrote each other, which is why ADR 0034 exists.

**The pin already moves without an explicit upgrade.** ADR 0064 made repair
re-pin to the current image, because rebuilding on an image believed to be
stale "re-creates the container that could not work." ADR 0021 §2's rule is
therefore already not "the pin moves only on `POST …/upgrade`"; it is "the pin
moves only where intent is recorded." That narrower rule is the one worth
keeping, and this ADR keeps it.

## Decision

**Resolving a harness image to a new digest upgrades that harness's stopped
sandboxes, by doing exactly what `POST …/upgrade` does.**

### 1. The trigger is the digest moving, and the action is the existing upgrade

**Wherever a harness config's `ImageDigest` is written to a value different
from the one it replaced, that config's eligible sandboxes are re-pinned.** The
rule is stated on the field, not on a list of functions, because it must hold
for a harness config however its image is resolved — a built-in reseeded at
startup and a custom harness re-pulled by its owner are the same event to a
sandbox running it.

Today that is two paths, and they are not equivalent in importance. `SeedBuiltIns`
re-inspects the built-in harnesses on every pass, which is what carries a dev
rebuild of a stable tag. `RefreshHarnessConfigImage` — `POST
…/harness-configs/{id}/refresh-image` — is the same event for a config
registered from a user-supplied image, which has no other trigger at all
(ADR 0016 §7). `CreateHarnessConfig` also writes a digest and is deliberately
exempt: the config is new, so nothing references it yet.

To keep the rule from decaying into those two call sites remembering
independently, both funnel their `previousDigest` comparison and the fan-out
through one helper on the harness config service. A future writer of the field
— repointing a config at a different image, which `UpdateHarnessConfig` cannot
do today — inherits the behavior by using it rather than by being amended into
a list.

The action is the existing upgrade: the same `imageRepin` applied through the
same `recordSandboxIntent` that `UpgradeSandbox` calls, decided by the same
`services.SandboxUpgradeTarget` the read path reports `upgrade.available` from.

The fan-out reaches the sandboxes through the seam that already exists for
this: `harnessconfigs.SandboxRuntime`, whose `RebindHarnessConfigSecrets`
repoints a config's sandboxes when a secret binding changes. A second method on
that interface — `UpgradeHarnessConfigSandboxes`, over its own eligibility
query — is the whole of the new plumbing.

So there is no new operation, no new convergence path, and nothing new for the
pool agent to understand. The decision is a call at the moment the digest
moves; the delivery is the ordinary reconcile, because the re-pin changes
`SandboxManifest.Fingerprint()` and the pool agent rebuilds any container whose
`discobox.spec_fingerprint` label no longer matches (ADR 0017 §5). From the
pool agent's side an automatic upgrade and a typed one are the same event.

This is also the whole difference from ADR 0016 §5, which advanced the pin as a
side effect *inside* a convergence — where it left no generation of its own to
point at, and no record that anything had decided. The pin still moves only in
a transaction that bumps the generation, writes desired state, and marks the
sandbox dirty. What is new is an **author** of that intent, not a new place the
pin moves.

### 2. Eligibility is one predicate over the two state axes

A sandbox is upgraded by the fan-out when all of these hold:

| Condition | Why |
| --- | --- |
| `desired_state = present` | Not on its way to archived or deleted. |
| `state = ready` | The reconciler has converged its container against its spec. Excludes `pending` and `awaiting_source`, which are using the pin right now. |
| `generation = observed_generation` | Nothing is mid-flight; never pile intent onto an unsettled row. |
| `error_message IS NULL` | A settled failure needs intent aimed at the failure, which is what repair is (ADR 0064). |
| `runtime_state = 'stopped'` | Observed down. Empty is excluded on purpose: ADR 0034 §2 makes "not observed" a different answer from `stopped`, and acting on it would be acting on no observation at all. |
| not in config mode | A config-mode sandbox runs a deliberately fixed image. |
| the target digest differs | The same rule the read path reports as `upgrade.available`. |
| the project's policy is not `manual` | §3. |

Nothing here is a heuristic and nothing is time-based. Every column has exactly
one writer, which is what makes the answer stable enough to act on unattended.

The split between the two halves is deliberate. The **state** conditions are the
eligibility query's, because they are questions about the row. The last two above
the policy are **not** restated there: they are `services.SandboxUpgradeTarget`,
the one implementation the read path also reports `upgrade.available` from, so
the query and the reported state cannot answer differently. Config mode is the
sharper case of the two — the rule is `mode != "config"`, and a column
comparison against `'run'` would also drop a legacy row whose mode is empty.

### 3. The policy is a project setting, and it defaults to on

`Project.SandboxUpgradePolicy` takes `automatic` and `manual`. Empty means the
server default, which is `automatic` — following ADR 0022 §4's precedent on
`ArchiveRetentionSeconds`, where a project that has never chosen tracks the
default as it changes rather than being frozen to whatever it was at create.

It exists because the cost is real and, unlike a typed upgrade, is paid without
anybody being asked. A rebuild discards everything written to the container's
filesystem outside the durable volumes: an `apt install`, a global npm package,
anything in a path the image does not declare as a volume. ADR 0016 §4 accepted
that cost on the condition that it "must be stated at the point of opt-in, not
just in docs" — and an unattended upgrade has no such point. The setting is
where the opt-out lives instead, and it is the field ADR 0016's Deferred
section named and conditioned on "once in-place upgrade has run for a release
cycle without data-loss reports."

A string rather than a boolean, so a later policy is a new value rather than a
breaking change.

## Alternatives rejected

**A standing scan instead of a fan-out.** Give the reconcile engine a resource
type whose `ScanDirty` returns every eligible sandbox, so the rule is
level-triggered like `ListSandboxRefsNeedingReconcile` and archive retention
are, and no sandbox can ever be missed. Rejected as machinery bought for a
problem that does not survive inspection. A fan-out misses a sandbox that is
running, unsettled, or failing at the instant the digest moves — and the cost
of that miss is being one image version behind, which `upgrade.available`
already reports and the next image resolution already corrects. Paying a new
resource type, a scan query, and a reconciler that converges nothing to close
that gap is not a trade worth making. Revisit only if a sandbox is observed
stranded on an image that never moves again.

**Re-pin inside `ensure` again, with a better predicate.** The rule in §2 could
be evaluated in the reconciler instead. Rejected: the pin would again move with
no generation of its own, so nothing could say when or why a sandbox changed
image, and the same path would advance the pin on marks it did not author — a
source change, a runtime-loss report. Every pin move sitting inside a recorded
intent is what makes `upgrade.available` and the sandbox's history mean
anything.

**Re-pin only, and let the next start pick the new image up.** The most natural
reading of "change the metadata at rest," and it does not work. Starting a
sandbox whose container exists resolves no image: `startLocked` starts the
container it has, and nothing on that path consults the pin. Making it consult
one means the pool agent holding the desired spec at rest — it does not; the
create request carries it — and would put a container rebuild and possibly an
image pull inside the latency of a user's first request. Worse, the auto-start
latch never reaches the control plane, so the rule would land on some starts
and not others: the precise flaw ADR 0021 §2 named. Rebuilding while the
sandbox is down reaches the same outcome at a moment when nobody is waiting.

**Include `failed` sandboxes.** A sandbox stranded by a pruned image reads
`failed` from `resolveSandboxImage`, and a re-pin is exactly its fix — ADR 0021
§2 accepted that exposure explicitly. Rejected for now: repair already re-pins
to the current image (ADR 0064) and is bound to `R` in the TUI list for this
shape, so the recovery exists and is one keystroke. Sweeping failures would
also retry every unrelated failure each time an image moves, turning a settled
verdict into a recurring one. Revisit if stranded-by-pruning is seen in the
field.

**Include archived sandboxes.** An archived sandbox has no container, so
re-pinning it at rest is free and its unarchive would build on the current
image. Rejected as a separate concern: its `desired_state` is `archived`, so
the generation bump this mechanism relies on would re-drive an archive rather
than a rebuild. It needs a pin write with no intent, which is a different
decision.

**Have the pool agent refuse to replace a running container for an unattended
change.** Between the fan-out reading `runtime_state = 'stopped'` and the pool
agent taking the sandbox's power lock, the auto-start latch (ADR 0017 §12) can
bring the sandbox up, and ADR 0021 §3 then restarts it into the new image. The
control plane cannot close that itself — ADR 0021 §4 is explicit that its view
of power is an observation that can be stale by the time the request lands,
while the pool agent holds the authoritative answer under a lock it already
takes. So closing it means marking the fan-out's generation on the sandbox
(a column, mirroring `RepairGeneration`), carrying it to the pool agent as a
flag on the create config, and handling a decline.

Rejected on cost. It is a new column, a change to `pool-agent/api/openapi/pool.yaml`,
and a third disposition for a create — and the decline's two natural handlings
are both broken. Leaving the generation unobserved so the backstop retries it
trips `sandboxProvisioningPending` (`attach_wait.go`), which reads an
unobserved generation as "still provisioning" and makes **attach wait**: a
healthy running sandbox would become unattachable for as long as it stayed up.
Leaving the pin advanced and settling makes `upgrade.available` — which
compares the pin to the config — report `false` about a container that is
behind, with nothing to correct it. The workable third option, restoring the
previous pin in the write that records the generation observed, is more
machinery still.

What is bought for that is closing a window measured in the gap between one
transaction committing and one HTTP request landing, whose outcome is a restart
into the newest image: what a typed `disco box upgrade` does on purpose. The
exposure is recorded in Consequences and accepted. Revisit if anyone actually
meets it.

**No policy field; make it unconditional.** Rejected: §3. There would be no way
to hold a discobox on the image it was built with, and the container-filesystem
cost would be paid with neither an opt-in nor an opt-out anywhere.

## Consequences

- Re-pulling a custom harness's image (`POST …/refresh-image`, or
  `disco box harness refresh-image`) upgrades that harness's stopped sandboxes,
  which it previously only made *reportable* as an available upgrade.
- `Project` gains `SandboxUpgradePolicy`, in `api/openapi/server.yaml`,
  contract first. That is the only schema change: the pool agent is untouched,
  because an automatic upgrade reaches it as the spec change a typed one
  already does.
- A sandbox that the auto-start latch brings up in the window between the
  fan-out reading it as stopped and the pool agent acting is restarted into the
  new image, exactly as a typed upgrade would restart it. Accepted rather than
  closed; see Alternatives rejected.
- A stopped discobox's container is rebuilt while it is stopped. Its ID,
  history, durable volumes, sources, and secrets survive; writes to the
  container's own filesystem outside those do not. That is ADR 0016 §4's cost,
  now paid unattended, and §3 is where a project declines it.
- Every stopped sandbox on a rebuilt harness image causes one container
  replacement per digest move, and an image pull on any pool that does not hold
  the digest. In the development loop the images are already local; on a remote
  pool this is background bandwidth, spent while nothing waits on it.
- ADR 0021's Consequences no longer hold on one point: a long-stopped sandbox
  is no longer arbitrarily far behind its harness config, unless its project
  has chosen `manual`.
- `disco box upgrade` is unchanged, and remains the way to move a *running*
  sandbox deliberately. A running sandbox is never selected by the fan-out;
  the one way it can still be restarted by one is the accepted window above.
