# 0122 — A window that polls lists the servers that answer, the primary included

- **Status**: Accepted
- **Supersedes**: [0116](0116-a-discobox-address-names-a-server-and-a-discobox.md) §4's "the primary not answering fails the command" **for the launcher only**. It stands for `discobox ls` and the picker, and the rest of §4 stands everywhere.
- **Date**: 2026-09-16

## Context

[ADR 0116](0116-a-discobox-address-names-a-server-and-a-discobox.md) §4 wrote
one rule for three listings — `discobox ls`, the discobox picker, and the
launcher: a registered server that does not answer within a bound is a note and
the listing is everything else, while the primary not answering fails the
command.

Two of those three are commands. They ask once, print, and exit, and somebody is
standing in front of the answer: waiting on the primary for as long as it takes
is right, and having nothing to print when it never answers is right.

The launcher is not a command. It is a window that asks again every five
seconds, for as long as it is open, and draws what it got. Under §4's rule as
written that meant:

- The primary's request was the one with no bound, so a primary that took the
  connection and never answered hung the listing for the life of the window,
  with the next tick stacking another request on top every five seconds.
- The primary's failure returned an error from the whole listing, so one server
  going away discarded the rows of every server that was up, and the window
  showed an error where the list had been.

Both are the same reading of §4: the primary is special, and its failure is the
listing's failure. In a command that reading is the user's own machine failing
to answer the question they just asked. In a window it means a machine that went
to sleep takes the other two with it.

## Decision

### 1. In the launcher, a listing is what the servers that answered said

The primary is a server like the others in the launcher's listing. One that
fails is reported as not answering, where a registered server is, and the
servers that answered are the listing.

`discobox ls` and the picker keep §4's rule unchanged: they are one question
with somebody waiting on it, and a client that cannot reach its own server has
nothing to print.

### 2. A window with one server keeps §4's rule

There is no listing without it and no other server's rows to fall back on, so
its failure is the listing's failure, reported as the window has always reported
it. The rule §4 wrote is the right rule wherever failing is the only honest
answer; what changes is that having other servers makes it the wrong one.

### 3. What is drawn says which of the two it is

A server that failed is `not answering` and its rows are gone. A server that has
been asked and has not answered yet is `still listing` and its rows are on the
way. They are different facts about a server and the window says them
differently; conflating them would mean either calling a slow server dead or
letting a dead one look like it was thinking.

**A dead primary is reported as an error rather than a note**, so it stays on
screen. Everything else the window does is the primary's — creating a discobox,
the harnesses, the secrets, the credential inbox — and each of those fails on
its own while it is down. One quiet line underneath all of that would read as
the smaller problem when it is the reason for the rest.

### Rejected

- **Keeping §4's rule and only bounding the primary's request.** It fixes the
  hang and leaves the worse half: a primary that is merely down still blanks
  every other server's rows and replaces the list with an error. The rows the
  window can show are exactly the ones it should show.
- **Reporting a dead primary as a note like any other server.** It reads as the
  smallest thing on a screen where nothing else works, for the reason in §3.
- **Giving the launcher no bound at all and relying on the retry.** A window
  lives for hours. A request with no bound against a wedged connection is a
  server that is never asked again, and a goroutine and a connection held until
  the window closes.
- **A short bound, as the commands use.** A server that takes half a minute to
  answer is a server whose answer is still wanted when it comes; cut off at a
  command's bound it would never land at all, however many times it was asked.
  The launcher's bound is minutes, and nothing waits on it — the poll draws what
  has arrived and the answer is drawn by whichever poll comes after.
- **Making the launcher's listing an error whenever any server failed.** It is
  §4's rule generalised the wrong way, and gives one sleeping laptop the power
  to empty the window.

## Consequences

- The launcher's `List` returns an error only when there is one server. With
  more than one, every failure is a named server in `Listing.Unreachable`, and
  the caller draws the rest.
- ADR 0116 §4's sentence about the primary is now about `discobox ls` and the
  picker. A reader looking for the launcher's rule reads this ADR and
  [cli/DESIGN.md](../../cli/DESIGN.md)'s "Many Servers".
- The window's polls — the listing, the machine readout, the credential inbox —
  are one-at-a-time and bounded, so what a server that stops answering costs a
  window open for an hour is one outstanding request rather than one per tick.
- A registered server being slow is now visible (`still listing`) where before
  it was silence followed by rows. A listing late as a whole says so on the
  band. Neither appears for an ordinary refresh, which is milliseconds.
