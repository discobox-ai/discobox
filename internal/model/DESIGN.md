# Model Design

This package contains the current persisted/API model. Today the Go structs carry
both GORM persistence tags and JSON/Huma API-facing tags.

## Core Entities

| Entity | Description |
| --- | --- |
| `Tenant` | Planned top-level boundary for users, projects, and their resources. |
| `User` | Authenticated person. Belongs to a tenant, owns projects, and creates sandboxes. |
| `Project` | Tenant-scoped group for sandboxes, provider configuration, and project events. |
| `ServerState` | Tenant-scoped generic key/value state for server preferences and one-time initialization flags. |
| `Sandbox` | Main managed runtime/session resource. Belongs to a project and is orchestrated. |
| `AgentConfig` | Project-scoped agent runtime configuration selected by sandboxes. |
| `AgentConfigDefinition` | Non-persisted, well-known template used by the UI/API to create an `AgentConfig`; definitions are not selectable by sandboxes. |
| `SandboxProviderInstance` | Project-scoped provider configuration for creating and managing sandboxes. |
| `Worker` | Provider-backed runtime worker for launching sandboxes. Has its own identity and public key; private key stays on the worker. Prewarmed workers belong to a provider instance/pool and can host many sandboxes. Scheduling uses `ready`, `schedulable`, and `degraded` columns; detailed condition data is opaque JSON for display. |
| `WorkerBootstrapToken` | Planned short-lived, one-time token used by a new worker to register its public key. |
| `WorkerAuthToken` | Planned short-lived runtime token issued after the worker proves possession of its private key; may be stateless. |
| `SandboxAccessIssuerKey` | Design-level name for the current `ProjectUserKey`: per-project, per-user issuer key used by the control plane to sign sandbox access tokens. |
| `ProjectEvent` | Append-only project-scoped resource event for list/watch sync and replay. |

## Persistence Scope

Models are split by database/schema scope:

- Global scope: `Tenant`, `User`.
- Tenant scope: `Project`, `Sandbox`, `AgentConfig`, `SandboxProviderInstance`, `Worker`,
  `ServerState`, worker tokens, sandbox access issuer keys, project events, and
  tenant-local orchestration tables.
- Non-persisted catalog scope: `AgentConfigDefinition`. These definitions are
  read-only application data and only supply defaults for creating persisted
  `AgentConfig` instances.

Tenant-scoped rows still carry `tenant_id` and user ID columns so tokens,
events, and audit records are self-describing. Those columns are shard
boundaries and references to global identities; they are not foreign keys from
tenant databases back into the global schema.

Tenant-local schemas must still define primary keys and unique indexes as if all
tenants could share one physical database/table set. The Postgres driver may
collapse global and all tenant-local data into the same physical database, so
tenant-scoped uniqueness must include a tenant boundary whenever the value is
only unique within one tenant. For project-scoped rows, `project_id` is a
sufficient boundary because projects are tenant-scoped; for tenant-level rows,
use `tenant_id`.

## Deletes

Mutable resource models use GORM's native soft-delete support by including a
`gorm.DeletedAt` field tagged with `gorm:"index" json:"-"`. Normal GORM
queries automatically exclude soft-deleted rows, while `Unscoped()` is reserved
for explicit administrative, recovery, or purge paths.

Do not add ad-hoc `deleted` booleans or nullable deletion timestamps for primary
resources. Use `gorm.DeletedAt` so deletes flow through GORM's built-in
`Delete` behavior and query scoping. Append-only/audit rows and operational
state rows that intentionally rely on hard deletes, such as project events or
server initialization state, should document that exception instead of adding
soft-delete fields.

## Shared Lifecycle Shape

Orchestrated resources embed `ResourceLifecycle`.

Current/proposed orchestrated resources:

- `Sandbox`
- `Worker`

Lifecycle fields include:

- `desiredState`: requested steady state.
- `phase`: observed user-facing state.
- `activeOperation`: current queued/running operation.
- `lastOperationStatus`: pending, running, success, or failed.
- `generation`: latest accepted intent.
- `observedGeneration`: latest reconciled generation.

## Relationship Sketch

```mermaid
erDiagram
    TENANT ||--o{ USER : has
    TENANT ||--o{ PROJECT : has
    TENANT ||--o{ WORKER : has

    USER ||--o{ PROJECT : owns
    USER ||--o{ SANDBOX : creates
    USER ||--o{ SANDBOX_ACCESS_ISSUER_KEY : has

    PROJECT ||--o{ SANDBOX : contains
    PROJECT ||--o{ SANDBOX_PROVIDER_INSTANCE : configures
    PROJECT ||--o{ AGENT_CONFIG : configures
    PROJECT ||--o{ PROJECT_EVENT : emits
    PROJECT ||--o{ SANDBOX_ACCESS_ISSUER_KEY : has
    PROJECT ||--o| SANDBOX_PROVIDER_INSTANCE : default_provider

    AGENT_CONFIG ||--o{ SANDBOX : runs
    SANDBOX_PROVIDER_INSTANCE ||--o{ SANDBOX : manages
    SANDBOX_PROVIDER_INSTANCE ||--o{ WORKER : runs
    WORKER ||--o{ WORKER_BOOTSTRAP_TOKEN : registers_with
    WORKER ||--o{ WORKER_AUTH_TOKEN : authenticates_with

    TENANT {
        string id
        string name
        string slug
    }

    USER {
        string id
        string tenant_id
        string email
        string provider
        string subject
    }

    PROJECT {
        string id
        string tenant_id
        string owner_user_id
        string name
        string slug
        string default_sandbox_provider_id
    }

    SERVER_STATE {
        string tenant_id
        string key
        json value
    }

    SANDBOX {
        string id
        string project_id
        string created_by_user_id
        string provider_instance_id
        string agent_config_id
        string source_url
        string source_ref
        string source_ref_type
        string source_directory
        json source_code_references
    }

    AGENT_CONFIG {
        string id
        string project_id
        string name
        string install_command
        string run_command
        json capabilities
    }

    SANDBOX_PROVIDER_INSTANCE {
        string id
        string project_id
        string type
        string name
        json config
        bytes encrypted_config
    }

    WORKER {
        string id
        string tenant_id
        string project_id
        string provider_instance_id
        string identity
        string public_key
        string key_type
        bool ready
        bool schedulable
        bool degraded
        json conditions
        datetime registered_at
        datetime last_seen_at
        datetime revoked_at
    }

    WORKER_BOOTSTRAP_TOKEN {
        string id
        string tenant_id
        string worker_id
        bytes token_hash
        datetime expires_at
        datetime used_at
        datetime revoked_at
    }

    WORKER_AUTH_TOKEN {
        string id
        string tenant_id
        string worker_id
        bytes token_hash
        datetime issued_at
        datetime expires_at
        datetime last_used_at
        datetime revoked_at
    }

    SANDBOX_ACCESS_ISSUER_KEY {
        string project_id
        string user_id
        string public_key
        bytes encrypted_private_key
        string key_type
        datetime rotated_at
        datetime revoked_at
    }

    PROJECT_EVENT {
        string id
        int seq
        string project_id
        string resource_type
        string resource_id
        string action
    }
```

Auth flows are documented in `internal/sandboxauth/DESIGN.md`.

Tenant database resolution is documented in `internal/database/DESIGN.md`.

## Worker Scheduling Status

Workers report three scheduling-relevant booleans directly on the worker row:

- `ready`: the worker agent/runtime is healthy.
- `schedulable`: the worker is willing to pull new sandbox work.
- `degraded`: the worker can still accept fallback work but should not be
  preferred.

Any richer Kubernetes-style conditions or pressure details are stored as an
opaque `conditions` JSON blob for display and diagnostics. The control plane
does not interpret that blob for scheduling.

The worker decides when local compute/storage/memory pressure should change
`schedulable` or `degraded`. The control plane uses a coarse preference:

- preferred: `ready=true`, `schedulable=true`, `degraded=false`.
- degraded: `ready=true`, `schedulable=true`, `degraded=true`.
- unavailable: not ready, not schedulable, drained, deleted, or revoked.
