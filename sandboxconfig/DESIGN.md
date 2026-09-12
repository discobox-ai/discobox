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
  `Provider`), sandbox-agent daemon settings (`AgentRuntime`), sources,
  model/prompt/user/git, `HarnessMode`, and per-sandbox env/files. `Env`
  includes pool-agent's proxy-trust env, and `ProxyEnvs` names those keys for
  sandbox-agent's runc wrapper. `Files` is the harness config's configured-file
  overlay. Each `Source` carries its ownership (`UID`/`GID`, absent when the
  pool cannot know them), the `BaseCommit`/`UpstreamRef` the agent's diff stat
  measures from, and `AwaitsDelivery` (see Readiness). `Git` is authorship,
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

`Config.WorkingRoot` is the one answer for the directory a sandbox works in,
`DefaultWorkingRoot` (`/workspace`) when the manifest names none. Pool-agent
writes the field and places sources under the same constant; boot creates that
directory and gives it to the sandbox user; sandbox-agent starts execs there;
the CLI derives the source destinations it asks for from it. Two of them
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
`SourcesReadyPath`, `/etc/discobox/ready`) is pool-agent's signal that every
source is materialized *and* the document beside it is final; the sandbox holds
its first harness launch and the start of repository-declared services on it,
so nothing runs against an empty workspace or a configuration about to be
replaced.

The signal is deliberately not the per-source materialized marker the sandbox
could read for itself: that marker is written when a checkout completes, which
is before pool-agent has re-read the project layer and decided whether the
container must be rebuilt to honor it. Only pool-agent can say the sandbox has
settled. A config that names no source awaiting delivery
(`Config.AwaitsSourceDelivery` false) — every clone-delivered sandbox — waits on
nothing.
See [ADR 0055](../docs/adr/0055-a-delivered-source-settles-before-its-sandbox-runs.md).

## Secrets

Secret values (resolved sentinels) are excluded from `Document` entirely —
see `docs/adr/0012-sandbox-config-is-three-attribute-owned-layers.md` §3.
They travel through a separate, independently-refreshed channel to
`/run/discobox/secrets/secrets.json`. Only the harness's secret declarations
ride the image layer (`ImageLayer.Secrets`), and the sandbox reads them for
`Delivery` alone: a file-delivered secret must not also be exported as an env
var.
