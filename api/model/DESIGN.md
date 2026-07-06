# API Model Design

`model` exposes stable aliases for generated Server REST API schema types.

## Responsibilities

- Keep a short import path for contract-facing OpenAPI schema types.
- Generate aliases from `../gen/oas_schemas_gen.go` so this package tracks the
  canonical OpenAPI contract automatically.
- Generate schema-only public contract models, such as `SandboxManifest`, from
  `../openapi/server.yaml` when those schemas are intentionally not reachable
  from REST operations.
- Keep transport clients, servers, handlers, and validators in `../gen`.

## Generation

Do not edit generated files by hand. Update `../openapi/server.yaml` and run:

```bash
go generate ./...
```
