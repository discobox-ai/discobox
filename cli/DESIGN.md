# CLI Design

The CLI module owns the `discobox` command implementation and talks to the
control plane through generated root-module API clients plus a few handwritten
transport helpers where OpenAPI does not model the stream.

## Package Map

| Package/path | Ownership |
| --- | --- |
| `cmd/discobox` | Binary entrypoint. |
| `internal/cli` | Cobra command tree, output formatting, generated API client usage, local server auto-start, and stream attach clients. |

## Attach Stream Pattern

Terminal and exec attach use the same framed stream protocol and should share
the transport/session mechanics in `internal/cli/attach_session.go`.

- Keep frame read/write, output frames, resize frames, signal forwarding,
  raw-terminal setup, close-input frames, and attach teardown in the shared
  framed attach session.
- Keep resource-specific behavior in the resource file: terminal detach
  filtering belongs with terminal commands, and exec interactive/non-interactive
  stdin behavior belongs with exec commands.
- Do not fork a second terminal/exec attach loop for a new stream feature. Add
  an option or callback to the shared session when the behavior is protocol
  plumbing; add resource-specific code only when the semantics differ.
