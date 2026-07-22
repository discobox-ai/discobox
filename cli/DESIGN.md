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

## Choosing a Sandbox Interactively

Commands that act on "the sandbox I am working in" take `--sandbox-id` and fall
back to `selectSandbox` (`internal/cli/picker.go`), never to a guess:

- Candidates are exactly what `disco ls` shows — `listProjectSandboxes` filtered
  to the current project directory's origin — so the command and the listing can
  never disagree.
- One candidate is used, none and several are errors, and several with a
  terminal on stdin and stderr open the inline Bubble Tea picker instead.
- The picker prompts on stderr so the command's stdout stays a clean stream.
- `pickOne` is resource-independent: callers supply `pickerItem`s and the
  wording for the empty and ambiguous cases.

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
  transport error alone is not the useful failure.
- Do not fork a second terminal/exec attach loop for a new stream feature. Add
  an option or callback to the shared session when the behavior is protocol
  plumbing; add resource-specific code only when the semantics differ.
- Harness-terminal attaches use the shared reconnecting framed transport in
  `internal/cli`. It retries websocket failures with capped exponential
  backoff, restores resize/readiness state so the sandbox shim repaints the
  terminal, and stops retrying once the authoritative exec record is terminal.
- Never queue input while an attach is disconnected. Input, signals, and other
  transient writes are dropped; the latest resize is retained and restored on
  the next connection. This prevents buffered keystrokes from being delivered
  unexpectedly after recovery.
- Connection lifecycle notifications are transport events, not terminal output.
  CLI attach ignores them; the TUI adapter maps them into its `TerminalEvent`
  stream.

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
