# Worker Agent API Model Design

`model` exposes stable aliases for generated worker-local sandbox operation
schema types.

## Responsibilities

- Keep a short import path for worker-local OpenAPI schema types.
- Generate aliases from `../gen/oas_schemas_gen.go` so this package tracks the
  canonical worker-agent OpenAPI contract automatically.
- Keep transport clients, servers, handlers, and validators in `../gen`.

## Generation

Do not edit `aliases_gen.go` by hand. Update `../openapi/worker.yaml` and run:

```bash
go generate ./...
```
