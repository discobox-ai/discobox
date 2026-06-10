# Design Overview

This repository uses package-local design notes. For any package, read docs in
this order:

1. Parent `DESIGN.md` / `REVIEW.md` files.
2. The package's own `DESIGN.md` / `REVIEW.md` files.
3. Child package docs only when working in that child package.

## System Pattern

The control plane stores desired resource intent, records resource changes, and
uses durable jobs to reconcile actual provider/runtime state.

High-level flow:

```text
API / CLI
  -> service layer accepts intent
  -> store persists resource changes
  -> orchestration ensures a durable reconcile job
  -> job executor/reconciler performs provider work
  -> store updates observed state
```

Important package docs:

| Package | Design notes |
| --- | --- |
| `internal/model` | Data model, lifecycle-bearing resources, planned tenant/worker model. |
| `internal/orchestration` | Desired-state reconciliation pattern. |
| `internal/service` | Sandbox lifecycle and service/reconciler behavior. |
| `internal/sandboxauth` | Sandbox access and worker auth design notes. |
| `jobqueue` | Durable job queue behavior. |
| `gormdb` | Database setup helper. |
