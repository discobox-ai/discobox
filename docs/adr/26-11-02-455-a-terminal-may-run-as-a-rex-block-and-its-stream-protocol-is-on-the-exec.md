# 26-11-02-455 — A terminal may run as a rex block, and its stream protocol is on the exec

- **Status**: Proposed
- **Date**: 2026-11-02
- **Relates to**:
  [0024](0024-ssh-is-a-control-plane-ingress-onto-execs.md) (SSH sessions are
  execs on the framed stream),
  [0027](0027-harness-terminals-run-as-a-shells-typed-in-job.md) (a harness is
  a shell's typed-in job),
  [0028](0028-exec-log-transcripts-persist-as-compressed-sqlite-rows.md)
  (transcripts),
  [0038](0038-terminal-identity-is-the-exec-id-terminals-revive-in-place.md)
  (a terminal's exec id is its identity; it revives in place),
  [0039](0039-attach-waits-for-readiness-at-every-tier.md) (attach without
  polling),
  [0115](0115-exec-state-converges-on-notifications-not-a-poll.md) (exec state
  converges on notifications),
  [0124](0124-a-terminal-is-active-while-what-it-shows-changes.md) (activity is
  a changed screen),
  [26-10-16-424](26-10-16-424-a-terminals-status-is-what-its-program-reports-over-osc-7501.md)
  (OSC 7501 status),
  [0145](0145-a-sandbox-declares-its-platform-and-a-non-linux-one-is-a-vm-template.md)
  §4 (supervision is a per-platform seam).

## Context

Every exec, interactive or not, runs under an `exec-shim`. The shim owns the
PTY, runs a `charmbracelet/x/vt` emulator for repaint, title, screen-change
time and `/screen`, and speaks the framed attach stream (`pkg/execstream/frame`,
`resume`, `host`). That stream goes from the shim through the agent, pool agent
and server, which all pass it through unread, to the CLI's raw attach, the
console's `termpane` panes, and the server's sshd.

Rex (superlogical.com/rex) is a terminal multiplexer server built on
libghostty. Its API is public and self-describing: a JSON control WebSocket
(`/api/control`, `rex.control.v1`) and one byte WebSocket per block's output
(`/api/data`, `rex.data.terminal.v1`). The output stream begins with a binary
libghostty snapshot (`GHOSTSNP`), followed by the program's output with rex's
own messages as nonce-tagged APC sequences (`resize`, `theme`,
`child_exited`). It has sessions, detached blocks, exit codes, OSC 7501 program
status (the same protocol as ADR 26-10-16-424), title and process events,
`terminal.format` (text or VT), and resumable output. Rex will be open source.
It cannot be redistributed yet, so no image can ship it.

We want discobox's interactive terminals to converge on rex. Rex then owns the
PTY and the emulator, its stream becomes the terminal stream, and a Rex client
can drive a discobox terminal natively. Probing the rex server (snapshot
`0.0.1793645657`, API revision 10) settled the questions this depends on:

- A rex block's processes are children of the rex server, in its cgroup. A
  `setsid` child survives `block.close`. A transient systemd scope can adopt
  the block's process: `StartTransientUnit` with `PIDs=`. After adoption,
  `KillUnit` delivered SIGTERM to the program's trap, rex reported exit 143,
  and stopping the scope killed every descendant, the `setsid` one included.
- Rex identifies a caller by how it connects. A root process connecting to the
  sandbox user's server socket is let in, as principal `uid 0`, and acts as
  that account.
- A terminal block takes no environment. Its options are `command`, `shell`,
  `login`, `cwd`, `path_prefix`, `size`, `theme`, `initial_input` and `exit`.
- Rex restores saved sessions on restart (`--restore`, default true). Block ids
  stay the same, but each block gets a plain login shell, not the command it
  ran.
- With a client connected, rex passes terminal queries (`ESC[c`, `ESC[6n`)
  through in the stream rather than answering them itself.
- The rex CLI honors a path prefix in a server URL
  (`-S http://host/v1/.../rex` asks for `.../rex/api/control`).

Rex has no pipe process, and it runs only on Linux and FreeBSD. Pipe execs,
services, monitors and one-shots, and darwin and Windows sandboxes, keep
today's runtime.

## Decision

### 1. `rex` is a sandbox feature, on only where a rex binary is present

`rex` is a feature tree like `desktop` (`sandbox-agent/features/rex`). It
provisions:

- the binary at `/opt/discobox/features/rex/bin/rex`;
- `discobox-rex.service`, which runs as the run user:
  `rex server run --listen unix:///run/discobox/rex/server.sock --restore=false`
  plus a state dir under `/var/lib/discobox/rex`.

It is Linux-only. On any other platform it reports `off`.

The binary is the flag. The platform bundle's `rex` tree carries the binary
only when the build was given one. Without it, provisioning reports the
feature `failed`, naming the missing binary. With it, the feature is on by
default, and a box can still turn it off through `features` in settings
(`pkg/sandboxconfig`). The agent treats rex as available when the feature is
`ready`, and only then. The decision is per box and is made at runtime. It
replaces nothing and is no build-time switch, because whether this box has
rex is a capability that really is present or absent at runtime.

A dev build gets the binary from `DISCOBOX_DEV_REX_BIN`, a path on the host.
The platform bundle's Dockerfile has an empty `FROM scratch AS rex` stage. When
the variable is set, `discobox-docker-image-watch` overrides that stage with
`--build-context rex=<dir>`, adds `rex: true` to the bundle's feature defaults,
and builds the dev CLI with the `rex` build tag (§6). Unset, every artifact is
exactly what it is today.

### 2. The stream protocol is an attribute of the exec

`SandboxExec` gains a required field `streamProtocol`, either `discobox` or
`rex`. When it is `rex`, it also gains `rex: {sessionId, blockId}`. The agent
fixes both when it starts a run. A record written before this field existed
reads as `discobox`, so no migration or backfill is needed. A revive (ADR 0038)
decides again, so a terminal can move to rex at its next run once the feature
becomes ready, and back again if the feature is turned off.

`CreateSandboxExecRequest` gains `streamProtocols`, the protocols the creating
client can attach with, most preferred first. Omitted, it means
`["discobox"]`. The agent picks the first one it can serve for the exec:

- `rex` only for an exec with `tty`, run as the run user, while rex is
  available;
- `discobox` otherwise.

Old clients, the sshd (ADR 0024), `discobox exec`, tools, services and
monitors send no list, so they keep today's protocol unchanged. The console's
`NewShell` and `NewTerminal` offer `["rex", "discobox"]` when the CLI was built
with rex support.

The agent creates the primary terminal itself, with no client involved. It
chooses `rex` whenever rex is available.

`SandboxExec` and `CreateSandboxExecRequest` also gain an optional
`terminalId`: the harness terminal a shell was opened for. It names a harness
terminal's exec id, and a create naming anything else fails with 422. It is
fixed at create and is independent of the protocol, so a `discobox` shell
carries it too. The console sends the id of the terminal whose pane has focus
when it opens a shell, and sends none when no terminal pane is focused. Every
other client sends none. A shell with no `terminalId` belongs to no terminal.

Deleting a harness terminal deletes, and so stops, every exec whose
`terminalId` names it. An exec with no `terminalId` is left alone. A terminal
that exits or revives keeps its shells, because its exec id is unchanged (ADR
0038).

### 3. Attach dispatches on the protocol

- **Discobox exec, discobox route:** `GET .../execs/{id}/attach` works as
  today.
- **Rex exec, discobox route:** after the same resolution (including
  relaunching `primary`, ADR 0039), the route answers without upgrading. It
  returns `409` with error code `stream_protocol` and the exec's
  `streamProtocol` and `rex` locator in `details`. A client that speaks rex
  dials the rex route. One that doesn't reports that the terminal needs a
  newer discobox. A client attaching to `primary` therefore still needs no
  prior poll.
- **The rex route:** `GET .../sandboxes/{id}/rex/{path}` forwards
  `api/control`, `api/data`, `api/help` and `llms.txt` to
  `/run/discobox/rex/server.sock`, with the prefix stripped. It takes the same
  scopes as exec attach (`ExecRead` and `ExecWrite`). The pool agent and the
  server route it through their existing reverse proxies, as they do attach.
  The agent dials as root, which rex admits as the run user's account (see
  Context), so every proxied client is the same rex user and data tickets work
  across them.

### 4. The agent runs a rex block through a launcher it adopts into a scope

Each harness terminal on rex has its own rex session, labeled with the
terminal's name. The session outlives the terminal's runs: the terminal's exec
record holds its `sessionId`, and a revive puts the new run in the same
session, creating it again only if it is gone. The terminal's run is a window
of its session. A shell with a `terminalId` is a window in that terminal's
session. A shell without one is a window in the primary terminal's session,
whether or not the primary is running, and the agent creates that session if
it is gone. When the box has no primary terminal, or its last run was on the
`discobox` protocol, an unassigned shell is a window in one shared session
labeled `discobox`. A shell never needs a terminal to exist.
So a Rex client opening a terminal's session sees the harness and the shells
beside it, and an unassigned shell sits with the primary.

The agent creates a terminal's first run with `session.create` and one
`initial_windows` entry, and every later run and every shell with
`session.new_window` (`focus: false`). Either way the window's layout is one
block of flavor `com.superlogical.terminal.shell` with:

- `shell: "none"`;
- `command: ["/opt/discobox/current/discobox-sandbox-agent", "rex-launch", <exec id>]`;
- the requested `size`;
- `exit.on_completion: false`.

`rex-launch` connects to the agent's launch socket
(`/run/discobox/rex-launch.sock`, writable only by the run user). The agent
then:

1. checks the peer's `SO_PEERCRED` pid against the rex server's cgroup and the
   run user's uid;
2. adopts that pid into `discobox-exec-<unit>.scope` with `StartTransientUnit`;
3. once the job is done, answers with the run's launch spec: argv, workdir and
   env, resolved exactly as `execs.Manager` resolves them today, including the
   login-shell-plus-typed-in-job form of ADR 0027.

The spec is single-use and never touches disk. The env never appears in
argv.

The launcher does not `exec` the command. It starts it as its child, in a
process group of its own that it makes the terminal's foreground group, and
stays its parent for the whole run, as the shim does today. It ignores job
control and termination signals, so it outlives the program. It exits with the
program's status, re-raising a fatal signal on itself, so rex's
`child_exited` reports the program's exit. It keeps its connection to the
launch socket for the run, reconnecting when the agent restarts. Over it the
launcher carries the program's output to the agent (§5) and takes signals
from it.

The scope is the run's unit, so systemd still owns the parts it owns today:

- `Stop` is `StopUnit`, which ends the whole tree. The launcher ignores the
  SIGTERM, waits for the program, sends what output remains, and exits;
- a signal is sent to the launcher, which delivers it to the program's
  process group as the shim's `procio` does today, and the exec's audit trail
  records it the same way;
- memory accounting and OOM attribution work as they do for a unit.

`execs.Watcher` sees the scope go away over the same D-Bus signals it watches
today (ADR 0115). The exit status is rex's `child_exited`, read on the agent's
control connection, or from `terminal.process.last_exit` after an agent
restart. Once the exit is recorded, the agent closes the block, and with it
its window. Deleting the exec closes the block too. Deleting a terminal
closes its block and those of the shells whose `terminalId` names it. Its
session is destroyed (`session.destroy`) only once no block is left in it, so
unassigned shells in the primary's session outlive the primary.

A rex server restart ends every block, because `--restore=false`. Its runs are
then `lost`, and terminals revive as they do today (ADR 0038). Rex's own
restore is never used.

### 5. The launcher keeps the transcript, and the agent reads the rest from rex

Everything the agent saves for a terminal today it saves for a rex-backed one:
the exec record and its runs, exec events, hook events, the transcript, and
the status fields `mergeExecStatus` fills from the shim's `/status`. Revive
(ADR 0038) is unchanged. Scrollback lives in rex's memory, as it lives in the
shim's today, and neither survives its process.

**Output transcript (ADR 0028).** The launcher holds a data connection to its
own block, opened as the run user over the rex socket, from before the
program starts until it exits. It discards the snapshot (everything before
the first `resize` message carrying the connection's nonce), strips rex's APC
messages, resumes its own dropped connection, and sends the output to the
agent in timestamped, sequenced chunks over the launch connection. The agent
writes them to `AsyncLogger` and acknowledges each chunk once its bucket is
flushed to the store. The launcher keeps unacknowledged chunks and resends
them when the agent comes back, so an agent restart or crash leaves no hole,
as the shim's own store connection guarantees today. The launcher's buffer is
bounded at the size of `AsyncLogger`'s; past it the oldest chunks are dropped
and the transcript records the gap. The launcher does not write the store
itself, because the database is the agent's and the launcher runs as the run
user.

**Input transcript.** Every client's input reaches rex as `terminal.write`
on a control connection the agent proxies (§3), and the agent's own writes
(the typed-in job of ADR 0027, sending input through the API) are its own
calls. The agent records each `terminal.write` for a rex-backed block as the
exec's input stream (`LogStreamInput`) before forwarding it.

**Status fields.** The agent holds one control connection to the rex server,
attached to every session it created (`session.attach` for each), and reads
each rex-backed run's fields from it:

- **title:** `title_changed`.
- **programStatus:** `program_status_changed` / `program_status_removed`, held
  with the rules of ADR 26-10-16-424.
- **outputAt:** the read time of the newest output chunk the launcher sent.
- **screenChangedAt (ADR 0124):** when the launcher's output pauses for 50ms
  (or after a second of output that never pauses), the agent calls
  `terminal.format` (`text`) and hashes it, together with the alternate-screen
  mode (`terminal.modes`). The definition of a change stays the same. Only the
  emulator doing the work is now rex's.
- **attacherCount and lastAccessedAt:** the proxied data connections the agent
  attributes to the block. The agent learns which block a ticket belongs to
  from the `data.connect` exchange it forwards on the proxied control
  connection. The launcher's own data connection is not an attacher.

**Screen reads.** `GetSandboxExecScreen`, and a wait result's `outputAt`,
read a rex-backed run the same way, into the same `ScreenText`:

- `lines` and `scrollback`: `terminal.format` (`text`), split at the screen's
  height from `terminal.size`, with the scrollback cut to the lines asked for;
- `rows`, `cols`: `terminal.size`;
- `cursorRow`, `cursorCol`: the cursor-position sequence a `vt`-format call
  with only `extra_cursor` ends with;
- `cursorVisible` and `altScreen`: modes 25 and 1049 from `terminal.modes`;
- `title`: `terminal.title`;
- `outputAt`: as above.

`autostop`, `agentstatus` and `ports` keep reading `execs.Exec` and do not
learn about rex.

### 6. Clients convert the rex stream to VT at the edge

A new root package, `pkg/rexstream`, is the one rex client. It speaks
`rex.control.v1` (hello, `session.attach`, `data.connect`, `terminal.write`,
`terminal.resize`, `session.own_block`, reconnect tokens) and
`rex.data.terminal.v1` (HELLO and RESUME). It converts a data stream into what
our clients already take:

- an ordinary VT byte stream: the snapshot decoded with libghostty-vt and
  re-encoded as a VT repaint, then the program's output;
- typed events for `resize`, `theme` and `child_exited`, with the nonce
  checked.

The CLI's raw attach and `termpane` consume the VT stream unchanged, and
neither gains a second emulator.

Input goes to `terminal.write` as the bytes the user's terminal produced. A
client claims the block (`session.own_block`) when it attaches and when its
pane takes focus. Rex's owner model replaces our multi-attacher resize rules.

libghostty-vt is a C/Zig library, and release CLIs build with
`CGO_ENABLED=0`. The decoder is therefore compiled only with the `rex` build
tag. A CLI built without it leaves `rex` out of the `streamProtocols` it
offers, and reports a rex-backed exec as needing a build with rex support.

### 7. A block a Rex client opens in a discobox session is a shell exec

The agent watches only sessions it created: one per harness terminal and the
shared `discobox` session. A session it did not create, and every block in
it, is the user's own and never an exec.

A block a Rex client opens in one of the agent's sessions (a split, a new
tab) is a shell exec, with `streamProtocol: rex`. Its `terminalId` is the
terminal whose session it is in, or none in the shared session. So a shell
split beside a terminal is deleted with it, like one the console opened for
it.

Such a block must start through the launcher, like every rex-backed run, so it
is adopted into a scope before it runs, gets the exec's environment, and keeps
a transcript (§4, §5). The run user's rex server is therefore configured so
that a shell block opened without a command starts
`discobox-sandbox-agent rex-launch --shell`. That launcher reads `REX_SESSION`
and `REX_BLOCK` from its environment, which rex sets in every terminal. Over
the launch socket it asks the agent to create the shell exec for that block.
It then runs the run user's shell with the launch spec it gets back. Rex has
no server-side startup command yet; Rex.app's is a per-host client setting.
Until rex has one, a block the agent did not create is not an exec, in its
own sessions too.

## Alternatives rejected

- **Run rex beside the shim as a viewer.** A rex block would run an attach
  client into the existing shim, so Rex.app could see discobox terminals. That
  keeps two emulators and two streams per terminal and converges nothing.
- **Implement the rex protocol in the agent over our shims.** That means
  matching a large and moving API (layouts, keymaps, server-side Lua, and the
  key and mouse encoding a Ghostty client expects the server to do) against a
  closed client, plus emitting `GHOSTSNP` from our emulator. Running the real
  server gets all of that for nothing.
- **Make rex the shim's process backend.** The shim stays and bridges a block
  in place of its PTY. That keeps our emulator, our stream and our repaint, so
  rex is only a process launcher with a worse lifecycle. It doesn't adopt the
  stream.
- **The agent translates rex into framed attach for every client.** Old
  clients would keep working, but the agent would need its own emulator to
  turn the snapshot into a repaint race-free. That is the shim again, and the
  conversion belongs at the edge.
- **The agent keeps the transcript from its own data connection.** Rex holds
  a dropped stream for 60 seconds, but the agent's count of bytes received
  and its unflushed transcript die with it, so a crash or slow restart leaves
  a hole the shim never had. The launcher lives exactly as long as the
  program, which is what the transcript needs.
- **The launcher `exec`s the program.** Nothing that lives as long as the
  program would then be left to carry its output and signals.
- **The launcher writes the transcript to the store itself.** The database is
  the agent's, and the launcher runs as the run user. It would also be a
  second writer the agent can't see.
- **One rex session for every discobox run.** A Rex client would see every
  terminal and shell in one flat session, and a split opened beside a harness
  would say nothing about which terminal it belongs with.
- **The rex session as the grouping.** Shells on the `discobox` protocol need
  the same grouping, and a rex restart loses every session. `terminalId` is
  on the exec, and the session is derived from it.
- **An unassigned shell defaults its `terminalId` to the primary.** Deleting
  the primary would then delete shells that were never opened for it. It sits
  in the primary's session, but belongs to no terminal.
- **Adopt a Rex-opened block after it starts, with the rex service's
  environment.** This has the same escape before adoption as below, and no
  per-exec environment or transcript. A server-side startup command makes
  both creation paths the same launcher handshake.
- **Adopt the block's process into a scope by pid after it starts.** Whatever
  the program forks before the adoption escapes the scope. The launcher
  handshake adopts before anything runs.
- **Pass the environment in argv (`env K=V …`).** Every process can read
  `/proc/*/cmdline`, and the environment carries credential placeholders and
  raw secrets. An env file is the same idea with a disk copy. The launch spec
  is delivered over a socket.
- **Rex's own session restore.** It brings blocks back as plain shells, not
  their commands. Our revive (ADR 0038) is the one way a terminal comes back.
- **An env var or build tag as the feature flag.** Neither can tell whether
  the box actually has a rex binary. The feature machinery already provisions
  trees, defaults them per platform, lets a box turn them off, and reports
  their state.
- **Choose the protocol per sandbox rather than per exec.** sshd, `discobox
  exec`, services and old clients cannot speak rex. A per-exec attribute lets
  one box serve both.

## Deferred

- **Root and other users' terminals on rex.** These need a rex server per
  account, through rex's login server (PAM, `accounts.jsonc`) or a second
  unit. They stay on `discobox` until a root terminal is wanted on rex.
- **Pipe execs on rex.** Revisit if rex adds a block flavor with separate
  stdout and stderr and an input close.
- **Shipping rex in images, and the `rex` build tag in release CLIs.** Revisit
  when rex is open source. The CLI needs libghostty-vt cross-built for every
  release target, through cgo or a WebAssembly build run by a pure-Go runtime.
- **Rex clients that can't send our token** (Rex.app, the `rex` CLI). These
  need a `discobox rex <box>` local forwarder, or rex's planned
  authentication. Revisit when a Rex client is the goal.
- **Blocks a Rex client opens in a discobox session (§7).** Revisit when rex
  has a server-side startup command for shell blocks.
- **Removing the `discobox` protocol for terminals.** Revisit once every Linux
  image ships rex and released CLIs carry the tag. The sshd (ADR 0024) still
  needs either it or its own rex client.

## Consequences

- Linux interactive terminals have two runtimes while the feature exists:
  shim and framed stream, or scope, rex block and rex stream. The exec record
  says which.
- A rex-backed run has a launcher process beside the program, as a shim-backed
  run has the shim. It is lighter (no PTY, no emulator), but it stays.
- A rex-backed exec has no `frame.Signal`, `CloseInput` or acknowledged,
  positioned input. Rex's resume covers output only, so input sent during a
  dropped connection may be lost. `terminal.write` takes UTF-8 text, so input
  that isn't valid UTF-8 cannot be sent.
- The rex server is a single point of failure for every rex-backed terminal in
  a box. A rex crash or upgrade loses them all, to be revived.
- Deleting a harness terminal now deletes the shells whose `terminalId` names
  it, on either protocol. Until this, deleting a terminal deleted only that
  exec. Shells with no `terminalId` are never deleted with a terminal.
- A Rex client and a discobox client sharing a block share one size, chosen
  by whichever claimed it last.
- The `GHOSTSNP` format is not stable across libghostty versions, so the CLI's
  libghostty-vt and the box's rex must stay paired. We accept the same
  coupling for our own stream, which is not stable across versions either.
- `rex` joins the features `image.json` knows. Its tree is empty in every
  build not given a binary, so release artifacts are unchanged.
