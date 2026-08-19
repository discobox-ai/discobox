# 0054. The workspace's two columns are terminals and shells, as the server records them

Status: Accepted

## Context

The launcher's workspace screen (`cli/internal/tui`) drew one discobox as two
columns: its primary harness terminal on the left, and every other live TTY
session as a tab on the right. That split was positional and hard-coded — one
pane on the left, all the rest on the right — so there was no way to ask for a
second terminal, and a harness terminal started elsewhere landed among the
shells.

Adding "open another terminal" (leader `c`) means the left side holds several
panes, and the screen has to say which side a session belongs on. Two sessions
that look identical in the exec listing cannot go to different columns, so the
question is what the columns are made of.

The workspace already holds one rule about itself: it mirrors the server rather
than remembering what was opened in this window (a session started from another
window, another machine, or a previous run of the launcher shows up on its own).
Restoring a layout on reattach has to come from the same place.

## Decision

The columns are the two kinds of session the server already records, not a
layout the client stores:

1. A session created in harness terminal mode — the primary, or any exec
   created with no command and no shell, which the sandbox-agent tags with the
   harness it resolved (`metadata.harnessId`) — is a **terminal**, and is drawn
   in the left column.
2. Every other TTY session — a shell, `disco exec` with a command — is a
   **shell**, and is drawn as a tab on the right.
3. The primary is the head of the left column and always pane 0. It is
   attached under the virtual `primary` exec id, which carries no creation
   time, so it sorts first however the attaches land.
4. Panes are numbered across the whole screen, terminals then shells, which is
   what the leader's digits and arrows count along.
5. `DataSource.NewTerminal` creates one by asking for an exec with no command,
   no shell and no harness id. Which harness that is, is the sandbox's answer.

No new API field, no exec metadata of the launcher's own, and no client-side
state that has to be reconciled.

## Consequences

- Reattaching restores the same two columns any other window would draw, from
  one `GET /execs`. There is nothing to persist and nothing to migrate.
- A harness terminal started from outside the launcher — `disco exec` in
  terminal mode, another window's leader `c` — appears on the left, where it
  belongs, instead of among the shells. This is a behavior change for existing
  sandboxes that already have non-primary harness terminals.
- The two columns mean two different things, so the keys do too: `s` opens a
  shell, `c` opens a terminal. `c` rather than `t` because `t` is stop in the
  list's key map, which the workspace carries whole (`paneOptions`), and
  because screen and tmux both create on `c` — and the leader is screen's.
- A sandbox whose harness is the `shell` built-in (ADR 0043) resolves terminals
  to that harness, so its left column holds login shells. That is correct: they
  are terminal-mode sessions of the harness the discobox runs, and they revive
  in place (ADR 0038) the way any terminal does, which a plain shell exec does
  not.

## Alternatives

**A column recorded in exec metadata (`metadata.pane = "left"`), with leader
`c` creating a plain shell there.** Rejected: it puts a client's window layout
into the sandbox's durable state, where every other client and every non-TUI
caller has to honor it or corrupt it, and it invents a second answer to
"what kind of session is this" beside the one the exec record already gives.
It also makes the left column hold something other than terminals, which leaves
the primary's own column with no meaning.

**Client-side layout state, persisted per sandbox on this machine.** Rejected:
the workspace's whole contract is that it shows the discobox as the server has
it. Remembered layout drifts the moment anything happens elsewhere, and it
cannot answer for sessions this machine never opened.

**Leave the left column single, and open new terminals as right-hand tabs.**
Rejected: it makes "terminal" and "shell" indistinguishable on screen, and the
primary's position — the one session whose ending ends the workspace — stops
meaning anything.
