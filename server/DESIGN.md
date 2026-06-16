# Server Module Design

The server module is the Discobox control plane implementation. It owns HTTP
composition, single-database persistence, project events, API-facing business logic,
and durable reconciliation submission. Stable contracts and generated API types
come from the root module; provider implementations come from sibling modules and
are wired only at composition boundaries.

## Module Role

```mermaid
flowchart LR
    clients[CLI / UI / API clients] -->|Server REST API| http[internal/server]
    http --> api[internal/api or internal/generatedapi]
    http --> database[internal/database]
    http -. passes GORM handles .-> store[internal/store]
    api --> service[internal/service]
    service --> store
    service --> events[internal/events]
    service --> orchestration[orchestration module]
    orchestration --> sandbox[internal/sandbox]
    sandbox --> providers[providers module]
    sandbox --> sandboxauth[internal/sandboxauth]
```

Keep the server as the control plane:

- Persist desired state before external runtime side effects.
- Publish project events and submit durable reconcile jobs in the same intent
  transaction.
- Run generation-scoped reconciliation; cancel stale jobs when newer intent
  supersedes them.
- Treat provider packages as runtime adapters, not as a place for server policy.

## API Boundary

The Server REST API contract is owned by the root module at
`api/openapi/server.yaml`. Server handlers should implement that contract instead
of deriving the contract from Go route registration.

Current transition state:

- `internal/server.NewRouter`, `NewApplicationRouter`, and `NewGeneratedRouter`
  compose chi routers around the generated OpenAPI server scaffold.
- `internal/generatedapi` adapts generated operations to API-facing services.
- `internal/api` owns service interfaces and aliases generated server DTOs for
  server packages during the generated-server migration.
- Project stream websocket and SSE routes stay hand-wired in
  `internal/projectstream`; generated OpenAPI scaffolding does not own streaming
  transport mechanics, and ogen skips `text/event-stream` operations.

New endpoints should prefer the contract-first generated path. Keep streaming
transports hand-wired unless the generator can own them behavior-compatibly.

Authorization must be decidable from request attributes available before body
interpretation: authenticated principal, method, route/path parameters, query
parameters, headers, and resource ownership loaded by those attributes. Do not
require request-body fields to decide whether the caller may access the target
resource; put authorization identity in the URL or other request metadata instead.

Narrow exception: `POST /api/workers/register` is a bootstrap credential
redemption flow, not normal resource access. Its body carries the project ID,
sandbox ID, one-time bootstrap token, and public key because a worker does not
yet have a runtime principal or token. The service may use those body fields
only to redeem the short-lived, one-time bootstrap token against the sandbox's
preassigned worker and issue the first runtime worker token. Subsequent worker
authorization must use request metadata and the authenticated worker principal,
such as `/api/workers/{workerId}/status`.

## Runtime Observability

`internal/server` owns optional process-level OpenTelemetry metrics startup as
part of HTTP server composition. When `OTEL_METRICS_EXPORTER=otlp` is configured,
startup initializes the global meter provider, exports metrics through OTLP/HTTP
using the standard OpenTelemetry exporter environment variables, and wraps all
HTTP traffic with OpenTelemetry HTTP instrumentation. Local Discobot development
services may provide an OTLP receiver/dashboard, but telemetry must remain
optional so normal server startup does not require an observability backend.

## Intent and Reconcile Flow

```mermaid
sequenceDiagram
    participant Client
    participant Router as internal/server
    participant API as API handler
    participant Service as internal/service
    participant Store as internal/store
    participant Jobs as orchestration
    participant Reconciler as internal/sandbox
    participant Provider as providers module

    Client->>Router: REST request
    Router->>API: decoded operation
    API->>Service: business command
    Service->>Store: transaction: resource + event + job
    Store-->>Service: committed intent
    Jobs->>Reconciler: generation-scoped job
    Reconciler->>Store: load latest resource generation
    Reconciler->>Provider: runtime action when current
    Reconciler->>Store: observed state + event
```

API handlers stay thin: decode generated OpenAPI DTOs, call services, and encode
responses. Services own validation, intent changes, event creation, and job
submission. Reconcilers own generation checks and runtime operation progress.

## Package Map

| Package/path | Ownership |
| --- | --- |
| `cmd/discobox-server` | Server binary entrypoint. |
| `internal/server` | HTTP startup, chi router composition, auth middleware wiring, generated route mounting, and hand-wired stream transport registration. |
| `internal/auth` | Request authentication, authorization, and principal context helpers. |
| `internal/server/defaults` | Startup/default identity initialization. |
| `internal/api` | Service interfaces and generated server DTO aliases used across server packages during the generated-server migration. |
| `internal/generatedapi` | Generated OpenAPI handler adapter layer for server business logic. |
| `internal/service` | API-facing business logic, default data initialization, intent transactions, and provider catalog behavior. |
| `internal/sandbox` | Server-owned sandbox/worker reconcilers, job submitters, root `sandboxprovider.ProviderManager` injection/usage, and sandbox-service glue. |
| `internal/database` | Database config, connection setup, and migrations. |
| `internal/store` | Persistence methods, resource transactions, project events, and durable job records. |
| `internal/events` | In-process project event broker for committed resource events. |
| `internal/projectstream` | Websocket/SSE project event streaming transports. |
| `internal/sandboxauth` | Sandbox access issuer keys and worker/sandbox auth token helpers. |
| `internal/secrets` | Encryption/sealing interfaces and implementations used by server persistence. |
| `internal/config` | Server configuration loading. |

## Dependency Rules

- Server code may depend on the root module for contracts, models, generated API
  code, and cross-module sentinel errors.
- Server composition may import provider implementations to register concrete
  providers; deeper packages should depend on provider interfaces.
- Providers, sandbox-agent code, and CLI code must not import server internals.
- Cross-module types and errors must live in public root packages, not under
  `server/internal`.
- Keep GORM access behind `internal/store`; services and reconcilers should not
  query database handles directly.

## Deeper Design Docs

| Package | Design notes |
| --- | --- |
| `internal/auth` | [`internal/auth/DESIGN.md`](internal/auth/DESIGN.md) |
| `internal/database` | [`internal/database/DESIGN.md`](internal/database/DESIGN.md) |
| `internal/projectstream` | [`internal/projectstream/DESIGN.md`](internal/projectstream/DESIGN.md) |
| `internal/sandbox` | [`internal/sandbox/DESIGN.md`](internal/sandbox/DESIGN.md) |
| `internal/sandboxauth` | [`internal/sandboxauth/DESIGN.md`](internal/sandboxauth/DESIGN.md) |
| `internal/service` | [`internal/service/DESIGN.md`](internal/service/DESIGN.md) |
| `internal/store` | [`internal/store/DESIGN.md`](internal/store/DESIGN.md) |

Add lower-level `DESIGN.md` files next to packages when a package gains its own
architecture rules. Keep this module-level doc focused on server boundaries and
cross-package flow.
