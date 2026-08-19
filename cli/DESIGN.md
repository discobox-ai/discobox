# CLI Design

The CLI module owns the `disco` command implementation and talks to the
control plane through generated root-module API clients plus a few handwritten
transport helpers where OpenAPI does not model the stream.

## Package Map

| Package/path | Ownership |
| --- | --- |
| `cmd/disco` | Binary entrypoint. |
| `internal/cli` | Cobra command tree, output formatting, local server auto-start, TUI API adapter, and the attach transports and policy layered on `execstream/client`. |
| `internal/sandboxcreate` | UI-independent client-side sandbox request preparation and creation, including prompt options, source resolution, workspace snapshots, environment/secrets, local user identity, and source push delivery. |
| `internal/origin` | Resolves the client host and project directory a sandbox is created from. Host identity itself is shared, in the root module's `internal/hostid`. |
| `internal/tui` | The `disco tui` launcher: Bubble Tea presentation and interaction state, expressed against its own `DataSource` interface. See [`internal/tui/DESIGN.md`](internal/tui/DESIGN.md). |
| `internal/portforward` | Frontend-independent dynamic port forwarding: local listeners kept in sync with a remote's announced ports, over a caller-supplied dialer. |
| `internal/keys` | The leader: its default, its `DISCOBOX_LEADER` override, normalization, and the byte a raw stream matches it as. Owned here because the launcher's panes and a plain attach must reserve the same key. |

## UI Dependency Direction

- Keep reusable sandbox creation workflows out of `internal/cli`; place them in
  `internal/sandboxcreate` so Cobra and TUI adapters consume the same behavior.
- `internal/tui` must not import `internal/cli`. It owns presentation state and
  frontend contracts only; API and terminal adapters belong outside it.
- `internal/cli` may adapt generated API clients and terminal transports to the
  TUI's interfaces, but must not become the owner of logic shared by frontends.
- The launcher never reimplements a command. From the list, `apply` is the
  Cobra command itself: `apiDataSource.Interact` builds it, binds it to the
  streams `tea.Exec` hands over while the window is suspended, and executes it.
- On the workspace screen, `apply` is drawn in an overlay pane instead, and is
  the same command — spawned as a child `disco apply <id>` on a local pty sized
  to the pane (`tui_local.go`). The pty is the child's *controlling* terminal,
  so anything reading its keys from `/dev/tty` reads them from the pane rather
  than from the real terminal, out from under the window drawing it. The child
  inherits this invocation's `--server`, `--project` and `--chdir`; the token
  goes through the environment rather than the argument list, which every
  process on the machine can read.
- Either way what runs is `disco apply` with its own flag defaults and terminal
  detection, not a second implementation that drifts from it. A launcher that
  cannot be reproduced from a shell is the thing to avoid.
- Bare `disco` runs the launcher when stdin and stdout are both terminals, and
  prints its help otherwise (`App.runTUI`, reached from the root command's
  `RunE` and from `disco tui`). Typing a program's name is how you ask for it,
  and the launcher is the one thing you can ask for without knowing a
  subcommand; a pipe, a script or CI expected output, and a full-screen window
  is not an answer to that. The root's `Args` is left at cobra's default, which
  turns an unrecognized first argument into "unknown command" rather than
  handing it to the launcher. The leader there comes from the environment only:
  a flag would have to be persistent to be reachable, and every subcommand would
  carry one that means nothing to it.
- `disco configure` is the same launcher opened on its harnesses screen
  (`tui.WithHarnesses()`), not a window of its own. See *Harness Configure
  Step*.
- `DISCOBOX_LEADER` sets the prefix key, normalized by `keys.NormalizeLeader`:
  a bare letter is taken as Ctrl-that, since a leader that is not a chord would
  be a character you could never type, and only a letter is accepted because the
  leader has to survive being turned back into the byte a terminal sends.
  `disco tui --leader` overrides it for the launcher; nothing else takes a flag.
  It is one key for both the launcher's panes and a plain attach's detach chord.
- Attach and shell are terminals rather than commands, and are drawn inside the
  window by the `termpane` module. `apiDataSource.Open` connects one:
  `framedTerminal` (`internal/cli/tui_terminal.go`) presents the framed exec
  attach as the byte stream a pane draws. Attach targets the virtual primary
  exec id and needs no start — the agent resolves the sandbox's current primary
  terminal and relaunches it if it has stopped. A shell creates a new
  interactive TTY exec, the same one `disco shell` with no command runs, carrying
  `COLORTERM` and `NO_COLOR` from this terminal (`paneTerminalEnv`) so the
  sandbox knows how much color to use. `TERM` is deliberately not forwarded: the
  terminal on the client side of a pane is an emulator, not the user's, and the
  sandbox's own `xterm-256color` default describes it — a forwarded
  `xterm-kitty` names a terminal the sandbox has no terminfo for. A created exec
  is not a running one: it is started only after the attach is up
  and sized, the order `attachSandboxExec` uses. Started first, its opening
  output would go out before anything was listening. `TestPaneTerminalsE2E`
  (opt-in, `DISCOBOX_PANE_E2E=1`) is what catches the omission, since a pane
  attached to an unstarted exec draws an empty screen forever with no error.

Local server auto-launch is a release-only capability. Normal and development
builds leave it disabled; release CLI binaries opt in at build time by setting
`cli.serverAutoLaunch` to `true` with the Go linker's `-X` flag. `--no-start`
remains the runtime override for release binaries.

Advanced configuration and low-level resource commands are grouped beneath the
visible `disco box` command: `project`, `sandbox`, `terminal`, `exec`,
`provider`, `pool`, `job`, `harnesses`, and `hooks` are not root commands.

`disco box project` is the only command group not scoped by the global
`--project` flag: its arguments name the project being acted on, resolved by
`resolveProjectID` from the same selectors `-p` accepts (the `default` alias, a
full or short ID, or the display name). `set-default` moves the flag
`default` resolves to, so it is how `-p`'s own default is chosen; there is no
unset. `create --from` copies an existing project's configuration
([ADR 0023](../docs/adr/0023-projects-are-created-by-copy-and-deleted-only-when-empty.md)),
with `--copy` selecting what comes across and `--copy none` taking nothing.

`disco shell` is the exception: the root command is the everyday one-shot "run
this in my sandbox" verb, while `box exec create` stays the raw, fully
configurable form (workdir, env, user, detach, explicit `-i`/`-t`). Both drive
the same exec create/attach/status sequence. The root form has no `-it`: stdin
is always attached, and a PTY is requested only when stdin, stdout, and stderr
are all terminals, so pipes and redirects behave like a local command. The attach
session writes stdout frames to stdout and stderr frames to stderr, with no
special case for the PTY: a TTY exec merges at the PTY and simply never sends a
stderr frame, which the client neither detects nor needs to.

`disco shell [SANDBOX_ID] [CMD...]` (`internal/cli/shell.go`) takes the sandbox
and the command as one positional list rather than a `--sandbox-id` flag, since
which sandbox to run in is picked as often by ID as left to the picker. Cobra
sees one flat `[]string` and cannot tell SANDBOX_ID from CMD apart, so
`resolveShellTarget` decides: `args[0]` is tried against the sandboxes
`disco ls` shows for the current project directory (`matchSandboxArg`) —

- A full generated ID (`id.IsGenerated`) is trusted outright: a resource prefix
  plus 16 random characters cannot collide with a real command word by
  accident.
- An exact sandbox name matches next, in full and never partially: a name is
  what the listing shows and what people type, and a partial one would compete
  with short-ID matching for the same argument.
- A short ID is matched against that candidate list exactly like an explicit
  SANDBOX_ID argument elsewhere. Zero matches is not an error — it just means
  `args[0]` was never a sandbox reference — but more than one is reported as
  ambiguous, since its shape said it was meant as an ID.

A match consumes `args[0]` as SANDBOX_ID and leaves the rest as CMD. No
match — including no arguments at all — means every argument is CMD, and the
sandbox falls back to the same picker `disco apply` uses when SANDBOX_ID is
omitted.

`disco shell` with no command runs the sandbox user's login shell. The CLI
never names that shell: it sets `shell: true` on the exec create request and
the sandbox resolves the run user's shell from its own passwd database,
because the local `$SHELL` describes this machine and says nothing about the
identity the exec runs as. `box exec create --shell` is the same request in
raw form.

`disco tools` groups the everyday development tools run *against* a sandbox —
`git` and `ssh` today. Which sandbox is the one thing every tool has in common,
so `--sandbox-id` is a persistent flag on `tools` itself and every subcommand
inherits it. Everything else belongs to the subcommand that means it, including
where the tool runs: `git` runs inside the sandbox and takes `--source`/`-s`,
while `ssh` runs on this machine and connects to the sandbox. Each then drives the same exec create/attach/status sequence as
`disco shell`. Flag parsing stops at the first positional argument
(`SetInterspersed(false)`), so everything from there on reaches the tool verbatim.

The default path sends no workdir and fetches no sandbox record: an exec with no
workdir already lands in the sandbox-agent's default exec directory, which is the
primary source's. Only `git --source` has to `GetSandbox` to turn a slug into a
directory. With a full `--sandbox-id` the whole command is create + attach +
start + status, so a one-shot `disco t git status` costs no round trip it does
not need.

`disco tools ssh` needs no SSH port on the server. The session is carried over
the endpoint the CLI already uses: a loopback TCP port opened for the life of
the command splices each connection to a `GET /ssh/connect` websocket, whose
byte stream the server hands to the same sshd its TCP listener feeds.
`endpoint.StartLoopbackProxy` cannot serve this — it is an HTTP reverse proxy,
and these are not HTTP bytes.

Everything the session needs is passed on the command line, so nothing is
written down: address and port from the bridge, `-l` from the sandbox ID, `-i`
from the managed key, and `UserKnownHostsFile` from a temp file holding the
host key this run fetched. `-F none` keeps the user's own `ssh_config` out of
it, since a `Host *` block there could otherwise override the identity or user
just resolved. The enrolled key is the single persisted thing, and
`resolveSSHIdentity` reuses an already-enrolled one rather than adding another.

Flag parsing is **off** for `tools ssh` (`DisableFlagParsing`), not merely
non-interspersed: `disco tools ssh -L 8080:localhost:3000` puts ssh's own flags
first, and cobra would reject them as unknown before the command ever ran. The
leading argument is taken as a sandbox only when it does not start with `-` and
`matchSandboxArg` recognizes it; everything else reaches ssh.

Reaching ssh is not the same as being appended. `ssh [options] host [command]`
is positional and this command supplies the host, so `splitSSHArgs` separates
the user's options from their remote command and places them either side of it.
Appending everything after the host only works on glibc, whose getopt permutes
argv; anywhere else every option would be sent to the remote as a command.

`ssh -f` is refused rather than passed through. It forks and returns, and the
bridge lives in this process, so honouring it would tear the connection down
under the backgrounded ssh — which is exactly what it did before the check
existed, silently. Backgrounding the whole command keeps both lifetimes
together and leaves one process to kill.

## Listing Order

Every listing the CLI renders — tables, `-q` ID lists, shell completions, the
picker, and the TUI — is ordered most recently touched first by
`sortedByRecency` (`internal/cli/output.go`). "Touched" is `recencyTime`: the
resource's update time, or its creation time when the resource has no update
time, either unset or not tracked at all (`SandboxExec`). The whole CLI answers
"what have I been working on" with one order, so a listing and the picker built
from it can never disagree.

The sort happens before the output-mode branch, so `-o json` is ordered the same
as the table: the order is the CLI's answer, not a table-rendering detail.

## Sandbox Display Name

The NAME a sandbox listing shows is `Sandbox.displayName`, computed by the
server (`services.SandboxDisplayName`) and read verbatim here — `disco ls`, the
launcher, and any other client name a sandbox the same way because none of them
derives it. Display only — name *resolution* (`matchSandboxArg`, `--name`
updates) still works on the configured name, and the TUI disables rename on a
row whose display name is not the configured one (`sandboxNameIsTitle`,
`internal/cli/sandbox_name.go`, feeding `Sandbox.NameIsTitle`) because a rename
would change nothing on screen.

## Choosing a Sandbox Interactively

Commands that act on "the sandbox I am working in" take a sandbox identifier —
as `--sandbox-id` (`box exec`, `box terminal`), an optional positional
`SANDBOX_ID` (`apply`, `attach`, `box get`), or a leading
positional argument shared with the command itself (`shell`, resolved by
`resolveShellTarget` rather than `selectSandbox`) — and fall back to
`selectSandbox` (`internal/cli/picker.go`) when it's omitted, never to a guess:

- Candidates are exactly what `disco ls` shows — `listProjectSandboxes` filtered
  to the current project directory's origin — so the command and the listing can
  never disagree.
- One candidate is used, none and several are errors, and several with a
  terminal on stdin and stderr open the inline Bubble Tea picker instead.
- The picker prompts on stderr so the command's stdout stays a clean stream.
- `pickOne` is resource-independent: callers supply `pickerItem`s and the
  wording for the empty and ambiguous cases.
- Typing in the picker fuzzy-filters and re-ranks the list (`internal/cli/fuzzy.go`):
  a subsequence match over each item's title, ID, and detail, scored to favour
  contiguous, word-start, and early matches, with title weighted over ID over
  detail. Matched runes are highlighted; the list scrolls in a 20-row window.
  Because every printable key extends the query, navigation is arrows/`ctrl+p`
  /`ctrl+n` only, and `esc` clears a non-empty query before it cancels.
- Ordering with no query is the last sandbox picked for this project (marked
  `last used`), then most recently updated; the tie-break between equally scored
  matches is most recently updated. Typing hands ranking entirely to the query:
  the remembered pick gets no standing edge once the user says what they want.
- The last pick per `recentKey` is remembered in
  `$XDG_STATE_HOME/discobox/cli/recent-selections.json` (`internal/cli/recent.go`),
  keyed `sandbox:<projectID>` because the candidate list is per project. It is
  derived convenience state, not configuration, so it lives under state, is
  written atomically, is trimmed to the most recent entries, and every read and
  write is best-effort — a missing, unwritable, or corrupt file only costs the
  ordering, never the command.

## Attach Stream Pattern

Terminal and exec attach use the same framed stream protocol and the same
session, `execstream/client`.

- The session is not defined here either. `execstream/client` owns the attach
  session — output demux, resize tracking, signal forwarding, suspend — and
  `execstream/frame` owns the wire format. This module supplies transports
  (`directAttachFrames`, the reconnecting frames) and policy, and never
  re-declares a frame constant: the compiler cannot catch a mirror that has
  drifted, and a corrupted stream is the first symptom.
  See [ADR 0008](../docs/adr/0008-attach-stream-packages.md).
- Everything the session does to this machine goes through `client.Console`:
  raw mode, terminal size, signal delivery, and stopping and resuming. The real
  one is `client.OSConsole`; it is the seam that lets suspend ordering and the
  signal set be tested without a PTY, including on platforms this repository
  cannot run.
- Keep frame read/write, output frames, resize frames, signal forwarding,
  raw-terminal setup, close-input frames, and attach teardown in that session,
  never in a caller.
- Keep resource-specific behavior in the resource file: terminal detach
  filtering belongs with terminal commands, and exec interactive/non-interactive
  stdin behavior belongs with exec commands.
- If the attach websocket cannot be opened, fetch the exec once more and report
  terminal status, exit code, and runtime error when it already exited. A gone
  shim socket commonly means the command finished before attach, so the
  transport error alone is not the useful failure. When the sandbox-agent
  rejected the attach with a definitive status (`404`, `409`) its own message is
  reported verbatim instead: it knows why, and the client's inference would only
  repeat or contradict it.
- Nothing polls for readiness before attaching. `disco run` creates, delivers
  the source if it must be pushed, and attaches; the attach itself waits, at
  each tier for what only that tier can see — the control plane for the sandbox
  to be dispatched to a live pool and to be usable rather than mid-delivery, the
  pool agent for the container, the sandbox agent for the primary terminal's
  launch and install (ADR 0039). The two loops that used to sit here — poll
  until `displayState: running`, then poll the exec list until a primary exists
  past `installing` — cost a request per second of provisioning for facts the
  server knew the instant they changed, and every client had to reimplement
  them. `--wait` on `box sandbox create` is a different thing and stays: there
  the wait *is* what was asked for.
- `primary` (`primaryExecID`) is accepted wherever a terminal/exec ID is: it is
  the sandbox-agent's virtual id for the current primary terminal, so it must
  bypass short-ID resolution and reach the agent unchanged. It is the only
  selector that relaunches a stopped primary terminal — attaching a real ID
  addresses one session and fails once that session has ended.
- Do not fork a second terminal/exec attach loop for a new stream feature. Add
  an option or callback to the shared session when the behavior is protocol
  plumbing; add resource-specific code only when the semantics differ.
- Harness-terminal attaches use the shared reconnecting framed transport in
  `execstream/resume`. It retries websocket failures with capped exponential
  backoff, restores resize/readiness state so the sandbox shim repaints the
  terminal, and stops retrying once the authoritative exec record is terminal.
- The reconnect decision (`sandboxExecAttachDone`, consulted before every redial)
  asks whether the command finished or the runtime disappeared underneath it. A
  recorded `exited` is the command's own result — Ctrl-D, the harness finishing —
  and ends the attach with its exit code; `failed` never started, so retrying is
  pointless. `lost` is an ungraceful disappearance (unit gone, no exit recorded):
  reconnect, because the redial's attach relaunches — but only for the virtual
  primary id, since nothing can ever revive a concrete one.
- A terminal cannot outlive its sandbox. When the exec read fails and the
  sandbox is `stopping`, `stopped`, or `failed`, the attach ends rather than
  reconnecting forever — the stop is observable, so the client acts on it instead
  of looping against a runtime that is gone. No attach restarts the sandbox;
  `disco attach` (`internal/cli/attach.go`) is deliberately a thin wrapper over
  `attachSandboxTerminal` with the virtual primary id and nothing else — sandbox
  autostart is a possible future addition to it, and until then the client never
  starts a sandbox to keep an attach alive.
- The way out of a terminal attach is the leader then `d` (`detachFilter`,
  `internal/cli/sandbox_terminals.go`), matched over the raw input bytes and
  nothing else: `execstream/client` never learns the chord, and never learns
  where the leader came from. The leader is the launcher's
  (`internal/keys`, `App.leader`), resolved once from `DISCOBOX_LEADER` in
  `App.validate` so a misspelling is reported before a terminal is handed over,
  and `App.detachHint` is the one spelling the messages print. Ctrl-D counts as
  the second key alongside a bare `d`, the leader typed twice sends one literal
  leader, and a leader that qualified nothing is delivered with the key that
  followed it — the same bargain `termpane` makes in a pane, so the keystrokes
  mean the same thing either way. It replaced Docker's Ctrl-P Ctrl-Q, which took
  a key programs want (Ctrl-P is history-back everywhere) and matched nothing
  the launcher did.
- Resumable actions (input, signals, and close-input) carry monotonically
  increasing positions. The client retains a bounded window until the shim
  acknowledges applying them; reconnect resends the unacknowledged suffix and
  the shim deduplicates it by logical-session token. A full window backpressures
  stdin instead of silently dropping accepted input. Resize is idempotent state,
  so only its latest value is retained and restored.
- A plain exec attach (`attachSandboxExec`, `internal/cli/sandbox_execs.go`) —
  `disco shell`, `box exec create`, `disco tools git`, everything that is not a
  harness terminal — follows the same PTY/no-PTY split as replay itself:
  `openExecAttachConn` picks the reconnecting transport, replay included, when
  `tty` is true, and the direct one otherwise. A TTY exec's screen can be
  repainted on reconnect exactly like a terminal's; a piped exec's output has
  no such buffer — resuming it would need byte-exact output positions, which
  the shim does not provide — so it stays direct and fails on disconnect.
  `SignalReady` and `OtherErr` are set to match: they only make sense once
  replay is in play, and only exist on the TTY branch.
- Connection lifecycle notifications are transport events, not terminal output.
  CLI attach ignores them. Nothing renders them today: the launcher suspends
  itself and hands the real terminal to `disco attach` rather than embedding a
  pane, so there is no second consumer to notify.
- Resumable attaches can subscribe to timing events without parsing terminal
  output. A websocket heartbeat measures the physical proxy path to the
  sandbox-agent; an action-acknowledgement sample measures from client
  acceptance until the exec host applied the positioned input at the PTY
  boundary. These are separate sources because a healthy websocket alone does
  not prove the shim is applying input promptly. Terminal output remains opaque:
  a low delivery RTT can exonerate the attach path, but the client cannot
  reliably correlate an arbitrary output frame to one keystroke or claim that
  the application echoed it. Follow the status interpretation and hysteresis
  policy in [`execstream/resume/DESIGN.md`](../execstream/resume/DESIGN.md)
  when exposing these events in a CLI or TUI.

## Port Forwarding (ADR 0049)

`disco proxy` holds a local port open for every port a sandbox is listening on.
The mechanics are `internal/portforward`, which knows nothing about sandboxes:
it is given a `Dialer` and a set of `Target`s, and owns picking local ports,
accepting, splicing, and reporting. `internal/cli/proxy.go` supplies the two
sandbox-shaped halves — the listing (`sandboxPortTargets`, the same agent
report the launcher's rows are drawn from) and the transport
(`sandboxTCPDialer`, `internal/cli/tcp_tunnel.go`) — and prints the events. The
launcher will supply the same two halves and draw them instead.

- The transport is the control plane's `/api/projects/{p}/sandboxes/{s}/tcp/attach`
  websocket, which is ADR 0024 §3's tunnel exposed at the HTTP edge. Each
  forwarded connection is one websocket, and `tcpTunnelConn` presents its
  `Input`/`Stdout`/`CloseInput` frames as a `net.Conn`, so everything above it
  is a plain TCP proxy and a half-close survives the trip (ADR 0024 §4).
- A port is bound at its own number when that is free and at the nearest one
  above it when it is not, so a sandbox's 8080 is `localhost:8081` when
  something local already has 8080. A privileged port gets one try at its own
  number and then the search at its unprivileged twin — 80 becomes 8080, 443
  becomes 8443 — because scanning 80..144 as an ordinary user only ever ends at
  a random ephemeral port.
- Bindings are sticky. A port that drops off the listing keeps its local port
  and is reported gone; the same number is reused when it comes back. A dev
  server restarting is the common case, and a URL the user has open must not
  move under them.
- `--port` narrows the set to the ports named, and forwards them whether or not
  the listing mentions them yet. Naming a port asserts it is there, and the
  report is a poll behind (ADR 0046); a flag that waits for the listing to agree
  is useless in the minute after a server starts, which is the minute it is
  reached for. What the listing does say about a named port — the address to
  dial, what it speaks — is still used.
- The launcher runs the same forwarder. Opening a workspace opens one
  (`apiDataSource.Forward`, `internal/cli/tui_forward.go`) and detaching closes
  it, so the local ports live as long as the screen showing them; the window
  sees only a `tui.Forward` — what is bound, and a wake-up when that changes —
  and draws the sandbox ports on its header as `8082->8080`, with the web ones
  linked to `http://localhost:8082`. It binds loopback only, with no `--address`
  to widen it: a window has no business opening a sandbox's ports to the network
  on the strength of having been attached to.
- The listing is polled rather than streamed. The project event stream carries
  resource identities, not bodies, so a change would have to be followed by the
  same read this loop already does; the ports themselves reach the control plane
  on the sandbox-agent's own cadence (ADR 0046), which is what bounds freshness
  either way.

## Pool Host Console

`disco box pool console [POOL_ID]` attaches this terminal to a root shell on
the machine hosting a pool's runtime — a WSL guest, a libkrun microVM, a
droplet, the local Docker host — for debugging that backend
([ADR 0051](../docs/adr/0051-the-pool-console-attaches-through-the-driver.md)).

It reuses the attach machinery rather than inventing a second one: the same
`execstream/client.Session` with raw mode, resize tracking, and the leader-key
detach chord that `disco attach` uses, over a websocket to the control plane's
console route. What differs is that there is no exec to create or start first
and no reconnecting transport — the console container is persistent, so a
dropped connection is reopened by running the command again, and there is no
session state on the client worth resuming.

The initial terminal size travels as query parameters on the open, because the
console's shell is started by the server before the session's own resize
tracking has said anything, and a first prompt drawn at 80x24 would then be
repainted.

## SSH Keys and Config (ADR 0024)

`disco box ssh-key` and `disco box ssh-config` are the CLI-side counterpart
to the SSH control-plane ingress (`server/internal/sshd/DESIGN.md`); they
are advanced/low-level configuration in the same sense as `box provider` or
`box pool`, so they live under `disco box` rather than at the root command
level or layering on the attach transports above.

- `disco box ssh-key add` with an explicit file (or `-` for stdin) argument
  enrolls that key directly. With no argument it lists public keys from a
  running `SSH_AUTH_SOCK` agent (falling back to `~/.ssh/*.pub`) and reuses
  the shared picker (`internal/cli/picker.go`) for the "which key" choice
  when there is more than one candidate — the same picker `disco shell`'s
  sandbox selection and `run --include-dirty`'s prompt use. This step is
  enrollment convenience only: listing an agent's public keys proves nothing
  about possession of the private half, and the actual authorization is the
  authenticated `CreateSSHKey` API call that follows, never agent presence
  itself (ADR 0024 §6).
- `disco box ssh-config` emits one `ssh_config(5)` `Host` stanza per sandbox in
  the current project plus a `known_hosts` line. Both the address and the host
  key come from `GET /ssh` (public, `server/internal/auth/DESIGN.md`), so
  nothing here hard-codes a port: `--host`/`--port` are overrides for what the
  server cannot know about, such as reaching it through a local forward, and
  are unset by default. The `known_hosts` host field is bracketed
  (`[host]:port`) for every port but 22, which `ssh` looks up under the bare
  hostname instead.
- It also generates and enrolls the key it points at, so the emitted config
  works on its own: an ed25519 key under the CLI state directory
  (`<state>/ssh/id_ed25519`, `0600`), enrolled in the project when the project
  does not already list its fingerprint. Enrollment keys on the fingerprint,
  not on having just generated the key, so repeat runs and second projects do
  not pile up duplicates. The private key is written in OpenSSH's own format,
  not the PKCS#8 PEM the server uses for its host key, because this file is
  read by the `ssh` binary rather than by `x/crypto/ssh`.
- Each stanza carries four `Host` patterns: the sandbox name and the sandbox
  ID, each bare and suffixed with `.discobox.internal`. The bare name is what
  anyone actually types; the suffixed form is the unambiguous spelling to fall
  back on, and is the reason dropping the suffix from the primary alias is
  affordable — a bare pattern lives in the same namespace as the user's real
  hosts, and the qualified one does not.
- The name is cosmetic — `User` carries the ID, which is what `ResolveUsername`
  routes on — but an alias must still mean exactly one sandbox, because `ssh`
  applies the *first* matching block and would otherwise silently reach the
  wrong one. Patterns are counted across the whole emitted config and any
  claimed twice is dropped from every stanza that wanted it. Server-side name
  uniqueness (`idx_sandbox_project_name`) means this no longer fires for
  name-versus-name; what it still catches is a name that spells another
  sandbox's ID, which claims both that sandbox's ID patterns at once. Names
  with whitespace or glob metacharacters never become patterns at all
  (`Host *` would capture every host).
- A sandbox whose every pattern is contested is emitted as a comment rather
  than a stanza: `Host` with no patterns is a syntax error that would break the
  whole file.
- `--write` writes the stanzas and the server's host key to two files under the
  CLI state directory, beside the generated key, and adds one `Include` line to
  `~/.ssh/config`. Both files are scoped to the project — `<state>/ssh/<project
  id>/{config,known_hosts}`, one `Include` each — because a run only knows
  about the project it was given: a shared file would drop every other
  project's stanzas on each write, and two projects can live on different
  servers with different addresses and host keys. The ID is resolved first, so
  the same project reached as `default` and by ID does not end up owning two
  files. Nothing else in `~/.ssh` is edited: this command owns those
  two files and rewrites them wholesale, so it never has to parse or merge into
  a config the user maintains by hand. The `Include` goes at the *top*, because
  ssh takes the first value obtained for each keyword and an `Include` placed
  after an existing `Host *` block would lose every setting that block sets. It
  is idempotent — re-running after creating a sandbox refreshes the stanzas and
  leaves one `Include`.
- A server with no SSH ingress writes an *empty* config rather than failing:
  these files mirror the server, so a project that stopped offering SSH must
  stop offering stanzas pointing at a port nothing answers on. The `Include`
  stays, so re-enabling needs no further edit to `~/.ssh/config`. Printing has
  nothing to mirror, so it still reports the error instead of emitting empty
  output to paste.
- Only the written form carries `UserKnownHostsFile`, pointing at the
  known_hosts it just wrote. Pinning the host key there keeps
  `StrictHostKeyChecking` meaningful without editing the file that records the
  user's trust in every other host they use; printed output cannot name a file
  the run never wrote, so it keeps emitting the line as a comment instead.

## Signals and Job Control

Keystrokes reach the remote job, never this process. Two mechanisms, chosen by
whether the attach has a PTY — not by which command is running:

- **Raw mode (any TTY attach: `run`, `box terminal attach`, `configure`,
  `shell`/`box exec create` with a PTY).** `MakeRaw` turns off ISIG, so Ctrl-C,
  Ctrl-Z, and Ctrl-\ are never signals here — they travel as the bytes 0x03,
  0x1a, 0x1c and the *remote* line discipline signals the remote foreground job.
  Nothing to forward, and the local CLI is never the target.
- **No PTY (`disco shell` into a pipe or redirect).** The local terminal is still
  cooked, so those keys raise real signals here. `proxySignals` catches them and
  sends a Signal frame instead of acting on them.

Ctrl-Z is handled, not merely forwarded: `Session.suspend` stops the remote job, hands
the terminal back in its pre-attach mode, stops this process, and on resume
takes the terminal back, sends CONT, and re-sends the window size (which may
have changed while stopped). Forwarding alone would leave the user attached to a
stopped job with no way to resume it; stopping alone would leave the remote
running unattended.

`OSConsole.Suspend` stops with **SIGSTOP**, not by resetting SIGTSTP's
disposition and re-raising it. A Go process that has notified SIGTSTP keeps a handler installed,
and the re-raised signal comes back to the handler instead of stopping — once
per suspend under job control, and in a livelock without it. SIGSTOP cannot be
caught and is never discarded for an orphaned process group. Only this process
is stopped, not the group, so a script that shares its process group is not
taken down with it.

Exit status follows the shell convention: a signal-killed command exits 128+N
(130 for Ctrl-C), decided by the sandbox-agent — see its `DESIGN.md` for why a
suspend request maps to SIGSTOP there too.

## Origin and Source Delivery

Every create carries an **origin**: this client's host identity plus the project
directory, which is the Git repository root of `-C` (the working directory by
default), or that directory itself outside a repository. `disco ls` filters on
its key rather than on the source root, because a local path identifies a
repository only on the machine holding it — it means nothing once the server is
remote, and collides across hosts and users.

Host identity comes from `internal/hostid` in the root module, deliberately
shared: a CLI and a server on the same machine must resolve the same value from
the same file, which is how the server recognizes a request as coming from its
own filesystem and binds the source instead of asking for a push.

After create, `sandboxcreate.DeliverSource` is called unconditionally and is a
no-op unless the server marked a source `delivery: push`. When it did, the
client waits for the sandbox to reach `awaiting_source`, pushes each such
source's commit to its branch (plus the workspace snapshot ref, when that
workspace is dirty — without it the sandbox comes up clean and the edits are
lost), and then reports the pushes complete. It pushes the commits the server
recorded at create, by explicit refspec, rather than whatever the local branches
now point at.

Delivery is decided per source, so the primary source and each `--include`
reference (below) are pushed or bound independently. Every push-delivered source
is pushed before any of them is reported: the sandbox resumes on that one
report, keyed by source slug, and resuming it earlier would start the harness
against a workspace still missing a source.

The pushes run out of the `sandboxcreate.LocalSources` that create returned, not
out of repositories re-resolved from the source arguments. For a directory with
no repository (below) that repository is a throwaway one holding the only copy
of what the sandbox was configured against, so it cannot be found again. Callers
`Close` them as soon as the sources have been delivered.

The push goes through the control plane's Git proxy, never directly to the
sandbox, which sits on a network the client cannot reach.

See [ADR 0001](../docs/adr/0001-sandbox-origin-and-remote-source-push.md).

## Uncommitted Work at Create

A dirty local workspace becomes a snapshot commit on top of the checked-out
commit, kept under `refs/discobox/run/`, and reaches the sandbox as uncommitted
changes on that same commit. `disco run --include-dirty` decides whether that
happens:

- `auto` (default) asks, and only when the workspace is actually dirty. The
  question is the standard picker, with "start from the last commit" leading, so
  the default answer is the one that carries nothing extra into the sandbox.
- `true` / `false` answer ahead of time; bare `--include-dirty` means `true`.
- Frontends express the question through `sandboxcreate.ConfirmIncludeDirtyFunc`
  rather than prompting themselves. A nil func means there is nobody to ask — no
  terminal — and the work is included: dropping a user's edits silently is worse
  than carrying them. The launcher does not use it: it owns the screen, so it
  asks in its own confirmation dialog and settles `IncludeDirty` to `true` or
  `false` before it calls the shared create at all.
- `true` is rejected for a remote URL or an explicit `@REF`, because a snapshot
  only ever sits on top of HEAD of a local working tree.

## Extra Sources

`disco run -i DIR` brings more sources into the same sandbox, repeated for more
than one. They become the create request's `sourceCodeReferences`, and the
mechanism they use is the primary source's, not a second one:

- Each is resolved by the same `resolveRunSource` — a local repository, a
  directory in no repository, or a remote URL, with the same `@REF` handling —
  so a reference carries a checkout commit and, when its working tree is dirty,
  its own snapshot. The dirty question is per source, because a working tree is:
  each reference is asked about its own uncommitted work rather than inheriting
  the primary source's answer.
- A local source keeps its own absolute path inside the sandbox, exactly as the
  primary source does, so `-i ../foo` lands at what `readlink -f ../foo` prints
  and a path means the same thing on both sides of the boundary. That path is
  also the reference's key in the request. A remote source has no host path to
  keep and goes under `/workspace/<slug>`.
- The slug is the directory's own name — `-i ../foo` is the source `foo` — and
  is what addresses its Git repository in a push. Two references that would take
  the same name are numbered here rather than left to the server, so the slug
  the client pushes to is the slug the server records. The primary source sends
  no slug at all: the server names it `primary`.
- Only the primary source names a working directory. A reference says where it
  goes and nothing about where the harness starts.

## Declared Sources

A repository can name the others it is worked on with, in
`.discobox/sources.json` at the primary source's root:

```json
{"foo": "https://github.com/acme/foo"}
```

`disco run` and the launcher both bring them in, as source code references
resolved exactly like `--include`:

- **A local checkout wins.** `foo` is looked for at the sibling of the primary
  source — `../foo` — and used when it is a directory, with its own
  dirty-workspace question and its own delivery. It is used whatever its
  `origin` says: a fork checked out next door is the usual reason for a
  mismatch and is what the caller has. The disagreement is reported rather than
  resolved silently, because a directory that merely shares the name looks
  identical from here.
- **A clone lands at the same path.** With no checkout, the declared URL is
  cloned to the sibling path the checkout would have occupied, not to
  `/workspace/<name>`. That is the point of declaring a source: `../foo` from
  the primary source resolves inside the sandbox whether or not the caller had
  foo checked out, so a script in the repository works for everyone.
- **The value is a Git URL, never a path.** Where a source is looked for locally
  is not the file's to say — the checkout is found by name, beside the primary
  source — so only the URL to fall back to belongs here. A path is refused
  rather than resolved: relative to the caller's working directory it would
  quietly bring in some other repository.
- `--include` outranks a declaration of the same source, and a declaration that
  resolves to something already brought in is skipped rather than refused.
- Only the primary source's file is read. There is no recursion, so a declared
  source's own declarations do nothing.
- The file is read from the working tree, not the checked-out commit. With
  `--include-dirty=false` the sandbox runs committed code from a source list the
  caller can see on disk, which is the less surprising of the two.
- A remote primary source declares nothing: the file lives in a checkout, and
  reading it would mean cloning the repository here first, which is the
  sandbox's job.
- `--declared-sources=false` leaves them out, for a caller who wants only what
  they named — or does not want a large clone on every run.

`sandboxcreate` resolves them and reports through
`PromptOptions.ReportDeclaredSource`; `disco run` prints one line per source on
stderr. The launcher passes no reporter — it owns its screen and has no status
line for per-source progress — so it brings the same sources in silently. See
[ADR 0056](../docs/adr/0056-a-repository-declares-the-sources-it-is-worked-on-with.md).

## A Directory That Is Not a Repository

Running against a directory in no Git repository is the same mechanism one step
further out: the directory's whole content is the uncommitted work of a
repository that does not exist yet.

`gitutil.InitOverWorkTree` builds that repository in a temporary directory whose
`core.worktree` points at the user's, so git acts on their files while writing
only into the temporary repository — no `.git` appears in the directory, and
deleting the repository leaves it as it was found. The base is a root commit of
the empty tree, the directory is snapshotted on top of it exactly as a dirty
workspace, and the sandbox comes up with the files as uncommitted changes.

- The source records `noLocalRepository`, and `localDirectory` still names the
  user's directory — never the temporary repository, which is one create's
  implementation detail. That flag is what makes the server choose `push`: there
  is nothing at that path to clone however reachable it is.
- Nothing is asked. The dirty-workspace question offers the last commit as its
  alternative and there is none, so `--include-dirty=false` and an explicit
  `@REF` are both rejected rather than reinterpreted.
- An empty directory is not an error: it snapshots nothing and the sandbox
  starts on the empty commit, which is the point of running in one.

See [ADR 0045](../docs/adr/0045-a-directory-with-no-repository-is-delivered-by-push.md).

## Git Transport to the Server

`git` is a subprocess that only understands URLs, so it cannot use the CLI's
local-IPC transport. `App.gitServerURL` bridges the gap: for a `unix://` or
`npipe://` endpoint it starts `endpoint.StartLoopbackProxy`, a loopback HTTP
listener that reverse-proxies onto the same server the API client uses, and
returns that address for the duration of the command. An `http(s)` endpoint is
already addressable and is returned unchanged.

Everything that shells out to git shares it — `sandboxcreate.DeliverSource`
(push at create) and `sandboxapply.FetchSource` (fetch at apply) — so the local
socket, which is the default endpoint, is not a server only half the CLI can
reach.

## Apply Output

`disco apply` (`internal/cli/apply.go`) performs git operations on the user's
own repository, so its output is an account of those operations rather than a
verdict. Every source reports the local repository and branch it lands on, the
sandbox repository and fetch ref it comes from, the base commit and *why* that
commit is the base (a prior recorded apply, or the merge base), each commit in
the range with author and date, and — once landed — the local commit each
sandbox commit became.

In printed text the two sides are **local** (this machine's repository) and
**sandbox**, never "host": the code and the API use "host" for machine identity
too (`internal/hostid`, `Origin.HostId`), and a reader of the output has no way
to tell the two senses apart. This is a wording rule for what users see —
identifiers and JSON keys keep ADR 0014's `Host*` vocabulary.

- `internal/cli/apply_report.go` owns the shape (`applyReport`) and its
  rendering (`applyPrinter`). Text output renders from the same struct that
  `-o json` emits, so the two can never describe different things.
- Every outcome is an `applyStatus` on the report, including failure:
  `applyOneSource` returns a report instead of an error, so a failed source
  keeps the context the run had already established rather than collapsing to
  one error line. The command's returned error is only the closing verdict.
- Failure statuses carry `nextSteps`: described sets of literal commands that
  resolve them, runnable by a human or an agent as printed. More than one step
  means alternatives, not a sequence — a dirty sandbox offers both committing
  the work there and `--allow-dirty` — and each re-run is spelled out for that
  source, `--dir` override included.
- A source applies only into the directory the sandbox actually came from:
  `resolveApplyHostDir` requires the sandbox's `Origin.HostId` to be this
  machine's `hostid`, the source to have recorded a `LocalDirectory`, and that
  directory to still exist. Each failure says which of those it was — another
  machine, a remote-cloned source, a moved checkout — because "pass --dir" is
  only actionable once you know why. `--dir` is the escape hatch and
  deliberately skips the check, so the report records how the directory was
  chosen (`hostPathOrigin`) and prints it under "chosen by".
- A merge base that does not exist means the target repository shares no
  history with the sandbox — almost always a `--dir` pointed at the wrong
  repository — and is reported as that rather than as a git error.
- `--allow-dirty` applies a source whose sandbox working tree is dirty. The
  status check still runs and its entries are still reported (with
  `dirtyIgnored`): the flag means the user chose to leave that work in the
  sandbox, not that nobody needs to know it is there.
- `--debug` additionally echoes every git command as it runs, via
  `gitutil.WithTracer`, on stderr so it never interleaves into the report.
  `gitutil` redacts credentials in traced arguments centrally, so no new git
  call site can leak a token by forgetting to.

## Harness Configure Step

`disco box harnesses configure` (`internal/cli/harness.go`) drives a harness's
interactive configure flow. The server owns applying the result; the CLI only
sequences the calls and hands the user the terminal in between:

1. `POST .../configure` — the server creates the ephemeral `harnessMode: config`
   sandbox and returns it.
2. `POST .../configure/attach` — the server seeds the previous configuration into
   it.
3. attach to the virtual `"primary"` exec (`primaryExecID`) — **this is what
   launches the configure command**, since the sandbox-agent defers the primary
   terminal in config mode. That ordering is why seeding always lands first, and
   why nothing waits for a terminal to appear first: none exists until attach.
4. `POST .../configure/commit` — the server reads the command's real exit status,
   applies the secrets and files it wrote, and deletes the sandbox.

`runHarnessConfigure` takes streams rather than a `*cobra.Command` so its caller
can hand it the real terminal via `tea.Exec`: the launcher's harnesses screen
does exactly that, through `apiDataSource.ConfigureHarness`.

`disco configure` (aliases `config`, `conf`, `c`, `init`) is the launcher opened
on that screen — `tui.WithHarnesses()`, reachable in the window itself on `F3` —
and nothing else. It is not a second menu over the same harnesses: managing them
and running something on one are the same job from two ends, and one list with
one set of keys is what the window already provides.

The window's half of the data seam lives in `internal/cli/tui_harnesses.go`:
`Harnesses`/`HarnessSecrets` map harness configs (plus the project's default
pointer and secret bindings) onto `tui.Harness`, `DoHarness` runs disable and
set-default, and `ConfigureHarness`/`EditHarnessFile` are the two that need the
real terminal and reuse `runHarnessConfigure` and `editHarnessFile` unchanged.
Disable releases the project default first when the target holds it, since the
server refuses to deconfigure a default harness.

Re-running reconfigures and clobbers any in-flight attempt. Nothing here parses
the configure output or creates secrets: a client that crashes mid-flow cannot
leave a half-applied harness, and an abandoned sandbox is reaped by the server.
See `server/internal/resources/harnessconfigs/DESIGN.md`.
