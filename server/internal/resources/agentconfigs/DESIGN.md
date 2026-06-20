# Agent Configs Design

`internal/resources/agentconfigs` owns agent config definitions and project-scoped
agent config API behavior.

- Static agent config definitions live here.
- Service methods validate project scope and config input before writing through
  `internal/store`.
- Keep transport DTO conversion in `internal/handlers`; this package may use
  generated DTO aliases from `internal/services`.

