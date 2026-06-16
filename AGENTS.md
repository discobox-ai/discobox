# Repository Guidelines

## Project Structure

- Root module `github.com/obot-platform/discobox`: stable contracts/API module.
- `api`: server API definitions and tests, pending schema-first conversion.
- `apiclient`: generated/client-side API helpers.
- `sandboxprovider`: sandbox provider Go contract, provider manager, and shared provider types.
- `cli`: nested Go module for the `discobox` CLI.
- `cli/cmd/discobox`: CLI entrypoint.
- `cli/internal/cli`: CLI command implementation.
- `server`: nested Go module for the control plane implementation.
- `server/cmd/discobox-server`: HTTP server entrypoint.
- `server/internal/server`: server startup and HTTP router wiring.
- `server/internal/service`: API-facing business logic, orchestration wrappers, and reconcilers.
- `server/internal/sandbox`: server-owned sandbox jobs, service glue, and reconcilers.
- `server/internal/store`: database access, split by resource.
- `server/internal/database`: database setup and tenant database resolution.
- `server/internal/projectstream`: project event streaming websocket and SSE routes.
- `server/internal/events`: project event broker.
- `server/internal/sandboxauth`: sandbox and worker authentication helpers.
- `providers`: nested Go module for Docker, VM, cloud, and worker-backed provider implementations.
- `worker-agent`: nested Go module for the worker agent implementation and local image watcher.
- `sandbox-agent`: nested Go module and image context for the sandbox agent runtime environment.
- `worker-agent/cmd/discobox-worker-agent`: worker agent entrypoint.
- `worker-agent/cmd/discobox-worker-agent-watch`: local worker-agent image rebuild watcher.
- `ui`: SvelteKit frontend application.
- `electron`: Electron shell for the UI.
- `docs`: user/developer documentation.
- `test`: integration and Bats tests.
- `gormdb`: nested Go module for DB setup helpers.
- `orchestration`: nested Go module for durable jobs and desired-state orchestration helpers.
- `DESIGN.md` / `REVIEW.md`: package-local design and review notes. Read the closest files in the current package and its parents before making design-sensitive changes.

## Commands

Use Taskfile targets through the Go tool-managed `task` binary:

```bash
go tool task --list
```

Common targets:

```bash
go tool task test       # root module tests
go tool task test:all   # root and nested module tests
go tool task check      # static checks
go tool task generate   # regenerate generated files
go tool task build      # build server, CLI, UI, and Electron shell
```

Prefer adding or updating `Taskfile.yml` targets instead of documenting ad hoc
commands here.

## Package Design Docs

Design guidance lives next to the code it describes:

- `DESIGN.md` explains the design of that package and its subdirectories.
- `REVIEW.md` lists review rules and pitfalls for that package and its subdirectories.

When working in a package, read `DESIGN.md` and `REVIEW.md` from the repository
root down to the package directory. Parent files provide broader context; closer
files override or specialize that guidance.

Lay out `DESIGN.md` files as a drill-down hierarchy:

- Root docs describe the system-level architecture and how major components
  relate.
- Child package docs describe that component's architecture at one deeper level.
- Do not duplicate lower-level details in parent docs; link to the child package
  or design doc instead.
- Prefer proper Mermaid diagrams for high-level structure and flows.
- Keep design and review docs optimized for LLM/agent context: short,
  directive, and easy to scan.
- Reference well-known patterns and project-specific decisions instead of
  explaining general concepts or restating code-level details.
