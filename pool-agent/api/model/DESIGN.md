# Pool Agent API Model Design

`model` exposes stable aliases for generated pool-local sandbox operation
schema types.

## Responsibilities

- Keep a short import path for pool-local OpenAPI schema types.
- Generate aliases from `../gen/oas_schemas_gen.go` so this package tracks the
  canonical pool-agent OpenAPI contract automatically.
- Keep transport clients, servers, handlers, and validators in `../gen`.
- Hold the hand-written wire constants that are contract but not schema, so
  both sides import one definition. `errors.go` holds the RFC 7807 `type`
  values: a status the API reuses for two conditions (409 is both "already
  exists" and "archived") needs something machine-readable to tell them apart,
  and the human-readable detail is not that.

## Generation

Do not edit `aliases_gen.go` by hand. Update `../openapi/pool.yaml` and run:

```bash
go generate ./...
```
