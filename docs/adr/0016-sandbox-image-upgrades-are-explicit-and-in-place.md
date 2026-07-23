# 0016 — Sandbox image upgrades are explicit, in-place, and digest-driven

- **Status**: Accepted
- **Date**: 2026-07-23
- **Closes**: the digest-pinning follow-on left open by
  [ADR 0012](0012-sandbox-config-is-three-attribute-owned-layers.md)

## Context

A sandbox binds to its harness image exactly once. `Sandbox.Image` is resolved
at create from the harness config (`resources/sandboxes/service.go`), stored,
and never resolved again. `ensureSandboxCreated` is ensure-exists, so a restart
reuses the container that already exists. Nothing in the system moves a live
sandbox onto a newer image, and nothing tells the user one exists.

The same reconcile path treats the rest of the harness config differently:
`createOptionsFromSandbox` re-reads `RunCommand`, `RelaunchCommand`, `Files`,
`Env`, and `Volumes` from the harness config on **every** start. Content drift
therefore lands on any restart while image drift never lands at all. That
asymmetry is the defect: the sandbox's declared behavior keeps moving forward
while the filesystem and the agent binary implementing it stay frozen at create.

It is not theoretical. Sandbox `sbx_kg0cc9fcm5bxbcka` was created from
`discobox-harness-claude-code:dev-873891e5a8b8`, built before ADR 0012's flat
`sandbox.json` and before the `io.discobox.harness.v1` → `io.discobox.image.v1`
label rename. The pool-agent had moved on; the image had not. Consequences, in
order:

1. The current pool-agent could not read the old image's label, so it wrote
   `harness.runCommand: null` into `sandbox.json`.
2. The stale `sandbox-agent` baked into that image could not decode the new flat
   schema's `user`/`sources`, so it resolved no home directory.
3. Its primary terminal failed to launch (`harness "claude-code" has files to
   install but no home directory could be resolved for the run user`), leaving
   an orphaned exec record with no unit and no socket.
4. `terminal attach` returned `500 dial unix …/ex_….sock: no such file or
   directory`.

Four layers of confusing failure for one fact — the sandbox was running an
obsolete image — that the API could not express and the user could not act on.

ADR 0012 already named the root of this and left it open. Its closing
**Follow-on, not resolved by this ADR** records that `harnessConfig.Image` — the
reference actually used to pull and run — is a mutable tag, that `ImageDigest`
is captured as an audit snapshot and never consulted by the pull/run path, and
that `RuntimeLayer.Image` ("the resolved digest ref") therefore assumes
something that is not true end-to-end. It called the gap "required for the
OCI-label snapshot to be trustworthy without re-inspection" and asked for a
companion change. **This ADR is that companion change**: an upgrade mechanism
with nothing but a mutable tag underneath it would report drift it cannot
actually pin, and re-pinning to a tag would leave the next `docker build` free
to move the sandbox again without anyone asking.

0012 sketched two ways to close it: pin `harnessConfig.Image` to
`image@sha256:…` at registration, or re-resolve on every sandbox create. The
first is not available. A locally built image has no manifest digest — `docker
image inspect discobox-harness-claude-code:local` reports `RepoDigests=[]` —
and the local `:local` and `dev-*` builds this project runs on are never pushed,
so there is no `image@sha256:…` reference to pin to. `defaultImageInspector`
already met this and chose the workable currency, in a comment that reads as the
prior half of this decision:

> Use the config digest so the recorded digest matches the local daemon's image
> ID for the same image; the manifest digest is only defined once an image is
> pushed, which local `:local` builds never are.

So `HarnessConfig.ImageDigest` is already a config digest, comparable against
both a local daemon's image ID and a remote image's config name. What is missing
is not a better identity — it is that nothing on the run path consults the one
we already have.

Two further facts make a fix tractable:

- **Sandbox state is not in the container.** `prepareSandboxVolumes` binds
  `/.discobox/{data,cache,config,sources,secrets}` from per-sandbox directories
  on the pool host. The workspace, the sources, the caches, and the secrets all
  outlive the container. Replacing the container is not a euphemism for starting
  over.
- **The pattern already exists one level up.** Pool containers carry
  `discobox.pool_agent.config_revision` and `discobox.pool_envelope` labels, and
  `shouldRemoveExistingContainer` recreates on drift. Pools may do this silently
  because they hold no user state. Sandboxes hold user state, so the same
  mechanism needs consent rather than autonomy.

One gap blocks any digest comparison today: `SeedBuiltIns` skips a harness
config whose image **reference** is unchanged (`if existing.Image == image {
continue }`), so a rebuild that reuses a tag — `:local`, or any stable dev tag —
never refreshes `ImageDigest`. The desired side has to move before the observed
side can be compared to it.

## Decision

**A sandbox pins the image identity it was built from, as a reference plus a
config digest that the pull/run path enforces rather than merely records.
Divergence from its harness config's current identity is reported as an
available upgrade, and applying it recreates the container in place.**

Pinning and upgrading are one decision, not two. A pin nothing enforces is the
audit field 0012 already complained about; an upgrade over an unenforced pin
reports drift it cannot hold still. Neither half stands alone.

### 1. The pin is a reference plus a config digest, and the run path enforces it

`Sandbox` gains `ImageDigest` alongside `Image`, written at create from the
harness config's snapshot and thereafter **only** by an upgrade. Together they
are the pin: `Image` is what to pull, `ImageDigest` is which image that must
turn out to be.

`sandbox.ImageRef` gains `Digest` alongside `Name`, and `ensureSandboxImage`
stops treating the digest as an audit field. After resolving or pulling `Name`,
it compares the resolved image's config digest to the pinned `Digest`:

- **The pinned digest is present locally** — run it, whatever the tag now points
  at. A tag that moved forward is not an instruction; it is the condition §2
  reports as an available upgrade, for the user to act on.
- **The pinned digest is absent and pulling `Name` produces it** — proceed. This
  is the ordinary registry case, where the tag still resolves to the pin.
- **The pinned digest is absent and pulling `Name` produces something else** —
  fail the operation naming both digests. Starting a sandbox that is not the
  sandbox that was pinned is the failure this ADR exists to prevent, and doing
  it quietly is worse than not starting.

The pin this verification runs against is the pin *after* policy has been
applied. A stopped sandbox re-pins on its way up (§5), so it verifies against
its new pin and this check passes; it does not fight the auto-upgrade.

This closes ADR 0012's follow-on. `RuntimeLayer.Image` becomes what 0012 assumed
it already was — a reference that resolves to one known image — and the
OCI-label snapshot taken at registration becomes trustworthy without
re-inspection, because the digest that snapshot was taken from is the digest the
run path verified.

A digest and not just a tag, because local rebuilds reuse tags: tag equality
would report "up to date" for every `:local` sandbox in the fleet, which is the
exact failure that motivated this ADR.

Sandboxes with no harness config (the default image) pin no digest and never
report an upgrade.

### 2. Upgrade availability is derived, never stored

The server compares the sandbox's pinned digest to its harness config's current
`ImageDigest` at read time. The harness config is already loaded with the
sandbox, so this costs nothing on list.

It is not a column. A stored flag would need invalidating on every harness
config write, from every path that touches one — seeding, registration,
configure, deconfigure — and the first missed path yields a sandbox that lies
about its own state.

### 3. The trigger is image digest drift, and only that

Three other candidate triggers are explicitly out:

- **Harness config content drift** (files, env, commands) already applies on the
  next restart. Reporting it as an *upgrade* would tell users to recreate a
  container to get something a restart delivers.
- **Agent contract version mismatch** — a `sandbox-agent`-reported contract
  version checked against the server's — would catch skew that a digest cannot:
  a stale agent inside an image whose digest is current. Deferred, not rejected;
  see below.
- **Pool or pool-agent drift** is the pool's business. Pools already auto-
  recreate on it, and surfacing it per sandbox multiplies one fact by the
  sandbox count.

### 4. Upgrading recreates in place, under the same sandbox ID

`POST /projects/{projectId}/sandboxes/{sandboxId}/upgrade` re-pins the sandbox
to its harness config's current `Image` and `ImageDigest`, bumps
`UpgradeGeneration`, and returns `202` with the sandbox — the same shape as
restart. The reconciler converges `UpgradeGeneration > UpgradedGeneration` by
destroying the container and creating it from the new pin, then setting
`UpgradedGeneration = UpgradeGeneration`.

The sandbox ID, its terminal and exec history, its secrets, and all five
pool-host volumes survive. **Writes made to the container's own filesystem,
outside the declared volumes and sources, do not.** That is the trade-off the
user opts into, and it must be stated at the point of opt-in, not just in docs.

Cloning to a new sandbox would preserve those writes, at the cost of doubling
resource use and splitting one unit of work across two IDs with two histories.
The in-place recreate matches what a sandbox already is: a disposable runtime
around durable volumes.

### 5. A stopped sandbox upgrades itself on its next start

A sandbox that is already stopped has no session to interrupt and no in-
container state a user is mid-way through relying on. On a stopped → running
transition the reconciler re-pins to the harness config's current identity
before creating the container. Starting a stopped sandbox is the moment its
runtime is built; building it deliberately obsolete serves nobody.

A **running** sandbox never changes image without the explicit action. In
particular, restart keeps the existing pin — restart means restart.

### 6. The runtime runs the pinned digest, and replaces a container that isn't it

The sandbox create request carries the pin — reference *and* digest — and the
runtime treats the **digest** as the identity to run. `Name` is used only to
pull when the digest is not present locally, never as the thing to resolve and
launch.

`DockerSandboxRuntime.CreateSandbox` returns the existing container when one is
present. It gains one rule: if that container's image ID differs from the
requested digest, remove and recreate it.

This is why the runtime must not resolve the tag itself. If it launched
`Name`, a tag that moved under a running sandbox would make the container look
drifted and the runtime would replace it — silently performing the upgrade that
§4 and §5 reserve for the control plane. Running the digest makes a moved tag a
non-event down here, and leaves exactly one component deciding when a sandbox
changes image.

The container's image ID and the pinned digest are the same currency — a config
digest — so this is one comparison, not a second identity scheme bolted next to
the first. Comparing image IDs rather than the `Config.Image` string is what
makes a same-tag rebuild detectable at the runtime layer.

No policy flag rides along in the request. The control plane only ever sends a
changed digest when it has decided to upgrade, so the runtime rule can be
unconditional and the policy stays in exactly one place.

### 7. The harness config snapshot tracks digests

`SeedBuiltIns` inspects the image and compares digest as well as reference,
refreshing the snapshot when either moves. Harness configs registered from a
user-supplied image get an equivalent refresh path; without one, `needsUpgrade`
can never fire for them, and the whole mechanism is dead code outside built-ins.

## API shape

`Sandbox` gains a read-only object, absent when the sandbox pins no digest:

```yaml
upgrade:
  available: boolean          # pinned digest differs from the harness config's
  currentImage: string        # what this sandbox is running
  currentImageDigest: string
  targetImage: string         # what its harness config resolves to now
  targetImageDigest: string
  reason: string              # enum, "imageDigestChanged" today
```

`reason` is an enum from the start so the deferred triggers can be added without
a breaking change to a boolean.

Also added: `upgradeGeneration` / `upgradedGeneration` on the sandbox, mirroring
`restartGeneration` / `restartedGeneration`; `upgrade` in the
`SandboxRuntime.activeOperation` enum; `model.SandboxUpgradeOperation`;
`ImageDigest` on `Sandbox`; and `Digest` on `sandbox.ImageRef`, which is a Go
contract rather than an API schema but changes with them.

`currentImageDigest` and `targetImageDigest` are config digests, matching what
`HarnessConfig.ImageDigest` already records and what a local daemon reports as
an image ID. They are not manifest digests and must not be rendered to users as
something they can `docker pull`.

The upgrade request body carries no client-supplied expected digest. Optimistic
concurrency against a moving target sounds safer than it is: in the local
development loop that motivates this ADR, the image rebuilds continuously, and a
409-on-moved-target turns "upgrade this sandbox" into a retry loop against a
target that was never wrong. The reconciler uses whatever the harness config
holds when it runs, which is always the newest thing available.

The API is schema-first: `api/openapi/server.yaml` changes first, then
`go tool task generate`.

## Alternatives rejected

**Pin `harnessConfig.Image` to a digest reference (`image@sha256:…`) at
registration**, as ADR 0012's follow-on suggested, making the reference itself
immutable and removing the need for any separate digest field. Rejected because
it does not work for this project's primary flow: a locally built image has no
manifest digest until it is pushed, and the `:local` / `dev-*` images that
`discobox-docker-image-watch` rebuilds are never pushed. Pinning a config digest
alongside a mutable reference, and verifying it at run, gets the same guarantee
for both local and registry-backed images.

**Adopt the tag's new target silently when the pinned digest is missing or
moved**, on the grounds that the newest image is usually what the user wants.
Rejected: that is the current behavior generalized, and it is what let a sandbox
diverge from its harness config without any record of having done so. An upgrade
the user did not ask for is indistinguishable, from inside the sandbox, from the
skew this ADR exists to eliminate.

**Keep `ImageDigest` audit-only and compare tags for upgrade availability.**
Rejected for the reason 0012 gave and the incident confirmed: dev rebuilds reuse
tags, so tag comparison reports "up to date" exactly when it matters most.

## Consequences

- ADR 0012's follow-on is closed. `RuntimeLayer.Image` resolves to one verified
  image, so pool-agent's OCI-label snapshot no longer rests on an assumption
  that was not yet true.
- A stale sandbox is now a first-class, visible state rather than a 500 from a
  missing socket four layers down.
- `disco box list` can mark upgradable sandboxes; `disco box upgrade <id>`
  applies one. Both must state that container-local writes are lost.
- Upgrading is disruptive by construction: the harness process dies with its
  container. Attached clients see the stream close and must reattach.
- An in-flight upgrade is an operation like any other, so the existing phase,
  `lastOperationStatus`, and project event-stream machinery carry it unchanged.
- The worker API's sandbox create request grows an image digest field, since the
  pin has to reach the runtime that enforces it (§6). Control plane, pool-agent,
  and the generated worker API model change together.
- A sandbox whose pinned image was garbage-collected off its pool host, with a
  tag that has since moved, now fails to start where it previously started
  something else. That is the intended trade, and the error names both digests
  so the fix — upgrade — is obvious from the message.
- Sandboxes created before this lands have no pinned digest. They report no
  upgrade until something re-pins them — acceptable, since the fleet is
  short-lived and a stopped sandbox re-pins on its next start.

## Deferred

- **Agent contract version reporting.** Revisit if skew is observed where the
  image digest matched but the agent inside was incompatible — the case digest
  comparison provably cannot catch.
- **A project- or pool-level `upgradePolicy` field.** Revisit once in-place
  upgrade has run for a release cycle without data-loss reports.
- **Batch upgrade** (a project-wide "upgrade all"). Revisit when a fleet is
  large enough that per-sandbox action is the bottleneck.
