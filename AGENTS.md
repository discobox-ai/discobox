# Repository Guidelines

## Project Structure

- `cmd/discobot-server`: HTTP server entrypoint.
- `cmd/openapi`: OpenAPI generation entrypoint used by `go generate`.
- `internal/api`: Huma/chi API operation definitions and tests.
- `internal/app`: application wiring.
- `internal/model`: combined GORM/API resource models.
- `internal/store`: database access, split by resource.
- `internal/service`: API services, sandbox orchestration, reconciliation, and operations.
- `internal/orchestration`: generic desired-state/job orchestration helper.
- `internal/jobs`: jobqueue payloads and executors.
- `internal/events`: project event broker.
- `gormdb`: nested module for DB setup.
- `jobqueue`: nested module for durable job execution.
- `docs`: design notes and orchestration pattern docs.

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

## Design Notes

The API uses desired-state reconciliation. Resource intent changes are persisted
with project events and durable reconcile jobs in one transaction. Reconcile
jobs run for one resource generation and cancel when superseded by newer intent.

See `docs/orchestration-pattern.md` before adding new orchestrated resources.
