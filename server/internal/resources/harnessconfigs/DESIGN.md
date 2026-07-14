# Harness Configs Design

`internal/resources/harnessconfigs` owns harness config definitions and project-scoped
harness config API behavior.

- Static harness config definitions live here.
- Service methods validate project scope and config input before writing through
  `internal/store`.
- Keep transport DTO conversion in `internal/handlers`; this package may use
  generated DTO aliases from `internal/services`.

