# 26-10-31-618 — A sandbox's name is followed by what its program reports

- **Status**: Accepted
- **Date**: 2026-10-31
- **Supersedes**, in part:
  [26-10-12-026](26-10-12-026-a-sandboxs-name-is-meta-and-renaming-it-is-a-meta-write.md)
  §3's "a set name is the display name" (it now leads it);
  [26-10-16-424](26-10-16-424-a-terminals-status-is-what-its-program-reports-over-osc-7501.md)
  §6's "the box's display name as `title`" (the display name can now hold the
  msg the record carries beside it).

## Context

A listing names a sandbox by `displayName`: its meta name when one is set,
else the window title its primary terminal last set (ADR 26-10-12-026 §3). The
title stood for what the work is about because the harness wrote it there:
Claude Code titled the window with the session's title.

Claude Code now speaks OSC 7501 (ADR 26-10-16-424). Told the protocol is
there, it reports what it is doing in the record's `msg` and leaves the window
title a fixed "Claude Code". Every box running it lists under that one title,
and a named box shows its name with nothing of what it is doing. What the
title used to say is now the msg.

## Decision

### 1. The msg stands where the title stood, and follows a set name

`displayName` becomes, from the newest primary session on the row:

- the meta name and the msg, as `name: msg`, when both are set;
- the meta name alone when there is no msg;
- the msg alone when there is no meta name;
- then, as before, the window title, `config.name`, and the ID.

The msg is made displayable (`programstatus.Displayable`) before it is joined.
The rule is `sandboxmeta.Title`, one function the server and the console
share, so the console's window title and primary tab say what the listing
says, from the stream's msg when it is newer than the poll.

### 2. Only while the sandbox is running

A stopped sandbox's row keeps the last status its agent posted and is never
rewritten. The msg counts only while the runtime state is `running`; a stopped
box lists under its name or title, the way `discobox ls`'s AGENT column
already shows no status for it.

### 3. The given name still resolves alone

A name typed to give a box keeps naming it: a `listedName` argument matches the
meta name as well as the display name, under the same ambiguity and short-ID
rules (`matchSandboxName`).

### 4. The console's record names the box without the msg

The OSC 7501 record the console re-emits (ADR 26-10-16-424 §6) carries the
given name as its `title`, else `config.name`, else the ID — never the display
name, which would repeat the `msg` beside it, from a poll that lags the stream.

## Alternatives rejected

**Show `name: msg` only in the console's window title and tab.** Rejected: the
listing would then name every unnamed Claude Code box "Claude Code", and
`discobox ls` would lose what tells two of them apart — the reason the title
was made the name in the first place.

**Name by the record's own `title` field.** Rejected: it is optional, Claude
Code does not send one, and a program that does sends its application's name,
not what the session is about.

**Use the msg only in `idle` or `done`, so the name churns less.** Rejected: a
working box is the one whose work a person wants to see named, and the states a
program passes through are its own to choose; filtering by state would hide the
msg exactly while it is current.

## Consequences

- A box's listed name changes as its program reports. A name copied from
  `discobox ls` can stop resolving once the next report lands; the given name,
  the configured name and the ID do not (§3).
- The ADR 26-10-12-026 objection to naming by the description — that it would
  rename a box every time its agent updated what it was doing — is accepted
  here for the msg: the msg is what the title was, and the title already moved
  with the harness.
- A harness that does not speak OSC 7501 is unchanged: it reports no msg, and
  its title names it as before.
