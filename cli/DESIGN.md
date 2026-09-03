# CLI Design

The CLI module owns the `discobox` command implementation and talks to the
control plane through generated root-module API clients plus a few handwritten
transport helpers where OpenAPI does not model the stream.

## Package Map

| Package/path | Ownership |
| --- | --- |
| `cmd/discobox` | Binary entrypoint. |
| `internal/cli` | Cobra command tree, output formatting, local server auto-start, TUI API adapter, and the attach transports and policy layered on `execstream/client`. |
| `internal/sandboxcreate` | UI-independent client-side sandbox request preparation and creation, including prompt options, source resolution, workspace snapshots, environment/secrets, local user identity, and source push delivery. |
| `internal/sandboxgit` | The client's git transport to a sandbox: the worktree and origin repository URLs the control plane proxies, bearer-token auth on those requests, and the client-side ref names that record what has been sent. Shared by create, apply and push. |
| `internal/sandboxpush` | `discobox push`: re-delivering a push-delivered source's commits into the origin repository its sandbox fetches from, under a lease (ADR 0058). |
| `internal/origin` | Resolves the client host and project directory a sandbox is created from. Host identity itself is shared, in the root module's `internal/hostid`. |
| `internal/gitunborn` | A repository with no commits: whether HEAD is unborn, and the tree of a working tree that has no HEAD to be read against. Shared by create (ADR 0083) and apply (ADR 0084), which both have to ask. |
| `internal/tui` | The `discobox tui` launcher: Bubble Tea presentation and interaction state, expressed against its own `DataSource` interface. See [`internal/tui/DESIGN.md`](internal/tui/DESIGN.md). |
| `internal/portforward` | Frontend-independent dynamic port forwarding: local listeners kept in sync with a remote's announced ports, over a caller-supplied dialer. |
| `internal/keys` | The leader: its default, its `DISCOBOX_LEADER` override, normalization, and the byte a raw stream matches it as. Owned here because the launcher's panes and a plain attach must reserve the same key. |

## UI Dependency Direction

- Keep reusable sandbox creation workflows out of `internal/cli`; place them in
  `internal/sandboxcreate` so Cobra and TUI adapters consume the same behavior.
- `internal/tui` must not import `internal/cli`. It owns presentation state and
  frontend contracts only; API and terminal adapters belong outside it.
- `internal/cli` may adapt generated API clients and terminal transports to the
  TUI's interfaces, but must not become the owner of logic shared by frontends.
- The launcher never reimplements a command, and never steps aside for one.
  `apply` is drawn in an overlay pane from either screen — the list and the
  workspace — and is the Cobra command itself, spawned as a child
  `discobox apply <id>` on a local pty sized to the pane (`tui_local.go`). The pty
  is the child's *controlling* terminal, so anything reading its keys from
  `/dev/tty` reads them from the pane rather than from the real terminal, out
  from under the window drawing it. The child inherits this invocation's
  `--server`, `--project` and `--chdir`; the token goes through the environment
  rather than the argument list, which every process on the machine can read.
- Where that pty comes from is `internal/localpty`, whose `PTY` is everything a
  pane needs of one: read, write, resize, close, and `io.EOF` when the command
  exits. Unix opens a pty pair through `creack/pty`; Windows creates a
  pseudo-console and starts the child itself, because a ConPTY is attached
  through a thread attribute `os/exec` cannot carry — so `localpty.Start` takes
  argv rather than an `*exec.Cmd`. See
  [ADR 0065](../docs/adr/0065-the-cli-owns-its-pty-seam-and-windows-gets-conpty.md).
- What runs is therefore `discobox apply` with its own flag defaults and terminal
  detection, not a second implementation that drifts from it. A launcher that
  cannot be reproduced from a shell is the thing to avoid.
- Bare `discobox` runs the launcher when stdin and stdout are both terminals, and
  prints its help otherwise (`App.runTUI`, reached from the root command's
  `RunE` and from `discobox tui`). Typing a program's name is how you ask for it,
  and the launcher is the one thing you can ask for without knowing a
  subcommand; a pipe, a script or CI expected output, and a full-screen window
  is not an answer to that. The root's `Args` is left at cobra's default, which
  turns an unrecognized first argument into "unknown command" rather than
  handing it to the launcher. The leader there comes from the environment only:
  a flag would have to be persistent to be reachable, and every subcommand would
  carry one that means nothing to it.
- `discobox configure` is the same launcher opened on its harnesses screen
  (`tui.WithHarnesses()`), not a window of its own. See *Harness Configure
  Step*.
- `discobox run` and `discobox attach` are the same launcher, opened on one run
  (`tui.WithRun`) and on one discobox (`tui.WithAttach`), not a terminal stream
  on the caller's screen. What run creates is a machine with terminals,
  services and ports on it, and the workspace screen is where all of that
  already is; a second, poorer view of the same session is the thing to avoid.
- **`discobox run` hands the window its request and the window makes the
  discobox** (`App.runWindowRequest` → `tui.WithRun` → `Model.runRequest`). It
  takes the path Enter in the prompt takes, so the question about uncommitted
  work is the window's own dialog and the wait is the window's own screen —
  which is the point: one flow, asked and drawn one way, however the run was
  started. The flags are still parsed on this side first
  (`sandboxcreate.ParsePromptOptions`): a flag that contradicts itself is the
  command's error to report, on the terminal, before a window opens over it.
- Such a window *is* the attach on what it opened or made, so leaving it leaves
  the window: the leader's `d`, and the primary session ending, close it
  (`Model.exit`) rather than falling back to a list nobody asked for, and an
  attach that never came up ends the window with its own failure, which
  `tui.Run` hands back as the command's error. Detaching still leaves every
  session running.
- `--raw` creates the discobox on this side and streams its terminal, which is
  what both commands always were: for a pipe, a recording, or a terminal to
  keep as it is. It is also what a run or attach with no terminal to draw a
  window on gets (`canOpenWindow`), the same rule bare `discobox` follows, and
  what `-d` implies — it prints the discobox on stdout, which a window would be
  sitting on.
- `discobox attach`'s window is opened on the sandbox record the command
  fetches rather than on its id, so the attach starts without waiting for a
  listing to come back. The window follows the server from there
  (`Model.currentBox`).
- `DISCOBOX_LEADER` sets the prefix key, normalized by `keys.NormalizeLeader`:
  a bare letter is taken as Ctrl-that, since a leader that is not a chord would
  be a character you could never type, and only a letter is accepted because the
  leader has to survive being turned back into the byte a terminal sends.
  `discobox tui --leader` overrides it for the launcher; nothing else takes a flag.
  It is one key for both the launcher's panes and a plain attach's detach chord.
- Attach and shell are terminals rather than commands, and are drawn inside the
  window by the `termpane` module. `apiDataSource.Open` connects one:
  `framedTerminal` (`internal/cli/tui_terminal.go`) presents the framed exec
  attach as the byte stream a pane draws. Attach targets the virtual primary
  exec id and needs no start — the agent resolves the sandbox's current primary
  terminal and relaunches it if it has stopped. A shell creates a new
  interactive TTY exec, the same one `discobox shell` with no command runs, carrying
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

## CLI State Directory

`cliStateDir()` (`internal/cli/statedir.go`) is `<state>` throughout this
document: the picker's memory, the launcher's unsent prompts, this machine's
iroh identity, the SSH identity, and the generated per-project `ssh_config`
files. It is state the CLI derives,
not configuration anyone edits, so it follows each platform's convention for
that — `$XDG_STATE_HOME` or `~/.local/state` on Unix, `%LOCALAPPDATA%` on
Windows, which is the local one rather than the roaming `%APPDATA%`: an SSH
identity belongs to a machine, and a path to a generated config on this disk
means nothing on another. `XDG_STATE_HOME` overrides it everywhere, Windows
included, because nothing there sets it by accident.

Everything created under it goes through `ensureStateDir`, which creates the
directory *and* restricts it to this user. The two are one step because a mode
is not a permission on Windows: `os.WriteFile(path, data, 0600)` sets the
read-only attribute at most and never writes an ACL, so a file lands with
whatever its parent grants — and a profile that grants a group anything grants
it on every private key written underneath. `restrictToUser`
(`statedir_windows.go`) replaces the inherited list with this user, SYSTEM and
Administrators, and takes ownership; on Unix it is a no-op, because the mode
bits already said it. Files are restricted individually as well as inheriting
from the directory, since a run before the fix may have left one readable.

This is not cosmetic: OpenSSH refuses to read a config, a `known_hosts` or a
private key another principal can reach, and reports only "Bad owner or
permissions". `assertPrivateToUser` in the tests checks the real thing on each
platform — the mode on Unix, the owner and every ACE on Windows.

The JSON memories under it — `recent-selections.json` and `prompt-drafts.json` —
are written by `writeStateFile` (`internal/cli/statefile.go`) through a
temporary file beside the target, so a crash mid-write cannot leave a reader
parsing half a file for the rest of the install's life. Each is bounded to a
number of entries and each is best-effort on every read and write: a missing,
unwritable or corrupt file costs the convenience and never the command.

- `prompt-drafts.json` (`internal/cli/drafts.go`) is the launcher's composer
  contents per project directory: what `tui.Session.Draft` is loaded from and
  what `DataSource.SaveDraft` writes. Keyed by the resolved
  `origin.ProjectPath`, because a prompt written in one checkout must not come
  back in another. An empty prompt deletes its entry rather than storing nothing
  under it, and a prompt past the cap is cut on a rune boundary — a state file
  is not where a pasted log belongs.

Local server auto-launch is a release capability. Normal and development builds
leave it disabled; release CLI binaries opt in at build time by setting
`cli.serverAutoLaunch` to `true` with the Go linker's `-X` flag.

`DISCOBOX_SERVER_AUTOLAUNCH` overrides that default in either direction, so the
behaviour can be exercised from a development build without cutting a release
one — otherwise the only way to test a change to it is to link a binary the way
a release links it. The development default stays off, because a developer runs
the server themselves under `task dev` and a second, quietly forked one would
race it for the same socket. `--no-start` is the last word either way: it is the
per-invocation override, and nothing about the build or the environment outranks
somebody typing it.

The launched process is this binary re-invoked, and the argv comes from
`App.serverLaunchArgs`, which reads the path off the command tree rather than
naming it. A path spelled by hand is a reference nothing checks: the server
command moved under `admin`, the literal `[]string{"server"}` did not, and every
release launched a child that exited instantly with `unknown command`.

Waiting on it is two-stage, against `health.Status` from `/healthz`. A server
that never answers has died and is given `StartTimeout`; one that answers
`starting` is working and is given `ReadyTimeout`, with each new phase drawn on
one status line — rewritten in place and taken back down before the command that
wanted the server writes anything — so the wait says what it is waiting for
without leaving a phase per line scrolled above unrelated output. The child's output goes to a log file
(`endpoint.ServerLogPath`), and the last launch's tail is what a failed wait
reports — the alternative, discarding it, is what made a server dying on startup
indistinguishable from one that was merely slow.

That log is the launched server's only account of itself: it has no terminal,
and nothing else records what it did. So it lives with the server's state rather
than beside the socket, which sits in a runtime directory the system clears; it
is appended to across launches behind a banner line, so the run that failed
survives the restart that followed it; and it rotates once at a size cap so
appending forever cannot fill a disk. Both ways of starting the process write
there — the systemd user unit gets `StandardOutput=append:` onto the same file,
so where a server's output lives does not depend on whether this machine had
`systemd-run`. `discobox admin server logs` reads it back, with `--follow`,
`--tail`, `--previous`, and `--path`.

Advanced configuration and low-level resource commands are grouped beneath the
visible `discobox admin` command: `project`, `sandbox`, `terminal`, `exec`,
`provider`, `pool`, `job`, `harnesses`, and `hooks` are not root commands.

The global `--project`/`-p` flag is hidden from help alongside `--chdir`: it
still works everywhere, and the launcher and scripts still pass it, but a
project is advanced configuration and belongs with the rest of it under
`discobox admin`.

`discobox admin project` is the only command group not scoped by the global
`--project` flag: its arguments name the project being acted on, resolved by
`resolveProjectID` from the same selectors `-p` accepts (the `default` alias, a
full or short ID, or the display name). `set-default` moves the flag
`default` resolves to, so it is how `-p`'s own default is chosen; there is no
unset. `create --from` copies an existing project's configuration
([ADR 0023](../docs/adr/0023-projects-are-created-by-copy-and-deleted-only-when-empty.md)),
with `--copy` selecting what comes across and `--copy none` taking nothing.

`discobox shell` is the exception: the root command is the everyday one-shot "run
this in my sandbox" verb, while `admin exec create` stays the raw, fully
configurable form (workdir, env, user, detach, explicit `-i`/`-t`). Both drive
the same exec create/attach/status sequence. The root form has no `-it`: stdin
is always attached, and a PTY is requested only when stdin, stdout, and stderr
are all terminals, so pipes and redirects behave like a local command. The attach
session writes stdout frames to stdout and stderr frames to stderr, with no
special case for the PTY: a TTY exec merges at the PTY and simply never sends a
stderr frame, which the client neither detects nor needs to.

`discobox shell [DISCOBOX_ID] [CMD...]` (`internal/cli/shell.go`) takes the sandbox
and the command as one positional list rather than a `--discobox-id` flag, since
which sandbox to run in is picked as often by ID as left to the picker. Cobra
sees one flat `[]string` and cannot tell DISCOBOX_ID from CMD apart, so
`resolveShellTarget` decides: `args[0]` is tried against the sandboxes
`discobox ls` shows for the current project directory (`matchSandboxArg`) —

- A full generated ID (`id.IsGenerated`) is trusted outright: a resource prefix
  plus 16 random characters cannot collide with a real command word by
  accident.
- An exact sandbox name matches next, in full and never partially: a name is
  what the listing shows and what people type, and a partial one would compete
  with short-ID matching for the same argument.
- A short ID is matched against that candidate list exactly like an explicit
  DISCOBOX_ID argument elsewhere. Zero matches is not an error — it just means
  `args[0]` was never a sandbox reference — but more than one is reported as
  ambiguous, since its shape said it was meant as an ID.

A match consumes `args[0]` as DISCOBOX_ID and leaves the rest as CMD. No
match — including no arguments at all — means every argument is CMD, and the
sandbox falls back to the same picker `discobox apply` uses when DISCOBOX_ID is
omitted.

`discobox shell` with no command runs the sandbox user's login shell. The CLI
never names that shell: it sets `shell: true` on the exec create request and
the sandbox resolves the run user's shell from its own passwd database,
because the local `$SHELL` describes this machine and says nothing about the
identity the exec runs as. `admin exec create --shell` is the same request in
raw form.

`discobox tools` groups the everyday development tools run *against* a sandbox —
`git`, `ssh`, and `vscode` today. Which sandbox is the one thing every tool has
in common, so `--discobox-id` is a persistent flag on `tools` itself and every
subcommand inherits it. Everything else belongs to the subcommand that means it,
including where the tool runs: `git` runs inside the sandbox and takes
`--source`/`-s`, while `ssh` and `vscode` run on this machine and connect to the
sandbox. Each then drives the same exec create/attach/status sequence as
`discobox shell`. Flag parsing stops at the first positional argument
(`SetInterspersed(false)`), so everything from there on reaches the tool verbatim.

The default path sends no workdir and fetches no sandbox record: an exec with no
workdir already lands in the sandbox-agent's default exec directory, which is the
primary source's. Only `git --source` has to `GetSandbox` to turn a slug into a
directory. With a full `--discobox-id` the whole command is create + attach +
start + status, so a one-shot `discobox t git status` costs no round trip it does
not need.

`discobox tools ssh` needs no SSH port on the server. The session is carried over
the endpoint the CLI already uses: a loopback TCP port opened for the life of
the command splices each connection to a `GET /ssh/connect` websocket, whose
byte stream the server hands to the same sshd its TCP listener feeds.
`endpoint.StartLoopbackProxy` cannot serve this — it is an HTTP reverse proxy,
and these are not HTTP bytes.

Everything the session needs is passed on the command line, so nothing is
written down: address and port from the bridge, `-l` from the sandbox ID, `-i`
from the managed key, and `UserKnownHostsFile` from a temp file holding the
host key this run fetched. `sshBridgeSession` opens those three together —
key, host key, bridge — because a bridge with no pinned host key is a
connection nothing can verify; `discobox cp` uses the same one, differing only
in how the client spells the port and names the sandbox. `-F none` keeps the
user's own `ssh_config` out of it, since a `Host *` block there could otherwise
override the identity or user just resolved. The enrolled key is the single
persisted thing, and `resolveSSHIdentity` reuses an already-enrolled one rather
than adding another.

Flag parsing is **off** for `tools ssh` (`DisableFlagParsing`), not merely
non-interspersed: `discobox tools ssh -L 8080:localhost:3000` puts ssh's own flags
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

`discobox cp` is `scp`, pointed at the same bridge (`internal/cli/cp.go`). It is
a root command rather than a `tools` subcommand because it is an everyday verb
with no `--discobox-id` to inherit: which sandbox is named inside each path.

Nothing in the CLI copies bytes. The sshd already answers the `sftp` subsystem
by running the sandbox's `sftp-server` as an exec
(`server/internal/sshd/DESIGN.md`), which is what a modern `scp` speaks, so
recursion, permissions and directory creation are scp's and the sandbox's
business. A `cp` built on the exec primitive instead — a `tar` pipe, the way
`docker cp` and `kubectl cp` work — would have reimplemented all of that against
a sandbox image that may or may not have `tar`, to reach the same place.

What the command owns is the two things scp cannot work out: the bridge's port,
key and pinned host key, and what a `DISCOBOX:PATH` operand means. The reference
before the colon is resolved by `shell`'s rule (`matchSandboxArg`), not
`--discobox-id`'s: it is typed alongside a path, so a name has to work there.
`selectSandbox` is deliberately not used for it — given a non-empty argument it
resolves IDs only and hands a name straight back, which here would become an SSH
username no sandbox answers to.

`splitSCPArgs` finds the operands — scp's option table is needed for that, so
`-o ProxyJump=x` is not read as a path — and `resolveCPOperands` rewrites each
one to `<sandbox id>@127.0.0.1:<path>`. The sandbox travels in the operand
rather than in a `-l`, so one command can name two different discoboxes; when it
does, `-3` is added. Current OpenSSH already routes an sftp-mode copy through
the local host and `-3` changes nothing there — it is pinned because the direct
path is one `-R`, one older client, or one `ssh_config` default away, and it
cannot work here: the source would dial `127.0.0.1:22` inside its own sandbox.

`splitCPPath` is scp's own `colon()` rule with one deliberate difference: a
leading colon is a discobox reference, not part of a filename. scp has no use
for the form and `:PATH` is the natural spelling for "the discobox I am already
working in". The rule has to match scp's in every other respect, because scp
applies it again to what this command emits — which is why a local relative
operand containing a colon (`sub/dir:name`) is emitted as `./sub/dir:name`, and
why a Windows drive letter is a path only on Windows.

Relative remote paths land in the sandbox user's home, not in a source working
tree: every SSH session channel asks for `workdir: "~"`
(`server/internal/sshd/DESIGN.md`), which is what makes `discobox cp x mybox:`
mean what it means everywhere else.

Flag parsing is off here for the reason it is off for `tools ssh`, and it costs
the same thing: with `DisableFlagParsing` cobra parses *no* flags for the
invocation, the root's persistent ones included, wherever they appear. So
`discobox --server X cp …` does not reach a different server — `DISCOBOX_SERVER`
and `DISCOBOX_PROJECT` are how a copy is pointed elsewhere, which is what the
help says. It is not only a parsing accident: `-p` and `-o` are both a global
shorthand and an scp option, and after `cp` they have to be scp's.

`discobox tools vscode` opens a sandbox in VS Code over Remote-SSH. Remote-SSH
drives the system `ssh` binary and reads `ssh_config`, so the only way to hand
it a host is to put the host where ssh finds it: the command refreshes the
project's managed config (`buildManagedSSHConfig` + `writeManagedSSHConfig`, the
same files `admin ssh-config --write` owns) and then runs `code --new-window
--folder-uri vscode-remote://ssh-remote+<alias>/<workdir>`.

A folder URI rather than `--remote` and a path, on every platform (ADR 0074 §4):
a bare path argument is the one thing VS Code's launcher rewrites. Started from
WSL it is normally the Windows build, whose CLI reads a path argument as a path
in *this* distribution, adds a `--remote wsl+<distro>` of its own, and opens the
local directory instead of the discobox. A URI carries its own authority and is
passed through untouched. `--remote` survives for the one case a folder URI
cannot express: a sandbox that never reported where its source landed, which
opens on the host with no folder.

Nothing is held open afterwards, which is the point of ADR 0057: the written
stanzas reach the server through a `ProxyCommand`, so the editor reconnects on
its own, tomorrow as much as now, with no port on the server to depend on. The alias is the sandbox's first surviving
`Host` pattern — its name where the name is unambiguous, its ID where it is not
— which is why `buildManagedSSHConfig` returns the aliases rather than letting a
caller guess them.

The window opens on the primary source's working directory, or the one
`--source` names. `tools git` can leave the directory unsaid because an exec
with no workdir lands in the sandbox's default; an SSH session cannot, because
it lands in the run user's home (`server/internal/sshd/DESIGN.md`). A sandbox
that never reported where its source landed still opens, on the host with no
folder.

The editor binary is resolved *before* anything is written: it is the one
failure nothing can fix after the fact. `--editor`, then `$DISCOBOX_VSCODE`,
then the first of `code`, `code-insiders`, `codium`, `cursor`, `windsurf` on
PATH — all the same CLI, so the only question is which is installed.

Which editor it turns out to be then decides *which ssh* the config is written
for (`sshTargetForEditor`). A Windows build launched from WSL connects with
Windows OpenSSH, on the other side of the boundary from this process, and gets
the Windows target described under [SSH Keys and Config](#ssh-keys-and-config-adr-0024).
This is the only command that chooses a target, because it is the only one that
knows which program will be driving ssh; `tools ssh` carries its own connection
and never touches `ssh_config`.

It runs with `DONT_PROMPT_WSL_INSTALL=1` (`vscodeQuietWSLPrompt`), always rather
than only under WSL: the variable means nothing elsewhere, and a conditional is
a second thing to get wrong. VS Code's launcher asks "install VS Code in Windows
instead… Continue anyway? [y/N]" when it finds itself inside WSL. That warning
is aimed at someone typing `code`; here the binary has already been chosen and,
when it is the Windows one, the config that side needs has already been written.
Unset, the prompt reads from a stdin nobody is typing at and the command hangs
or takes the default No.

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
server (`services.SandboxDisplayName`) and read verbatim here — `discobox ls`, the
launcher, and any other client name a sandbox the same way because none of them
derives it. Display only — name *resolution* (`matchSandboxArg`, `--name`
updates) still works on the configured name.

The launcher's row therefore carries both: `Sandbox.Name` is the display name
and `Sandbox.ConfigName` the configured one (`toTUISandbox`, trimmed the way
the server trims it before falling back to the id). Its status line says the
configured name under the cursor, since that is the handle every other command
takes, and rename is refused on a row where the two differ (`nameIsTitle`) —
the name on screen is the terminal's, and a rename would change nothing there.

## Choosing a Sandbox Interactively

Commands that act on "the sandbox I am working in" take a sandbox identifier —
as `--discobox-id` (`admin exec`, `admin terminal`), an optional positional
`DISCOBOX_ID` (`apply`, `attach`, `admin get`), or a leading
positional argument shared with the command itself (`shell`, resolved by
`resolveShellTarget` rather than `selectSandbox`) — and fall back to
`selectSandbox` (`internal/cli/picker.go`) when it's omitted, never to a guess:

- Candidates are exactly what `discobox ls` shows — `listProjectSandboxes` filtered
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
  `<state>/recent-selections.json` (`internal/cli/recent.go`),
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
- Nothing polls for readiness before attaching, on either path. `discobox run`
  creates, delivers the source if it must be pushed, and attaches — in the
  window or as a stream; the attach itself waits, at
  each tier for what only that tier can see — the control plane for the sandbox
  to be dispatched to a live pool and to be usable rather than mid-delivery, the
  pool agent for the container, the sandbox agent for the primary terminal's
  launch and install (ADR 0039). The two loops that used to sit here — poll
  until `displayState: running`, then poll the exec list until a primary exists
  past `installing` — cost a request per second of provisioning for facts the
  server knew the instant they changed, and every client had to reimplement
  them. `--wait` on `admin box create` is a different thing and stays: there
  the wait *is* what was asked for.
- The wait is narrated, and the narration never gates it. `attachSandboxTerminal`
  starts `watchProvisioning` before the dial and takes it down the moment the
  dial returns — which is exactly when there is nothing left to wait for, since
  the sandbox agent accepts the websocket only once the primary terminal is
  launched and installed. It reads the discobox's recorded phase rather than
  deciding anything from it, and an unreadable discobox says nothing rather than
  saying something wrong. See "Saying What a Wait Is For".
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
  `discobox attach --raw` (`internal/cli/attach.go`) is deliberately a thin wrapper
  over `attachSandboxTerminal` with the virtual primary id and nothing else — sandbox
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
- The other way out is repeated Ctrl-C, and it is the session's, not this
  module's: `execstream/client` owns the escape because the evidence it needs
  is the stream's own (`execstream.Delivery` acknowledgements), and because a
  stalled attach must be escapable from the signal path too, where no input
  copier runs. Interrupts count only while the remote demonstrably has not
  applied them and only at human cadence, so hammering Ctrl-C at a healthy
  remote still reaches the remote; the second unanswered one prints
  `interruptNotice` and the third ends the attach with `client.ErrInterrupted`,
  which every attach call site turns into a silent exit 130 (`interruptedExit`),
  the status a shell reports for an interrupted command. The message is this
  module's because it names the thing that went quiet and ends its lines the way
  the attach's terminal mode requires. Contrast the detach chord: that one is a
  configurable key with no protocol meaning, so the session never learns it.
- Resumable actions (input, signals, and close-input) carry monotonically
  increasing positions. The client retains a bounded window until the shim
  acknowledges applying them; reconnect resends the unacknowledged suffix and
  the shim deduplicates it by logical-session token. A full window backpressures
  stdin instead of silently dropping accepted input. Resize is idempotent state,
  so only its latest value is retained and restored.
- A plain exec attach (`attachSandboxExec`, `internal/cli/sandbox_execs.go`) —
  `discobox shell`, `admin exec create`, `discobox tools git`, everything that is not a
  harness terminal — follows the same PTY/no-PTY split as replay itself:
  `openExecAttachConn` picks the reconnecting transport, replay included, when
  `tty` is true, and the direct one otherwise. A TTY exec's screen can be
  repainted on reconnect exactly like a terminal's; a piped exec's output has
  no such buffer — resuming it would need byte-exact output positions, which
  the shim does not provide — so it stays direct and fails on disconnect.
  `SignalReady` and `OtherErr` are set to match: they only make sense once
  replay is in play, and only exist on the TTY branch.
- Connection lifecycle notifications are transport events, not terminal output.
  CLI attach ignores them; the launcher, which draws the session in a pane,
  renders them as that pane's status (`framedTerminal.Events` →
  `tui.TerminalEvent`), because a reconnect never appears in the output — the
  stream simply carries on — so it is reported there or not at all.
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

`discobox proxy` holds a local port open for every port a sandbox is listening on.
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
  `Input`/`Stdout`/`CloseInput`/`CloseOutput` frames as a `net.Conn`, so
  everything above it is a plain TCP proxy and a half-close survives the trip
  in both directions (ADR 0024 §4): `CloseWrite` sends `CloseInput`, and an
  incoming `CloseOutput` is this conn's `io.EOF` — which says nothing about
  whether it can still be written to.
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
- The listing is what the sandbox says it serves, not only what it was seen
  listening on: a service may declare a port the sandbox cannot discover — one
  a nested container published or a socket-activated unit bound, root's socket
  either way ([ADR 0076](../docs/adr/0076-a-service-may-declare-a-port-discovery-cannot-see.md))
  — and it arrives in the same array, with no addresses and a `declared` mark.
  Nothing here treats it specially: a port with no reported address is dialed at
  the default host, which is the answer for a published or activated one.
- The listing is polled rather than streamed. There is no project event stream
  to subscribe to — the one that existed promised a resumable list-then-watch it
  could not deliver and was removed (ADR 0061) — and the ports themselves reach
  the control plane on the sandbox-agent's own cadence (ADR 0046), which is what
  bounds freshness either way.

## Resource Views

`discobox admin pool resources [POOL_ID]` and `discobox admin box resources
DISCOBOX_ID` read the accounting the pool agent reports (ADR 0071). Both are
plain reads of `pool.resources` and `discobox.runtime.resources` — the CLI
computes no rates of its own, because the figures are only comparable when one
agent differenced them over one tick.

The pool view exists for its **totals**, not its rows. Per-discobox CPU is
additive and comparable within a pool, so the table sums it and subtracts it
from the pool's own load: what is left is overhead, and naming it (the pool
agent, BuildKit, the registry, the proxy) is what turns "the pool is busy and
every discobox is idle" from a mystery into a build in flight.

Three display rules follow from the contract rather than from taste:

- **Absent is not zero.** A discobox with no rate prints `-`, never `0.00`.
  "Not measured yet" and "idle" are different claims, and the first report after
  an agent restart has counters but no rate.
- **Memory is two columns.** CHARGED is the cgroup's; RESIDENT is the processes'
  summed, which double-counts shared pages. Neither substitutes for the other,
  so neither is dropped.
- **Cache appears once, at the pool.** It is one shared tree, so a per-discobox
  cache column would sum to N times the truth.

Processes are ranked by rate, which is the pool agent's ranking, not a
re-sort here. `--processes` folds the per-discobox process tables into the pool
view for the case where the ranking alone does not name the culprit.

Generated API types are held **by pointer** when they go into a `map[string]any`
for `--output json`. Their optional fields encode through their own
`MarshalJSON`, and a value inside an `any` is not addressable, so `encoding/json`
would reflect over the optionals instead and fail on every unset one.

## Pool Host Console

`discobox admin pool console [POOL_ID]` attaches this terminal to a root shell on
the machine hosting a pool's runtime — a WSL guest, a libkrun microVM, a
droplet, the local Docker host — for debugging that backend
([ADR 0051](../docs/adr/0051-the-pool-console-attaches-through-the-driver.md)).

It reuses the attach machinery rather than inventing a second one: the same
`execstream/client.Session` with raw mode, resize tracking, and the leader-key
detach chord that `discobox attach` uses, over a websocket to the control plane's
console route. What differs is that there is no exec to create or start first
and no reconnecting transport — the console container is persistent, so a
dropped connection is reopened by running the command again, and there is no
session state on the client worth resuming.

The initial terminal size travels as query parameters on the open, because the
console's shell is started by the server before the session's own resize
tracking has said anything, and a first prompt drawn at 80x24 would then be
repainted.

## Pool Host Logs

`discobox admin pool logs [POOL_ID]` prints what that pool's backend recorded
about its host — a guest's serial console, a Docker daemon's journal, whatever a
scripted backend prints — with `--tail` and `--follow`. It is the console's
companion for a host with no shell to attach to: the console needs the host's
Docker daemon, and this is what says why there isn't one.

It shares none of the attach machinery, because there is nothing to attach to:
it is a plain streaming `GET` copied to stdout. Two details make it usable as a
tool rather than only as a screenful:

- The log goes to stdout and everything the CLI has to say goes to stderr, so
  `discobox admin pool logs > console.txt` captures the host's log and nothing
  else. What the backend actually opened is named on that stderr line, since
  there is no uniform pool host log
  (`server/providers/DESIGN.md#pool-host-logs`) and the bytes do not say which
  record they are.
- `--tail` defaults to 200 rather than to everything. A guest console log spans
  every boot the pool has ever had, and the operator running this almost always
  means the most recent one; `--tail 0` asks for the whole thing.

## SSH Keys and Config (ADR 0024)

`discobox admin ssh-key` and `discobox admin ssh-config` are the CLI-side counterpart
to the SSH control-plane ingress (`server/internal/sshd/DESIGN.md`); they
are advanced/low-level configuration in the same sense as `admin provider` or
`admin pool`, so they live under `discobox admin` rather than at the root command
level or layering on the attach transports above.

- `discobox admin ssh-key add` with an explicit file (or `-` for stdin) argument
  enrolls that key directly. With no argument it lists public keys from a
  running `SSH_AUTH_SOCK` agent (falling back to `~/.ssh/*.pub`) and reuses
  the shared picker (`internal/cli/picker.go`) for the "which key" choice
  when there is more than one candidate — the same picker `discobox shell`'s
  sandbox selection and `run --include-dirty`'s prompt use. This step is
  enrollment convenience only: listing an agent's public keys proves nothing
  about possession of the private half, and the actual authorization is the
  authenticated `CreateSSHKey` API call that follows, never agent presence
  itself (ADR 0024 §6).
- `discobox admin ssh-config` emits one `ssh_config(5)` `Host` stanza per sandbox in
  the current project plus a `known_hosts` line. The stanzas name **no address**
  (ADR 0057): they carry a `ProxyCommand` that runs this executable as `discobox
  --server <endpoint> admin ssh-proxy`, which splices its own stdio onto `GET
  /ssh/connect` — the same door `discobox tools ssh`'s bridge dials, with `ssh`
  owning the process instead of a loopback port. There is no other way in: the
  server binds no SSH port. Everything built on `ssh` rather than on our client
  — Remote-SSH, `scp`, `git` — reaches a sandbox wherever this CLI reaches the
  API, and nothing has to be configured, published, or firewalled for it.
- The `ProxyCommand` records the absolute path from `os.Executable()` and the
  `--server` value, both shell-quoted: ssh runs it through a shell, with
  whatever environment its caller had — and that caller is often a GUI editor
  whose PATH and environment are not the shell's. The quoting is `/bin/sh`'s
  everywhere but Windows, where OpenSSH hands the line to `%COMSPEC%` — and it
  is the *target's* shell that decides, not `runtime.GOOS`, because a config
  written from WSL for Windows is read by a shell this process does not have.
- `sshTarget` (`internal/cli/ssh_target.go`) is the OpenSSH installation an
  emitted config is written for: the state directory holding its files, the
  `ssh_config` that gains the `Include`, how a path inside it is spelled, and
  the `ProxyCommand` that reaches this CLI from it. Every managed path carries
  both spellings — the one this process opens and the one the reading ssh uses
  — because on WSL they are different files to look at and the same file in
  fact. `admin ssh-config` always writes for this machine's own ssh.
- The host key is verified under `HostKeyAlias`, one name per project
  (`<project id>.discobox.internal`), which is also the `known_hosts` host
  field. A stanza with no address gives ssh nothing to derive a name from. The
  project ID is resolved for printing as well as writing, since the alias is
  derived from it. (`knownHostsHost`'s bracketed `[host]:port` form survives for
  `tools ssh`'s temporary known_hosts, which really is per port.)
- `GET /ssh` carries the host key and nothing else — `sshHostKey` is the one
  reader — because there is no address to discover and nothing to enable.
- It also generates and enrolls the key it points at, so the emitted config
  works on its own: an ed25519 key under the CLI state directory
  (`<state>/ssh/id_ed25519`, private to this user), enrolled in the project when the project
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
  `~/.ssh/config`. It also removes the `Include` lines it wrote to a state
  directory it no longer uses (`dropStaleManagedIncludes`, recognizing the
  `.../discobox/cli/ssh/<project>/config` shape it writes and nothing else
  does). ssh fails outright on an `Include` it will not read, so a line left
  pointing at a moved — or, on Windows, wrongly permissioned — file breaks every
  connection through the config, the ones the new line was written to enable
  included. Other projects' lines under the current state directory stay: one
  per project is the design, and the user's own `Include`s are never touched. Both files are scoped to the project — `<state>/ssh/<project
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
- A project with no sandboxes writes an *empty* config rather than skipping the
  write: these files mirror the server, so a project whose last sandbox is gone
  must stop offering stanzas for it. The `Include` and the `known_hosts` line
  stay — the host key belongs to the server, not to any sandbox — so the next
  populated run needs no further edit to `~/.ssh/config` and no second trip to
  re-pin the same server.
- A successful prompt sandbox create refreshes these files after its source has
  been delivered, from both `discobox run` and the launcher. This is the same
  operation as `admin ssh-config --write`, including key enrollment and WSL's
  two targets, so a newly created sandbox is immediately available to OpenSSH
  clients without a separate command.
- **WSL** is the one place where the ssh that connects is not on the same side
  of the machine as the CLI (ADR 0074). A Windows VS Code runs Windows
  `ssh.exe`, which reads `%USERPROFILE%\.ssh\config`, opens files by their
  drive path, and cannot execute a Linux binary. Such a machine has *two* ssh
  installations, so `machineSSHTargets` answers with both and every command that
  writes writes both — `admin ssh-config --write` as much as `tools vscode`,
  since which side a tool drives is not something either command can know
  (ADR 0078 §3). The stanzas are built once and rendered per target: what
  differs is how a path is spelled and what the `ProxyCommand` runs, not which
  sandboxes exist or which key authenticates. Printed output stays this side
  only — it is for pasting in by hand. The Windows side's own files are stanzas
  under
  `%LOCALAPPDATA%\discobox\cli\ssh\<project>\`, an `Include` in the Windows
  user's `~/.ssh/config`, a `ProxyCommand` of `wsl.exe -d <distro> -e sh -c
  "exec '<linux discobox>' --server '…' admin ssh-proxy"`, and a copy of the
  enrolled private key beside them, refreshed on every run. Resolving that side
  needs interop, so failing to is a warning — this side's config is still
  written and still correct — except for `tools vscode` launching a Windows
  editor, where it is the whole point and therefore an error. Windows is asked where its
  own folders are (`cmd.exe /c echo %LOCALAPPDATA%`, `wslpath -u`) rather than
  assumed — and `cmd.exe` itself is taken from PATH, or, in a distribution
  configured not to inherit the Windows one, translated from the Windows path it
  is always installed at. Whether the editor is a Windows program comes from `wslpath -w`
  rather than a `/mnt` prefix — the mount root is configurable, and a path
  inside the distribution answers as a `\\wsl.localhost` UNC share. What the
  boundary costs is in `internal/cli/wsl.go`; each write names the file it
  wrote, so which side is which is visible without knowing the layout.
- Two things about that boundary are counter-intuitive enough that the first
  implementation got both wrong, and neither failure names itself. Both are
  ADR 0078. **Quoting**: `wsl.exe` does not strip the double
  quotes `cmd.exe` leaves in place, so a word it reads itself carries none, and
  the command goes to `sh -c` as one double-quoted argument with POSIX quoting
  inside. Quoting each word the Windows way got the Linux side an `execvp` of a
  program named `"…"`, and ssh a UTF-16 error message where the banner belonged
  — which it reports as "banner line contains invalid characters".
  **The key's ACL**: set with `icacls` and read back, never inherited. A file
  written from WSL onto a drive mount carries an explicit `S-1-5-32` ACE, one
  created from Windows inherits whatever the profile grants below it, and
  `os.WriteFile` over an existing file keeps the old DACL. ssh refuses a private
  key that any of those leave reachable by another principal, so the run fails
  here rather than leaving Remote-SSH to complain about a file the user never
  created.
- Values that can contain a space are quoted: `IdentityFile`,
  `UserKnownHostsFile`, and the `Include` line (`sshConfigQuote`, and
  `sshConfigFields` reading them back so a re-run recognizes its own line). A
  Windows profile under a name with a space in it is ordinary, and unquoted ssh
  reads the first word as the whole filename.
- Only the written form carries `UserKnownHostsFile`, pointing at the
  known_hosts it just wrote. Pinning the host key there keeps
  `StrictHostKeyChecking` meaningful without editing the file that records the
  user's trust in every other host they use; printed output cannot name a file
  the run never wrote, so it keeps emitting the line as a comment instead.

## Signals and Job Control

Keystrokes reach the remote job, never this process. Two mechanisms, chosen by
whether the attach has a PTY — not by which command is running:

- **Raw mode (any TTY attach: `run`, `admin terminal attach`, `configure`,
  `shell`/`admin exec create` with a PTY).** `MakeRaw` turns off ISIG, so Ctrl-C,
  Ctrl-Z, and Ctrl-\ are never signals here — they travel as the bytes 0x03,
  0x1a, 0x1c and the *remote* line discipline signals the remote foreground job.
  Nothing to forward, and the local CLI is never the target.
- **No PTY (`discobox shell` into a pipe or redirect).** The local terminal is still
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
default), or that directory itself outside a repository. `discobox ls` filters on
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
source's commit to its branch in that source's *origin* repository (plus the
workspace snapshot ref, when that workspace is dirty — without it the sandbox
comes up clean and the edits are lost), and then reports the pushes complete. It
pushes the commits the server recorded at create, by explicit refspec, rather
than whatever the local branches now point at, and records each under
`refs/discobox/origin/<sandbox>/<slug>/<branch>` as the lease a later
`discobox push` holds.

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
`internal/sandboxgit` owns that addressing for every direction it runs in — the
two repository URLs, the bearer token as a config override rather than in the
URL, and the client-side ref names.

`CreatePromptSandbox` and `DeliverSource` both take a `sandboxcreate.Report` and
call it as they enter each step. These steps are this process's own work, so
nothing else can say which one is underway; the words live in `sandboxcreate`
so `discobox run` and the launcher cannot describe the same stage differently,
while where the line is drawn and when it is cleared stays each frontend's.

See [ADR 0001](../docs/adr/0001-sandbox-origin-and-remote-source-push.md).

## Re-delivering Source (`discobox push`)

`discobox push` (`internal/cli/push.go`, `internal/sandboxpush`) sends local commits
into the origin repository a push-delivered source's sandbox fetches from, so
work done here since create can be rebased onto there. It acts per source —
every push-delivered source of the sandbox, or the one `--source` names — since
delivery is decided per source and a reference can need a push while the primary
source is bound. Nothing in the sandbox moves: it gains `origin/<branch>`, and
whoever is working in it rebases when they choose. It is transport only — no
phase, no completion call, no state change.

The ref update is a lease rather than a force or a plain fast-forward. A local
rebase or amend is the normal way a branch moves, so non-fast-forward has to be
possible; a stale second machine silently rewinding a sandbox's origin does not.
So the push is `--force-with-lease` against the commit this client last pushed;
with no such record git's own fast-forward rule stands and only `--force` can
rewind. Before pushing, a history sharing no merge base with the commit the
sandbox was created from is refused — nothing there could rebase onto it —
deliberately *not* requiring that commit to be an ancestor, which a local rebase
rewrites away routinely. Uncommitted changes are reported, never pushed.

`--branch` pushes another local branch under its own name, which the sandbox sees
as `origin/<that branch>`, leaving the branch it tracks alone.

**A discobox still waiting for its source is delivered instead.** A create can
fail after the discobox is already provisioned — the push itself failed, or the
pool was wedged while it ran — and what is left is a discobox that is correct in
every way except that nobody handed it its source. It sits in `awaiting_source`
forever: the resume is `CompleteSandboxSourcePush`, which only the create path
called, so a rebase-time push would send commits into its origin and leave it
exactly as parked as it found it.

So when the discobox is in `awaiting_source`, `discobox push` performs the
create's own delivery instead of the rebase-time one: each source is pushed at
the commit it was pinned to at create — not whatever the local branch points at
now — along with the workspace snapshot the create captured, and the whole set
is reported complete, which starts the discobox. `sandboxcreate.DeliverSource`
does the work in both directions; what a later delivery has to rebuild is the
`LocalSources` a create carried in memory, which `NewLocalSources` files from
each source's recorded `localDirectory` (or a `--dir` override, as `apply` and a
rebase-time push resolve it).

Every source is resolved and checked before any of them is pushed
(`CheckDeliverable`), because the discobox resumes on one report covering all of
them: a delivery that cannot finish must not leave half of itself in an origin.
Three things make one impossible, and each is refused by name — the commit is
gone from the local repository, the workspace snapshot ref is gone, or the
source was created from a directory with no repository of its own, whose
throwaway repository was deleted when that run ended (ADR 0045) and took the
only copy of those commits with it. `--source`, `--branch`, and `--force` all
describe a rebase-time push and are refused here rather than ignored.

See [ADR 0058](../docs/adr/0058-a-push-delivered-source-has-a-pool-side-origin.md).

## Saying What a Wait Is For

Creating a discobox and attaching to it can take minutes behind a cold image
pull, and neither the create nor the attach says anything while it does. Two
mechanisms fill that in, and they cover disjoint halves of the wait
([ADR 0060](../docs/adr/0060-provisioning-progress-is-a-recorded-phase-the-client-polls.md)).

**Client-side steps** are reported by `sandboxcreate` as it takes them, above.

**Provisioning** is the pool agent's work, recorded on the discobox as
`runtime.provisionProgress`. It is read by polling, deliberately: each read is
the current truth, so a missed update is not a lost one and there is no gap to
recover from. This is not the readiness poll ADR 0039 removed — that one gated
the attach and cost a round trip per second for an answer the server
volunteered; this one runs beside a wait that is already correct, ends when that
wait does, and its worst failure is a late line.

`sandboxcreate.ProvisionStatus` turns one discobox into one line, most specific
answer first: a settled failure or a parked source push is an answer, a recorded
phase beats any inference from state, and a phase that has not been restated
within `ProvisionProgressFresh` is describing the past rather than the present —
the record is the last report and is never cleared. A discobox with nothing left
to provision reports **nothing**, so the caller keeps its own line: what remains
then is the harness install and the terminal launch, which no channel reports
upward. A phase this build does not know is spelled out rather than dropped,
because a CLI is routinely older than the control plane it talks to.

Two waits narrate from it, and the difference between them is only where the
reads come from.

- **The attach** has `watchProvisioning`, a loop that exists to read. Its first
  read waits one interval, so an attach onto a running discobox connects inside
  it and asks the server nothing at all: narration costs a request only when
  there is a wait worth narrating.
- **The wait for a discobox to park ready for its source** narrates out of the
  reads it is already making. `awaitSourceRequested` polls the discobox anyway,
  and everything it is waiting for — the image pull above all, since the
  container is created before the discobox parks — is on the record it already
  has. Its phases replace `StepAwaitingSource` on the line as they are recorded,
  which is why the longest step in a create is no longer the quietest one.

`statusLine` is where it lands for the commands: one rewritable line on a
terminal, appended lines off one, and cleared before the stream is handed to
anything else. The launcher renders the same reports on its busy line instead;
see the launcher's design doc.

**While it is up it owns the row it is on, so everything else the command
writes to that stream goes through it.** A line that stays goes through
`print`, which erases the row, writes the line where the scrollback keeps it,
and draws the status line again underneath; written past it instead, a report
comes out with the spinner glued to its front —
`⠧ preparing sourcesource x: /home/ada/src/x`. Anything that draws its own
screen on the stream — the create's two confirmation questions — takes the row
with `suspend`, whose returned func gives it back. Suspending is not clearing:
the wait carries on underneath, reports keep landing, and the last of them is
what the line says when it comes back.

## Uncommitted Work at Create

A dirty local workspace becomes a snapshot commit on top of the checked-out
commit, kept under `refs/discobox/run/`, and reaches the sandbox as uncommitted
changes on that same commit. `discobox run --include-dirty` decides whether that
happens:

- `auto` (default) asks, and only when the workspace is actually dirty. Where
  it asks depends on who is creating: `discobox run` creates in the window, so
  the question is the window's own dialog (`Model.workspaceChecked`) — one
  question, whose answer is then carried to the create as `true` or `false` for
  every source it cuts from. `--raw`, `-d` and a run with no terminal create on
  this side and ask with the standard picker, per source, with "start from the
  last commit" leading so the default answer carries nothing extra. The picker
  draws on the stream the create is narrating on, so it suspends the status
  line for as long as it is up (see "Saying What a Wait Is For").
- `true` / `false` answer ahead of time; bare `--include-dirty` means `true`.
- Frontends express the question through `sandboxcreate.ConfirmIncludeDirtyFunc`
  rather than prompting themselves. A nil func means there is nobody to ask — no
  terminal — and the work is included: dropping a user's edits silently is worse
  than carrying them. The launcher does not use it: it owns the screen, so it
  asks in its own confirmation dialog and settles `IncludeDirty` to `true` or
  `false` before it calls the shared create at all. It asks about the primary
  source, which is the one it can see a working tree for; the answer stands for
  every source that create cuts from, and an extra `-i` source whose tree is
  dirty when the primary's is clean is carried in under the nil-func default.
- `true` is rejected for a remote URL or an explicit `@REF`, because a snapshot
  only ever sits on top of HEAD of a local working tree.
- The same flag settles the same question for a source directory in no
  repository, where the uncommitted work is the whole directory; that question
  is `sandboxcreate.ConfirmCopyDirectoryFunc`. See "A Directory That Is Not a
  Repository".

## Extra Sources

`discobox run -i DIR` brings more sources into the same sandbox, repeated for more
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
  also the reference's key in the request. On a Windows client it is the path in
  WSL's spelling — `E:\srcoo` is `/mnt/e/src/foo` — which is the same mirror
  the primary source gets; see the sandbox creation design doc. A remote source
  has no host path to keep and goes under `/workspace/<slug>`.
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

`discobox run` and the launcher both bring them in, as source code references
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
`PromptOptions.ReportDeclaredSource`; `discobox run` prints one line per source on
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
- The user is asked first, through `sandboxcreate.ConfirmCopyDirectoryFunc`, and
  not copying leads: `discobox run` in a home directory must not carry the home
  directory. Declining is an answer, not a cancel — it resolves to no source at
  all, the request "No Source At All" below describes, and nothing is built over
  the directory to reach it. `--include-dirty` answers ahead of time, `false`
  meaning the same. What declining does *not* produce is a checkout of nothing at
  the directory's own path: that put an empty, push-delivered repository over
  whatever lives at that path inside the discobox, which is `$HOME` in the case
  the question exists to catch.
- The question is asked before `gitutil.InitOverWorkTree`, because indexing the
  directory is the cost it exists to avoid. It carries a running size —
  `sandboxcreate.MeasureDirectory` walks the directory in the background and the
  frontend polls it, saying "calculating…" until the walk is done. The walk is a
  filesystem estimate: sizes on disk, symlinks counted as links, unreadable
  subtrees skipped, and no `.gitignore` applied. It never reads low, which is
  what the question needs.
- An explicit `@REF` is still rejected: there is no history to name.
- An empty directory is not asked about and not an error: it snapshots nothing
  and the sandbox starts on the empty commit at the directory's own path, which
  is the point of running in one — a project that does not exist yet, with
  somewhere for `discobox push` to carry the work back to. Nothing was declined
  there, so unlike a declined directory it keeps its source.
- With no terminal there is nobody to ask and the directory is copied, the same
  way a dirty workspace is.

See [ADR 0045](../docs/adr/0045-a-directory-with-no-repository-is-delivered-by-push.md),
[ADR 0073](../docs/adr/0073-a-directory-with-no-repository-is-copied-only-when-asked.md)
and [ADR 0077](../docs/adr/0077-declining-a-directory-copy-creates-a-discobox-with-no-source.md).

## A Repository With No Commits

`git init` and nothing since leaves an *unborn* HEAD — it names a branch that
does not exist yet — so there is no commit to check out and nothing in the
repository to clone. `gitunborn.HeadIsUnborn` tests for it before a base commit is
demanded, and the answer is the shape above one step back in: nothing has ever
been committed, so the working tree is uncommitted work on a base of nothing.

The difference from a directory in no repository is that no repository has to be
built. The base — a root commit of the empty tree — the snapshot, and their refs
are written into the repository the user made. `refs/heads/<branch>` is never
touched, so HEAD stays unborn and their first commit stays theirs.

- The source records `noLocalCommits`, and that is what makes the server choose
  `push`: a clone of the repository yields nothing, and the base commit lives
  only in objects this client wrote. It is deliberately not `noLocalRepository`,
  which additionally means the commits are gone with the repository built for
  that one run. These are in the user's repository and stay there, so a later
  `discobox push` delivers this source out of it and `CheckDeliverable` finds
  both objects.
- The base commit gets a `refs/discobox/run/` ref of its own. It is what the
  sandbox checks out and what the snapshot is measured against on both ends, so
  it has to be a ref rather than a loose object — an empty repository's base is
  pointed at by nothing else at all.
- `gitunborn.WorkspaceTree` writes the working tree from an index of its own
  that starts empty, because `gitutil.CurrentWorkspaceTree` seeds its index from
  HEAD. `git add` fills it, honoring `.gitignore` and skipping `.git`, and the
  repository's real index — which may already hold paths staged for that first
  commit — is untouched. Apply reads the same function to check nothing has
  changed since ("Applying Into a Repository With No Commits" below).
- Nobody is asked. The directory question exists because a directory in no
  repository may be a home directory somebody ran in by accident, and `git init`
  is the user saying it is not; the dirty-workspace question has no alternative
  to offer, because there is no last commit. `--include-dirty=false` still
  answers ahead of time, and its answer is the empty base commit at the
  repository's own path — not "no source", because a repository is a project
  whose path the user established.
- An explicit `@REF` is rejected with a message naming the repository and its
  missing history, rather than git's "Needed a single revision".

See [ADR 0083](../docs/adr/0083-a-repository-with-no-commits-is-uncommitted-work-on-an-empty-base.md).

## No Source At All

`discobox run --no-source` (`PromptOptions.NoSource`) creates a discobox with
nothing materialized in it — the shape the harness configure sandbox already
had, reached deliberately. It is not "a source that resolved to nothing": no
`config.source` is sent at all, and the create request carries no local source
for delivery to push.

The flag is one of two ways in. The other is answering "do not copy" to a
directory in no repository (above): resolution itself answers "no source", and
everything below applies to it identically. An `--include` that answers the same
way is left out of the discobox rather than brought in empty.

`-C` still applies and still means what it always did. What it names is the
*origin* — the host and project directory the create came from — and the Git
authorship the discobox commits under, both read from the client's disk. So a
sourceless discobox is filed under the directory you ran in and listed there like
any other; only what would have been checked out is left out. `-i` is unaffected:
`--no-source -i ../foo` is a discobox holding `foo` and nothing else.

A ref is refused rather than dropped: `@REF` names a commit to check out and
there is nothing to check it out of. Declared sources fall away on their own,
since the file that declares them lives in a checkout there is none of.

In the launcher this is the Source row's last entry rather than a flag of its
own; see the launcher design doc.

## Git Transport to the Server

`git` is a subprocess that only understands URLs, so it cannot use the CLI's
local-IPC transport. `App.gitServerURL` bridges the gap: for a `unix://` or
`npipe://` endpoint it starts `endpoint.StartLoopbackProxy`, a loopback HTTP
listener that reverse-proxies onto the same server the API client uses, and
returns that address for the duration of the command. An `http(s)` endpoint is
already addressable and is returned unchanged.

Everything that shells out to git shares it — `sandboxcreate.DeliverSource`
(push at create), `sandboxapply.FetchSource` (fetch at apply) and
`sandboxpush.Push` (re-push) — so the local socket, which is the default
endpoint, is not a server only half the CLI can reach.

## Apply Output

`discobox apply` (`internal/cli/apply.go`) performs git operations on the user's
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
  repository — and is reported as that rather than as a git error. A source
  that never had a shared history to begin with does not look for one; see
  "Applying Into a Repository With No Commits" below.
- `--allow-dirty` applies a source whose sandbox working tree is dirty. The
  status check still runs and its entries are still reported (with
  `dirtyIgnored`): the flag means the user chose to leave that work in the
  sandbox, not that nobody needs to know it is there.
- `--debug` additionally echoes every git command as it runs, via
  `gitutil.WithTracer`, on stderr so it never interleaves into the report.
  `gitutil` redacts credentials in traced arguments centrally, so no new git
  call site can leak a token by forgetting to.

### Applying Into a Repository With No Commits

A discobox created from a repository that had no commits ("A Repository With No
Commits" above) carries `noLocalCommits`, and that is what apply reads to handle
the round trip back. See
[ADR 0084](../docs/adr/0084-the-first-apply-into-a-repository-with-no-commits-is-its-history.md).

- The base is the source's own `checkout.commit` — the empty base the discobox
  started from — reported as `baseOrigin: "discobox-base"`. There is no merge
  base to look for: such a repository shares no history with anything by
  construction. This holds on every apply of that discobox, not only the first,
  which is what lets a user who has since committed something of their own get
  the discobox's commits cherry-picked on top of it.
- While the local repository still has no commits, `gitapply.AttemptRoot` lands
  them instead of `Attempt`. It cherry-picks onto an unborn HEAD of its own — a
  scratch worktree detached at the empty base, then `git checkout --orphan` — so
  the discobox's `discobox run empty base` is replayed away and the first
  sandbox commit becomes the repository's root, authored by whoever wrote it.
  The branch HEAD already names is then created at the applied tip and
  `git reset --hard` fills the index and working tree.
- That reset is guarded: the local working tree must still be exactly what the
  discobox was created from — the workspace snapshot's tree, or the empty tree
  for a discobox created from an empty repository. The snapshot ref is read from
  the local repository and re-fetched from the discobox's origin if it has been
  pruned, so a missing ref never reads as a changed working tree. A tree that
  differs is `blocked`, with the differing paths in `localChanges` and a next
  step that works: commit them, and the same range applies on top. The refusal
  says which of the two reasons it is — a carried working tree that has changed
  since, or files the discobox was never given — because only the first is the
  user having done something, and "put it back the way the discobox found it"
  is a way out of the first and an instruction to delete their files in the
  second.
- `cli/internal/gitunborn` holds the two questions create and apply both ask —
  whether HEAD is unborn, and what the working tree holds when there is no HEAD
  to read it against — so neither can disagree with the other about what
  `.gitignore` means.

## Harness Configure Step

`discobox admin harnesses configure` (`internal/cli/harness.go`) drives a harness's
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

`discobox configure` (aliases `config`, `conf`, `c`, `init`) is the launcher opened
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
