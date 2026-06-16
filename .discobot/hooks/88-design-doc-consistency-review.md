---
name: Design document consistency review
type: file
engine: ai
pattern: "**/*"
description: Review code changes against applicable DESIGN.md files and flag missing design doc updates
phase: review
---
Review the changed files for consistency with the repository's design documents.

Scope this hook to implementation-impacting changes. Treat these as code or
behavior changes unless the diff is clearly documentation-only:

- Go, JavaScript, TypeScript, Svelte, shell, SQL, Dockerfile, YAML, TOML, JSON,
  generated-code inputs, build scripts, task files, hooks, and configuration
  files.
- API contracts and generated-code configuration.
- Tests, when they document or encode behavior that affects package design.

For each changed implementation file:

1. Find and read applicable `DESIGN.md` files from the repository root down to
   the file's directory. Parent design docs provide broader context; closer docs
   override or specialize parent guidance.
2. If a package has a nearby `REVIEW.md`, read it too, but use this hook's main
   focus for design consistency rather than general code quality.
3. Check whether the code change follows the relevant design guidance,
   including package boundaries, module boundaries, dependency direction,
   ownership of responsibilities, persistence/transaction rules, reconciliation
   semantics, API-contract rules, and documented runtime flows.
4. Check whether the behavior or architecture changed enough that an applicable
   `DESIGN.md` is now stale, incomplete, misleading, or missing important new
   concepts.

Report actionable findings when either:

- The changed code appears inconsistent with the applicable design documents.
- The changed code appears valid, but the relevant design documents should be
  updated to describe the new behavior, ownership, dependency, package role,
  route/API contract, persistence rule, reconciliation rule, or operational
  flow.

Important reporting rules:

- Do not make design document changes as part of this hook.
- If a design document needs an update, report that as an issue and name the
  most relevant `DESIGN.md` file to update.
- Prefer high-signal findings. Do not require design doc updates for trivial
  implementation details, mechanical renames already reflected in docs, typo
  fixes, generated output with no source/design change, or tests that only add
  coverage for already-documented behavior.
- If the design documents already cover the changed behavior adequately, do not
  report an issue.
- Include enough context for a developer to decide whether to change code or
  update a design doc, but avoid prescribing large speculative rewrites.
