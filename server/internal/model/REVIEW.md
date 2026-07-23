# Model Review Notes

- Keep `model.AllModels()` in sync with persisted model additions.
- Orchestrated resources should embed `ResourceLifecycle` and define clear operation specs.
- Avoid duplicating common lifecycle fields in design diagrams for each resource.
- Be explicit about whether a type is implemented today or design-level/planned.
- If API and DB shapes diverge significantly, consider DTOs rather than adding model hacks.
- Never add `gorm.DeletedAt`, a `deleted` boolean, or a nullable deletion timestamp. Deletes are hard; a tombstone still occupies its table's unique indexes and makes the deleted thing unrecreatable. See ADR 0010.
- Enum-valued fields must keep their `enum:"..."` tag in sync with `api/openapi/server.yaml`; `enumsync_test.go` enforces this both ways. When adding an enum value, change the tag and the yaml together, then run `go tool task generate`. New contract-only enums must be classified in the test's alias or yaml-owned lists.
