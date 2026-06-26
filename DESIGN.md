# Design Overview

## System Pattern

The server stores desired resource intent and reconciles actual sandbox runtime
state through sandbox providers. Providers own runtime-specific mechanics and
report or expose observed runtime state back to the server.

Accepted intent changes are persisted with project events and durable reconcile
jobs in one transaction. Reconcile jobs target one resource generation and are
canceled when superseded by newer intent.

## High-Level System Design

At the system boundary, Discobot is three cooperating concepts:

- **Server**: the control plane and API surface. It stores desired state and
  coordinates reconciliation.
- **Sandbox provider**: the Go-level runtime integration interface implemented
  by provider backends.
- **Worker-local sandbox operations API**: the REST/OpenAPI runtime-operation
  interface exposed by a worker agent for sandboxes hosted on that worker.
- **Sandbox agent API**: the future in-sandbox REST/OpenAPI interface exposed
  from inside an individual sandbox environment.

```mermaid
flowchart LR
    cli["CLI"] -->|"generated client"| server["Server / control plane"]
    clients["API clients / UI"] --> server
    server -->|"Go interface"| provider["Sandbox provider"]
    provider -->|"delegates access"| sandbox["Worker-local sandbox operations API"]
    server -->|"REST/OpenAPI through provider"| sandbox
    provider -->|"observed runtime state"| server
```

The root design intentionally stops at this integration view. Interface details
belong to the owning component docs.

## API Contracts

Use contract-first REST API development for public and provider-delegated REST
surfaces:

- Server REST API: control plane API consumed by CLI, UI, and external clients.
- Worker-local sandbox operations API: runtime operations exposed by worker
  agents and reached through provider-delegated access.
- Sandbox agent API: in-sandbox API exposed by the sandbox-agent runtime.

The OpenAPI contract is the canonical API definition. Generate server handlers,
client types, validators, and documentation from the contract instead of deriving
the contract from Go handler code. Current contracts are intentionally split by
surface:

- `api/openapi/server.yaml` is the canonical control-plane REST API contract.
- `worker-agent/api/openapi/worker.yaml` is the canonical worker-local sandbox
  operations API contract. `worker-agent/generate.go` generates combined
  client/server transport code into `worker-agent/api/gen` and stable schema
  aliases into `worker-agent/api/model`; `worker-agent/server` adapts the
  generated server scaffold to local runtime operations.
- Sandbox-agent terminal routes are canonical in `api/openapi/server.yaml` and
  marked for sandbox-agent subset generation. `api/openapi/sandbox.yaml` is
  generated from that server contract and must not be edited directly.
  Schema-only sandbox-agent contracts, such as `/etc/discobox/sandbox.json`,
  live in `api/openapi/server.yaml` and use `x-sandbox-agent-component: true`
  so the subset generator includes them in `api/openapi/sandbox.yaml` even when
  no REST operation references them.

## Target Module Boundaries

Make the repository root the stable contracts/API module. Server-owned
persistence, provider contracts, and provider implementations live in the server
module so provider adapters can use server-internal control-plane contracts.

```mermaid
flowchart TD
    cli["github.com/obot-platform/discobox/cli"] --> root["github.com/obot-platform/discobox"]
    server["github.com/obot-platform/discobox/server"] --> root
    server --> providers["github.com/obot-platform/discobox/server/providers"]
    server --> orchestration["github.com/obot-platform/discobox/orchestration"]
    server --> hooks["github.com/obot-platform/discobox/hooks"]
    server --> gormdb["github.com/obot-platform/discobox/gormdb"]
    hooks --> root
    hooks --> gormdb
    providers --> serverInternal["github.com/obot-platform/discobox/server/internal"]
    providers --> workerAgent["github.com/obot-platform/discobox/worker-agent"]
    workerAgent --> root
    sandboxAgent["github.com/obot-platform/discobox/sandbox-agent"] --> root
```

- Root module: public API definitions, control-plane OpenAPI documents,
  generated API clients/scaffolds, cross-module sentinel errors, IDs, worker
  boot metadata contracts, and client-facing stream DTOs.
- CLI module: `discobox` command implementation; depends on root generated
  clients/contracts for normal user commands and talks to the control plane
  through the Server REST API. Its `discobox server` subcommand embeds the
  server module's public runtime entrypoint so local auto-launch can re-exec the
  current CLI binary instead of depending on a separate `discobox-server`
  executable.
- Server module: control plane implementation, persistence models, sandbox
  provider Go interfaces, provider manager, and Docker/VM/cloud/worker-backed
  provider implementations.
- Hooks module: standalone hook discovery, watch, execution, daemon, and status
  primitives. It depends inward on stable contracts and shared infrastructure
  helpers such as `gormdb`, but must not depend on server internals. See
  [`hooks/DESIGN.md`](hooks/DESIGN.md).
- Worker-agent module: in-guest worker process, worker-local runtime DTOs, and
  generated worker-local sandbox operations API server adapter; depends on root
  worker boot contracts and OpenAPI contracts.
- Root module: local Docker development image watcher for worker-agent and
  sandbox-agent images.
- Sandbox-agent module: in-sandbox agent REST API runtime environment and agent
  implementation; depends on root contracts and generated API types.

Worker-agent and sandbox-agent implementations cannot depend on packages under
Go `internal/` outside their module. Provider implementations are part of the
server module and may depend on `server/internal`.

Root module package map:

| Package/path | Ownership |
| --- | --- |
| [`api/openapi`](api/openapi) | Canonical OpenAPI source contracts owned by the root module: the server REST API, plus generated sandbox-agent subset output. Worker-agent-owned contracts live under `worker-agent/api/openapi`. |
| [`api/gen`](api/gen) | Generated client/server API scaffold from `api/openapi/server.yaml`, plus handwritten client helpers for transports OpenAPI generation cannot own. |
| [`api/sandboxgen`](api/sandboxgen) | Generated client/server API scaffold from generated `api/openapi/sandbox.yaml`, the sandbox-agent subset of the server contract. |
| [`api/model`](api/model) | Generated stable aliases for server REST API schema types. |
| [`harness`](harness) | Coding-agent hook registration drivers for sandbox terminal agents. |
| [`id`](id) | Shared identifier helpers. |

Submodule package docs belong in their owning module trees and are intentionally
not listed here.

## UI Design System

The `ui` package uses SvelteKit, Tailwind CSS, and shadcn-svelte as its design
system. UI work should use generated shadcn-svelte primitives from
`ui/src/lib/components/ui` and shadcn token utilities such as `bg-background`,
`text-foreground`, `border-border`, `bg-card`, `text-muted-foreground`, and
`text-destructive`.

The global UI CSS should follow the shadcn-svelte Tailwind/token layer used by
Discobot, with theme state driven by the `.dark` class, `data-theme`, and CSS
variables.
