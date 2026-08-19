# Repository Guidelines

## Project Structure

- Root module `github.com/obot-platform/discobox`: stable contracts/API module.
- `api`: server API definitions and tests, pending schema-first conversion.
- `cli`: nested Go module for the `disco` CLI.
- `cli/cmd/disco`: CLI entrypoint.
- `cli/internal/cli`: CLI command implementation.
- `server`: nested Go module for the control plane implementation.
- `server/cmd/discobox-server`: HTTP server entrypoint.
- `server/internal/server`: server startup and HTTP router wiring.
- `server/internal/service`: API-facing business logic, orchestration wrappers, and reconcilers.
- `server/internal/sandbox`: sandbox provider Go contract, provider manager, and shared provider types.
- `server/internal/store`: database access, split by resource.
- `server/internal/database`: database setup and resolution.
- `server/internal/events`: project event broker.
- `server/internal/auth/sandbox`: sandbox and pool agent authentication helpers.
- `server/providers`: Docker, VM, cloud, and pool-backed provider implementations.
- `pool-agent`: nested Go module for the pool agent implementation.
- `sandbox-agent`: nested Go module and image context for the sandbox agent runtime environment.
- `termpane`: nested Go module; a reusable Bubble Tea component that draws a live terminal from any stream. No dependency on the rest of the repository.
- `pool-agent/cmd/discobox-pool-agent`: pool agent entrypoint.
- `cmd/discobox-docker-image-watch`: local Docker image rebuild watcher for pool-agent and sandbox-agent images.
- `docs`: user/developer documentation.
- `test`: integration and Bats tests.
- `gormdb`: nested Go module for DB setup helpers.
- `orchestration`: nested Go module for durable jobs and desired-state orchestration helpers.
- `DESIGN.md` / `REVIEW.md`: package-local design and review notes. Read the closest files in the current package and its parents before making design-sensitive changes.

## Git Workflow

Work directly on whatever branch is already checked out. If the session starts
on `main`, commit to `main`; if it starts on a feature branch, keep committing
to that branch.

Do not create new branches or worktrees unless explicitly told to.

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

## Architecture Decision Records

`docs/adr` records decisions and the alternatives rejected. Write an ADR only
when a plausible alternative was rejected for a non-obvious reason, or something
was deferred with a condition for revisiting it. Otherwise skip it and update
the relevant `DESIGN.md`; most changes need no ADR.

ADRs are immutable once accepted — supersede, never edit. They live outside the
`DESIGN.md`/`REVIEW.md` drill-down hierarchy and are not read root-down: they
are history, while `DESIGN.md` is current state.

The process is Nygard-style ADRs plus current-state design docs:

1. Draft the ADR as `Proposed` and land it on its own before implementation.
   Flipping it to `Accepted` is the decision gate; implementation builds
   against an accepted ADR.
2. During implementation, the accepted ADR is the spec. `DESIGN.md` never
   describes in-progress or planned work.
3. Every change that alters the architecture updates the affected `DESIGN.md`
   files in the same change as the code, so design docs and code are never out
   of sync.
4. Sequencing and implementation plans belong in the task/branch that does the
   work, never in an ADR or `DESIGN.md`.
5. If implementation proves a decision wrong: amend while nothing has shipped
   against it, supersede after.

See `docs/adr/README.md`.
