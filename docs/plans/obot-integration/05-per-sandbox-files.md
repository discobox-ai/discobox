# WI-05 — Per-sandbox runtime-layer files

**Goal:** let a caller supply files for one sandbox through the API, and have
them land in the sandbox's runtime config layer.

Read `00-CONTEXT.md` first. **Independent — can start now.** This is the
smallest and best-defined item in the programme: the destination already exists
and is already merged correctly; only the pipe to it is missing.

## Why

Obot delivers a hosted agent's startup document (`/etc/obot/agent.json`) and its
skill file trees (`/etc/obot/skills/<name>/...`) as ordinary sandbox files. It
does not copy files into a running sandbox; Discobox materializes them while
reconciling the sandbox. Discobox does not parse or understand any of it — the
content is opaque.

Files are also generally useful. Today the only way to get a file into a sandbox
is through a harness config, which is project-scoped and shared.

## Current state

The destination and its merge semantics are already built and tested:

- `sandboxconfig/document.go:57` — `RuntimeLayer.Files []File`, documented as
  "overlays onto the image's declared files, by path".
- `sandboxconfig/effective.go:96` and `mergeFiles` at `effective.go:146` —
  image and runtime entries merge by path (later wins), then `ProjectLayer.FilesAdd`
  appends new paths only. Covered by `TestEffective_FilesOverlayByPath`
  (`sandboxconfig/effective_test.go:110`).
- `docs/adr/0012-sandbox-config-is-three-attribute-owned-layers.md` and
  `sandboxconfig/DESIGN.md` describe the three attribute-owned layers.

And it is never populated. The one construction site,
`buildSandboxDocument` in `pool-agent/sandboxruntime/runtime.go:621`, sets
model, prompt, harness mode, env, and user from the create request — but not
files, because the request has nowhere to carry them:

- `api/openapi/server.yaml:1715` — `SandboxCreateConfig` has no `files`.
- `api/openapi/server.yaml:1641` — `SandboxConfig` has no `files`.
- `pool-agent/api/openapi/pool.yaml` — `SandboxConfig` (~line 204) has no
  `files` either.
- `model.Sandbox` (`server/internal/model/model.go:555`) has no files column.
  Compare `model.HarnessConfig.Files` / `ConfiguredFiles` (`model.go:289`,
  `model.go:297`), which is the existing project-scoped mechanism and a good
  reference for the `File` shape.

## Scope

A four-hop plumb-through, plus one storage decision:

1. `api/openapi/server.yaml`: add files to `SandboxCreateConfig` and
   `SandboxConfig`. Reuse or mirror the existing harness-config file shape
   (`HarnessConfigFile`, `server.yaml:142`) rather than inventing a second one.
2. `server/internal/model/model.go`: a files column on `Sandbox`, with a
   migration.
3. `pool-agent/api/openapi/pool.yaml`: add files to `SandboxConfig` so the
   create request can carry them.
4. `pool-agent/sandboxruntime/runtime.go`: populate `doc.Runtime.Files` in
   `buildSandboxDocument`.
5. Run `go tool task generate` after each contract edit.
6. Update `sandboxconfig/DESIGN.md` if the runtime layer's file provenance
   changes meaningfully, and the sandbox package `DESIGN.md` files you touch.

## Out of scope

- Making files updatable on a *running* sandbox — WI-04 owns the declarative
  update path and will classify this field once it exists. Expect "mutable with
  restart": the upstream ADR says a change to the rendered agent document or
  skill files changes the revision key and restarts the sandbox.
- Secret values in files. Secrets never appear in file content; they travel
  through the separate sentinel/secret channel to
  `/run/discobox/secrets/secrets.json` (ADR-0012 §3). Do not add a secret-valued
  file type.

## Design questions for the engineer

- **Size and count limits.** Skill trees can be large and every file is stored
  in the sandbox row and shipped in the create request. Is there a cap? Where is
  it enforced?
- **File shape.** Reuse `HarnessConfigFile` directly, or a sandbox-specific
  type? Check whether the harness-config type carries fields (mode, ownership,
  secret binding) that do not apply here.
- **Interaction with harness-config files.** The merge is defined at the
  `sandboxconfig` layer — image files, then runtime files by path, then project
  `FilesAdd`. Confirm where a harness config's `ConfiguredFiles` enters that
  merge today, so per-sandbox files compose predictably rather than surprising a
  user who set both.

## Done when

- A sandbox created with files has them present in the running sandbox at the
  declared paths.
- Overlay-by-path against image-declared files behaves as
  `TestEffective_FilesOverlayByPath` specifies.
- Migration upgrades an existing database in place.
- `go tool task check-hooks` passes.
</content>
</invoke>
<parameter name="description">Write WI-05 per-sandbox files brief