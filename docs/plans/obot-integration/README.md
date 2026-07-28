# Obot managed-runtime integration — work items

Discobox is the first real agent-runtime backend for Obot, per Obot's accepted
ADR-0001 (`obot/docs/adr/0001-agent-runtime-backend-interface.md`). This
directory breaks the Discobox half into work items that can be picked up
independently.

These are implementation plans, not design records. Per `CLAUDE.md`, sequencing
and implementation plans belong to the task doing the work — they never go in an
ADR or a `DESIGN.md`. Decisions go to `docs/adr/`; current-state architecture
goes to the relevant `DESIGN.md`.

**Read [`00-CONTEXT.md`](00-CONTEXT.md) before any individual item.** Each item
assumes it and does not repeat it.

| Item | Scope | Can start |
| --- | --- | --- |
| [01](01-adr.md) | Discobox ADR: managed layer, identity vs. hard deletes, overcommit | now — do this first |
| [02](02-service-auth.md) | Service authentication and project-scoped service authorization | now |
| [03](03-managed-resources.md) | Managed pool and managed sandbox resources | after 01 accepted |
| [04](04-declarative-sandbox-update.md) | Declarative sandbox update, in place or by replacement | now |
| [05](05-per-sandbox-files.md) | Per-sandbox runtime-layer files | now |
| [06](06-pool-envelope-and-placement.md) | Pool suspension, envelope enforcement, overcommit placement | suspension now; placement after 01 |
| [07](07-live-utilization.md) | Live pool and per-sandbox utilization | now |
| [08](08-managed-command-policy.md) | Managed command policy and event correlation metadata | after 03's model lands |
| [09](09-contract-tests.md) | Managed contract test suite | after 03's API shape freezes |

Items 04, 05, 06, and 07 each stand on their own merits and can land before the
managed layer exists. Item 03 is what turns them into an Obot integration.

`00-CONTEXT.md` lists the files that more than one item touches — chiefly
`api/openapi/server.yaml` and `server/internal/model/model.go`. Rebase often.
