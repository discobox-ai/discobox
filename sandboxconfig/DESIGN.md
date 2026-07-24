# Sandbox Config Design

`sandboxconfig` assembles a sandbox's effective runtime configuration from
three attribute-owned layers, per
`docs/adr/0012-sandbox-config-is-three-attribute-owned-layers.md`. It is a
hand-written root-module library, not OpenAPI-generated: `sandbox.json` is an
internal contract between pool-agent and sandbox-agent, not a REST schema.

## Layers

- `RuntimeLayer`: control-plane/pool-agent-owned identity, resources,
  sources, model/prompt/user, and per-sandbox env/files. Its `Image` is the
  resolved image identity the pool host launched, not the mutable reference it
  was asked for; `Effective` drops it, so it survives only in `_provenance` as
  the record of what a sandbox actually ran (ADR 0016).
- `ImageLayer`: the harness contract and defaults snapshotted from the
  registered image's OCI label (`harness.ImageLabel`) at harness registration
  time, and re-snapshotted whenever that image's digest moves — a stable tag is
  rebuilt in place, so registration is not a one-time read (ADR 0016).
- `ProjectLayer`: the resolved source repository's contribution, read once by
  pool-agent at the commit it clones. Optional — nil when the project
  supplies nothing.

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

Secret values (resolved sentinels) are excluded from `Document` entirely —
see `docs/adr/0012-sandbox-config-is-three-attribute-owned-layers.md` §3.
They travel through a separate, independently-refreshed channel to
`/run/discobox/secrets/secrets.json`.
