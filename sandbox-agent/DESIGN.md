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
| `boot` | The PID-1 `init` flow: resolves the sandbox user (replacing the retired `entrypoint.sh`), wires image-declared data/cache volumes and manifest sources from the primary volumes, binds the config volume onto `/etc/discobox`, seeds the user's `~/.gitconfig`, writes desktop drop-ins, then execs the container's real init (systemd). See ADR 0007. |
| `config` | Local boot/config file parsing, environment overrides, defaults, and validation. Owns `image.json` parsing, including the `volumes` declaration and `%HOME%`/`%UID%`/`%GID%` token resolution. |
| `server` | HTTP router, generated OpenAPI handler adapter, PASETO auth middleware, and identity/scope validation. Also hand-registers the exec attach/start routes and, for ADR 0024's SSH ingress, `GET .../tcp/attach` (`tcp_attach.go`): dials `host:port` from inside this process — sharing the sandbox's network namespace, unlike the pool-agent's container-IP-only `/http/{port}` — and bridges the raw TCP bytes to `execstream/frame` `Input`/`Stdout`/`CloseInput` frames over a websocket, gated by the `tcp:connect` scope. |
| `runuser` | The sandbox's run identity: merges the image/manifest/request layers ([`sandboxuser`](../sandboxuser/DESIGN.md)) and completes them against the image's own passwd/group database. The **only** package that reads those files for resolution, so faking them fakes them for every consumer including the `setuid` path. Used by `boot`, `execs`, and `terminal` so none of them derives identity separately. See [runuser/DESIGN.md](runuser/DESIGN.md). |
| `execs` | The sandbox runtime primitive: exec lifecycle, runtime metadata, systemd unit abstraction, stdout/stderr or PTY logging, shim launch, status socket, and attach. Harness terminals are execs. |
| `execs` (`shim.go`) | Per-exec child process: the local Unix socket attach/status/start API, the audit log, and the runtime status file. It no longer owns the process itself — see `procio`. |
| `procio` | Running a process and owning its descriptors: PTY versus pipes, stdin close, signal mapping, and exit status. No sockets, no frames, no attach — which is what makes its traps testable with a real process and nothing else. |
| `terminal` | Harness-terminal layer built on top of `execs`: image harness resolution, hook/file setup, primary-terminal lifecycle, and revive-in-place. A terminal is an exec created in harness mode, tagged `harnessId`/`primary` in exec metadata, and its exec id is its durable identity across runs (ADR 0038); all runtime mechanics belong to `execs`. |
| `shimruntime` | The platform half of an exec attach: Unix socket setup, the HTTP upgrade, the PTY, and the screen emulator behind repaint-on-attach. The stream itself — attachers, ordering, buffering, exit retention — is the root module's `execstream/host`, which this drives and implements `host.Replayer` for. |
| `hooks` | Local Unix-socket collector and publisher protocol for coding-harness lifecycle hook payloads. |
| `runcca` | The sandbox's runc wrapper (`cmd/discobox-runc`): installed as `runc` ahead of the real one on containerd's and dockerd's PATH, it mounts the sandbox's CA trust bundles and injects proxy-trust env into every container's OCI spec, so a user's Dockerfile or `docker run` never needs MITM awareness. See ADR 0020, which supersedes 0015's NRI plugin — containerd invokes NRI only from its CRI path, which dockerd does not use. Handles both `runc create` (the `docker run` path, via the containerd shim) and `runc run` (the `docker build` path, via BuildKit's executor). Bundles are staged per boot by `discobox-trust-ca.service` under `/run/discobox/proxy/ca-bundles`; they cannot live beside the CA in `/etc/discobox/proxy`, which is pool-agent's read-only mount. |
| `nestedbridge` | Discovers the nested Docker daemon's bridge address and publishes it under `/run` for the bridge-facing proxy forwarder and the runc wrapper. Also enumerates this sandbox's own directly-connected IPv4 networks (`LocalSubnets`), the resolution target for `sandboxconfig.LocalSubnetsToken`. |
| `proxyenv` | Renders `sandbox.json`'s proxy-trust env (`Env`/`ProxyEnvs`) as a systemd `EnvironmentFile`, for `docker.service` — started by socket activation, not spawned by sandbox-agent, so it inherits no container env and cannot be reached by any per-container injection. Resolves `sandboxconfig.LocalSubnetsToken` against `nestedbridge.LocalSubnets()`, the same substitution `runcca.proxyEnv` applies for nested containers. Run at boot by `discobox-render-proxy-env.service`, ordered before `docker.service`, writing to `/run/discobox/proxy/proxy.env` (not `/etc/discobox`, which is pool-agent's read-only mount). |
| `dockercache` | The sandbox's `docker` CLI wrapper (`cmd/discobox-docker`): installed as `docker` ahead of the real CLI on PATH, it gives `docker build` a BuildKit local cache on the pool-shared cache volume so a build in one sandbox is reused by the others. Only build commands are rewritten; everything else is exec'd straight through. |
| `agentstatus` | Computes the status a pool agent polls (ADR 0030): per-source git status and a diff stat against the manifest's base commit via bounded `git status`/`git diff --shortstat` shelling, and per terminal session (every terminal still on record, never one-shot execs) the harness session state (plus the OSC title each session's program last set, read from its shim's emulator) via the root module's `harness.SessionStateDeriver` capability (full state machine for claude-code; generic exec-liveness fallback otherwise). Computed fresh on every call, never cached. |
| `resources` | Opaque cgroup/procfs/systemd-style resource snapshot collection for exec runtimes. |
| `store` | Sandbox-local SQLite/GORM audit log, observed terminal state snapshots, retained resource blobs, and compressed exec/terminal transcript chunks (see ADR 0028). |
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
- Any code that reads `sandbox.json`'s `Env` to actually launch or configure
  something (an exec, a nested container's OCI spec, a systemd
  `EnvironmentFile`) must resolve `sandboxconfig.LocalSubnetsToken` via
  `nestedbridge.LocalSubnets()` first — pool-agent cannot know a sandbox's own
  directly-connected networks and leaves the token as a placeholder. Resolve it
  at the point of use, not once and cached: the nested Docker bridge and any
  user-created networks only exist after the sandbox has booted, so a value
  resolved earlier goes stale. `execs.EnvWithRuntimeDefaults`, `runcca.proxyEnv`,
  and `proxyenv.Render` are the three current call sites.
- Load the single immutable harness contract from
  `/usr/share/discobox/image.json`. Commands, static files, and config-mode
  behavior are image-owned; the sandbox manifest contributes selection, mode,
  and a non-secret project file overlay.
- Volume wiring is declarative and image-owned. `image.json` lists the paths the
  image needs persisted (`data`) or shared across the pool's sandboxes
  (`cache`); the pool host mounts the primary volumes (`/.discobox/{data,cache,config,sources,secrets}`)
  and the `boot` init flow wires each declared path onto its backing volume —
  bind when the target is empty, overlay (lower = image content) when it ships
  content. Cache paths are always a direct shared bind, never an overlay, because
  the cache volume is shared across concurrently running sandboxes. Sources are
  bind-mounted from `/.discobox/sources/<slug>` onto the targets named in the
  manifest. See ADR 0007.
- A clone-delivered local source's live origin, when present, arrives at
  `/.discobox/origins/<slug>` as a plain read-only bind the pool host already
  made directly onto that final path before the container started — unlike
  `sources`, `boot` does not rebind it from anywhere; it is simply present by
  the time `boot` runs. See ADR 0026.
- Render templated harness files locally at installation time against the public
  `SandboxConfig` object from the manifest. Keep API field names as the template
  surface and expose only deterministic, non-secret formatting helpers.
- Treat systemd as the source of truth for terminal unit liveness. Runtime JSON
  files identify known terminals; reconciliation joins those files with systemd
  status and shim status. Durable database-only exec records must be reconciled
  before they are returned: restore manager-owned runtime/socket paths, and do
  not preserve stale `starting` or `running` state when the unit is gone. "Gone"
  means unloaded, not inactive: `systemctl show` succeeds for a unit systemd
  never heard of and calls it inactive, so `UnitStatus.Loaded` — not a status
  error — is what demotes a vanished exec to `lost`. For a terminal, `exited`/
  `failed`/`lost` means "not running, revivable" rather than gone: its exec id
  is a durable identity, and attach/start relaunches it in place (ADR 0038).
- Resolve an exec's workdir after its run user and env, never before: an empty
  request takes the sandbox's configured default (the primary source
  directory), a relative path joins the working root, and a leading `~`/`~/`
  expands against the run user's home directory (`execs.HomeDir`, shared with
  the terminal layer so the two cannot disagree). `~` is how a caller outside
  the sandbox — the SSH ingress, whose sessions must start where a login shell
  would — names a path only the sandbox can resolve. An unresolvable `~` is an
  error, not a fallback to the default: silently starting somewhere else is
  what puts `scp` uploads in the source tree.
- Keep terminal and exec history local. The SQLite store records append-only
  lifecycle events, latest observed runtime state, and retained opaque resource
  samples, but REST runtime state should be derived from runtime/systemd/shim
  observations instead of an in-memory cache.
- The status endpoint (`GET .../status`, `status:read` scope) is answered
  fresh on every request from the authenticated caller — pool-agent's standing
  poll loop, per ADR 0030. It is never cached and sandbox-agent never pushes
  it anywhere on its own initiative, consistent with the boundary rule above.
- A terminal is one primitive: an exec created in harness mode. The `terminal`
  layer resolves the image harness (or the `shell` fallback harness — a login
  shell — when the image has no harness, or when the manifest declares a
  harness with no command, which is the control plane's way of naming that
  same shell without knowing which shell the run user has: ADR 0032), applies
  image/project files and hooks,
  injects the hook/terminal env, then calls `execs.Manager` with `TTY`,
  `harnessId`/`primary` metadata, `Shell: true`, and — for every harness except
  the `shell` fallback (which already is the shell) — `StartupCommand` set to the
  resolved harness command. `execs.Manager` never learns what a harness is;
  `StartupCommand` is a generic exec-primitive capability, not a harness concept.
  One `execs.Manager` runtime backs both plain execs and terminals.
- A terminal's exec id is its durable identity (ADR 0038). Attaching to or
  starting an ended terminal — its own id or the virtual `primary` alias —
  revives it in place: `terminal.Service.Revive` re-resolves env/secrets and
  the harness relaunch command (never the initial prompt), re-ensures hooks and
  files, and calls the generic `execs.Manager.Relaunch`, which fences the old
  run and starts a fresh transient unit generation (`discobox-exec-<id>-g<N>`)
  under the same exec id, socket, and runtime paths. Exec fields describe the
  current run; per-run history stays in the append-only event log and
  transcript store. Plain execs are never revived. `EnsurePrimary` revives the
  newest dead primary record on later boots instead of creating a sibling, so
  the session list holds one entry per terminal identity.
- A harness terminal never execs the harness binary directly. `execs.Manager`
  resolves `Shell: true` to the run user's login shell (as for a plain `shell:
  true` exec) and reports that shell as the exec's `Command` — what is literally
  executed. `StartupCommand`, when set, is a second argv the shim types into that
  shell's PTY once it starts, quoted with `execs.QuoteShellCommand` and followed
  by a newline, exactly as if the user had typed it at the prompt: the harness
  therefore runs as the shell's foreground job, not as the exec's own process.
  Injection needs no readiness handshake — the PTY's cooked-mode input queue
  holds the bytes until the shell's own first read, regardless of profile/rc
  timing. This is why Ctrl-Z can suspend a harness at all: see the orphaned
  process group rule below. `SandboxExec.command` and `.startupCommand` report
  the two argvs separately; CLI/API display prefers `startupCommand` when set.

## Trust Store on the Boot Path

`discobox-trust-ca.service` is ordered `Before=discobox-sandbox-agent.service`,
so nothing about a sandbox exists — no agent, no primary terminal, nothing to
attach to — until it finishes. It is therefore held to a different standard than
the rest of the boot: it must be proportional to the CAs it is actually adding.

It does not run `update-ca-certificates`. That tool rebuilds the whole store at
a flat ~0.8s whether it is adding one certificate or 150, which was 1.7s of a
~4.2s wait for an attachable terminal. Instead:

- The image ships the finished system store at **`/opt/discobox/ca-certificates`**,
  built in the Dockerfile by a copy of `update-ca-certificates` with
  `ETCCERTSDIR` redirected there.
- At boot `discobox-ca-anchor -store /etc/ssl/certs` seeds what is missing from
  it, appends every anchor to the bundle, and hashes only the anchors
  (`runcca.MaterializeTrustStore`). ~85ms.

Two constraints on any change here:

- **The prebuilt store cannot live at `/etc/ssl/certs`.** `runcca` bind-mounts a
  staging directory over that path in every container this sandbox's dockerd
  starts, *including BuildKit's*, so a build running inside a sandbox writes
  there into the mount and loses it at layer commit. That is why the image used
  to ship an empty trust store, and why anything built for the store goes under
  `/opt`.
- **An existing bundle is never replaced.** A nested sandbox boots with one its
  host's wrapper already placed, carrying the host's CA; overwriting it with the
  image's cuts off the egress path the outer proxy owns. Anchors are appended to
  whatever is there.

Subject hashes come from `openssl x509 -hash`, not from Go. The value is a
digest over a canonicalized subject encoding, and getting it subtly wrong would
misplace a link on the path that decides what the sandbox trusts, to save one
10ms exec.

## Development Images

`task build` is the no-argument build entry point for binaries and all local
images. `task build:images` builds only the pool host, sandbox base, and included
harness images.

`task dev` starts `cmd/discobox-docker-image-watch`, which initially builds the
pool, base sandbox, Codex, and Claude Code images. Each harness
Dockerfile extends `discobox-sandbox-agent:local` through its
`SANDBOX_AGENT_IMAGE` argument. The watcher tracks shared Docker/runtime inputs
plus each harness folder's Dockerfile, `image.json`, and configure script.
Harness-specific changes rebuild only that harness image; shared changes rebuild
the base and all affected harness images. Every successful build writes its
content-addressed development image reference to `.env`; pool and sandbox base
images also write their image digest. The watcher atomically writes the complete
reference-to-image-ID set as `.tmp/discobox-dev-images.json` and enables
development image synchronization in `.env`. On restart the server converges
that manifest onto every Docker daemon before reconciling its pool-agent
container, so local-VM, cloud, exec, and host-Docker providers use the same
development images without a registry.

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
- Bringing a terminal up is single-flighted, keyed by what is being brought up
  (`Service.singleFlightLaunch`). Boot launches the primary from a goroutine
  started just before the HTTP server serves, and clients attach without first
  polling for a terminal (ADR 0039), so boot and a first attach overlap by
  construction. Both a first launch and a revive are check-then-act over records
  nothing else serializes — `execs.Manager` keeps no in-process lock and `List`
  re-reads from disk — so concurrent callers otherwise both act:
    - A duplicated **first launch** gives the sandbox two primary terminals, and
      both callers read the durable launched-marker before either writes it, so
      the prompt runs twice.
    - A duplicated **revive** is worse than wasted work. Both callers derive the
      next unit generation from the same stale record, so they land on the same
      unit name, and the second one's socket removal — which exists to fence the
      *previous* run — deletes the socket the first one's shim has just bound,
      leaving a live run nothing can attach to.
  The primary launch is keyed by the virtual `"primary"` id, which is never a
  real exec id; a revive is keyed by the terminal's own exec id, which is its
  durable identity (ADR 0038). A primary launch that decides to revive uses the
  record's id, so an attach addressing that terminal directly contends on the
  same key rather than reviving it a second time.
- Joining a launch is also the readiness wait: it completes only after install
  and start, so `ResolvePrimary` never hands back a terminal whose shim is not
  listening yet. The whole decision runs under the latch, not just the launch —
  a record exists in `starting` from the moment `execs.Create` writes it, so a
  liveness check outside the latch would return a terminal nothing can attach to
  yet. Each launch runs under a context detached from whichever caller started
  it — an attach that times out must not abort an install that boot and other
  joiners are waiting on — while each joiner waits under its own context,
  bounded by `terminalReadyTimeout`. A failed launch is reported to everyone
  joined to it and clears the key, so the next attach retries.
- `"primary"` (`terminal.PrimaryExecID`) is a virtual exec id accepted anywhere
  the exec API takes one. It always names the sandbox's current primary
  terminal; attach and start resolve it through `terminal.ResolvePrimary`, which
  relaunches a stopped primary and returns the terminal that launch produced
  rather than re-scanning, while reads (get, logs, events, delete) resolve
  it read-only so a client's done-check observes a real exit instead of
  triggering a resume. A real exec id never relaunches: an id names one session,
  and once the shim behind it is gone the attach fails with `execs.ErrSessionGone`
  → `409`, whose message reports the exit status and points at `"primary"` when
  the dead exec was the primary terminal. The control plane proxies exec ids
  opaquely, so clients just send this value.
- Git authorship is seeded per key, never overwritten. `boot.seedGitConfig`
  applies `sandbox.json`'s `git` object to `<home>/.gitconfig` after `seedHome`
  — after, because `seedHome` chowns the tree recursively, and because git's
  lock-and-rename leaves a rewritten file owned by boot. It sets `user.name` and
  `user.email` independently, and only where `git config --get` resolves nothing;
  the unit is the key, not the file, since a `.gitconfig` holding only aliases
  has no identity and is exactly the case that needs seeding. The read is not
  scoped to `--global`, so an identity the image shipped in `/etc/gitconfig`
  counts as an answer. Git is asked rather than the file parsed — the same rule
  the CLI follows reading the identity on the way in — and an image with no `git`
  skips the step rather than failing to boot. Changing your local git identity
  therefore does not propagate into existing sandboxes; see
  [ADR 0042](../docs/adr/0042-git-authorship-identity-is-a-first-class-sandbox-property.md)
  for the condition for revisiting that.
- Run identity is owned by [`runuser`](runuser/DESIGN.md): one call resolves who
  a process runs as, so nothing re-derives it. `execs.User` is that package's
  type. `execs.Manager.ResolveUser` is the entry point for execs and terminals —
  it applies the request-vs-manifest and group rules, then resolves — and
  `terminal` asks it rather than rebuilding the user from `ExecDefaults`. See
  [REVIEW.md](REVIEW.md) for the mistakes this prevents and
  [ADR 0025](../docs/adr/0025-the-sandbox-user-is-one-contract-resolved-inside-the-sandbox.md)
  for why.
- Which shell a user has is sandbox knowledge, so an exec request asks for one
  (`shell: true`) instead of naming it. `execs.ResolveShell` answers from the run
  user's passwd entry — the current process user when the exec inherits the
  agent's identity — falling back to `$SHELL` from the exec environment and then
  a `/bin/bash`, `/bin/sh` probe when the entry is missing (a bare UID) or names
  a login-refusing shell. `execs.ShellCommand` wraps it as a login shell argv,
  and the `shell` fallback harness uses the same call, so a shell terminal and a
  `shell: true` exec can never resolve differently. The resolved argv is what the
  exec record reports, so a shell exec is self-describing after the fact.
  `shell` is mutually exclusive with `command` and `harnessId`: an empty command
  still means "run the harness" for every caller that did not ask for a shell.
  `CreateRequest.ShellCommandLine` (API field `shellCommandLine`) is the one
  exception to "shell means an interactive login shell": set alongside
  `shell`, it runs the resolved shell with `-lc <ShellCommandLine>` instead.
  It exists for ADR 0024's SSH ingress — SSH's `exec "cmd"` channel type
  carries one opaque command-line string, and sshd, running outside the
  sandbox, cannot resolve a login shell path itself the way a `shell: true`
  request already lets a caller avoid naming one.
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
  client can route each the way a local command does — `disco shell cmd
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
  by this rule when it is a byte, not a frame, and there is a shell in front of
  the command: the remote line discipline signals the foreground job, whose
  group has a parent (the shell) in the same session and so is not orphaned.
  This is exactly what a harness terminal's `Shell: true` + `StartupCommand` buys
  it (see above) — the harness is never the exec's session leader, so its
  process group is never orphaned in the first place. A command that *is* the
  session leader (a plain exec, `disco shell -t sleep 30`) cannot be stopped by
  Ctrl-Z for the same orphan rule — `ssh host sleep 30` behaves identically —
  which is exactly why terminals stopped taking that path.
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
  screen, capped scrollback (`DefaultScrollbackLines`), the cursor position, the
  input/rendering modes a TUI set before the client connected (mouse, bracketed
  paste, cursor keys, cursor visibility — tracked by scanning the output stream,
  since the emulator does not expose them), and the window title (held from the
  emulator's OSC callback, since it has no accessor either, and replayed as
  `OSC 1`/`OSC 2` — a title that was never set writes nothing rather than an
  empty one, which would clear whatever the client's own terminal had). Only TTY execs have a
  screen: `Runtime.EnableScreen` installs the `Replayer` once the PTY exists, so a
  pipe exec never waits on a repaint handshake it cannot satisfy. The
  durable transcript (`AsyncLogger`) is not used for attach — it backs only
  the `terminal logs` command (full forensic transcript). It batches an
  exec's output into compressed rows in the sandbox-local sqlite store
  (`store.ExecLogChunk`) rather than tmpfs files, so transcripts survive a
  pool container restart; see ADR 0028 for why and the read-staleness
  tradeoff that follows from batching.
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
- Reconnecting terminal clients establish an opaque logical session through
  `execstream/resume`. Input, signal, and close-input actions are positioned and
  acknowledged only after the shim applies them. The host retains the highest
  applied position for the process lifetime, so retransmission after a lost
  acknowledgement is deduplicated rather than applied twice. Ready remains
  connection-local and resize remains coalesced idempotent state.
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
