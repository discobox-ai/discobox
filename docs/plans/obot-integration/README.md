# Obot managed-runtime integration — work items

> Status (checked against the code 2026-09-11): not started. No managed
> resource, service credential, pool suspension, per-sandbox files, or
> declarative sandbox update has landed. Two things moved underneath these
> plans: overcommit placement (WI-01 decision 3, WI-06 scope 2) was decided for
> every pool by
> [ADR 0029](../../adr/0029-sandboxes-have-no-per-sandbox-resource-requests.md)
> and implemented; and per-pool/per-sandbox resource figures shipped as the
> persisted pool-agent report of
> [ADR 0071](../../adr/0071-resource-accounting-is-a-pool-agent-differenced-report.md),
> not WI-07's unpersisted read. The project event stream and the broker under
> it are gone
> ([ADR 0061](../../adr/0061-the-client-facing-project-event-stream-is-removed.md),
> [ADR 0081](../../adr/0081-project-events-are-not-persisted-and-the-wait-polls.md)).

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
| [06](06-pool-envelope-and-placement.md) | Pool suspension, envelope enforcement, overcommit placement | suspension now; overcommit placement already shipped (ADR 0029) |
| [07](07-live-utilization.md) | Live pool and per-sandbox utilization | re-plan against ADR 0071 first |
| [08](08-managed-command-policy.md) | Managed command policy and event correlation metadata | after 03's model lands |
| [09](09-contract-tests.md) | Managed contract test suite | after 03's API shape freezes |

Items 04, 05, 06, and 07 each stand on their own merits and can land before the
managed layer exists. Item 03 is what turns them into an Obot integration.

`00-CONTEXT.md` lists the files that more than one item touches — chiefly
`api/openapi/server.yaml` and `server/internal/model/model.go`. Rebase often.
