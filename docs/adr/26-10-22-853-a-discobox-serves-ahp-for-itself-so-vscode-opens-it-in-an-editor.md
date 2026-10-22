# 26-10-22-853 — A discobox serves AHP for itself, so VS Code opens it in an editor

- **Status**: Accepted
- **Date**: 2026-10-22
- **Extends**: [ADR 26-10-17-354](26-10-17-354-discobox-admin-ahp-serves-discoboxes-as-agent-host-sessions.md),
  which this leaves standing

## Context

`discobox admin ahp` (ADR 26-10-17-354) serves every discobox to VS Code's
Agents window from one WebSocket host in the CLI. Its sessions cannot be opened
in a full editor. VS Code's Open in Editor opens the machine it reached the
agent host on, not anything the host says: for an SSH, WSL, tunnel or dev
container host it opens `ssh-remote+<host>` (or the like) with the session's
folder, and for a WebSocket host it opens an empty window by design
(`openInVSCodeUtils.ts`, "Remote Agent Host filesystem connections belong to
the originating window"). VS Code also rewrites every folder a WebSocket host
advertises into its own `agent-host:` scheme, so advertising a Remote-SSH folder
does not help; that was tried and opened an empty window.

VS Code's SSH remote agent host is the model that does open an editor: VS Code
SSHes to a host, runs a command there that starts an AHP host and prints
`ws://127.0.0.1:PORT?tkn=TOKEN`, and tunnels to it over that SSH connection.
`chat.sshRemoteAgentHostCommand` names the command; without it VS Code installs
its own CLI and runs its own agent host.

discobox already gives every discobox an SSH host in the `ssh_config` it writes,
and VS Code's SSH host picker lists them. A throwaway prototype — a small AHP
host copied into a discobox and named in `chat.sshRemoteAgentHostCommand` —
showed in a real VS Code Insiders Agents window that VS Code connects to it over
discobox's SSH, browses the discobox's folders through the host's
`resource*` commands, opens its terminals, and that Open in Editor opens a full
Remote-SSH window on that discobox.

## Decision

### 1. The sandbox agent serves AHP for its own discobox

`discobox-sandbox-agent ahp` runs an AHP host for the discobox it is in. VS Code
starts it over SSH as the sandbox user; it listens on loopback with a fresh
token, prints the WebSocket URL VS Code waits for, and exits when its SSH
channel closes. It needs no control plane: everything it serves is local.

- **Session.** One: this discobox, its working tree as the working directory —
  a plain local path, which is what Open in Editor opens over Remote-SSH.
- **Chats and terminals.** As ADR 26-10-17-354 §§3–4 models them — harness
  terminals are chats whose running turn shows the live screen, a message is
  typed in, and VS Code's terminals are shells — reached through the sandbox
  agent's local exec API rather than the control plane, so they are the same
  execs the console and `discobox attach` show.
- **Files.** The `resource*` commands and `createResourceWatch` are the local
  filesystem, with the sandbox user's own permissions.
- **Changesets.** The working tree against the discobox's diff base (the
  branch view) and its uncommitted changes, from local `git`.

### 2. The sandbox agent gives the sandbox user a local exec API

The in-box host runs as the sandbox user, who cannot reach the agent's
PASETO-authenticated routes or the shims' sockets. The agent serves the exec
operations the host needs — list, create a shell or a harness session, attach,
type — on a local Unix socket the sandbox user can reach, as the hooks
collector already does for its publishers.

### 3. VS Code reaches it through `chat.sshRemoteAgentHostCommand`

The user sets `chat.sshRemoteAgentHostCommand` to `discobox-sandbox-agent ahp`
and connects to a discobox with VS Code's "Connect to Remote Agent Host via
SSH". `discobox admin ahp` prints the setting beside its own entry. The setting
is one for every SSH agent host and VS Code labels it "For development", which
this accepts for now; see Deferred.

### 4. Both hosts stand

The CLI host stays the place every discobox on every server is listed and
created. The in-box host is one discobox, opened in full: its files, its
changesets, and Open in Editor. Neither replaces the other.

## Alternatives considered

- **A fake SSH server in the CLI** that answers VS Code's agent host command and
  tunnels to the CLI host. It carries the agent host connection, but Open in
  Editor then opens a Remote-SSH window on that fake host: its authority is one
  per agent host entry, decided before VS Code knows which session's folder it
  opens, so it cannot be routed to the right discobox.
- **Advertising Remote-SSH folders from the CLI host.** VS Code wraps them into
  `agent-host:` and opens an empty window (tried).
- **Running the CLI's host inside the discobox**, scoped to it. It reaches its
  own discobox only through the control plane, under a standing credential
  grant (`discobox-access`), for operations that are local.
- **VS Code's own agent host in the discobox** (no command override). It works
  and opens an editor, but runs VS Code's agents beside the discobox's harness,
  not the harness itself.

## Deferred

- **Dropping the development setting.** VS Code's ordinary SSH path installs
  its CLI and discovers agent hosts already running on the machine through its
  endpoint registry (`agent endpoints`). If the sandbox agent can register its
  host there, VS Code would find it without `chat.sshRemoteAgentHostCommand`.
  Revisit once that registry is confirmed stable enough to write to.
- **A native filesystem API in the control plane.** Wanted on its own; this
  ADR does not need it, since the in-box host reads files locally. A separate
  ADR.
