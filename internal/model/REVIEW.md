# Model Review Notes

- Keep `model.AllModels()` in sync with persisted model additions.
- Orchestrated resources should embed `ResourceLifecycle` and define clear operation specs.
- Avoid duplicating common lifecycle fields in design diagrams for each resource.
- Be explicit about whether a type is implemented today or design-level/planned.
- If API and DB shapes diverge significantly, consider DTOs rather than adding model hacks.
