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
| `internal/diffrender` | Unified-diff parsing and terminal layout, with no knowledge of sandboxes or the API. |

## UI Dependency Direction

- Keep reusable sandbox creation workflows out of `internal/cli`; place them in
  `internal/sandboxcreate` so Cobra and TUI adapters consume the same behavior.
- `internal/tui` must not import `internal/cli`. It owns presentation state and
  frontend contracts only; API and terminal adapters belong outside it.
- `internal/cli` may adapt generated API clients and terminal transports to the
  TUI's interfaces, but must not become the owner of logic shared by frontends.
- The launcher never reimplements a command. `apply` is the Cobra command
  itself: `apiDataSource.Interact` builds it, binds it to the streams `tea.Exec`
  hands over while the window is suspended, and executes it.
- `diff` and `status` are drawn in a pane instead, and are the same commands —
  spawned as a child `disco diff <id>` on a local pty sized to the pane
  (`tui_local.go`). The pty is the child's *controlling* terminal, which is the
  point: a pager reads its keys from `/dev/tty`, so without one `less` would
  take them from the real terminal, out from under the window drawing it. The
  child inherits this invocation's `--server`, `--project` and `--chdir`; the
  token goes through the environment rather than the argument list, which every
  process on the machine can read.
- Either way what runs is `disco diff` with its own rendering, flag defaults,
  pager and terminal detection, not a second implementation that drifts from it.
  A launcher that cannot be reproduced from a shell is the thing to avoid.
- `disco tui --leader`/`DISCOBOX_LEADER` sets the terminal pane's prefix key,
  normalized by `tui.NormalizeLeader`: a bare character is taken as Ctrl-that,
  since a leader that is not a chord would be a character you could never type.
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
- A short ID is matched against that candidate list exactly like an explicit
  SANDBOX_ID argument elsewhere. Zero matches is not an error — it just means
  `args[0]` was never a sandbox reference — but more than one is reported as
  ambiguous, since its shape said it was meant as an ID.

A match consumes `args[0]` as SANDBOX_ID and leaves the rest as CMD. No
match — including no arguments at all — means every argument is CMD, and the
sandbox falls back to the same picker `disco apply`/`disco diff` use when
SANDBOX_ID is omitted.

`disco shell` with no command runs the sandbox user's login shell. The CLI
never names that shell: it sets `shell: true` on the exec create request and
the sandbox resolves the run user's shell from its own passwd database,
because the local `$SHELL` describes this machine and says nothing about the
identity the exec runs as. `box exec create --shell` is the same request in
raw form.

`disco tools` groups the everyday development tools run inside a sandbox against
one of its sources — `tools git` today. Which sandbox to run in is the one thing
every tool has in common, so `--sandbox-id` is a persistent flag on `tools`
itself and every subcommand inherits it. Everything else, including where in the
sandbox the tool runs (`git`'s `--source`/`-s`), belongs to the subcommand that
means it. Each then drives the same exec create/attach/status sequence as
`disco shell`. Flag parsing stops at the first positional argument
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
as `--sandbox-id` (`box exec`, `box terminal`), an optional positional
`SANDBOX_ID` (`status`, `diff`, `apply`, `attach`, `box get`), or a leading
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
  rather than prompting themselves. A nil func means there is nobody to ask — no
  terminal — and the work is included: dropping a user's edits silently is worse
  than carrying them. The launcher does not use it: it owns the screen, so it
  asks in its own confirmation dialog and settles `IncludeDirty` to `true` or
  `false` before it calls the shared create at all.
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

## Status

`disco status` (`internal/cli/status.go`) is `git status` for a sandbox's source
working trees. It is deliberately thin: it selects a sandbox the way `apply` and
`diff` do — optional positional `SANDBOX_ID`, otherwise `selectSandbox` — shares
`selectSources` so `--source` means the same thing in all three, and runs one
`git status` per source in that source's working directory.

- No scratch index, unlike `diff`: `git status` already reports files git has
  never been told about, so the working tree as it stands is exactly the
  subject and nothing has to be constructed to see it.
- No `sh -c` either. The command is argv, so a user-supplied pathspec is an
  argument and never shell syntax; pathspecs come after `--`, which is also what
  keeps the optional `SANDBOX_ID` positional unambiguous (`cmd.ArgsLenAtDash`).
- The output streams through the normal exec attach, unparsed and unrendered.
  Nothing here has to understand git's output, and a PTY — asked for only when
  stdin, stdout, and stderr are all terminals — gets git's own color and
  columns for free.
- The flags are `git status`'s own, in git's spelling, for the subset that still
  means something against a working tree that is not on this machine. `-s` is
  therefore git's `--short`, and `--source` has no shorthand in this command.
  A mode-taking flag needs its value attached with `=` (`-u=no`), because pflag
  reads git's compact `-uno` as further shorthands.
- `--color` is `-c color.status=<when>`, not a flag: `git status` has none of
  its own. `auto` is git's default and is passed nothing, so a PTY colors and a
  pipe does not, with no special case.
- Per-source headings are printed only when there is more than one source, and
  only to stdout when stdout is a terminal and no machine-readable format
  (`--porcelain`, `-z`) was asked for; otherwise they go to stderr, so
  `disco status --porcelain` is git's bytes and nothing else.
- A source that cannot be reported is reported and the rest still run; the
  command's error is the closing verdict, as in `apply` and `diff`.

There is no pager, following `git status` itself.

## Diff

`disco diff` (`internal/cli/diff.go`) answers "what has this sandbox changed?"
It selects a sandbox exactly like `apply` — optional positional `SANDBOX_ID`,
otherwise `selectSandbox` — and shares `selectSources` with it, so `--source`
names the same thing in both.

- The base is `checkout.commit`, so `disco diff` means what
  `git diff <that commit>` means inside the sandbox. It is displaced only by
  the merge base with `refs/remotes/origin/<checkout.refName>`, and only when
  that merge base is a strict descendant of `checkout.commit` — so it excludes
  commits the sandbox pulled rather than wrote, is a no-op when nothing was
  integrated, and never moves the base backwards after an upstream rewrite. `--base` overrides both, and takes the
  keyword `snapshot` for the dirty-workspace ref.
- Resolution happens **inside the sandbox**, from its own refs
  (`internal/cli/diff_base.go`), and the chosen base and its reason are
  reported with every diff. See
  [ADR 0018](../docs/adr/0018-disco-diff-resolves-its-base-inside-the-sandbox.md)
  for why it is not computed locally the way `apply`'s is, and why the
  dirty-workspace snapshot is not the default: excluding work the user handed
  the sandbox makes the command answer "nothing changed" about a workspace
  full of changes.
- The right-hand side is the sandbox's whole working state written into a
  scratch index (`GIT_INDEX_FILE`, seeded from the real index for its stat
  cache) as a tree object, so the diff is tree against tree. Comparing against
  the working tree instead cannot see files git was never told about, and
  against a base that *does* contain them reports them as deletions. The
  repository's own index is never touched — no `git add` into it, not even an
  intent-to-add — so diffing cannot disturb the work going on in the sandbox.
- Pathspecs narrow the `git add` as well as the diff. Only what they cover is
  ever read, so entries outside them keep whatever the seeded index held and
  untracked files outside them are never hashed — without this, a diff of one
  directory still pays to hash the whole tree and then discards it.
- Building that tree is the expensive part: `git add` hashes and compresses
  every untracked file into the sandbox's object database, on every run,
  leaving unreachable objects behind. `checkUntrackedPayload`
  (`internal/cli/diff_untracked.go`) measures the payload first with
  `git ls-files -o` — the same walk, honoring the same ignore rules, without
  the hashing — and past `--max-untracked` refuses, naming the largest
  directories. It runs once per source before the mode branch, so one
  measurement covers the streamed diff, the rendered one, and the commit
  `--base local` fetches. It never silently excludes: a diff that omits an
  agent's work is worse than one that says why it stopped.
- The diff runs *inside* the sandbox, not by fetching to this machine. That is
  what lets it show uncommitted work, which `apply`'s fetch cannot see, and it
  needs no local repository at all.
- The whole thing is one `sh -c` script per source, with every word shell-quoted
  in Go (`shellCommand`), which is what lets a user-supplied pathspec through
  safely.
- A source that cannot be diffed is reported and the rest still run; the
  command's error is the closing verdict, as in `apply`.

### Flags

The flags are `git diff`'s own, in git's spelling, for the subset that still
means something here. The ones that choose *what* to compare are deliberately
absent: the right-hand side is always the sandbox's working tree, and the left
is the base above, which `--base` is the one way to override. Pathspecs come after `--`, so the optional
`SANDBOX_ID` positional stays unambiguous (`cmd.ArgsLenAtDash`).

Flags that select one of git's own output formats (`--stat`, `--numstat`,
`--name-only`, …) are passed straight through *and* switch off rendering: what
they ask for is git's output, so rendering it would be answering a different
question.

### Two Output Paths

| | when | how |
| --- | --- | --- |
| Rendered | stdout is a terminal and no raw-output flag | capture, parse, lay out |
| Passed through | redirected, `--patch`, or a git output format | stream the exec |

- Rendered output is a document, so its per-source headings go to stdout with
  it. Passed-through output is a patch, so nothing but the patch reaches stdout
  and the headings go to stderr — `disco diff > sandbox.patch` stays a patch
  file `git apply` accepts.
- The rendered path is the only one that buys the whole diff up front
  (`sandboxCommandOutput`): layout cannot start until the text is complete. The
  passed-through path streams via the normal exec attach, which is what a piped
  patch or a very large diff wants, and which gets a PTY — and therefore git's
  own color — only when all three streams are terminals.
- Resolving the base is its own exec, ahead of the diff, so the base and its
  reason can be reported rather than inferred from the output.
- `diffView` makes every terminal-dependent decision once — render or stream,
  width, color, background, pager — against the **real** stdout, before the
  pager replaces it. Measuring width or querying the background once output
  goes down a pipe answers about the pipe, not the screen. The color profile is
  likewise detected from the terminal and only then forwarded to the pager.

### Paging

At a terminal the output is paged, following git: `DISCOBOX_PAGER`, `GIT_PAGER`,
`PAGER`, then `less`, with `LESS=FRX` and `LV=-c` supplied only when the user
has not set them. `R` is not optional — without it a rendered diff arrives as
literal escape codes. A pager of `cat` means "do not page", and `--no-pager`
says the same on the command line. Nothing redirected is ever paged, which is
what keeps `disco diff > x.patch` and the tests writing straight through.

Quitting the pager closes the pipe, so every write after that fails with
`EPIPE`. That is a reader who has seen enough, not a failure: it ends the run
without an error, without a nonzero exit, and without starting the next source
whose output nothing would read either. No signal is involved — Go re-raises
SIGPIPE only for writes to fd 1 and 2, and the pager's stdin is neither, so the
deferred close still runs and the pager exits cleanly instead of being orphaned.
A redirected `disco diff | head` is not paged, writes to fd 1, and dies on
SIGPIPE like any other Unix tool, which is the behavior to keep.

Under a pager the streamed path asks for no PTY — stdout is a pipe — so git
would emit no color of its own; it is passed `--color=always` explicitly
instead, exactly as git does for itself. The rendered path never gets that flag:
it colors the patch itself, and escape codes in the text would corrupt the
parse.

### Comparing Against This Machine

`--base local` and `--apply-preview` are the two modes whose sides start out on
different machines (`internal/cli/diff_local.go`). Two working trees cannot be
diffed where they sit and only committed objects travel, so the sandbox's
working state is written as a tree, wrapped in a commit under
`refs/discobox/diff/<sandbox>/<slug>` with `HEAD` as its parent — which keeps
the fetch incremental — and fetched through the same proxy `apply` uses. This
machine's side is built by `gitutil.CurrentWorkspaceTree`. Neither index is
touched on either end.

They differ only in the left-hand side, which is why they live in `diff` rather
than in `apply`: every other flag — the git ones, the pathspecs, the rendered
view, the pager — is unaffected by that choice.

- `--base local` compares against this machine's working tree, so your own
  uncommitted work counts as a difference.
- `--apply-preview` compares against **where `apply` would start** — the last
  recorded `AppliedSourceCommit` for the source, else `gitapply.MergeBase` with
  local `HEAD`, both resolved here because that is where `apply` resolves them.
  It answers "what would applying this land here?", and excludes your own local
  work entirely. It is exclusive with `--base`, which it chooses itself.

Both need the source's local repository, resolved by `apply`'s own
`resolveApplyHostDir` and overridable with `--dir slug=path`. That precondition
is exactly what ADR 0018 keeps out of the default base, which is why these are
opt-in.
- `--color=auto|always|never` is git's, and only decides color. Rendering is
  decided by the terminal and the flags above, never by `--color`.

Rendering itself lives in `internal/diffrender`, not here: it is a unified-diff
parser plus a layout — including syntax highlighting of the code inside the
diff — with no knowledge of sandboxes, and `internal/tui` may want it later. See
that package for the layout and highlighting rules.

The diff algorithm is never ours. git computes every diff, inside the sandbox,
so `-w`, `-M`, and `-U` behave exactly as they do in a shell and the output
stays a patch `git apply` accepts. The only comparison this repository performs
is the intra-line emphasis in `diffrender`, over two already-paired lines.

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
   why there is no `waitForPrimaryTerminal` here: no terminal exists until attach.
4. `POST .../configure/commit` — the server reads the command's real exit status,
   applies the secrets and files it wrote, and deletes the sandbox.

`runHarnessConfigure` takes streams rather than a `*cobra.Command` so its caller
can hand it the real terminal via `tea.Exec`: the inline `disco configure` menu
(`internal/cli/configure.go`) does exactly that.

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
