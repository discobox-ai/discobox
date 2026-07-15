# Repository Guidelines

## Project Structure

- Root module `github.com/obot-platform/discobox`: stable contracts/API module.
- `api`: server API definitions and tests, pending schema-first conversion.
- `cli`: nested Go module for the `discobox` CLI.
- `cli/cmd/discobox`: CLI entrypoint.
- `cli/internal/cli`: CLI command implementation.
- `server`: nested Go module for the control plane implementation.
- `server/cmd/discobox-server`: HTTP server entrypoint.
- `server/internal/server`: server startup and HTTP router wiring.
- `server/internal/service`: API-facing business logic, orchestration wrappers, and reconcilers.
- `server/internal/sandbox`: sandbox provider Go contract, provider manager, and shared provider types.
- `server/internal/store`: database access, split by resource.
- `server/internal/database`: database setup and resolution.
- `server/internal/projectstream`: project event streaming websocket and SSE routes.
- `server/internal/events`: project event broker.
- `server/internal/auth/sandbox`: sandbox and worker authentication helpers.
- `server/providers`: Docker, VM, cloud, and worker-backed provider implementations.
- `worker-agent`: nested Go module for the worker agent implementation.
- `sandbox-agent`: nested Go module and image context for the sandbox agent runtime environment.
- `worker-agent/cmd/discobox-worker-agent`: worker agent entrypoint.
- `cmd/discobox-docker-image-watch`: local Docker image rebuild watcher for worker-agent and sandbox-agent images.
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
go tool task check-hooks # validate hook definitions and hook-related code
go tool task rerun-hooks # re-run failed or never-run hooks
go tool task generate   # regenerate generated files
go tool task build      # build server, CLI, and hooks CLI
```

At the end of a code-changing task, run `go tool task check-hooks` to validate
the code and hook definitions before handing work back. If you suspect the
check-hooks output is stale (for example, a reported failure refers to code you
have already fixed), run `go tool task rerun-hooks` to re-run failed hooks,
then run `go tool task check-hooks` again.

Prefer adding or updating `Taskfile.yml` targets instead of documenting ad hoc
commands here.

## Implementation Quality

Prefer proper structural changes over compatibility shims or narrow patches.

- Do not introduce optional interfaces for behavior that the system now
  requires. Add required methods to the core interface and update all
  implementations.
- Avoid optional interfaces by default. Use them only when the optionality
  provides an actual functional runtime benefit inherent to the system, such as
  a capability that genuinely may or may not exist at runtime. Do not use
  optional interfaces to avoid updating implementations, preserve old call
  shapes, or make a diff smaller.
- Do not add wrapper types, adapter layers, or small abstraction seams just to
  avoid touching callers. If the design belongs in an existing core type, put
  it there.
- Do not add helper wrappers around existing functions just to preserve old call
  shapes or reduce call-site edits. Change the call sites directly instead;
  unnecessary wrappers become maintenance cruft.
- Treat existing databases and persisted state as durable. Do not delete or
  recreate a database to apply a schema change. Design schema changes with a
  safe upgrade path, including migrations and backfills when required.
- Avoid tiny tactical patches when the correct fix crosses package boundaries.
  Follow the ownership path through the codebase and update the model,
  interfaces, implementations, tests, and call sites together.
- Keep abstractions justified by durable ownership or meaningful complexity
  reduction. If an abstraction only exists to make the diff smaller, remove it.

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
