# Pool Agent API Model Design

`model` exposes stable aliases for generated pool-local sandbox operation
schema types.

## Responsibilities

- Keep a short import path for pool-local OpenAPI schema types.
- Generate aliases from `../gen/oas_schemas_gen.go` so this package tracks the
  canonical pool-agent OpenAPI contract automatically.
- Keep transport clients, servers, handlers, and validators in `../gen`.

## Generation

Do not edit `aliases_gen.go` by hand. Update `../openapi/pool.yaml` and run:

```bash
go generate ./...
```
