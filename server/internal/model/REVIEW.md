# Model Review Notes

- Keep `model.AllModels()` in sync with persisted model additions.
- Orchestrated resources should embed `ResourceLifecycle` and define clear operation specs.
- Avoid duplicating common lifecycle fields in design diagrams for each resource.
- Be explicit about whether a type is implemented today or design-level/planned.
- If API and DB shapes diverge significantly, consider DTOs rather than adding model hacks.
- Primary mutable resources should use GORM's native `gorm.DeletedAt` soft-delete field with `gorm:"index" json:"-"`; avoid bespoke delete markers.
- Use hard deletes only for documented exceptions such as `AgentConfig`, append-only/audit rows, short-lived operational state, or rows whose deletion is the state transition.
