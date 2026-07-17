# CLI Design

The CLI module owns the `disco` command implementation and talks to the
control plane through generated root-module API clients plus a few handwritten
transport helpers where OpenAPI does not model the stream.

## Package Map

| Package/path | Ownership |
| --- | --- |
| `cmd/disco` | Binary entrypoint. |
| `internal/cli` | Cobra command tree, output formatting, local server auto-start, TUI API adapter, and stream attach clients. |
| `internal/sandboxcreate` | UI-independent client-side sandbox request preparation and creation, including prompt options, source resolution, workspace snapshots, environment/secrets, and local user identity. |
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
visible `disco box` command: `sandbox`, `terminal`, `exec`, `provider`, `worker`,
`job`, `harnesses`, and `hooks` are not root commands.

## Attach Stream Pattern

Terminal and exec attach use the same framed stream protocol and should share
the transport/session mechanics in `internal/cli/attach_session.go`.

- Keep frame read/write, output frames, resize frames, signal forwarding,
  raw-terminal setup, close-input frames, and attach teardown in the shared
  framed attach session.
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

`runHarnessConfigure` takes streams rather than a `*cobra.Command` so the TUI can
share it, handing it the real terminal it restores via `tea.Exec`.

Re-running reconfigures and clobbers any in-flight attempt. Nothing here parses
the configure output or creates secrets: a client that crashes mid-flow cannot
leave a half-applied harness, and an abandoned sandbox is reaped by the server.
See `server/internal/resources/harnessconfigs/DESIGN.md`.
