# API Model Design

`model` exposes stable aliases for generated Server REST API schema types.

## Responsibilities

- Keep a short import path for contract-facing OpenAPI schema types.
- Hold only generated `type X = apigen.X` aliases (`aliases_gen.go`), so this
  package tracks the canonical OpenAPI contract automatically: one alias per
  `components.schemas` entry in `../openapi/server.yaml` that ogen emits as a
  named type in `../gen/oas_schemas_gen.go`. A schema ogen does not emit as a
  named type gets no alias.
- Keep transport clients, servers, handlers, and validators in `../gen`. The
  sandbox-agent subset scaffold (`../sandboxgen`) has no alias package.

The sandbox's effective runtime configuration (`/etc/discobox/sandbox.json`)
is not a REST contract type and is not generated here. It is the hand-written
`sandboxconfig` package (repo root) — see `../../sandboxconfig/DESIGN.md` and
`docs/adr/0012-sandbox-config-is-three-attribute-owned-layers.md`.

## Generation

Do not edit generated files by hand. Update `../openapi/server.yaml` and run,
from the repository root:

```bash
go tool task generate
```

The `go:generate` directives live in the root `generate.go`, not in this
package: ogen regenerates `../gen` first, then `../internal/genmodelaliases`
writes `aliases_gen.go` from the contract and the regenerated schemas. The
same generator produces `pool-agent/api/model`.
