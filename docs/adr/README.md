# Architecture Decision Records

An ADR records a decision, the alternatives rejected, and why — at the time it
was made.

## When to write one

Only when either is true:

- A plausible alternative was rejected for a non-obvious reason.
- Something was deferred with a specific condition for revisiting it.

Otherwise skip the ADR and update the relevant `DESIGN.md`. Most changes do not
need one.

## Rules

- **Immutable.** Never edit an accepted ADR. Superseded by a new one that links
  back; mark the old one `Superseded by NNNN`.
- **Outside the drill-down hierarchy.** ADRs live here, never next to code.
  `DESIGN.md`/`REVIEW.md` are read root-down by agents on every task; ADRs are
  not, and must not be, because they are history rather than current state.
- **Not a substitute for `DESIGN.md`.** When the work lands, update the live
  design docs to describe what now exists. The ADR keeps the "why we didn't";
  `DESIGN.md` keeps the "what is".

## Status lifecycle

`Proposed` → `Accepted` → (`Superseded by NNNN`)

Use `Rejected` for decisions considered and declined; keep the file.

## Index

| ADR | Title | Status |
| --- | --- | --- |
| [0001](0001-sandbox-origin-and-remote-source-push.md) | Sandbox origin and remote source push | Proposed |
| [0002](0002-harness-config-is-the-only-harness-concept.md) | Harness config is the only harness concept | Proposed |
