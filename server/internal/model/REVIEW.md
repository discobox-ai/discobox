# Model Review Notes

- Keep `model.AllModels()` in sync with persisted model additions. `Job` is not persisted — it is an API view of a pending reconcile mark — and stays out of it.
- Orchestrated resources (`Sandbox`, `Pool`) embed `ResourceLifecycle`; only `generation`/`observedGeneration` are the orchestration contract, and there are no operation records (ADR 0017).
- A resource's spec is an anonymously embedded, flat manifest (`SandboxManifest`, `PoolManifest`; ADR 0017 §11). Put a field in `SandboxManifest` only if changing it requires a container rebuild; `SandboxManifest.Fingerprint()` is the drift check.
- Avoid duplicating common lifecycle fields in design diagrams for each resource.
- Document implemented types only; a planned type belongs in an ADR, not in the model docs.
- If API and DB shapes diverge significantly, consider DTOs rather than adding model hacks.
- Never add `gorm.DeletedAt`, a `deleted` boolean, or a nullable deletion timestamp. Deletes are hard; a tombstone still occupies its table's unique indexes and makes the deleted thing unrecreatable. See ADR 0010.
- Enum-valued fields must keep their `enum:"..."` tag in sync with `api/openapi/server.yaml`; `enumsync_test.go` enforces this both ways. When adding an enum value, change the tag, the registry slice (`PoolStates`, `SandboxStates`, ...; checked against the generated API enums by `enum_contract_test.go`), and the yaml together, then run `go tool task generate`. New contract-only enums must be classified in the test's `yamlEnumAliases` or `yamlOwnedEnums` list.
