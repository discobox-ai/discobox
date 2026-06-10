# Model Design

This package contains the current persisted/API model. Today the Go structs carry
both GORM persistence tags and JSON/Huma API-facing tags.

## Core Entities

| Entity | Description |
| --- | --- |
| `Tenant` | Planned top-level boundary for users, projects, and their resources. |
| `User` | Authenticated person. Belongs to a tenant, owns projects, and creates sandboxes. |
| `Project` | Tenant-scoped group for sandboxes, provider configuration, and project events. |
| `Sandbox` | Main managed runtime/session resource. Belongs to a project and is orchestrated. |
| `SandboxProviderInstance` | Project-scoped provider configuration for creating and managing sandboxes. |
| `Worker` | Planned provider-backed runtime worker for launching sandboxes. Has its own identity and public key; private key stays on the worker. |
| `WorkerBootstrapToken` | Planned short-lived, one-time token used by a new worker to register its public key. |
| `WorkerAuthToken` | Planned short-lived runtime token issued after the worker proves possession of its private key; may be stateless. |
| `SandboxAccessIssuerKey` | Design-level name for the current `ProjectUserKey`: per-project, per-user issuer key used by the control plane to sign sandbox access tokens. |
| `ProjectEvent` | Append-only project-scoped resource event for list/watch sync and replay. |

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

    USER ||--o{ PROJECT : owns
    USER ||--o{ SANDBOX : creates
    USER ||--o{ SANDBOX_ACCESS_ISSUER_KEY : has

    PROJECT ||--o{ SANDBOX : contains
    PROJECT ||--o{ SANDBOX_PROVIDER_INSTANCE : configures
    PROJECT ||--o{ PROJECT_EVENT : emits
    PROJECT ||--o{ SANDBOX_ACCESS_ISSUER_KEY : has
    PROJECT ||--o| SANDBOX_PROVIDER_INSTANCE : default_provider

    SANDBOX_PROVIDER_INSTANCE ||--o{ SANDBOX : manages
    SANDBOX_PROVIDER_INSTANCE ||--o{ WORKER : runs
    SANDBOX ||--o{ WORKER : uses
    WORKER ||--o{ WORKER_BOOTSTRAP_TOKEN : registers_with

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

    SANDBOX {
        string id
        string project_id
        string created_by_user_id
        string provider_instance_id
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
        string sandbox_id
        string provider_instance_id
        string identity
        string public_key
        string key_type
        datetime registered_at
        datetime last_seen_at
        datetime revoked_at
    }

    WORKER_BOOTSTRAP_TOKEN {
        string id
        string worker_id
        bytes token_hash
        datetime expires_at
        datetime used_at
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
