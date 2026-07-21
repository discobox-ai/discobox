# Model Review Notes

- Keep `model.AllModels()` in sync with persisted model additions.
- Orchestrated resources should embed `ResourceLifecycle` and define clear operation specs.
- Avoid duplicating common lifecycle fields in design diagrams for each resource.
- Be explicit about whether a type is implemented today or design-level/planned.
- If API and DB shapes diverge significantly, consider DTOs rather than adding model hacks.
- Never add `gorm.DeletedAt`, a `deleted` boolean, or a nullable deletion timestamp. Deletes are hard; a tombstone still occupies its table's unique indexes and makes the deleted thing unrecreatable. See ADR 0010.
