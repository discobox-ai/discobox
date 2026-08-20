# 0012 — Sandbox config is three attribute-owned layers, merged by a shared library

- **Status**: Accepted
- **Date**: 2026-07-23

## Context

The sandbox's effective configuration is assembled from multiple sources today,
and each pairing of sources has grown its own, different resolution rule:

- **`sandbox.json`** (`model.SandboxManifest`, authored by pool-agent from the
  control plane's resolved `SandboxConfig`) carries identity, resources,
  sources, prompt/model, and a `resolvedHarnessConfig` overlay
  (`api/model/sandbox_manifest_gen.go`).
- **`image.json`** (baked into the image, `config.ImageConfig`,
  `sandbox-agent/config/image.go`) carries volumes, image env defaults, and the
  immutable harness contract (`harness.Image`): commands, files, secrets
  declarations.
- The same `harness.Image` value is also projected into the OCI label
  `io.discobox.harness.v1` (`harness/driver.go:18-20`) so the control plane can
  snapshot harness metadata at registration time
  (`server/internal/resources/harnessconfigs/image.go`) without pulling the
  image's filesystem layers.
- **`.discobox/{harness.json,harness-config.json,sandbox.json}`** in the
  checked-out project repo can rename or replace the resolved harness
  (`sandbox-agent/terminal/service.go:429-459,486-578`).
- **`DISCOBOX_*` environment variables** override scalar fields of the loaded
  manifest as a second, parallel path (`sandbox-agent/config/config.go:241-255`).

Three problems fall out of this:

1. **Precedence is ad hoc and non-monotonic.** The image wins over the manifest
   for harness commands, the manifest wins over the image for harness
   id/name, and the project repo wins over both in the fallback path — encoded
   as a chain of `if empty, try next` calls in `terminal.Service`, not as a
   declared rule. A new source (or a new field) means finding the right spot in
   that chain by inspection.
2. **The same domain object is duplicated across sources with different write
   grants**, and the duplication is invisible in the type system. `harness.Image`
   is reused verbatim for the image file, the OCI label, and (in shape, via
   `SandboxManifestResolvedHarnessConfig`) the manifest overlay. Nothing stops a
   future manifest field from silently acquiring the same name as an
   image-owned field and being merged by accident.
3. **`DISCOBOX_*` env vars are a second, silent writer** for fields the manifest
   already sets (identity, paths, listen address), with no visibility into
   which one actually took effect at boot.
4. **Secret values are not distinguished from ordinary config.** A resolved
   sentinel (`server/internal/resources/sandboxes/secrets.go:22,51-55,414-423`)
   is written straight into the same `env` map as ordinary environment
   variables, inside the same document as everything else. It is
   format-shaped to look like a real credential
   (`docs/adr/0009-previous-configure-secrets-are-prefixed-sentinels.md`), so
   generic secret scanners flag it, and — being a live capability the proxy
   will swap for the real value on egress
   (`proxy/internal/secrets/secrets.go:154-250`) — it is sensitive in a way
   the rest of the document is not, but there is nowhere in the current schema
   to say so.

## Decision

**The sandbox's configuration is one document, `Document{Runtime, Image,
Project}`, whose three fields are independently-typed layers. Each layer type
lists only the attributes that layer may set — not a mirrored domain object
per layer. Pool-agent is the sole assembler: it computes `Effective(Document)`
once, at sandbox creation, and writes the result to a single file that the
sandbox mounts read-only. Sandbox-agent never merges anything; it reads one
static file plus the independently-dynamic secrets file. `image.json` is
deleted — the OCI label becomes the only carrier of image-owned data, for
both the control plane and pool-agent. `DISCOBOX_*` env var overrides are
removed. Secret values and secret bindings are excluded from `Document`
entirely.**

### 1. Layers are attribute grants, not domain objects

`RuntimeLayer`, `ImageLayer`, and `ProjectLayer` are three distinct Go types.
A field appears on a layer's type if and only if that layer may set it.
Where two layers legitimately contribute to the same named attribute (for
example `runCommand`, owned by the image but overridable by the project), both
layers declare that field independently, at the type level — there is no
shared `Harness` struct embedded in more than one layer. This is deliberate:
modeling "the harness" as one object that every layer either includes or
omits forces an all-or-nothing grant. Modeling it as separate attributes
(`RunCommand`, `Secrets`, `ConfigCommand`, ...) lets each layer's grant be
exactly as narrow as it should be — e.g. the project layer gets `RunCommand`
and `RelaunchCommand` but not `Secrets`, `Files`, or `ConfigCommand`, because
those fields simply do not exist on `ProjectLayer`.

Illustrative shape (not final field-for-field):

```go
type RuntimeLayer struct {
    SandboxID, Image string           // Image here is the resolved digest ref
    Provider         Provider
    AgentRuntime     AgentRuntime
    Resources        Resources
    Sources          []Source
    Model            Model
    Prompt           []string
    User             User
    Env              map[string]string
    HarnessMode      string           // selection, not capability
    Files            []File           // overlay onto image files, by path
}

type ImageLayer struct {
    HarnessID, HarnessName, HarnessDescription string
    RunCommand, RelaunchCommand, ConfigCommand []string
    Files   []File
    Volumes []Volume
    Env     map[string]string
    // No Secrets field: see decision 3.
}

type ProjectLayer struct {
    RunCommand, RelaunchCommand []string // override grant only
    WorkingDirectorySubpath     string
    FilesAdd                    []File   // append-only, path-namespaced
}

type Document struct {
    Runtime RuntimeLayer
    Image   ImageLayer
    Project ProjectLayer
}
```

### 2. One shared library is the merge behavior — and pool-agent is its only runtime caller

`Effective(doc Document) Config` lives in the root module
(`github.com/discobox-ai/discobox`) as a well-tested library, but it has
exactly one caller at runtime: pool-agent, once, when it resolves a sandbox's
configuration at source-clone time. Sandbox-agent does not import it and does
not merge anything — by the time a container boots, the merge has already
happened and the result is sitting in a file. The control plane does not call
it either; it only needs the image label for harness registration/validation
(section 6) and hands its resolved `SandboxConfig` down as pool-agent's
`RuntimeLayer` input.

This is a deliberate narrowing from "shared by every consumer" to "computed
once by the one component that owns doing so." The earlier problems this ADR
opens with — non-monotonic precedence, ad hoc fallback chains in
`terminal.Service` — existed because resolution happened piecemeal, at
multiple points in time, by multiple components. Collapsing resolution to one
place, one time, removes the class of bug, not just the symptom.

Each field's merge rule is a few lines of Go, not a lookup against a
generic ownership table:

- **Single-writer** fields (`Resources`, `Sources`, `Secrets`, `Volumes`, ...)
  copy straight from their one owning layer; no other layer's type has the
  field, so there is nothing to accidentally merge.
- **Override-grant** fields (`RunCommand`, `RelaunchCommand`) take the image's
  value, replaced wholesale by the project's value if non-empty.
- **Overlay-by-key** fields (`Files`) merge image and runtime entries by
  `path`; a later entry with a matching path replaces, a new path appends.
- **Additive-default** fields (`Env`) start from the image's map, then runtime
  entries fill in and override — image contributes only keys runtime did not
  set.

Code, not a declarative table, is the behavioral spec: merge rules are
expressed and tested the same way as any other Go logic, and a change to
precedence is a diff to one function with its own test coverage, not a
scattered set of `if empty` checks across `terminal.Service`.

### 3. Secrets — both declaration and value — are excluded from `Document`

Neither the `Secrets []Secret` declaration nor resolved sentinel values live
in `Document`, on any layer:

- **Declaration** (`{envName, required}`) stays where it already effectively
  lives today: server-side, read from `image.json`/the OCI label at harness
  registration time, for validation
  (`server/internal/resources/harnessconfigs/image.go`,
  `validateImageHarness`). Sandbox-agent does not need it — it only needs to
  know what to inject at exec time, which the mechanism below provides
  directly.
- **Values** (resolved sentinels) are delivered via a separate file, keyed
  outside the `Document` type entirely:
  `/run/discobox/secrets/secrets.json`, mode `0600`, owned `root:root`.
  Sandbox-agent runs as root and is the only reader; the harness process
  (running as the unprivileged sandbox user) never gets filesystem access to
  it. The file's contents reach the harness process the same way they do
  today — as env vars set by sandbox-agent at `exec()` — but the file itself
  is written and refreshed independently of `Document`, on its own schedule
  (grant approval, rotation, OAuth refresh via
  `docs/adr/0011-oauth-secrets-refresh-server-side-on-resolve.md`), mirroring
  the fsnotify-watched pattern the pool-agent's proxy already uses for its own
  `secrets.json` (`pool-agent/proxyagent/secrets.go`).

This keeps `Document` fully static once assembled for a sandbox: safe to hash,
diff, cache, and log without redaction, and free of anything a generic secret
scanner should flag. Sandbox-agent's boot-time env for the harness process is
the union of `Effective(doc).Env` (static) and the current contents of the
secrets file (dynamic), computed once at each exec, not folded into one
document with two different lifecycles.

### 4. `DISCOBOX_*` env var overrides are removed

`applyEnv` (`sandbox-agent/config/config.go:241-255`) and the corresponding
env vars are deleted. The manifest file is the only source for every field
they used to override. A dev/debug session without a manifest is out of
scope for this decision; if one is needed later, it should construct a real
(if minimal) `Document`, not reintroduce a second override channel.

### 5. `resources` duplication within the manifest is collapsed

`resources.cpuCores/memoryMb/diskMb/timeoutSeconds` and
`config.cpuVcpus/memoryBytes/storageBytes` are today two representations of
the same values inside the same manifest. `RuntimeLayer.Resources` becomes the
single representation.

### 6. `image.json` is deleted; the OCI label becomes the sole image-metadata carrier

Sandbox-agent no longer reads any file baked into the image. Everything
`image.json` carried today — `env`, `volumes`, and the harness contract — is
read once by pool-agent from the OCI label at resolution time (pool-agent
receives it via the server, which already inspects the label at harness
registration; see `server/internal/resources/harnessconfigs/image.go`) and
folded into `RuntimeLayer`/`ImageLayer` before `Effective()` runs. Because the
label's scope grows from "harness only" to the full former `image.json`
payload, it is renamed `io.discobox.image.v1` (was `io.discobox.harness.v1`)
to describe what it now actually carries. `/usr/share/discobox/image.json`,
`config.ImageConfig`, and `sandbox-agent/config/image.go` are deleted.

This also simplifies the boot sequence from ADR 0007: today, boot init reads
`image.json` in-container for `volumes` *before* the `/.discobox/config` bind
exposes the manifest, then separately reads the pre-bind manifest copy for
`sources`. With `image.json` gone, both `volumes` and `sources` are already
present in the one pre-bind manifest read — one file read where there were
two.

### 7. Project source is resolved once, at clone time, never read inside a running sandbox

Pool-agent reads `.discobox/project.json` (or equivalent) from the cloned
repo at the commit it resolves, at the moment it first materializes the
source — the same moment it computes `Effective()` and writes the resulting
file. The project layer's contribution is authoritative for that commit and
is baked into the written document; it is not re-read, and has no further
effect, once the sandbox is running. `sandbox-agent`'s in-container reader of
`.discobox/{harness.json,harness-config.json,sandbox.json}`
(`terminal.Service.localHarnessConfig`) is deleted along with the fallback
chain it fed.

A consequence worth stating plainly: if new commits are later pulled into an
already-running sandbox's source, the project layer's contribution already
baked into the written document does not update. Re-resolving it is a
deliberate operation (re-clone, or an explicit reconfigure), not something
that happens implicitly on next boot.

### 8. The written document is read-only, and carries a diagnostic `_provenance` alongside the effective config

Pool-agent writes the assembled result to the same host-backed path ADR 0007
already established (`.discobox/config/sandbox.json`, bind-mounted to
`/etc/discobox/sandbox.json`), but that bind is now explicitly `ro`. The
sandbox cannot alter its own configuration by any means, including editing
`.discobox/` files in its own source checkout post-boot — that path was never
read again in the first place (section 7), and now the file it would need to
tamper with can't be written from inside the container regardless.

The `Effective(Document)` fields sit directly at the document's top level —
`sandbox.json` *is* the effective config, not a wrapper around it — with a
single sibling key for diagnostics:

```json
{
  "apiVersion": "discobox.dev/sandbox/v1",
  "sandboxId": "...",
  "provider": { "...": "..." },
  "resources": { "...": "..." },
  "sources": [ "..." ],
  "harness": { "...": "..." },
  "...": "every other Effective(Document) field, flat, exactly as sandbox-agent's Config type decodes it",

  "_provenance": {
    "runtime": { "...": "raw RuntimeLayer, as pool-agent assembled it" },
    "image":   { "...": "raw ImageLayer, as read from the io.discobox.image.v1 label" },
    "project": { "...": "raw ProjectLayer, as read from .discobox/project.json at the resolved commit; omitted if the project supplied nothing" }
  }
}
```

`_provenance` is diagnostic only, leading underscore intentional (marks it as
not-a-real-value the same way sentinel-format fields are marked sensitive
elsewhere in this design — here signaling "informational, not authoritative"
rather than "sensitive"). `sandbox-agent`'s `Config` type has no field for it
— decoding a struct with named fields for every effective attribute simply
leaves the unknown `_provenance` key on the wire, ignored, the same way it
would ignore any other field it doesn't declare — so nothing in the runtime
path can come to depend on its shape. It exists so a human or an inspection
tool can open a live sandbox's `sandbox.json` and see which layer contributed
a given effective value, without cross-referencing pool-agent logs or
re-cloning the source. Its schema is free to change without a version bump.

Keeping the effective fields flat rather than nested under an `effective` key
also means `sandbox.json`'s shape doesn't change from what it is today
(a document whose top-level fields are the sandbox's config) — only its
*contents* change (fully pre-merged, no more `resolvedHarnessConfig` overlay
semantics), and the one addition is the sibling `_provenance` key.

### 9. `harnessMode` is fixed for the sandbox's lifetime

`run` and `config` mode sandboxes are different sandbox instances — `disco
configure` launches a dedicated config-mode sandbox rather than switching a
running one's mode. There is therefore no legitimate writer for
`sandbox.json` after pool-agent creates it: sandbox-agent reads the file
exactly once, at boot, and never needs to watch it for changes (unlike the
secrets file, which genuinely is live and must be watched).

## Alternatives rejected

**A flattened document with a declarative field-to-owner-layer table,
validated at runtime.** Considered first: one merged schema, with a table
saying which layer may set which field, checked when a layer's raw file is
parsed. Rejected in favor of per-layer typed structs because the table only
catches a violation at validation time, and only if someone remembers to keep
the table in sync with the schema. A layer whose Go type simply does not have
the field cannot violate the rule at all — the type system enforces the
ownership boundary instead of a side table describing it.

**One `Harness` struct shared across layers (mirroring `harness.Image` in
each layer that touches it).** The natural first design once the harness
became a named concept: give the image layer a `Harness` object, and let
runtime/project layers embed their own `Harness` (possibly with omitted
fields) to contribute overlays. Rejected because it reintroduces an
all-or-nothing grant at the object boundary: to let the project layer
override `runCommand`, it would need its own `Harness` object, which either
mirrors every field (reopening the door to overriding `secrets` or `files` by
accident) or requires a bespoke partial `Harness` shape per layer anyway — at
which point it is not one shared object, it is what the attribute-grant model
already is, with an extra layer of indirection. "Harness" remains a real
concept (most of its attributes originate in the image), but it is not a
schema boundary that other layers inherit wholesale.

**Keep `DISCOBOX_*` env var overrides.** Rejected outright per the decision to
have the manifest file be the sole source of truth for boot config — a silent
second writer for the same fields is exactly the ambiguity this ADR removes
elsewhere in the design; keeping it for identity/path fields alone would be
inconsistent with that goal.

**Keep secret sentinel values inline in `Document.Runtime.Env`, as today.**
Simplest, no new file, no new read path. Rejected for two reasons: sentinel
values change independently of everything else in the document (grant
approval, rotation, OAuth refresh), which fights the goal of `Document` being
a static, cacheable, freely-loggable artifact; and mixing format-shaped
fake credentials into a general-purpose config file is precisely what causes
generic secret scanners to flag the whole file, when only a narrow, isolable
part of it is actually sensitive-shaped.

**Move the secret declaration (`Secrets []Secret`, names/required only) into
`Document` alongside the values.** Considered, since it would let
sandbox-agent validate at boot that every declared secret has a binding.
Rejected: that validation is a registration-time server concern today
(`validateImageHarness`), not a boot-time sandbox-agent concern, and the
declaration is static per image digest — it does not need to travel in a
per-sandbox document at all. Adding it back would only reintroduce
secret-shaped vocabulary into `Document` for a check that already happens
earlier, in the right place.

**Own the secrets file by the sandbox user (`uid:gid` matching the harness
process) instead of `root:root`.** Would let the harness process read the
file directly instead of only receiving values via injected env. Rejected:
sandbox-agent already runs as root and is the only process that needs to read
the file; making it root-owned costs the harness process nothing (it gets the
same values via its exec-time env either way) while preventing any
unprivileged process in the container from tampering with the file to forge a
sentinel string.

## Consequences

- `api/model/sandbox_manifest_gen.go`, `sandbox-agent/config/config.go`,
  `harness/driver.go` are restructured around the three-layer `Document`
  type. `sandbox-agent/config/image.go` (`config.ImageConfig`) is deleted
  outright, not restructured — sandbox-agent no longer parses any
  image-baked file. `terminal.Service.resolveHarness`/`applyLocalHarnessConfig`
  and its fallback chain are deleted; harness selection is a fact read
  from `effective`, not resolved in-container.
- `Effective(Document) Config` lives in the root module, but is called only
  by pool-agent, at sandbox creation. One behavioral spec, one set of tests,
  and — because it runs once rather than being invoked by multiple
  components at multiple times — one moment where resolution can go wrong,
  not several.
- `sandbox-agent/config/config.go:241-255` (`applyEnv`) and the `DISCOBOX_*`
  env vars it reads are deleted.
- A new root-owned, `0600` file, `/run/discobox/secrets/secrets.json`, is
  introduced as the sole carrier of resolved secret sentinel values, watched
  and re-read independently of `sandbox.json`. Server-side code that today
  writes sentinels into `sandbox.Env`
  (`server/internal/resources/sandboxes/secrets.go`) writes to this channel
  instead.
- `resources.*` and `config.cpuVcpus/memoryBytes/storageBytes` collapse into
  one `RuntimeLayer.Resources` representation; callers reading the duplicate
  fields are updated.
- `/usr/share/discobox/image.json` is deleted from every harness image. The
  OCI label is renamed `io.discobox.harness.v1` → `io.discobox.image.v1` and
  its payload grows from `harness.Image` alone to the full former
  `image.json` shape (`{apiVersion, env, volumes, harness}`). Every harness
  Dockerfile's `LABEL` instruction and the `HARNESS_METADATA` build-arg
  plumbing (Taskfile, `discobox-docker-image-watch`, the test-build hook)
  are updated accordingly.
- The `/.discobox/config` → `/etc/discobox` bind from ADR 0007 becomes
  explicit `ro`. The written `sandbox.json`'s top-level fields are the
  effective config, unchanged in shape from today; it gains one new sibling
  key, diagnostic `_provenance` (raw per-layer inputs, unversioned, never
  decoded by any runtime component).
- `.discobox/{harness.json,harness-config.json,sandbox.json}` in a project
  repo becomes a pool-agent-time input only, read once at the resolved
  commit when the source is first materialized. It is never read inside a
  running sandbox; edits to it after that point have no effect on the
  sandbox without an explicit re-resolution (re-clone or reconfigure).
- **Follow-on, not resolved by this ADR:** `harnessConfig.Image` (the
  reference actually used to pull/run a sandbox,
  `server/internal/resources/sandboxes/service.go:172,184-185`) is a mutable
  tag today; `ImageDigest` is captured only as an audit snapshot and is never
  consulted by the pull/run path
  (`server/internal/sandbox/capabilities.go:9-11`,
  `server/internal/resources/sandboxes/reconciler.go:513`). This ADR's
  `RuntimeLayer.Image` assumes a digest-pinned reference, which is not yet
  true end-to-end; closing that gap (pinning `harnessConfig.Image` to
  `image@sha256:...` at registration, or re-resolving on every sandbox
  create) is required for the OCI-label snapshot to be trustworthy without
  re-inspection, and should be tracked as a companion change.
