# Sandbox Config Design

`sandboxconfig` assembles a sandbox's effective runtime configuration from
three attribute-owned layers, per
`docs/adr/0012-sandbox-config-is-three-attribute-owned-layers.md`. It is a
hand-written root-module library, not OpenAPI-generated: `sandbox.json` is an
internal contract between pool-agent and sandbox-agent, not a REST schema.

## Layers

- `RuntimeLayer`: control-plane/pool-agent-owned identity, resources,
  sources, model/prompt/user, and per-sandbox env/files. `Git` is authorship,
  never run identity — a separate field precisely because `User` is shared with
  `exec create`, where a committer has no meaning
  ([ADR 0042](../docs/adr/0042-git-authorship-identity-is-a-first-class-sandbox-property.md)).
  `User` records the
  request verbatim — every field optional, names unresolved, a wholly empty
  `User` meaning the image's own account — because only the sandbox can resolve
  it ([ADR 0025](../docs/adr/0025-the-sandbox-user-is-one-contract-resolved-inside-the-sandbox.md)). Its `Image` is the
  resolved image identity the pool host launched, not the mutable reference it
  was asked for; `Effective` drops it, so it survives only in `_provenance` as
  the record of what a sandbox actually ran (ADR 0016).
- `ImageLayer`: the harness contract and defaults snapshotted from the
  registered image's OCI label (`harness.ImageLabel`) at harness registration
  time, and re-snapshotted whenever that image's digest moves — a stable tag is
  rebuilt in place, so registration is not a one-time read (ADR 0016).
- `ProjectLayer`: the resolved source repository's contribution, read once by
  pool-agent at the commit it clones. Optional — nil when the project
  supplies nothing. A source the client delivers by push is empty at that
  moment, so pool-agent reads it again when the push lands and rebuilds the
  container if the project declared anything
  ([ADR 0055](../docs/adr/0055-a-delivered-source-settles-before-its-sandbox-runs.md)).

Each layer type lists only the attributes that layer may set. Where two
layers legitimately contribute to the same named attribute (e.g.
`RunCommand`, image-owned but project-overridable), both layers declare that
field independently — there is no shared domain object embedded in more than
one layer.

## `Effective`

`Effective(Document) (Config, Provenance)` is the one merge function, called
exactly once per sandbox, by pool-agent, at sandbox creation. Merge rules by
field category:

- **Single-writer**: copy straight from the one layer whose type has the
  field.
- **Override-grant** (`RunCommand`, `RelaunchCommand`): image's value,
  replaced wholesale by the project's if non-empty.
- **Overlay-by-key** (`Files`): image and runtime entries merge by `path`
  (later entry wins); `ProjectLayer.FilesAdd` appends new paths only, never
  overriding an existing one.
- **Additive-default** (`Env`): image fills only the keys runtime did not
  set.

`Config` is the flat shape sandbox-agent decodes from `sandbox.json` — no
further merging happens at boot. `Provenance` carries the raw per-layer
inputs for the diagnostic `_provenance` sibling key; it is never decoded by
any runtime component and its shape may change freely.

## Readiness

`Source.AwaitsDelivery` marks a source whose content is not in place when the
container is created — a push-delivered one, still being sent by the client.
`SourcesReadyFileName` (`/etc/discobox/ready`) is pool-agent's signal that every
source is materialized *and* the document beside it is final; the sandbox holds
its first harness launch on it, so nothing runs against an empty workspace or a
configuration about to be replaced.

The signal is deliberately not the per-source materialized marker the sandbox
could read for itself: that marker is written when a checkout completes, which
is before pool-agent has re-read the project layer and decided whether the
container must be rebuilt to honor it. Only pool-agent can say the sandbox has
settled. A config that names no source awaiting delivery — every clone-delivered
sandbox, and every sandbox created before the field — waits on nothing.
See [ADR 0055](../docs/adr/0055-a-delivered-source-settles-before-its-sandbox-runs.md).

Secret values (resolved sentinels) are excluded from `Document` entirely —
see `docs/adr/0012-sandbox-config-is-three-attribute-owned-layers.md` §3.
They travel through a separate, independently-refreshed channel to
`/run/discobox/secrets/secrets.json`.
