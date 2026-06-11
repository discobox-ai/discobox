# Repository Guidelines

## Project Structure

- `cmd/discobot-server`: HTTP server entrypoint.
- `cmd/openapi`: OpenAPI generation entrypoint used by `go generate`.
- `internal/api`: Huma/chi API operation definitions and tests.
- `internal/server`: server startup and HTTP router wiring.
- `internal/model`: combined GORM/API resource models.
- `internal/store`: database access, split by resource.
- `internal/service`: API services, sandbox orchestration, reconciliation, and operations.
- `internal/orchestration`: generic desired-state/job orchestration helper.
- `internal/jobs`: jobqueue payloads and executors.
- `internal/events`: project event broker.
- `gormdb`: nested module for DB setup.
- `jobqueue`: nested module for durable job execution.
- `DESIGN.md` / `REVIEW.md`: package-local design and review notes. Read the closest files in the current package and its parents before making design-sensitive changes.

## Commands

Run root tests:

```bash
go test ./...
```

Run nested module tests:

```bash
(cd gormdb && go test ./...)
(cd jobqueue && go test ./...)
```

Regenerate OpenAPI:

```bash
go generate ./...
```

## Package Design Docs

Design guidance lives next to the code it describes:

- `DESIGN.md` explains the design of that package and its subdirectories.
- `REVIEW.md` lists review rules and pitfalls for that package and its subdirectories.

When working in a package, read `DESIGN.md` and `REVIEW.md` from the repository
root down to the package directory. Parent files provide broader context; closer
files override or specialize that guidance.

## Design Notes

The API uses desired-state reconciliation. Resource intent changes are persisted
with project events and durable reconcile jobs in one transaction. Reconcile
jobs run for one resource generation and cancel when superseded by newer intent.

See `internal/orchestration/DESIGN.md` and `internal/orchestration/REVIEW.md`
before adding new orchestrated resources.
