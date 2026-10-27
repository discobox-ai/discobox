# Repository Guidelines

## Project Structure

- Root module `github.com/discobox-ai/discobox`: stable contracts/API module.
  `go.work` joins it and the nested modules below.
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
  `server/providers/dockerworker/sandbox-image` is the Docker driver's Debian
  sandbox base image every sandbox container runs, with its platform and
  harness bundles applied, and every Docker pool's container, with the pool
  bundle applied.
- `pool-agent`: nested Go module for the pool agent implementation.
- `pool-agent/cmd/discobox-pool-agent`: pool agent entrypoint.
- `sandbox-agent`: nested Go module for the sandbox agent runtime environment, and the platform bundle's build context.
- `access`: nested Go module for `discobox-access`, the in-sandbox client of the agent credentials protocol.
- `termpane`: nested Go module; a reusable Bubble Tea component that draws a live terminal from any stream. No dependency on the rest of the repository.
- `pkg`: the root module's shared cross-module library packages (`pkg/execstream`,
  `pkg/agentcreds`, `pkg/sandboxconfig`, `pkg/sandboxuser`, `pkg/runcca`,
  `pkg/endpoint`, and more); see the package map in `DESIGN.md`. New shared
  packages go here, not at the repository root.
- `harness`, `proxy`: root-module components kept at the root because each is
  a component in its own right (one bundle build context per harness; the pool
  proxy) rather than a library package.
- `internal/cmd`: development and build tools, under `internal` because they are
  this repository's own and nothing outside it should build them:
  `discobox-docker-image-watch` (local sandbox base image, and agent, pool,
  platform and harness bundle, rebuild watcher), `discobox-dev-lock` (one `task dev` loop per checkout),
  `discobox-gzip` (portable file compression for the build),
  `discobox-native-ci` (native macOS build/sign/checks and Windows tests/smoke),
  `discobox-server-manifest` (the server manifest a release CLI is linked with),
  `discobox-installers` (stamps the install scripts a release uploads with that
  release and its binaries' digests), `discobox-installer-logo` (draws the console's
  mark into those scripts, from the console's own cell data), `discobox-winres` (the
  Windows version resource a release executable links, and the check that it did),
  `discobox-judge-eval` (asks a harness's judge about the recorded cases in
  `test/judge-evals` and scores it).
- `scripts`: shell and Node helpers the Taskfile and hooks run.
- `docs`: user/developer documentation and ADRs (`docs/adr`).
- `test`: Bats integration tests, the test-only harness stub image, terminal performance tests, and the judge evals.
- `DESIGN.md` / `REVIEW.md`: package-local design and review notes. Read the closest files in the current package and its parents before making design-sensitive changes.

## Git Workflow

Work directly on whatever branch is already checked out. If the session starts
on `main`, commit to `main`; if it starts on a feature branch, keep committing
to that branch.

Do not create new branches or worktrees unless explicitly told to.

## Commands

The host development toolchain comes from `nix develop` or direnv. CI and
release toolchains run in Dagger containers and require the pinned Dagger CLI
(`v1.0.0-beta.21`) and a container runtime. See `docs/dagger.md`.

```bash
dagger check                         # complete portable CI, builds and trust checks
dagger api call ci-artifacts export --path=build/ci
dagger api call preview export --path=build/release/preview
go tool task --list                  # shortcuts and host development tasks
go tool task dev                     # local development server and watcher
```

`Taskfile.yml` delegates CI, verification, generation, and release operations
to `.dagger/modules/pipeline`. Implement new pipeline behavior there, not in
Actions or Taskfile shell blocks. Dagger never calls Task. Native macOS and
Windows jobs invoke `internal/cmd/discobox-native-ci` directly; Task exposes
local shortcuts. Publishing requires same-version/revision macOS artifacts,
signing/smoke evidence, and verified final digests. A Linux pass does not prove
native execution; inspect all three CI jobs.

At the end of a code-changing task, run `go tool task check-hooks`. If a hook
failure names code already fixed, run `go tool task rerun-hooks` and check
again. Hooks remain owned by `.discobox/hooks` and run the same Dagger-backed
entry points as developers. `discobox-hooks` is a root `go.mod` tool.

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

An ADR's ID is `YY-MM-DD-RRR`: the date it is written plus three random decimal
digits, generated rather than taken from a sequence, so concurrent branches
cannot collide. Never renumber an ADR, and never pick "the next number" — ADRs
`0001`–`0149` predate the scheme and keep their four-digit numbers, except four
renumbered to resolve a duplicated number. See `docs/adr/README.md`.

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
