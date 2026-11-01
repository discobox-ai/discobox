# 26-10-31-618 — A sandbox's name is followed by what its program reports

- **Status**: Accepted
- **Date**: 2026-10-31
- **Supersedes**, in part:
  [26-10-12-026](26-10-12-026-a-sandboxs-name-is-meta-and-renaming-it-is-a-meta-write.md)
  §3's "a set name is the display name" (it is now the title the msg follows);
  [26-10-16-424](26-10-16-424-a-terminals-status-is-what-its-program-reports-over-osc-7501.md)
  §6's "the box's display name as `title`" (the display name can now hold the
  msg the record carries beside it).

## Context

A listing names a sandbox by `displayName`: its meta name when one is set,
else the window title its primary terminal last set (ADR 26-10-12-026 §3). The
title stood for what the work is about because the harness wrote it there:
Claude Code titles the window with the session's title, which it generates
from the first prompt.

Claude Code now speaks OSC 7501 (ADR 26-10-16-424). Told the protocol is
there, it reports what it is doing in the record's `msg` and drops the
spinner glyph from its title; the title is still the session's. The `msg` is
set only while it works (the tool call it is making) or waits on a person
(what it waits for). Idle, and done in an interactive session, it reports no
`msg`.

Every Claude Code box nonetheless listed as "Claude Code", the title it falls
back to before a session has one. The harness's env set
`CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`, which also stops Claude Code
generating the session's title, so the fallback was all it ever wrote. And a
named box showed its name alone, with nothing of what it was doing.

## Decision

### 1. The msg follows the resolved title: `title: msg`

`displayName` becomes, from the newest primary session on the row,
`title: msg`, or the title alone while there is no msg, where:

- the title is the first set of the meta name, the window title, `config.name`
  and the ID — a set name replaces the window title rather than heading it;
- the msg is the one the program last reported over OSC 7501.

So a box keeps one name, and says what it is doing beside it while its agent
works or waits. The msg never stands in for the title. The msg and the window
title are both made displayable (`programstatus.Displayable`) and cut to the
msg's limit: both are a program's free text, posted from inside the box. The
rule is `sandboxmeta.Title`, one function the server and the console share, so
the console's window title and primary tab say what the listing says, from the
stream's msg and the pane's title when they are newer than the poll, and the
title the poll reported until the pane has drawn one. Both read the title and
the msg from the posted sessions the same way
(`programstatus.PrimaryTitleAndMsg`).

The Claude Code harness stops setting `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`
and sets `DISABLE_TELEMETRY`, `DISABLE_ERROR_REPORTING` and
`DISABLE_FEEDBACK_COMMAND` in its place, which keep telemetry, error reports
and `/feedback` off and let Claude Code title the session.

### 2. Only while the sandbox is running

A stopped sandbox's row keeps the last status its agent posted and is never
rewritten. The msg counts only while the runtime state is `running`; a stopped
box lists under its name or title, the way `discobox ls`'s AGENT column
already shows no status for it.

### 3. The given name still resolves alone

A name typed to give a box keeps naming it: a `listedName` argument matches the
meta name as well as the display name, under the same ambiguity and short-ID
rules (`matchSandboxName`).

### 4. The console's record names the box without the program's words

The OSC 7501 record the console re-emits (ADR 26-10-16-424 §6) carries the
given name as its `title`, else `config.name`, else the ID — never the display
name, which can repeat the `msg` beside it, from a poll that lags the stream.

## Alternatives rejected

**Show the program's words only in the console's window title and tab.**
Rejected: `discobox ls` would then lose what a named box is doing, which is
what tells two named boxes apart — the reason the title was made
the name in the first place.

**Name by the record's own `title` field.** Rejected: it is optional, Claude
Code does not send one, and a program that does sends its application's name,
not what the session is about.

**The msg after a set name and in place of the title, as first decided**
(`name: msg`, else the name alone, the msg alone, then the window title).
Rejected: a harness reports no msg while idle, which is most of a box's life,
so a box nobody named fell back to the title anyway, and while it worked it
lost the session's title, which is what it is for, exactly while it was
busiest.

**The msg in the title's place, else the title, after a set name, as first
amended** (`name: msg`, else `name: title`). Rejected: what a box is called
turns into what it is doing whenever it works, and back when it stops, for a
named box and an unnamed one alike. The msg only following the resolved title
keeps one name throughout and still says what the box is doing.

**The set name, then the title, then the msg (`name: title: msg`).** Rejected:
a set name is what a person or the agent chose to call the box in place of what
the harness wrote, not a prefix to it, and three parts crowd a listing's column
and a tab.

**Always the title, never the msg, so the name churns less.** Rejected: a
working box is the one whose work a person wants to see named, and the title
says what the session is about, not what it is doing; the msg is what tells a
working box's work apart while it is current.

## Consequences

- A box's listed name changes as its program reports. A name copied from
  `discobox ls` can stop resolving once the next report lands; the given name,
  the configured name and the ID do not (§3).
- The ADR 26-10-12-026 objection to naming by the description — that it would
  rename a box every time its agent updated what it was doing — is accepted
  here for the msg: the words after the title move while the agent works,
  and go when it stops; the title does not.
- A harness that does not speak OSC 7501 reports no msg; its title names it
  as before, unless a set name replaces it.
- Once a box is named, a session title (`/rename` in Claude Code) no longer
  shows; only editing the name changes what the box is called.
- Claude Code makes one more small model call per session, to title it, over
  the `api.anthropic.com` route its egress already allows.
- Dropping `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC` turns back on everything
  else Claude Code holds back under it, not only the title: background pull
  request status polls through `gh` and `glab`, usage-quota probes, MCP
  directory and plugin lookups, trusted-device enrollment, cloud sessions,
  artifacts and uploads. It was set for the credential judge, which is gone.
  In a restricted discobox the proxy refuses what its egress does not allow,
  and Claude Code treats those as the failed background calls they are.
