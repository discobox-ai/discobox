# 26-10-22-499 — A discobox has a files API that acts as its user

- **Status**: Accepted (supersedes in part [26-10-16-535](26-10-16-535-a-pasted-image-is-carried-into-the-discobox-as-a-file.md) §2's
  exec of `sh -c 'cat > …'` and its directory under `${TMPDIR:-/tmp}`;
  decides what ADR 26-10-17-354 §6 put out of scope)
- **Date**: 2026-10-22

## Context

Nothing outside a discobox reads or writes a file in it through a route made
for files. Every path today is one of three things:

- **An exec of a shell command.** A pasted image is `sh -c 'cat > …'` with the
  bytes on stdin (ADR 26-10-16-535); a tool's files are `printf %s "$2" >`
  with the content in argv, capped at 128 KiB; a host-side tool script is
  written with `mktemp` and `chmod`; harness configure seeds and reads back its
  configuration with `cat` through the one-shot attach, whose comment says
  "There is no files API yet". ADR 0002 introduced that one-shot form as "the
  primitive a files API would otherwise have to invent".
- **sftp over the SSH ingress** — `discobox cp` is the user's `scp`, and the
  sftp subsystem is itself an exec of the image's `sftp-server` (ADR 0024).
- **git** — `discobox apply` and push, which carry commits, not files.

So a file is reached only through whatever the image has (`sh`, `cat`,
`sftp-server`), every byte written lands in the exec's input log, a stat or a
listing is a command whose output has to be parsed, and quoting is every
caller's own problem. There is no way to ask what a directory holds.

Two things now want files directly. The CLI's AHP host (ADR 26-10-17-354)
serves every discobox to VS Code but has no `resource*` commands: VS Code's
folder picker, file links and diffs are empty for its sessions (§6 there put
"`resource*` filesystem access" out of scope). The in-box host
(ADR 26-10-22-853) has them only because it runs in the discobox, and that ADR
deferred this one: "A native filesystem API in the control plane. Wanted on its
own." And the CLI and server's own exec-based writes above would be simpler,
and keep content out of the exec log, with one.

## Decision

### 1. The sandbox agent serves files; the control plane forwards them

The sandbox agent gets a files API, and the server and pool agent forward it on
the path every exec route already takes (CLI → server → pool agent → sandbox
agent, each checking its token), under
`/api/projects/{projectId}/sandboxes/{sandboxId}/files`:

| Operation | Shape |
|---|---|
| stat | type, size, mode, modification time, a version tag, a link's target |
| list | a directory's entries, each with its stat |
| read | the content, streamed; a byte range |
| write | the content, streamed: create, replace, or create only; only if still at a version |
| mkdir | with its parents |
| remove | a file, or a directory with what it holds |
| move, copy | within the discobox; replace only when asked |

Content is a request or response body, never JSON and never argv, so there is
no size cap beyond what the discobox holds. The operations are what VS Code's
`resource*` commands need, so the AHP hosts' file operations
(`ahphost.Boxes`, keyed by discobox) are these, unchanged: over this API from
the CLI, and in-process in a discobox, whose host already is its user.

### 2. Every operation acts as the discobox's user

A files request has no user. Every operation runs as the discobox's default
user, resolved as an exec that names nobody is (`execs.Manager.ResolveUser`),
with that user's permissions — never as the agent's root. On Linux the agent
does the work in a helper process started with that user's credential, as it
starts an exec's shim; where a sandbox has one account (ADR 0145 §5) the agent
is that user and does it itself. A file only root may touch is reached the way
it is today, with an exec that names root.

Paths are absolute, or relative to that user's home, as `discobox cp` and the
SSH ingress treat them; a leading `~` is that home.

### 3. Its own scopes, and reads start a discobox as a log read does

The routes take `files:read` (stat, list, read) and `files:write` (the rest),
granted with — and alongside — `exec:read` and `exec:write`, so a role can let
someone see a discobox's files without running commands in it. A sandbox's own
principal has them only as it has the exec scopes.

In the server's table of what each forwarded route is for
(ADR 26-10-12-711 §1), a read has no last value to answer with: stat, list and
read are `read`, which starts a stopped discobox and repairs nothing, and the
writes are `action`. A caller that must not wake a discobox — the CLI's AHP
host, where observing never wakes (ADR 26-10-17-354) — does not ask one that is
not running.

### 4. What moves onto it

- The CLI's AHP host serves `resource*` for every discobox it lists, through
  its `Boxes` over this API. Watches and changesets stay the in-box host's
  `Workspace`: this API has no watch (Deferred) and no git.
- A pasted image (ADR 26-10-16-535 §2, whose bytes then leave the exec input
  log), a tool's files and a host-side tool script are written through it,
  and harness configure seeds and reads its configuration through it, instead
  of execs of `sh`, `cat` and `printf`. The image and the script go under
  `/tmp`, not `${TMPDIR:-/tmp}`: a write names its path, and `TMPDIR` is the
  user's shell's, which the API does not read.
- `discobox cp` stays `scp` over sftp: recursion, permissions and resumption
  are `scp`'s, and the CLI design rejected re-implementing them over another
  transport (`cli/DESIGN.md`, `cp`).

## Alternatives considered

- **Keep execs of shell commands.** They depend on the image's `sh` and
  `cat`, cap content in argv at 128 KiB, put every byte written in the exec's
  input log, return a listing as text to parse, and leave quoting to each
  caller.
- **sftp for everything.** Structured, and already there for `cp`, but it
  needs `sftp-server` in every image, an SSH session through the ingress for
  each caller, and an sftp client in the server and the CLI's AHP host.
- **The agent does the operations itself, as root, after checking
  permissions.** Every check races the path it checked — a symlink swapped in
  between — and the agent sees what root sees; the sandbox user's own view is
  only had by being that user.
- **Switch the agent's own thread to the user** (`setfsuid`, `setgroups` on a
  locked OS thread). Go's `syscall.Setgroups` applies to every thread of the
  process, the raw per-thread calls bypass it, and a thread that escapes the
  lock with the wrong ids is a privilege bug. A process of the user's own is
  what an exec already is.
- **A user field, as an exec has.** The API would then be a second way to act
  as root, granted by a scope meant for reading the user's files. Root stays
  with exec, whose `exec:write` already means it.

## Deferred

- **Watching.** A stream of changes under a path, for the CLI's AHP host to
  keep VS Code's explorer current (`createResourceWatch`). Revisit when that
  host serves files and a stale explorer is what its users meet.
- **Trees.** Copying a directory in or out as an archive in one request.
  `discobox cp -r` is `scp`'s; revisit if images without `sftp-server` make it
  fail.
- **A stopped discobox's files without starting it**, read from its volume by
  the pool. Revisit if reads that start a discobox prove expensive enough to
  matter.
