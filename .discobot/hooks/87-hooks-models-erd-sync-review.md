---
name: Hooks models ERD sync review
type: file
engine: ai
pattern: "hooks/models/models.go"
description: Verify hooks/models/DESIGN.md ERD stays in sync with hooks/models/models.go
phase: review
---
Review changes to `hooks/models/models.go` against `hooks/models/DESIGN.md`.

Focus specifically on the `## Schema ERD` Mermaid diagram and the short table notes below it.

When `hooks/models/models.go` changes:

1. Read `hooks/models/models.go` and list the exported GORM table models:
   - structs with a `TableName()` method, and
   - structs returned by `AllModels()`.
2. Read `hooks/models/DESIGN.md`.
3. Verify the ERD includes every hook-owned database table from `models.go`.
4. Verify each ERD table includes the durable columns represented by the GORM struct fields, using the same JSON/database-oriented names where practical.
5. Verify generated-ID behavior and primary-key intent are not contradicted by the ERD.
6. Verify relationships shown in the ERD are still reasonable for the current model fields.
7. Verify every table in the ERD has a documented purpose in the prose below it.
8. Verify the prose below the ERD is not stale for any added, removed, or renamed model.

Report actionable findings only when the design doc is stale, incomplete, misleading, or inconsistent with `models.go`. If the ERD and nearby prose are already in sync, report no issues.

Do not require the ERD to document indexes, GORM tags, JSON-only helper value types, or non-table helper structs unless they affect schema intent. Do require each hook-owned table to have a short, accurate purpose description.
