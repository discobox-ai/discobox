# Sandbox Harness Design

This module owns the sandbox runtime environment and in-sandbox agent REST API
implementation.

The Go implementation serves the generated sandbox-agent subset of
`api/openapi/server.yaml` using the generated `api/sandboxgen` server scaffold.
It validates the sandbox's hard-coded project/sandbox identity, accepts
short-lived control-plane-signed tokens, and owns sandbox-local harness terminal
runtime operations.

## Package Map

| Package/path | Ownership |
| --- | --- |
| `cmd/discobox-sandbox-agent` | Binary entrypoint, config loading, signal handling, and server startup. Also dispatches the `init` PID-1 subcommand. |
| `boot` | The PID-1 `init` flow: resolves the sandbox user (replacing the retired `entrypoint.sh`), wires image-declared data/cache volumes and manifest sources from the primary volumes, binds the config volume onto `/etc/discobox`, writes desktop drop-ins, then execs the container's real init (systemd). See ADR 0007. |
| `config` | Local boot/config file parsing, environment overrides, defaults, and validation. Owns `image.json` parsing, including the `volumes` declaration and `%HOME%`/`%UID%`/`%GID%` token resolution. |
| `server` | HTTP router, generated OpenAPI handler adapter, PASETO auth middleware, and identity/scope validation. |
| `execs` | The sandbox runtime primitive: exec lifecycle, runtime metadata, systemd unit abstraction, stdout/stderr or PTY logging, shim launch, status socket, and attach. Harness terminals are execs. |
| `execs` (`shim.go`) | Per-exec child process: the local Unix socket attach/status/start API, the audit log, and the runtime status file. It no longer owns the process itself — see `procio`. |
| `procio` | Running a process and owning its descriptors: PTY versus pipes, stdin close, signal mapping, and exit status. No sockets, no frames, no attach — which is what makes its traps testable with a real process and nothing else. |
| `terminal` | Harness-terminal layer built on top of `execs`: image harness resolution, hook/file setup, and primary-terminal lifecycle. A terminal is an exec created in harness mode, tagged `harnessId`/`primary` in exec metadata; all runtime mechanics belong to `execs`. |
| `shimruntime` | The platform half of an exec attach: Unix socket setup, the HTTP upgrade, the PTY, and the screen emulator behind repaint-on-attach. The stream itself — attachers, ordering, buffering, exit retention — is the root module's `execstream/host`, which this drives and implements `host.Replayer` for. |
| `hooks` | Local Unix-socket collector and publisher protocol for coding-harness lifecycle hook payloads. |
| `nrica` | The sandbox's NRI plugin (`cmd/discobox-nri-ca`): on every container a nested Docker daemon creates, mounts the sandbox's CA trust bundles and injects proxy-trust env, so a user's Dockerfile or `docker run` never needs MITM awareness. See ADR 0015. |
| `resources` | Opaque cgroup/procfs/systemd-style resource snapshot collection for exec runtimes. |
| `store` | Sandbox-local SQLite/GORM audit log, observed terminal state snapshots, and retained resource blobs. |
| `Dockerfile` | Debian-based base sandbox runtime image with Docker, development tools, Chromium, socket-activated desktop access, code-server, and Nix tooling. Harness image builds live in their owning `harness/<type>` folders. |

## Boundary Rules

- Implement the generated in-sandbox terminal and exec API subset from `api/sandboxgen`;
  canonical route and DTO definitions live in `api/openapi/server.yaml`.
- Depend on root contracts and generated API types only for cross-module data.
- Do not import server internals or provider implementation packages.
- Keep pool registration and control-plane bootstrapping in the `pool-harness`
  module unless a shared contract belongs in the root module.
- Do not call back to the pool host-harness or server; resolved config is injected
  into the sandbox and read locally.
- Load the single immutable harness contract from
  `/usr/share/discobox/image.json`. Commands, static files, and config-mode
  behavior are image-owned; the sandbox manifest contributes selection, mode,
  and a non-secret project file overlay.
- Volume wiring is declarative and image-owned. `image.json` lists the paths the
  image needs persisted (`data`) or shared across the pool's sandboxes
  (`cache`); the pool host mounts four primary volumes (`/.discobox/{data,cache,config,sources}`)
  and the `boot` init flow wires each declared path onto its backing volume —
  bind when the target is empty, overlay (lower = image content) when it ships
  content. Cache paths are always a direct shared bind, never an overlay, because
  the cache volume is shared across concurrently running sandboxes. Sources are
  bind-mounted from `/.discobox/sources/<slug>` onto the targets named in the
  manifest. See ADR 0007.
- Render templated harness files locally at installation time against the public
  `SandboxConfig` object from the manifest. Keep API field names as the template
  surface and expose only deterministic, non-secret formatting helpers.
- Treat systemd as the source of truth for terminal unit liveness. Runtime JSON
  files identify known terminals; reconciliation joins those files with systemd
  status and shim status. Durable database-only exec records must be reconciled
  before they are returned: restore manager-owned runtime/socket paths, and do
  not preserve stale `starting` or `running` state when the unit is gone.
- Keep terminal and exec history local. The SQLite store records append-only
  lifecycle events, latest observed runtime state, and retained opaque resource
  samples, but REST runtime state should be derived from runtime/systemd/shim
  observations instead of an in-memory cache.
- A terminal is one primitive: an exec created in harness mode. The `terminal`
  layer resolves the image harness (or the `shell` fallback harness — a login
  shell — when the image has no harness), applies image/project files and hooks,
  injects the hook/terminal env, then calls `execs.Manager` with
  the resolved command, `TTY`, and `harnessId`/`primary` metadata. `execs.Manager`
  never learns what a harness is. Plain execs and terminals currently use
  separate `execs.Manager` instances (distinct runtime dirs); the API-level merge
  to a single `/execs` surface is pending.

## Development Images

`task build` is the no-argument build entry point for binaries and all local
images. `task build:images` builds only the pool host, sandbox base, and included
harness images.

`task dev` starts `cmd/discobox-docker-image-watch`, which initially builds the
pool, base sandbox, Codex, Claude Code, and OpenCode images. Each harness
Dockerfile extends `discobox-sandbox-agent:local` through its
`SANDBOX_AGENT_IMAGE` argument. The watcher tracks shared Docker/runtime inputs
plus each harness folder's Dockerfile, `image.json`, and configure script.
Harness-specific changes rebuild only that harness image; shared changes rebuild
the base and all affected harness images. Every successful build writes its
content-addressed development image reference to `.env`; pool and sandbox base
images also write their image digest.

- Every sandbox has a default terminal: on sandbox start the harness always
  launches exactly one primary terminal (`terminal.Service.EnsurePrimary`), so
  clients such as `discobox run` can rely on one existing and attach to it. The
  first start runs the resolved harness with the manifest prompt as arguments;
  later starts run the harness's `relaunchCommand` to resume the previous session
  instead of replaying the prompt. First-vs-subsequent is decided by a durable
  marker in the SQLite store (`AgentState`), so it survives restarts. When no
  harness is configured the primary terminal is a login shell (harness id `shell`)
  and the prompt is not passed, since a shell would run it as a command. The
  launched exec is tagged `primary` in metadata by the sandbox-agent; that tag
  cannot be requested through the terminal create API.
- There is one shim (`execs/shim.go`) and one framed attach mechanism. Attacher
  tracking, frame writes, output broadcast, exit frame emission, and pending
  resize state belong to `execstream/host` in the root module; keep Unix socket
  setup, the HTTP upgrade, and everything touching the PTY in `shimruntime`; keep
  process startup, status persistence, stream logging, and stdin-close behavior
  in `execs`. Before publishing terminal status or an exit frame, drain the
  PTY/pipes and flush the asynchronous log queue so status means all command
  output is available. The exec shim serves both TTY (terminal, `exec -t`) and
  stdout/stderr-pipe (plain exec) modes.
- stdout and stderr are separate frames, never merged by the shim: `frame.Stdout`
  and `frame.Stderr` (and the matching `LogStream` values on the audit log), so a
  client can route each the way a local command does — `disco exec cmd
  2>/dev/null` drops only stderr. Merging is the client's to do and loses no
  information; merging in the shim is irreversible. A TTY exec has nothing to
  split, since the kernel merges both onto the PTY before the shim reads them, so
  it emits `frame.Stdout` only and simply never uses `frame.Stderr`. Nothing on
  the wire distinguishes that from a pipe exec that wrote nothing to stderr, and
  nothing should. Only `frame.Stdout` is screen state.
- Frame types take the file descriptor numbers they carry — `Input` 0, `Stdout`
  1, `Stderr` 2 — with control frames (`Resize` 3 … `Ready` 8) after them. The
  wire format and its types live in the root module's `execstream/frame`, shared
  with the CLI, so the two ends of a stream cannot disagree about it. See
  [ADR 0008](../docs/adr/0008-attach-stream-packages.md).
- A pipe exec's output pipes are created by `procio` (`os.Pipe`), never by
  `cmd.StdoutPipe`/`StderrPipe`. `cmd.Wait` closes the pipes it made as soon as
  the process exits, and the owner waits in a goroutine alongside the readers, so
  those pipes race the readers and silently discard a fast command's entire
  output. `procio` also closes its copies of the write ends right after `Start`,
  so the readers see EOF at exit.
- Signal frames act on the exec's process group (`kill(-pgid)`) via
  `procio.Process.Signal`, which is its own session because every exec starts
  with `Setsid`. That also means the group is
  permanently *orphaned* — no member has a parent in the same session — and the
  kernel discards SIGTSTP, SIGTTIN, and SIGTTOU sent to an orphaned group. A
  `TSTP` frame therefore maps to **SIGSTOP**, which is never discarded; mapping
  it to SIGTSTP silently does nothing. Ctrl-Z typed into a TTY exec is unaffected
  because it is a byte, not a frame: the remote line discipline signals the
  foreground job, which is a child group of the shell and not orphaned. A command
  that *is* the session leader (`disco exec -t sleep 30`) cannot be stopped by
  Ctrl-Z for the same orphan rule — `ssh host sleep 30` behaves identically.
- Exit status uses the shell convention for signal deaths: `128+signum`, so an
  interrupted command reports 130 rather than Go's `ExitCode() == -1`, which
  loses the signal and reads as a generic failure. `procio.Status` carries it.
- An attacher joins the broadcast set before the `101` response is written, not
  after: a client that sees `101` may start the process immediately, and output
  broadcast before registration is lost. That ordering is now structural rather
  than a convention — `host.Attach` registers, then invokes
  `AttachOptions.Ready`, which is where `HandleAttach` writes its `101`, so a
  caller cannot reorder the two. The attacher registers buffering, so live
  frames cannot race the handshake bytes onto the wire.
- Terminal attach supports `?replay=true`, which repaints the current screen
  before live output so a client that connects after a program has been running
  sees its state, not just output produced from the attach onward. The repaint
  is a snapshot of an in-memory terminal emulator (`shimruntime.screenBuffer`, reached through `host.Replayer`,
  backed by `charmbracelet/x/vt`), not the raw transcript: the emulator is fed
  every output chunk in `Broadcast`, and a snapshot serializes the current
  screen, capped scrollback (`DefaultScrollbackLines`), the cursor position, and
  the input/rendering modes a TUI set before the client connected (mouse,
  bracketed paste, cursor keys, cursor visibility — tracked by scanning the
  output stream, since the emulator does not expose them). Only TTY execs have a
  screen: `Runtime.EnableScreen` installs the `Replayer` once the PTY exists, so a
  pipe exec never waits on a repaint handshake it cannot satisfy. The
  disk log (`AsyncLogger`) is no longer used for attach — it backs only the
  `terminal logs` command (full forensic transcript).
- Race-free snapshot: `host.Stream.Broadcast` feeds the `Replayer` and snapshots
  the attacher set under one lock, and registration captures the `Replayer`
  snapshot under that same lock. Every output chunk
  therefore falls on exactly one side of the attach: already absorbed into the
  snapshot, or buffered as a live frame from registration onward and flushed
  after the snapshot — so nothing is lost or duplicated. The shim withholds the
  snapshot until the client sends a `frame.Ready` (the CLI sends it once its
  output reader is running): writing during the HTTP upgrade handshake risks
  losing the leading bytes at an intermediate proxy hop that buffers them before
  its tunnel is wired up. `frame.Ready` proves the tunnel is established end to
  end; a bounded timeout still repaints (best effort) for clients that never
  send it.
- Terminal-query answering: the screen emulator responds to queries in the
  output stream (DA1, DSR, DECRQM, ...) by writing answers to an unbuffered
  internal pipe. `Runtime.pumpScreenResponses` must always drain that pipe —
  an undrained pipe blocks `Broadcast` inside the runtime mutex and deadlocks
  the whole shim (Claude Code emits DA1 right after its first paint). While no
  client is attached the answers are fed to the PTY so a headless TUI blocked
  on a startup query comes up; while a client is attached they are dropped,
  because the client's real terminal sees the query in the raw stream and
  answers it.
- The screen fails open, never closed: every emulator call runs under
  `Runtime.runScreenLocked`, which recovers a panic by dropping the screen —
  repaint-on-attach degrades to plain live streaming instead of the emulator
  bug killing the exec. The PTY handle outlives the screen for this reason.
- The program's repaint is authoritative: after a replay (snapshot present or
  not), `Runtime.redrawAfterReplay` jiggles the PTY one row smaller and back,
  so SIGWINCH makes the program redraw itself and the client converges to the
  program's real screen even when the snapshot was imperfect or missing.
- No phantom deadlines on attach: `http.Server` per-request read/write
  deadlines survive hijacks and websocket accepts, so long-lived attach
  streams must not inherit them. The shim and `shimproxy.AttachHTTPUpgrade`
  clear conn deadlines after hijacking, the harness HTTP servers set no
  `ReadTimeout`/`WriteTimeout` (only `ReadHeaderTimeout`/`IdleTimeout`), and
  both websocket ends of an attach (CLI dial, `shimproxy.AttachWebSocket`)
  run keepalive ping loops that close the tunnel when the peer stops
  answering.
