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
| `internal/tui` | Bubble Tea presentation and interaction state, expressed against its own `DataSource` interface. |

## UI Dependency Direction

- Keep reusable sandbox creation workflows out of `internal/cli`; place them in
  `internal/sandboxcreate` so Cobra and TUI adapters consume the same behavior.
- `internal/tui` must not import `internal/cli`. It owns presentation state and
  frontend contracts only; API and terminal adapters belong outside it.
- `internal/cli` may adapt generated API clients and terminal transports to the
  TUI's interfaces, but must not become the owner of logic shared by frontends.

Local server auto-launch is a release-only capability. Normal and development
builds leave it disabled; release CLI binaries opt in at build time by setting
`cli.serverAutoLaunch` to `true` with the Go linker's `-X` flag. `--no-start`
remains the runtime override for release binaries.

Advanced configuration and low-level resource commands are grouped beneath the
visible `disco box` command: `sandbox`, `terminal`, `exec`, `provider`, `pool`,
`job`, `harnesses`, and `hooks` are not root commands.

`disco exec` is the exception: the root command is the everyday one-shot "run
this in my sandbox" verb, while `box exec create` stays the raw, fully
configurable form (workdir, env, user, detach, explicit `-i`/`-t`). Both drive
the same exec create/attach/status sequence. The root form has no `-it`: stdin
is always attached, and a PTY is requested only when stdin, stdout, and stderr
are all terminals, so pipes and redirects behave like a local command. The attach
session writes stdout frames to stdout and stderr frames to stderr, with no
special case for the PTY: a TTY exec merges at the PTY and simply never sends a
stderr frame, which the client neither detects nor needs to.

`disco exec` with no command runs the sandbox user's login shell. The CLI never
names that shell: it sets `shell: true` on the exec create request and the
sandbox resolves the run user's shell from its own passwd database, because the
local `$SHELL` describes this machine and says nothing about the identity the
exec runs as. `box exec create --shell` is the same request in raw form.

`disco tools` groups the everyday development tools run inside a sandbox against
one of its sources — `tools git` today. Which sandbox to run in is the one thing
every tool has in common, so `--sandbox-id` is a persistent flag on `tools`
itself and every subcommand inherits it. Everything else, including where in the
sandbox the tool runs (`git`'s `--source`/`-s`), belongs to the subcommand that
means it. Each then drives the same exec create/attach/status sequence as
`disco exec`. Flag parsing stops at the first positional argument
(`SetInterspersed(false)`), so everything from there on reaches the tool verbatim.

The default path sends no workdir and fetches no sandbox record: an exec with no
workdir already lands in the sandbox-agent's default exec directory, which is the
primary source's. Only `git --source` has to `GetSandbox` to turn a slug into a
directory. With a full `--sandbox-id` the whole command is create + attach +
start + status, so a one-shot `disco t git status` costs no round trip it does
not need.

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

## Choosing a Sandbox Interactively

Commands that act on "the sandbox I am working in" take a sandbox identifier —
as `--sandbox-id` (`exec`) or an optional positional `SANDBOX_ID` (`apply`,
`box get`) — and fall back to `selectSandbox` (`internal/cli/picker.go`) when
it's omitted, never to a guess:

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
  sandbox autostart will be a future `disco attach` behavior, and until then the
  client never starts a sandbox to keep an attach alive.
- Resumable actions (input, signals, and close-input) carry monotonically
  increasing positions. The client retains a bounded window until the shim
  acknowledges applying them; reconnect resends the unacknowledged suffix and
  the shim deduplicates it by logical-session token. A full window backpressures
  stdin instead of silently dropping accepted input. Resize is idempotent state,
  so only its latest value is retained and restored.
- Plain exec attaches remain direct and fail on disconnect. Resuming a pipe exec
  requires byte-exact output positions as well as input resumption; a terminal
  screen repaint is not an acceptable substitute for a piped stdout stream.
- Connection lifecycle notifications are transport events, not terminal output.
  CLI attach ignores them; the TUI adapter maps them into its `TerminalEvent`
  stream.
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

## Signals and Job Control

Keystrokes reach the remote job, never this process. Two mechanisms, chosen by
whether the attach has a PTY — not by which command is running:

- **Raw mode (any TTY attach: `run`, `box terminal attach`, `configure`,
  `exec`/`box exec create` with a PTY).** `MakeRaw` turns off ISIG, so Ctrl-C,
  Ctrl-Z, and Ctrl-\ are never signals here — they travel as the bytes 0x03,
  0x1a, 0x1c and the *remote* line discipline signals the remote foreground job.
  Nothing to forward, and the local CLI is never the target.
- **No PTY (`disco exec` into a pipe or redirect).** The local terminal is still
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
no-op unless the server marked the source `delivery: push`. When it did, the
client waits for the sandbox to reach `awaiting_source`, pushes the source's
commit to its branch (plus the workspace snapshot ref, when the workspace is
dirty — without it the sandbox comes up clean and the edits are lost), and then
reports the push complete. It pushes the commit the server recorded at create,
by explicit refspec, rather than whatever the local branch now points at.

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
  rather than prompting themselves. A nil func means there is nobody to ask —
  no terminal, or the TUI, which owns the screen — and the work is included:
  dropping a user's edits silently is worse than carrying them.
- `true` is rejected for a remote URL or an explicit `@REF`, because a snapshot
  only ever sits on top of HEAD of a local working tree.

## Git Transport to the Server

`git` is a subprocess that only understands URLs, so it cannot use the CLI's
local-IPC transport. `App.gitServerURL` bridges the gap: for a `unix://` or
`npipe://` endpoint it starts `localipc.StartLoopbackProxy`, a loopback HTTP
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
   why there is no `waitForPrimaryTerminal` here: no terminal exists until attach.
4. `POST .../configure/commit` — the server reads the command's real exit status,
   applies the secrets and files it wrote, and deletes the sandbox.

`runHarnessConfigure` takes streams rather than a `*cobra.Command` so its callers
can share it: the full `tui` dashboard and the inline `disco configure` menu
(`internal/cli/configure.go`) both hand it the real terminal via `tea.Exec`.

`disco configure` (aliases `config`, `conf`, `c`, `init`) is a small inline (no
alternate screen) Bubble Tea menu over the project's harnesses for the common
enable/reconfigure, disable, and set-default actions. Enable/reconfigure reuses
`runHarnessConfigure` through `tea.Exec`; disable and set-default are plain API
calls. Disable confirms first, since deconfigure deletes the agent's secrets and
files. It is the focused counterpart to the `box harnesses` subcommands.

Re-running reconfigures and clobbers any in-flight attempt. Nothing here parses
the configure output or creates secrets: a client that crashes mid-flow cannot
leave a half-applied harness, and an abandoned sandbox is reaped by the server.
See `server/internal/resources/harnessconfigs/DESIGN.md`.
