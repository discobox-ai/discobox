# API Design

`api` owns the Unix-socket OpenAPI contract and exported user-facing API
metadata shared by daemon handlers, manager/service code, client code, CLI code,
and tests. Stable socket DTOs live in the `api/model` subpackage. The parent
package keeps the audit event type catalog used by `discobox-hooks events
--list-types`.

## Responsibilities

- Maintain the canonical hooks daemon OpenAPI contract at `openapi/hooks.yaml`.
- Keep core reusable schemas under `openapi/components/model`; these schemas are
  referenced by the main contract and exported through generated aliases in
  `model`.
- Generate contract-first client/server scaffolding and schema DTOs into `gen`
  with ogen.
- Generate stable aliases for core generated schema types into `model` as part
  of `go generate`.
- Keep stable handwritten-style socket DTOs in `model` when generated optional
  wrappers would leak transport-generator details into service, client, or CLI
  code.
- Define endpoint-specific response shapes that are not plain database models.
- Own the user-facing audit event type catalog and detail-field schema exposed
  through `KnownEventTypes`, `EventTypeInfo`, and `EventDetailInfo`.
- Keep HTTP routing, persistence, and process execution out of this package.

## Generation

Run generation from this package with:

```bash
go generate ./...
```

The generation directive uses the root module's tool-managed `ogen` binary and
writes generated client, server, and schema files to `hooks/api/gen`. A second
generation step scans top-level schema names in `openapi/components/model/*.yaml`
and writes alias types to `hooks/api/model/aliases_gen.go`. Keep generated files
checked in.

## DTO Policy

Prefer generated DTOs from `gen` for the OpenAPI transport adapter. Prefer
`api/model` for stable code-facing socket API models, including `Hook`,
`HookStatus`, `Event`, run/change/snapshot/queue views, response wrappers, and
request payloads. The parent `api` package should not define endpoint DTOs; keep
those in `api/model` so the parent can focus on contract generation and
user-facing API metadata.

List endpoints that expose persisted state, such as `/runs`, `/changes`,
`/snapshots`, and `/queue`, should return projected API DTOs rather than raw
GORM model rows. Put reusable database-shaped schemas under
`openapi/components/model`; keep endpoint-specific transport projections under
`openapi/components/transport`.

## Events API

`GET /events` returns a JSON snapshot of recent audit events.

`GET /events/stream` is the SSE form of the same audit stream. It sends one
JSON-encoded `Event` DTO per SSE message, uses the event row ID as the SSE `id`,
and supports browser/client reconnects via the `Last-Event-ID` header. The
daemon implementation polls the store once per second for events after the last
sent cursor.

`EventTypeInfo`, `EventDetailInfo`, and `KnownEventTypes` are exported API
surface. `KnownEventTypes` is the canonical user-facing catalog of audit event
type names, descriptions, and expected `details` object fields. The CLI uses it
for `discobox-hooks events --list-types`. Whenever daemon, manager, or store
code adds a production audit event type, add its description and applicable
detail-field schema to this catalog rather than duplicating an event list in the
CLI.

`EventDetailInfo.Required` is enforced by the store before `hook_events` rows are
written. Unknown event types or missing required detail fields return an error in
normal runtime paths and panic under Go test binaries so unit tests catch schema
drift at the emission site.
