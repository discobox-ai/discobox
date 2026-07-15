# Harness Configs Design

`internal/resources/harnessconfigs` owns harness config definitions and project-scoped
harness config API behavior.

- Harness configs register Docker images. Explicit registration inspects the
  local or registry image config, requires the `io.discobox.harness.v1` label,
  validates it, and snapshots its digest and non-secret metadata.
- Built-in definitions identify included harness images; runtime commands stay
  authoritative inside each image.
- Project bindings remain control-plane state. Image metadata declares only
  secret requirements, never secret values.
- Service methods validate project scope and config input before writing through
  `internal/store`.
- Keep transport DTO conversion in `internal/handlers`; this package may use
  generated DTO aliases from `internal/services`.
- Harness config files are literal by default. Files marked `template` are rendered
  inside the sandbox against the public `SandboxConfig` JSON shape; definitions
  must not invent a parallel set of runtime variables.
