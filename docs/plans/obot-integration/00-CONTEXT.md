# Shared context: Obot managed-runtime integration

Read this before picking up any `NN-*.md` work item in this directory. Each work
item assumes it and does not repeat it.

## The larger picture

Obot is a separate product that defines hosted agents, decides who may use them,
and stores each user's agent configuration. It needs an external system to
actually run those agents. Obot's accepted ADR-0001
(`../obot/docs/adr/0001-agent-runtime-backend-interface.md`) defines a
provider-neutral agent-runtime backend interface and names **Discobox** as its
first real backend.

That ADR refers to Discobox as "Disco2" because the checkout directory is
`disco2`. That naming is wrong. Always write **Discobox**.

The ownership chain:

```
Obot HostedAgentInstance (desired config, authorization, secrets)
  -> resolved into runtime primitives by Obot
  -> Discobox REST API (managed sandbox)
  -> Discobox generation-based reconciliation
  -> pool and sandbox runtime
```

Obot stays authoritative for desired state. Discobox stays authoritative for its
own runtime generation and observed phase. **Discobox learns nothing about Obot
concepts** — no hosted agents, MCP servers, skills, model aliases, or access
rules. Obot resolves all of that into files, environment variables, secret
references, a source, a model, and a prompt before anything crosses the REST
boundary. The managed API must remain useful to any upstream controller that can
supply a stable identity, an opaque revision, and a sandbox configuration.

The deployment mapping Obot assumes:

- one Obot deployment -> one administrator-created Discobox project it owns
  exclusively (Obot never creates or deletes the project);
- one Obot allocation -> one managed Discobox pool in that project;
- one Obot hosted agent instance -> one managed Discobox sandbox in that pool.

## The core new idea: managed resources

Ordinary Discobox sandbox operations are user-command oriented and addressed by
a Discobox-generated ID. **Managed** operations are upstream-controller oriented
and addressed by an *upstream-owned stable external ID* plus an *opaque desired
revision*. The upstream controller retries, so every managed operation must be
idempotent, and a lost response followed by a repeated `PUT` must never produce
a duplicate runtime.

Two properties drive most of the design:

- **The external identity is stable; the concrete resource is not.** Changing an
  immutable field may force Discobox to replace the concrete sandbox. The
  managed identity, and its correlation to upstream, survives that.
- **Revisions are opaque equality tokens.** Discobox persists the supplied
  revision verbatim, compares it only for equality, and never orders or derives
  it. Secret *values* never participate in a revision; changing the value behind
  an existing secret ID refreshes the secret channel without restarting the
  sandbox, while adding or removing a secret reference is a configuration change.

## Repository conventions that apply to every item

- Read `CLAUDE.md` at the repo root first.
- Read `DESIGN.md` and `REVIEW.md` from the repo root down to the package you
  are working in. Closer files specialize the guidance in parent files.
- Every change that alters architecture updates the affected `DESIGN.md` files
  **in the same change**. `DESIGN.md` describes current state only, never
  planned work.
- `docs/adr/` records decisions and is immutable once accepted — supersede,
  never edit. Most changes need no ADR; WI-01 covers the ones that do.
- Prefer proper structural changes over compatibility shims. Do not add optional
  interfaces, wrapper types, or adapter layers to avoid touching call sites.
- Treat persisted state as durable. Schema changes need a safe upgrade path with
  migrations and backfills, not a recreated database.
- Work on the branch that is already checked out. Do not create branches or
  worktrees unless told to.
- Finish with `go tool task check-hooks`. If its output looks stale, run
  `go tool task rerun-hooks`, then check again.
- The OpenAPI contract is canonical and code is generated from it. Edit
  `api/openapi/server.yaml` or `pool-agent/api/openapi/pool.yaml`, then run
  `go tool task generate`. Never hand-edit generated files.

## Collision surface between parallel items

These files are touched by more than one work item. Coordinate, rebase often,
and expect mechanical but real conflicts:

| File | Items |
| --- | --- |
| `api/openapi/server.yaml` (~5700 lines) | WI-03, WI-04, WI-05, WI-06, WI-07 |
| `server/internal/model/model.go` | WI-03, WI-05, WI-06 |
| `pool-agent/api/openapi/pool.yaml` | WI-05, WI-07 |
| `server/internal/resources/sandboxes/` | WI-04, WI-05, WI-08 |
| `server/internal/resources/pools/` | WI-06, WI-07 |

Each item should add its new schemas as a self-contained block rather than
interleaving edits into existing schemas where avoidable, and re-run
`go tool task generate` after every rebase.

## Suggested parallel tracks

| Track | Items | Blocked by |
| --- | --- | --- |
| Decision | WI-01 (ADR) | nothing — start first |
| Trust boundary | WI-02 (service auth) | nothing |
| Managed layer | WI-03 (managed resources) -> WI-08 (command policy + event metadata) | WI-01 accepted |
| Sandbox capability | WI-04 (declarative update + replace), WI-05 (per-sandbox files) | nothing; both land independently of WI-03 |
| Pool | WI-06 (suspension, envelope, overcommit) | WI-01 accepted for the overcommit decision |
| Observability | WI-07 (live utilization) | nothing — fully independent |
| Verification | WI-09 (contract test suite) | WI-03's API shape frozen |

WI-04, WI-05, WI-06, and WI-07 are all valuable on their own merits and can land
before the managed layer exists. WI-03 is the item that turns them into an Obot
integration.

## Non-goals for the whole programme

- Historical utilization charts, streaming metrics, billing-grade accounting.
- Reservation-based admission or VM-style bin packing for sandboxes.
- Obot-side work of any kind. This repository implements the Discobox half only.
- Push-delivered Git sources for managed sandboxes. Those need an interactive
  client to complete the source push, so the managed contract rejects them.
</content>
</invoke>
<parameter name="description">Write shared context file