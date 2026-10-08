# Sandbox Config Design

`sandboxconfig` assembles a sandbox's effective runtime configuration from
three attribute-owned layers, per
`docs/adr/0012-sandbox-config-is-three-attribute-owned-layers.md`. It is a
hand-written root-module library, not OpenAPI-generated: `sandbox.json` is an
internal contract between pool-agent and sandbox-agent, not a REST schema.

## Layers

Pool-agent assembles the `Document` (`buildSandboxDocument` in
`pool-agent/sandboxruntime`) from the pool create request, which carries the
control plane's resolved harness config.

- `RuntimeLayer`: control-plane/pool-agent-owned identity (`SandboxID`,
  `Provider` — which also carries the public keys the sandbox trusts and, as
  `Pool`, where its pool serves it), sandbox-agent daemon settings
  (`AgentRuntime`), sources,
  model/prompt/user/git, the create-time `Description` (only a seed for the
  sandbox's meta file, ADR 0136), `HarnessMode`, and per-sandbox env/files. `Env`
  includes pool-agent's proxy-trust env, and `ProxyEnvs` names those keys for
  sandbox-agent's runc wrapper. `Files` is the harness config's configured-file
  overlay. Each `Source` carries its ownership (`UID`/`GID`, absent when the
  pool cannot know them), the `BaseCommit`/`UpstreamRef` the agent's diff stat
  measures from, `AwaitsDelivery` (see Readiness), and how the sandbox agent
  checks it out when it clones it (ADR 0126 §4): `RefName`/`RefType`,
  `UpstreamURL`, and a dirty `Workspace` snapshot. `SourceMaterializedMarker`
  is the file inside a checkout's `.git` that says it has been materialized
  once; the sandbox agent, which materializes every source, reads it first and
  writes it last (a pool wrote an empty one, before the sandbox cloned its own). `Git` is authorship,
  never run identity — a separate field precisely because `User` is shared with
  `exec create`, where a committer has no meaning
  ([ADR 0042](../docs/adr/0042-git-authorship-identity-is-a-first-class-sandbox-property.md));
  `GitIdentity.Configured` is the one "was any given" test.
  `User` (an alias of `sandboxuser.User`) records the
  request verbatim — every field optional, names unresolved, a wholly empty
  `User` meaning the image's own account — because only the sandbox can resolve
  it ([ADR 0025](../docs/adr/0025-the-sandbox-user-is-one-contract-resolved-inside-the-sandbox.md)). Its `Image` is the
  resolved image identity the pool host launched, not the mutable reference it
  was asked for; `Effective` drops it, so it survives only in `_provenance` as
  the record of what a sandbox actually ran (ADR 0016).
- `ImageLayer`: the harness contract (ID, name, description, run/relaunch/config
  commands) and defaults (files, volumes, env, additional groups, secret
  declarations) snapshotted from the registered image's OCI manifest labels —
  its own `harness.ImageLabel` merged over every layer it inherited from the
  images it was built `FROM` (ADR 0086 §2). The control plane snapshots it at
  harness registration and re-snapshots it when a re-inspection finds the
  digest moved — on every server start for built-in harnesses, on an explicit
  refresh for registered ones — because a stable tag is rebuilt in place, so
  registration is not a one-time read (ADR 0016).
- `ProjectLayer`: the resolved source repository's contribution
  (`.discobox/project.json` in the primary source), read by pool-agent at the
  commit it clones. Optional — nil when the project supplies nothing. A source
  the client delivers by push is empty at that moment, so pool-agent reads it
  again when the push lands and rebuilds the container if it differs from the
  project layer recorded in `_provenance`
  ([ADR 0055](../docs/adr/0055-a-delivered-source-settles-before-its-sandbox-runs.md)).

Each layer type lists only the attributes that layer may set. Where two
layers legitimately contribute to the same named attribute (e.g.
`RunCommand`, image-owned but project-overridable), both layers declare that
field independently — there is no shared domain object embedded in more than
one layer.

## Static and dynamic

`sandbox.json` is the sandbox's **static** half, its bootstrap: placed before
the agent exists and never rewritten, it holds identity, public keys to trust
(`ControlPlanePublicKeyName`, `PoolPublicKeyName`), where the pool is
(`PoolEndpoints`), and the create-time config — and no private key, secret or
idle timeout. Everything that can change while the sandbox exists is its
**dynamic** half, the `RuntimeConfig` below. Where the sandbox listens for its
pool (`SandboxEgressListenAddress` and its siblings) is neither: it is the
sandbox's own, a constant here so the pool can make the two settings that must
agree with it — the proxy env and the DNS server a runtime hands the sandbox.
See [ADR 26-10-08-127](../docs/adr/26-10-08-127-a-sandboxs-bootstrap-is-static-and-the-intake-carries-the-rest.md).

## `Effective`

`Effective(Document) (Config, Provenance)` is the one merge function, and
pool-agent is its only runtime caller: it runs whenever pool-agent writes a
sandbox's container — at creation, on a spec change such as an image upgrade,
and on the rebuild a delivered source's project layer forces. Merge rules by
field category:

- **Single-writer**: copy straight from the one layer whose type has the
  field. The image's harness fields land in the nested `Config.Harness`.
- **Override-grant** (`RunCommand`, `RelaunchCommand`): image's value,
  replaced wholesale by the project's if non-empty.
- **Overlay-by-key** (`Files`): image and runtime entries merge by `path`
  (later entry wins); `ProjectLayer.FilesAdd` appends new paths only, never
  overriding an existing one.
- **Additive-default** (`Env`): image fills only the keys runtime did not
  set.

`Config` (`apiVersion: discobox.dev/sandbox/v1`) is the flat shape
sandbox-agent decodes from `sandbox.json` in the read-only config volume at
`SandboxConfigDir` (`/etc/discobox`) — no further merging happens at boot.
`Provenance` carries the raw per-layer inputs for the diagnostic `_provenance`
sibling key; no runtime component in the sandbox decodes it (pool-agent reads
back its project layer to detect a change) and its shape may change freely.

`Config.SandboxGroups` is the one answer for the sandbox user's supplementary
groups: a request `User.AdditionalGroups` replaces the image's
`AdditionalGroups` entirely, and naming none inherits them (ADR 0025 §2). Boot
and the exec defaults both read it.

`Config.WorkingRoot` is the one answer for the directory a sandbox works in:
the manifest's, or its platform's default (`sandboxpath.Paths.WorkingRoot`:
`/workspace` on Linux and macOS, `C:\workspace` on Windows) when it names none.
Pool-agent writes the field and places sources under the same default; boot
creates that directory and gives it to the sandbox user; sandbox-agent starts
execs there; the CLI derives the source destinations it asks for from it. Two of them
disagreeing means a sandbox working in a directory boot never chowned, or beside
a checkout it cannot see — and the CLI is the one server-side defaults cannot
correct, since the destination it names is explicit in the create request.

## Local subnets token

`LocalSubnetsToken` (`%LOCAL_SUBNETS%`) is an opaque placeholder pool-agent
writes into env values that must name the sandbox's own networks — in practice
`NO_PROXY`/`no_proxy`. Pool-agent decides *where* local subnets belong;
sandbox-agent decides *what* they are and substitutes them with
`ResolveLocalSubnetsToken` wherever that env is consumed (execs, `proxyenv`,
the `runcca` runc wrapper).

## Readiness

`Source.AwaitsDelivery` marks a source whose content is not in place when the
container is created — a push-delivered one, still being sent by the client.
The file `SourcesReadyFileName` (`ready`, seen in the sandbox as
`SourcesReadyPath`, `/etc/discobox/ready`) is the signal that every source is
materialized *and* the bootstrap beside it is final; the sandbox agent writes
it when an applied runtime-config document grants readiness (`SourcesDelivered`).
The sandbox holds its first harness launch and the start of repository-declared
services on it, so nothing runs against an empty workspace, a configuration
about to be replaced, or before its secrets and proxy credential have arrived.

The signal is deliberately not the per-source materialized marker the sandbox
could read for itself: that marker is written when a checkout completes, which
is before pool-agent has re-read the project layer and decided whether the
container must be rebuilt to honor it. Only pool-agent can say the sandbox has
settled. A sandbox a pool delivers to (`Provider.AwaitsRuntimeConfig`, a pool
key named) waits for its first document whatever its sources are; one with no
pool key and no source awaiting delivery (`Config.AwaitsSourceDelivery` false)
waits on nothing.
See [ADR 0055](../docs/adr/0055-a-delivered-source-settles-before-its-sandbox-runs.md).

## Secrets

Secret values (resolved sentinels) are excluded from `Document` entirely —
see `docs/adr/0012-sandbox-config-is-three-attribute-owned-layers.md` §3.
They travel in the runtime-config document's `SecretEnv` (sentinels only), which
the sandbox agent writes to `/run/discobox/secrets/secrets.json`. Only the
harness's secret declarations ride the image layer (`ImageLayer.Secrets`), and
the sandbox reads them for `Delivery` alone: a file-delivered secret must not
also be exported as an env var.

## Runtime config

`RuntimeConfig` (`runtimeconfig.go`) is the pool's whole view of a *running*
sandbox, delivered as one revisioned document to the sandbox agent's
`PUT .../runtime-config` and read back with `GET`
([ADR 0126](../docs/adr/0126-a-sandbox-does-not-share-a-host-or-a-filesystem-with-its-pool.md)
§3). It is what replaces staging files into a sandbox's volumes after create;
`sandbox.json` and its layers above remain the create-time placement.

- **Contents.** `Agent` — the sandbox-agent settings the pool may change
  (`idleTimeout`, applied to the running agent when delivered). `SecretEnv` —
  env name to sentinel, never a resolved value (the Secrets rule above is
  unchanged). `Proxy` — the CAs, the sandbox's client keypair (the key is
  delivered, never read back) and the registry namespace: the credential only.
  Where the pool is comes from the bootstrap and where the sandbox listens is
  its own, so the document carries no bridge; the sandbox renders its bridge
  configs from the three. `Sources` — per source, its in-sandbox `Target`
  (required with an origin), `OriginURL`, `OriginToken` (the pool's
  `origin:fetch` sandbox token, when the origin takes one), pinned `Commit` and
  `Delivered`. The agent clones each from its origin
  ([`sandbox-agent/sourceconverge`](../sandbox-agent/DESIGN.md)); how it is
  checked out stays in `sandbox.json`'s `Source` of the same slug.
- **Whole, not incremental** (ADR 0017): absent means the sandbox no longer has
  it. One revision names one document: newer applies, older is ignored, the same
  revision with different content is a conflict (`SameDocument`).
- **Readiness** is `SourcesDelivered`: every source `Delivered`, vacuously true
  with none. The pool sets `Delivered` only once it has settled the spec on that
  source, so it means what the readiness file always meant (ADR 0055).
- **`Validate` is the whole refusal.** Everything a delivery can be wrong about —
  revision, durations, env names, PEM CAs, a keypair that does not load, the
  namespace, a source target that is not a clean absolute path by the sandbox's own platform's rules (`sandboxpath`, which the caller passes: the agent passes its own, ADR 0145 §6), an
  origin with no target, a token that is not one line — is checked before
  anything is written, so a refused document changes nothing.
- **Who may deliver one.** `RuntimeConfigScope`, signed by the pool with the key
  the bootstrap names under `PoolPublicKeyName`; no wildcard grants it.
- The wire schema is `SandboxRuntimeConfig` in `api/openapi/server.yaml`; the
  sandbox agent converts the generated type to this one through JSON, and its
  round-trip test fails when the two drift. The agent's apply side is
  [`sandbox-agent/intake`](../sandbox-agent/DESIGN.md).

