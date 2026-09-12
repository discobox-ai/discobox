# Repository Guidelines

## Project Structure

- Root module `github.com/discobox-ai/discobox`: stable contracts/API module.
  `go.work` joins it and the nested modules below.
- `base-image`: the shared Debian/Docker/systemd image the pool-agent and
  sandbox-agent images are built FROM (and, through the sandbox-agent image,
  every harness image).
- `vm-image`: the pool VM guest image every VM-backed provider boots, plus the
  libkrunfw-patched kernel libkrun alone needs. Both are released on their own
  lines and pulled from a registry at run time.
- `api`: the Server REST API contract: canonical OpenAPI documents
  (`api/openapi`), ogen-generated scaffolds (`api/gen`, `api/sandboxgen`),
  model aliases (`api/model`), and their generators (`api/internal`).
- `cli`: nested Go module for the `discobox` CLI.
- `cli/cmd/discobox`: CLI entrypoint.
- `cli/internal/cli`: CLI command implementation.
- `server`: nested Go module for the control plane implementation.
- `server/cmd/discobox-server`: HTTP server entrypoint.
- `server/internal/server`: server startup and HTTP router wiring.
- `server/internal/handlers`: HTTP API handlers.
- `server/internal/service`: aggregates the resource services and owns process-level service startup, shutdown, and default data.
- `server/internal/resources`: per-resource API services, lifecycle intent, and the reconcilers that converge it.
- `server/internal/reconcile`: level-triggered reconciliation engine (dirty set, lease-based claiming).
- `server/internal/sandbox`: sandbox provider Go contract, provider manager, and shared provider types.
- `server/internal/store`: database access, split by resource.
- `server/internal/database`: database setup and resolution.
- `server/internal/auth`: API authentication and authorization; `auth/sandbox` issues sandbox access tokens, `auth/poolagent` signs control-plane requests to pool agents.
- `server/providers`: Docker, VM, cloud, and pool-backed provider implementations.
- `pool-agent`: nested Go module for the pool agent implementation.
- `pool-agent/cmd/discobox-pool-agent`: pool agent entrypoint.
- `sandbox-agent`: nested Go module and image context for the sandbox agent runtime environment.
- `access`: nested Go module for `discobox-access`, the in-sandbox client of the agent credentials protocol.
- `termpane`: nested Go module; a reusable Bubble Tea component that draws a live terminal from any stream. No dependency on the rest of the repository.
- Other root-module packages (`execstream`, `harness`, `proxy`, `agentcreds`,
  `sandboxconfig`, `sandboxuser`, `runcca`, `endpoint`, and more): shared
  cross-module contracts and components; see the package map in `DESIGN.md`.
- `internal/cmd`: development and build tools, under `internal` because they are
  this repository's own and nothing outside it should build them:
  `discobox-docker-image-watch` (local base, pool-agent, sandbox-agent, and
  harness image rebuild watcher), `discobox-dev-lock` (one `task dev` loop per checkout),
  `discobox-gzip` (portable file compression for the build),
  `discobox-server-manifest` (the server manifest a release CLI is linked with),
  `discobox-installers` (stamps the install scripts a release uploads with that
  release and its binaries' digests), `discobox-installer-logo` (draws the TUI's
  mark into those scripts, from the TUI's own cell data).
- `scripts`: shell and Node helpers the Taskfile and hooks run.
- `docs`: user/developer documentation and ADRs (`docs/adr`).
- `test`: Bats integration tests, the test-only harness stub image, and terminal performance tests.
- `DESIGN.md` / `REVIEW.md`: package-local design and review notes. Read the closest files in the current package and its parents before making design-sensitive changes.

## Git Workflow

Work directly on whatever branch is already checked out. If the session starts
on `main`, commit to `main`; if it starts on a feature branch, keep committing
to that branch.

Do not create new branches or worktrees unless explicitly told to.

## Commands

The toolchain comes from the Nix flake. Enter it with `nix develop`, or let
direnv do it via `.envrc`; every command below assumes that shell.

Use Taskfile targets through the Go tool-managed `task` binary:

```bash
go tool task --list
```

Common targets:

```bash
go tool task test       # root module tests
go tool task test:all   # root and nested module tests
go tool task check      # static checks
go tool task check-hooks # wait for background hook work and report what failed
go tool task rerun-hooks # re-run failed or never-run hooks
go tool task generate   # regenerate generated files
go tool task build      # build all binaries and local Docker images
```

What CI runs, and what to run before pushing something build-related:

```bash
go tool task ci:check   # check, the windows/amd64 cross type-check, and Dockerfile COPY paths
go tool task ci:test    # every module's tests, the way CI runs them
go tool task verify     # fmt, go.mod, generated files, and Mermaid are current
```

At the end of a code-changing task, run `go tool task check-hooks` before
handing work back. The hooks in `.discobox/hooks` run in the background as
files change — formatting, tidy, codegen, Dockerfile builds, lint, tests,
Mermaid validation — and this is what
reports whether any of them failed. If its output looks stale, meaning a
reported failure names code you have already fixed, run
`go tool task rerun-hooks` and check again. The hooks are this repository's;
what runs them is the `discobox-hooks` tool from `discobox-ai/hooks`,
declared in the root `go.mod`.

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
