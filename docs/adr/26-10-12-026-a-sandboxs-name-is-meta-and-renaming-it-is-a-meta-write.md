# 26-10-12-026 — A sandbox's name is meta, and renaming it is a meta write

- **Status**: Accepted (§3 superseded in part by [26-10-31-618](26-10-31-618-a-sandboxs-name-is-followed-by-what-its-program-reports.md): a set name leads the display name, followed by the program's msg)
- **Date**: 2026-10-12
- **Supersedes**, in part:
  [0136](0136-a-sandboxs-meta-lives-in-the-sandbox-and-the-server-caches-it.md)
  §1's "exactly two fields" (a third, `name`, joins them);
  [26-10-08-447](26-10-08-447-a-discobox-tags-the-discoboxes-it-creates.md)
  §1's "no name", which was about `config.name` and stays true of it — the
  route it opened now carries the meta name too (§5).

## Context

A listing calls a sandbox by `displayName`: the window title its primary
terminal last set, then `config.name`, then its ID
(`services.SandboxDisplayName`). The title is the harness's — Claude Code
writes one from the first prompt — and `config.name` is a generated pair of
words that the title hides as soon as there is one. Nothing a person sets
outranks either, so a box that is "Commit files" stays "Commit files"
(issue #160).

The reporter tried the two things that look like a rename. Writing a
`description` into `~/.discobox/meta.json` changed the description, which is a
separate field shown beside the name (ADR 0136). `/rename` in the harness
changes the title where the harness supports it, and lasts until the harness
titles the session again.

A sandbox already keeps what it is for in its meta file, which the agent
inside edits directly, the server caches on the sandbox row, and
`PATCH sandboxes/{id}/meta` writes from outside (ADR 0136).

## Decision

### 1. The name is a third field of the meta file

`~/.discobox/meta.json` gains `name`: what a person or the agent calls this
sandbox. It is one line of display text, trimmed, at most 128 bytes, of
printable characters only — no control characters and no invisible format
characters; absent or empty means no name has been set. It is not
an identifier: it need not be unique, and nothing is keyed by it.

All three fields are optional, in the file and on the wire. The file already
reads that way — `{}` is a valid meta — and the API's `SandboxMeta`, which
requires `tags` today, stops requiring it: a meta is whatever subset of
`name`, `description` and `tags` it holds, and an absent field is an unset one.
A field the schema does not know still fails the read, as it does now.

The file stays the system of record, with the rules ADR 0136 gave it: the
agent in the sandbox sets its own name by writing the file, and the `sandboxmeta`
package owns the shape for the sandbox agent, the server and the CLI.

### 2. The row caches it with the rest of the meta

The sandbox row gains a nullable `meta_name` column beside `description` and
`tags`, recorded from the status report's `meta` and from the answer to an API
write, under the same `meta_observed_at` and the same newer-wins rule (ADR 0136
§§2, 4). It is what names a stopped sandbox in a listing. Existing rows read as
"no name set" and need no backfill. The export spec carries the copy the way it
carries the description (ADR 0136 §6).

### 3. A set name is the display name

`displayName` becomes: the meta name when one is set, then the primary
terminal's title, then `config.name`, then the ID. It is still computed on the
server from what is already on the row.

The harness's title is not lost: `runtime.agentStatus.sessions[].title` still
carries it for a client that wants to show it.

`config.name` is untouched and is not what a rename edits. It stays the
generated handle a sandbox is created with, and stays resolvable on the command
line. A set name resolves the way a title does today, through the listed name
(`listedName`), where it must be unique among the candidates.

### 4. Renaming is a meta write

There is no rename route. `PATCH sandboxes/{id}/meta` gains an optional `name`
— a string sets it, an empty string clears it, and leaving it out leaves it
alone — and it is carried to the sandbox agent, applied to the file, and
recorded from the agent's answer exactly as a description is (ADR 0136 §3).

The CLI gains `discobox rename DISCOBOX [NAME]`, resolving its argument the way
`tag` does; with no `NAME` it clears the name, and the listing goes back to the
title. `discobox tag` keeps to tags and the description.

### 5. A rename is like any other meta write

A change that carries `name` is treated as one that carries a tag: a stopped
sandbox is started to take it, the write is gated as `exec:write`, and nothing
is recorded that the sandbox does not hold (ADR 0136 §3).

Whoever may change a sandbox's meta may name it. The sandbox names itself by
writing its file. The sandbox role's `PATCH sandboxes/{id}/meta` for a
discobox the caller created carries whatever the route carries, so a lead
names its workers (ADR 26-10-08-447).

## Alternatives rejected

**Rename edits `config.name` on the row.** Rejected: it makes the server the
system of record for one fact about a box whose other such facts live in the
box. The agent inside could not name its own box without an API and a
credential, a name set in the file would be a second name beside it, and
`config.name` is also the handle scripts resolve a box by — a rename would move
it under them. ADR 0136 rejected the server-owned description for the same
reasons.

**`displayName` prefers the first line of the description.** Rejected: the
description is prose the agent rewrites as the work moves, often several
lines, and a listing already shows its first line beside the name. Making it
the name would rename a box every time its agent updated what it was doing.

**Leave it to the harness's title, and document `/rename`.** Rejected: not
every harness has one, the harness may title the session again, and a title is
gone from a session that has ended and been replaced. The request is for a
name that stays where a person put it.

**Refuse a rename on a stopped sandbox.** Rejected: a tag change on the same
route starts the sandbox, and one route giving two answers for a stopped
sandbox, by field, is a rule every caller would have to learn. Consistency
costs booting a box to change its label.

**Record the name on the row for a stopped sandbox and deliver it at the next
start, so nothing is booted.** Rejected: the row would hold what the sandbox does not, which ADR
0136 §3 rules out, and the server would need a queue of pending meta changes
and a rule for one that meets a file the agent edited in the meantime.

## Consequences

- A person names a box with `discobox rename`, the agent inside names
  it by writing `name` into its meta file, and either shows in `discobox ls`,
  the console and the window title: at once through the API, within a status
  poll (15 s) from the file.
- Renaming a stopped sandbox starts it, as tagging one does.
- Two boxes may carry the same name. A command given that name refuses it as
  ambiguous, as it does two boxes with the same title today.
- A meta file with a `name` does not read on a sandbox agent from before this
  change, which accepts no field but `description` and `tags`: it reports
  `metaError` and refuses API writes until the box is upgraded or the field
  removed (ADR 0136). That is accepted. No upgrade path is built for it, and
  the same holds for any reader from before this change that meets the new
  field or a meta without `tags`.
- The console's rename (`e` in the list) wrote `config.name` and was refused
  on a box showing a title, because the name it edited was not the one on the
  row. It becomes this meta write and loses that refusal. `discobox admin box
  update --name` stays the raw edit of `config.name`.
