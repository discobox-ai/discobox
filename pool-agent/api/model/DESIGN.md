# Pool Agent API Model Design

`model` exposes stable aliases for generated pool-local sandbox operation
schema types.

## Responsibilities

- Keep a short import path for pool-local OpenAPI schema types. Both sides of
  the API import it: the pool agent (`pool-agent`, `server`, `sandboxruntime`)
  and the control plane's `server/providers/poolruntime` client.
- Alias every `components.schemas` entry in `../openapi/pool.yaml` that ogen
  emits as a type in `../gen/oas_schemas_gen.go`, so this package tracks the
  canonical pool-agent OpenAPI contract automatically.
- Keep transport clients, servers, handlers, and validators in `../gen`.
- Hold the hand-written wire constants that are contract but not schema, so
  both sides import one definition. `errors.go` holds the RFC 7807 `type`
  values (`ErrorTypeSandboxArchived`): a status the API reuses for two
  conditions (409 is both "already exists" and "archived") needs something
  machine-readable to tell them apart, and the human-readable detail is not
  that. The pool agent's handlers set it; `poolruntime` matches on it.

## Generation

Do not edit `aliases_gen.go` by hand. Update `../openapi/pool.yaml` and run
`go tool task generate` from the repository root, or `go generate ./...` from
the `pool-agent` module root. The directives live in `pool-agent/generate.go`:
ogen regenerates `../gen` first, then `../internal/genmodelaliases` writes
`aliases_gen.go` from the spec and the fresh `oas_schemas_gen.go`.
