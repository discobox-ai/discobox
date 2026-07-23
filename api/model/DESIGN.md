# API Model Design

`model` exposes stable aliases for generated Server REST API schema types.

## Responsibilities

- Keep a short import path for contract-facing OpenAPI schema types.
- Generate aliases from `../gen/oas_schemas_gen.go` so this package tracks the
  canonical OpenAPI contract automatically.
- Keep transport clients, servers, handlers, and validators in `../gen`.

The sandbox's effective runtime configuration (`/etc/discobox/sandbox.json`)
is not a REST contract type and is not generated here. It is the hand-written
`sandboxconfig` package (repo root) — see `../../sandboxconfig/DESIGN.md` and
`docs/adr/0012-sandbox-config-is-three-attribute-owned-layers.md`.

## Generation

Do not edit generated files by hand. Update `../openapi/server.yaml` and run:

```bash
go generate ./...
```
