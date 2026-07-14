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
| `execs` | The sandbox runtime primitive: exec lifecycle, runtime metadata, systemd unit abstraction, stdout/stderr or PTY logging, shim launch, status socket, and attach. Agent terminals are execs. |
| `execs` (`shim.go`) | Per-exec child process that owns the PTY/pipes and local Unix socket attach/status/start API, used by both plain execs and terminals. |
| `terminal` | Agent-terminal layer built on top of `execs`: agent resolution, install (run as ephemeral execs), and primary-terminal lifecycle. A terminal is an exec created in agent mode, tagged `agentId`/`primary` in exec metadata; all runtime mechanics belong to `execs`. |
| `terminal/frame` | Docker-exec-style binary stream framing shared by exec attach endpoints. |
| `shimruntime` | Shared local shim attach runtime for Unix socket setup, HTTP upgrade handling, framed stream attachers, broadcast, exit frames, and pending resize state. |
| `hooks` | Local Unix-socket collector and publisher protocol for coding-agent lifecycle hook payloads. |
| `resources` | Opaque cgroup/procfs/systemd-style resource snapshot collection for exec runtimes. |
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
- Render templated agent files locally at installation time against the public
  `SandboxConfig` object from the manifest. Keep API field names as the template
  surface and expose only deterministic, non-secret formatting helpers.
- Treat systemd as the source of truth for terminal unit liveness. Runtime JSON
  files identify known terminals; reconciliation joins those files with systemd
  status and shim status.
- Keep terminal and exec history local. The SQLite store records append-only
  lifecycle events, latest observed runtime state, and retained opaque resource
  samples, but REST runtime state should be derived from runtime/systemd/shim
  observations instead of an in-memory cache.
- A terminal is one primitive: an exec created in agent mode. The `terminal`
  layer resolves the agent (explicit request, sandbox resolved config, local
  repo `.discobox` config, or default), runs the agent's install command as an
  ephemeral exec, injects the hook/terminal env, then calls `execs.Manager` with
  the resolved command, `TTY`, and `agentId`/`primary` metadata. `execs.Manager`
  never learns what an agent is. Plain execs and terminals currently use
  separate `execs.Manager` instances (distinct runtime dirs); the API-level merge
  to a single `/execs` surface is pending.
- On sandbox start the agent launches one primary terminal from the manifest
  prompt (`terminal.Service.EnsurePrimary`). The first start runs the resolved
  agent with the prompt as arguments; later starts run the agent's
  `relaunchCommand` to resume the previous session instead of replaying the
  prompt. First-vs-subsequent is decided by a durable marker in the SQLite store
  (`AgentState`), so it survives restarts. The launched exec is tagged
  `primary` in metadata by the sandbox-agent; that tag cannot be requested
  through the terminal create API.
- There is one shim (`execs/shim.go`) and one framed attach mechanism. Keep Unix
  socket setup, HTTP upgrade, attacher tracking, frame writes, output broadcast,
  exit frame emission, and pending resize handling in `shimruntime`; keep
  process startup, status persistence, stream logging, and stdin-close behavior
  in `execs`. The exec shim serves both TTY (terminal, `exec -t`) and
  stdout/stderr-pipe (plain exec) modes.
- Terminal attach supports `?replay=true`, which repaints the current screen
  before live output so a client that connects after a program has been running
  sees its state, not just output produced from the attach onward. The repaint
  is a snapshot of an in-memory terminal emulator (`shimruntime.screenBuffer`,
  backed by `charmbracelet/x/vt`), not the raw transcript: the emulator is fed
  every output chunk in `Broadcast`, and a snapshot serializes the current
  screen, capped scrollback (`DefaultScrollbackLines`), the cursor position, and
  the input/rendering modes a TUI set before the client connected (mouse,
  bracketed paste, cursor keys, cursor visibility — tracked by scanning the
  output stream, since the emulator does not expose them). Only TTY execs have a
  screen (`Runtime.EnableScreen`); plain execs attach without a repaint. The
  disk log (`AsyncLogger`) is no longer used for attach — it backs only the
  `terminal logs` command (full forensic transcript).
- Race-free snapshot: `shimruntime.Broadcast` feeds the emulator and snapshots
  the attacher set under one lock, and `addReplayAttacher` captures the emulator
  snapshot and registers the attacher under that same lock. Every output chunk
  therefore falls on exactly one side of the attach: already absorbed into the
  snapshot, or buffered as a live frame from registration onward and flushed
  after the snapshot — so nothing is lost or duplicated. The shim withholds the
  snapshot until the client sends a `frame.Ready` (the CLI sends it once its
  output reader is running): writing during the HTTP upgrade handshake risks
  losing the leading bytes at an intermediate proxy hop that buffers them before
  its tunnel is wired up. `frame.Ready` proves the tunnel is established end to
  end; a bounded timeout still repaints (best effort) for clients that never
  send it.
