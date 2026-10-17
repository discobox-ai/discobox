# 26-10-17-354 — `discobox admin ahp` serves discoboxes as Agent Host Protocol sessions

- **Status**: Accepted
- **Date**: 2026-10-17

## Context

VS Code's Agent Sessions view is a client of the
[Agent Host Protocol](https://github.com/microsoft/agent-host-protocol) (AHP):
JSON-RPC 2.0 over a WebSocket, with Redux-style channels (`ahp-root://`,
`ahp-session:`, `ahp-chat:`, terminals, changesets, …) that a client subscribes
to for a snapshot plus an ordered action stream. VS Code connects to an
external host listed in `chat.remoteAgentHosts` (`name`, `address`,
`connectionToken`).

AHP models a session as chats made of turns, response parts and tool calls.
Discobox has none of that: a box runs a harness in a terminal, and all Discobox
knows of the agent is the screen and what it reports over OSC 7501
([ADR 26-10-16-424](26-10-16-424-a-terminals-status-is-what-its-program-reports-over-osc-7501.md)).
A chat therefore cannot carry the conversation; it can only carry a screen.

The AHP spec is a draft whose shapes change incompatibly between releases. Its
Go module (`clients/go`) is a client: its `ahptypes` and reducers are usable
by a host, but there is no host side to reuse.

## Decision

### 1. The host runs in the CLI, across every registered server

`discobox admin ahp` runs an AHP host in the CLI process: a WebSocket server
bound to loopback, gated by a connection token, printing the
`chat.remoteAgentHosts` entry that reaches it. It speaks to every registered
server through the CLI's existing server set, as `discobox ls` does, using the
public API only. No server or sandbox-agent code learns AHP.

### 2. A discobox is a session

Each box on each server is one `ahp-session:` whose URI names the server and
the box. The session summary is derived, never stored:

| AHP | Discobox |
|---|---|
| `provider`, `RootState.agents` | the box's harness; the configured harnesses |
| `title` | the box's name |
| `project`, `workingDirectories` | the project; the box's source directory |
| `status` activity bits | the primary terminal's OSC 7501 state: `working` → `InProgress`, `blocked` → `InputNeeded`, `error` → `Error`, `idle`/`done` → `Idle`; no report → `Idle` |
| `activity` | the report's `title`/`msg` |
| `IsArchived` | the box is archived |
| `createSession` | create a box, as `discobox run` does |
| `disposeSession` | **archive** the box |

`disposeSession` archives rather than deletes: VS Code issues it from a
session-list gesture, and a box holds unapplied work.

### 3. A discobox terminal is a chat; its screen is a terminal tool result

Each of the box's harness terminals is one `ahp-chat:`; the primary terminal
is `defaultChat`, and `createChat` starts another harness session in the box.
A chat holds one turn whose single tool call never completes and whose result
is a `ToolResultTerminalContent` naming an `ahp-terminal:` channel bound to
that exec — the stream `discobox attach --raw <box>` attaches to. VS Code
renders it as a terminal in the chat and opens it as an interactive terminal
(`AgentHostPty`), dispatching `terminal/input` and `terminal/resized`.

A message the user sends in the chat is typed into that terminal followed by
Enter; an attached image is carried into the box as a file
([ADR 26-10-16-535](26-10-16-535-a-pasted-image-is-carried-into-the-discobox-as-a-file.md))
and its path typed.

### 4. A discobox shell is an AHP terminal

`RootState.terminals` lists the boxes' shells (`shell: true` execs);
`createTerminal` opens one in the box its claim's session names. Terminal I/O
maps one-to-one onto exec stream frames: `terminal/input` → Input,
`terminal/resized` → Resize, Stdout → `terminal/data`, Exit →
`terminal/exited`. Command detection is not offered. Harness terminals are
reached only through their chat (§3), not listed.

### 5. Observing never wakes

Subscribing to a session, chat or terminal of a stopped or sleeping box does
not start the box or relaunch a stopped exec — unlike `attach --raw`, which
relaunches a stopped primary terminal. The chat shows the terminal's last
screen as an exited terminal. Only sending a message or typing into a terminal
starts what it needs.

### 6. Out of scope for now

Changesets (git state and `apply`), `resource*` filesystem access (`cp`),
`ahp-tcp:` (listening ports), automations, telemetry and authentication beyond
the connection token. Each has a plausible mapping; none is needed to show and
drive boxes from Agent Sessions.

## Alternatives considered

- **An MCP App per chat.** The host advertises a synthetic MCP server
  customization with `mcpApp` and an `mcp://` channel; the chat's tool call
  carries `_meta.ui.resourceUri`, and `resources/read` returns an xterm.js page.
  VS Code supports it. Rejected: the page must reach the byte stream either over
  a loopback WebSocket allowed by its CSP — which breaks once VS Code is not on
  the CLI's machine — or by polling `tools/call`, and Discobox would ship and
  maintain a web terminal that VS Code already has natively.
- **A canvas per chat** (`ChatState.canvases`, an HTTP(S) URL). Rejected: the
  canvas channel is experimental, VS Code renders it only on desktop, and it
  needs the same web terminal.
- **Mapping the harness's conversation onto turns.** Rejected: Discobox does
  not see the conversation, and parsing each harness's screen is the
  harness-specific code ADR 26-10-16-424 removed.
- **The host in the Discobox server.** Rejected for now: the CLI already holds
  every server's credentials and the server set, and keeps a draft protocol out
  of the control plane. Revisit if a client must reach boxes without a CLI
  running.
