# CLI Design

The CLI module owns the `discobox` command implementation and talks to the
control plane through generated root-module API clients plus a few handwritten
transport helpers where OpenAPI does not model the stream.

## Package Map

| Package/path | Ownership |
| --- | --- |
| `cmd/discobox` | Binary entrypoint. |
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

Low-level inspection and control commands are grouped beneath the hidden
`discobox debug` command: `sandbox`, `terminal`, `exec`, `provider`, `worker`,
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

## Harness Config Definition Configure Step

`discobox debug harnesses enable` (`internal/cli/harness.go`) first registers
the definition's image-backed HarnessConfig, then runs an optional sandbox with
`harnessMode: config`, unless `--no-configure` is passed. It reuses the existing sandbox lifecycle and attach
helpers rather than introducing new ones: `waitForSandbox`/`waitForPrimaryTerminal`
(`run.go`) to launch and locate the primary terminal, `attachSandboxTerminal`
to let the user answer prompts, and `createSandboxExec`/`attachSandboxExec`/
`returnSandboxExecStatus` (`sandbox_execs.go`) to `cat` back
`/run/discobox/harness-configure.json` once the primary terminal exits 0. This
orchestration is entirely client-side. Failures delete both the ephemeral
sandbox and the not-yet-enabled HarnessConfig. Credential values returned by
config mode become encrypted project secrets bound to the harness.
Codex, Claude Code, and OpenCode definitions all enable this flow; the actual
prompting and credential conversion commands are baked into their images.
