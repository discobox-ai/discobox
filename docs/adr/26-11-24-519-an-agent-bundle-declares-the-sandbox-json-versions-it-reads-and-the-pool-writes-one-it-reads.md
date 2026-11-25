# 26-11-24-519 — An agent bundle declares the sandbox.json versions it reads, and the pool writes one it reads

- **Status**: Accepted
- **Date**: 2026-11-24
- Accepted by the owner by merging #1654, 2026-11-25
- **Issue**: #1409
- **Relates to**: [0012](0012-sandbox-config-is-three-attribute-owned-layers.md)
  (`sandbox.json` and its `apiVersion`),
  [0016](0016-sandbox-image-upgrades-are-explicit-and-in-place.md) (deferred
  an agent contract version that the agent reports at run time; this is not
  that, see Alternatives),
  [0082](0082-a-stopped-sandbox-tracks-its-harness-image.md) (a project's
  `manual` upgrade policy),
  [26-10-24-953](26-10-24-953-every-box-is-provisioned-from-an-ordered-list-of-bundles-and-a-pool-is-a-box.md)
  §2 (the agent bundle, placed by the pool),
  [26-10-16-423](26-10-16-423-a-harness-is-a-provisioning-bundle-and-the-agent-provisions-any-box.md)
  §7 (pins that move restart a delivered box in place)

## Context

\#250 moved both of Discobox's `apiVersion` identifiers from `discobox.dev`, a
namespace Discobox does not own, to `discobox.ai`. Readers accept both. One
writer still writes the old value. Checked against `main` at `e6f5ea446`:

- **The writer.** `sandboxconfig.Effective` writes `LegacyAPIVersion`
  (`discobox.dev/sandbox/v1`, `pkg/sandboxconfig/effective.go:101`). A pool
  agent writes `sandbox.json` for whichever sandbox agent the box is pinned
  to, and that agent can be older than the pool agent. An agent from before
  \#250 refuses any other value (`sandbox-agent/config/config.go:166`, and
  `pool-agent/REVIEW.md`'s rule that a strictly checked value changes on the
  writer only once every agent the pool may write for reads the new one).
  The dual reader first shipped in `v0.20.0-rc.1`. No stable release has it.
- **What the pool knows about the agent.** The create request pins the
  agent as a bundle name and an opaque digest (`server/internal/model/model.go:926`,
  `pool-agent/assetcache/record.go`). It carries no version. The pool
  already resolves that bundle's manifest for its own platform, because it
  places the bundle's one layer itself (`assetcache.Cache.AgentLayer`,
  `pool-agent/assetcache/cache.go:211`). The agent bundle is
  `FROM scratch` with one `COPY` and no labels (`sandbox-agent/Dockerfile`,
  target `agent`). The agent's status capabilities
  (`pool-agent/statuspoll.go`) are too late to help: `sandbox.json` is read
  at boot, before the agent reports anything.
- **Pins that never move.** A stopped box is re-pinned automatically only
  when its project's policy is not `manual`
  (`server/internal/resources/sandboxes/service.go`, `upgradeStoppedSandboxes`).
  A box on `manual` keeps an agent from before \#250 indefinitely, and so
  does a box from before bundles, whose delivery names no agent and whose
  container keeps the agent placed in it then (`Delivery.PlacesAgent`).
- **When the file is written.** On the Docker path `sandbox.json` is written
  only when a container is built (`writeSandboxHarnessConfig`,
  `pool-agent/sandboxruntime/runtime.go:791`). A box whose pins moved and
  whose base did not is restarted in place and keeps its file
  (`runtime.go:638`, `movedPins`). The machine path renders `sandbox.json`
  at every placement (`MachineSandboxRuntime.place`,
  `pool-agent/sandboxruntime/machine.go:1207`, `renderBootstrap`).
- **Image manifests.** Every manifest Discobox ships declares
  `discobox.ai/image/v1`. The server still reads a layer that declares
  `discobox.dev/image/v1` (`LegacyImageAPIVersion`,
  `harness/imagelayer.go:190`). Bundles already in registries carry it. A
  third party's bundle built `FROM` one of them inherits its label. A legacy
  layer is read as it stands and never rewritten, so a config snapshotted
  from it keeps its manifest digest (`harness/DESIGN.md`).

The issue asks when the writer can move, and when the two legacy reads can
end. A rule based on age, such as "the writer moves once the oldest agent a
pool may write for reads the new value", has no end date here: a `manual`
box can hold a pre-#250 agent for as long as it exists.

## Decision

### 1. The agent bundle declares which `sandbox.json` versions it reads

The agent bundle's image config carries one label,
`io.discobox.agent.v1.sandbox-config`, whose value is a JSON array of the
`sandbox.json` `apiVersion` values that agent accepts. It is set in
`sandbox-agent/Dockerfile`'s `agent` target from the same constants the
agent checks (`sandboxconfig.APIVersion`, `sandboxconfig.LegacyAPIVersion`),
and a test holds the label and `unmarshalConfig`'s accepted set equal. The
bundle stays one layer: a label is config, not a layer, so
`agentlayer.ExtractAgent` and `ErrNoAgentLayer` are unaffected.

The label describes the artifact the pool actually places. That is the
difference from a version number: a digest can come from a release, a main
build, a provider's pinned or override bundle, or a local dev build, and only
the bundle itself can say what it reads.

### 2. The pool writes the newest value the pinned agent declares, or the legacy one

Whenever a pool agent renders `sandbox.json`, it chooses `apiVersion` from the
agent bundle pinned by the request it is acting on:

- If the bundle's label lists `APIVersion`, it writes `APIVersion`.
- If the bundle has no label, it writes `LegacyAPIVersion`. That covers every
  agent built before this change, and every agent from before \#250, which
  reads nothing else.
- If the delivery names no agent (a box from before bundles), it writes
  `LegacyAPIVersion`.
- A label that lists neither value is refused, as an agent bundle that is not
  one layer is refused (`ErrNoAgentLayer`). The pool never guesses a value
  the agent might refuse.

`Effective` stops choosing the value. The caller passes it in, so the one
place that decides is the pool, which holds the bundle. The pool reads the
label from the config blob of the platform manifest that `AgentLayer`
already resolves, through the same asset cache and the same per-sandbox pin
scope. An error reading it fails the render, as an unreadable agent layer
does.

### 3. A moved agent pin that cannot read the file's value rebuilds the container

On the Docker path, the pool records beside the sandbox's tree (not inside
its volumes, which the sandbox can write) the `apiVersion` it last wrote into
`sandbox.json`. Pins move only through a create. When a create finds the pins
moved and the base unchanged (`movedPins`), it checks the new agent's
declared set against that record, whether the box is running or stopped. If
the new agent does not accept the recorded value, the move is a reason to
rebuild the container, which writes a fresh `sandbox.json` (§2) and keeps the
box's power state (0021). Otherwise the create goes on as it does today: a
running box restarts in place, and a stopped one boots on the new pins at its
next start. A box with no record, from before this change, holds
`LegacyAPIVersion`, which every agent built so far accepts.

This covers a pin that moves *backwards*: a provider's override pointing at
an unlabelled bundle, for a box whose file says `discobox.ai/sandbox/v1`.
The machine path needs nothing more, because it renders at every placement
(§2).

### 4. The sandbox agent keeps reading `discobox.dev/sandbox/v1`

`LegacyAPIVersion` stays, on both sides, for these recorded reasons:

- **Writer.** It is what §2 writes for unlabelled agents and for boxes from
  before bundles. Those exist for as long as `manual` boxes do.
- **Reader.** A Docker box keeps the file it was built with across in-place
  pin moves (§3 only rebuilds when the new agent *cannot* read it), so a box
  built today holds a legacy file under every later agent. A pool agent from
  before this change also writes the legacy value for every agent, and pools
  can lag their server (a provider's pinned pool bundle). An agent that
  dropped the legacy value would fail to boot in both cases. The reader costs
  one `case` label.

Revisit when `sandbox.json` gets a `v2`. At that point §1's label is how a
pool learns whether an agent reads `v2`, and the `v1` readers, both
namespaces included, are judged together.

### 5. The server keeps reading `discobox.dev/image/v1`

`LegacyImageAPIVersion` stays, read-only, for the reason `harness/DESIGN.md`
already gives. Bundles in registries carry it, third-party bundles built
`FROM` them inherit it, and a legacy layer is never rewritten, so that a
snapshotted config keeps its manifest digest. Nothing writes it, and nothing
in this ADR changes that. Revisit only with an `image/v2`.

## Consequences

- Every box whose agent was built after this change gets
  `discobox.ai/sandbox/v1`. No box that boots today stops booting. Boxes
  pinned to older agents keep getting the value those agents read.
- A pool agent gains one small fetch per agent bundle digest, the config
  blob, which the asset cache already serves and keeps.
- The next strict change to `sandbox.json` has a mechanism ready: the
  pool reads what the pinned agent declares, rather than waiting for an
  age-based floor that `manual` boxes never reach.
- The issue's "Done when" is met by the writer moving for new agents, and by
  both legacy constants staying with their reasons recorded (§§4–5), not by
  their removal.
- `pkg/sandboxconfig/DESIGN.md`, `pool-agent/REVIEW.md`'s `sandbox.json` rule
  and `sandbox-agent/DESIGN.md` are updated with the code to describe the
  label and the choice.

## Open for implementation

- Exactly where the Docker path keeps the §3 record. It may extend an existing
  per-sandbox record beside the tree, such as the one `recordAgentTLS`
  writes, or be a file of its own.
- Whether the label is read once per digest and memoised, as manifests are
  (`Cache.manifests`), or each time.

## Amendments

Amended from its implementation's review, before anything shipped against it.
§2 said an error reading the label fails the render. The pool now reads a
bundle's config only for an agent layer new to the sandbox: beside the tree,
`sandbox-config.json` records the value the file holds, an agent layer known
to read it, and the newest that layer reads, so a routine create or start
asks no registry. A sandbox whose file was written before that record, and
whose bundle can no longer be read, gets the legacy value again when its layer
is the one last placed in it: that agent booted on the legacy file. Without
this, a box on `manual` pinned to a main build that its registry has pruned
(after fourteen days) would stop booting at its next create or start, which
the Consequences rule out. Any other unreadable bundle still fails the render.

## Alternatives rejected

- **A written skew floor** ("a pool agent writes for agents from the
  previous stable release on", flipping the writer after v0.20.0 is stable).
  A `manual` box, or a box from before bundles, keeps a pre-#250 agent past
  any floor. Under a floor, that box would stop booting, or `manual` would
  have to stop meaning manual and force the upgrade. Both break the issue's
  "no sandbox that was bootable before stops booting".
- **The server names the value on each create** (a new pool API field). The
  server knows the agent only by the same name and digest the pool does, so
  it would have to read the bundle's label itself, or map digests to
  releases, which main builds, overrides and dev bundles defeat. The pool API
  schemas are `additionalProperties: false`, so the field would also need a
  capability gate (`pool-agent/REVIEW.md`), and pools that lag the server
  would get it last. That is more parts than the pool reading the bundle it
  already holds.
- **A run-time agent contract version** (0016's deferred option). The agent
  reports after it boots, and `sandbox.json` is what it boots on.
- **Rewrite `sandbox.json` at every Docker start**, then drop the legacy
  reader. That changes the Docker bootstrap from create-time-only, where
  everything after creation goes through the runtime-config intake (pool
  `DESIGN.md`, Bootstrap), to save one `case` label. It still would not cover
  pools older than the change.
- **Change nothing** (write legacy and read both, forever). That is safe
  today, but it leaves `pool-agent/REVIEW.md`'s rule with no way to be met
  for `manual` boxes the next time a strictly checked value changes.
