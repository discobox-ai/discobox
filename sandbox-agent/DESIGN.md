# Sandbox Agent Design

This module owns the sandbox runtime environment and in-sandbox agent REST API
implementation.

The Go implementation serves the generated sandbox-agent subset of
`api/openapi/server.yaml` using the generated `api/sandboxgen` server scaffold.
It validates the sandbox's hard-coded project/sandbox identity, accepts
short-lived control-plane-signed tokens, and owns sandbox-local agent terminal
runtime operations.

## Package Map

| Package/path | Ownership |
| --- | --- |
| `cmd/discobox-sandbox-agent` | Binary entrypoint, config loading, signal handling, and server startup. |
| `config` | Local boot/config file parsing, environment overrides, defaults, and validation. |
| `server` | HTTP router, generated OpenAPI handler adapter, PASETO auth middleware, and identity/scope validation. |
| `terminal` | Agent terminal lifecycle, runtime metadata, systemd unit abstraction, and attach proxying. |
| `terminal/shim` | Per-terminal child process that owns the PTY and local Unix socket attach/status API. |
| `terminal/frame` | Docker-exec-style binary stream framing shared by terminal attach endpoints. |
| `execs` | Ephemeral sandbox exec lifecycle, runtime metadata, systemd unit abstraction, stdout/stderr or PTY logging, and shim launch. |
| `shimruntime` | Shared local shim attach runtime for Unix socket setup, HTTP upgrade handling, framed stream attachers, broadcast, exit frames, and pending resize state. |
| `hooks` | Local Unix-socket collector and publisher protocol for coding-agent lifecycle hook payloads. |
| `resources` | Opaque cgroup/procfs/systemd-style resource snapshot collection for terminal runtimes. |
| `store` | Sandbox-local SQLite/GORM audit log, observed terminal state snapshots, and retained resource blobs. |
| `Dockerfile` | Debian-based systemd sandbox runtime image with Docker, development tools, Chromium, socket-activated desktop access, code-server, and Nix tooling. |

## Boundary Rules

- Implement the generated in-sandbox terminal and exec API subset from `api/sandboxgen`;
  canonical route and DTO definitions live in `api/openapi/server.yaml`.
- Depend on root contracts and generated API types only for cross-module data.
- Do not import server internals or provider implementation packages.
- Keep worker registration and control-plane bootstrapping in the `worker-agent`
  module unless a shared contract belongs in the root module.
- Do not call back to the worker-agent or server; resolved config is injected
  into the sandbox and read locally.
- Treat systemd as the source of truth for terminal unit liveness. Runtime JSON
  files identify known terminals; reconciliation joins those files with systemd
  status and shim status.
- Keep terminal and exec history local. The SQLite store records append-only
  lifecycle events, latest observed runtime state, and retained opaque resource
  samples, but REST runtime state should be derived from runtime/systemd/shim
  observations instead of an in-memory cache.
- On sandbox start the agent launches one primary terminal from the manifest
  prompt (`EnsurePrimary`). The first start runs the resolved agent with the
  prompt as arguments; later starts run the agent's `relaunchCommand` to resume
  the previous session instead of replaying the prompt. First-vs-subsequent is
  decided by a durable marker in the SQLite store (`AgentState`), so it survives
  restarts. The launched terminal carries the `primary` flag; that flag is
  owned by the sandbox-agent and cannot be requested through the terminal
  create API.
- Terminal and exec shims share the same framed attach mechanics. Keep Unix
  socket setup, HTTP upgrade, attacher tracking, frame writes, output broadcast,
  exit frame emission, and pending resize handling in `shimruntime`; keep
  resource-specific process startup, status persistence, stream logging, stdin
  close behavior, and signal targeting in `terminal/shim` or `execs`.
- Do not duplicate attach-loop or resize-handling logic between terminal and
  exec shims. Extend `shimruntime` when a new behavior is stream plumbing, and
  leave only semantic differences in the terminal or exec package.
- Terminal attach supports `?replay=true`, which streams the recorded session
  history before live output. The disk log (`AsyncLogger`) is the authoritative
  history; the shim never buffers the whole transcript in memory. Race-free
  cutover: `shimruntime.Broadcast` advances an output-byte offset and snapshots
  the attacher set under one lock, so a replay attacher registers atomically
  with the offset it captures. Every output chunk falls on exactly one side of
  the cutover — replayed from disk (below) or buffered live from registration
  (at/above) — so nothing is lost or duplicated. The shim waits for the async
  logger to persist the cutover bytes (`WaitForFlush`) before reading history
  back with `terminal.StreamOutput` (output entries only; the PTY already echoes
  input into output). The shim withholds the history stream until the client
  sends a `frame.Ready` (the CLI sends it once its output reader is running):
  writing history during the HTTP upgrade handshake risks losing the leading
  bytes at an intermediate proxy hop that buffers them before its tunnel is
  wired up. `frame.Ready` proves the tunnel is established end to end; a bounded
  timeout still replays (best effort) for clients that never send it.
