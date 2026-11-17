# 26-11-17-583 — A sleep may leave a note for the next start

- **Status**: Accepted
- **Date**: 2026-11-17
  Decided by the owner on #1262 ("I'd rather have sleep take a note"), 2026-11-17,
  choosing this design over the alternatives below; accepted with the change
  that implements it.
- **Amends**: [26-10-16-337](26-10-16-337-a-discobox-asks-to-sleep-and-names-when-to-wake.md)
  §2 (a note is no longer only a wake's contents), §3 (`sleep --note` needs no
  wake) and §7 (a boot hands over the notes for the next start with the wakes
  that fell due);
  [26-10-16-420](26-10-16-420-a-docker-pool-freezes-a-sleeping-sandbox-and-a-person-can-suspend-one.md)
  §4 (a thaw sets them aside as it sets aside the due wakes).
- **Relates to**: issue #518.

## Context

`discobox-power sleep --note TEXT` refuses a note with no `--wake-in` or
`--wake-at`, because under ADR 26-10-16-337 §2 a note is the contents of a wake
file. With no wake there is no file to hold it, and nothing to hand it back.

An agent that is done, rather than waiting on something with a time, still
has something to say to whoever picks the box up next: why it stopped, what it
left, what to look at. A worker that delivered a PR and powers off is the
common case. It was told to sleep with a note and no wake, and the command
failed (#518). The owner asked that `sleep` take a note on its own.

## Decision

**A note with no wake is a note for the next start: a file in
`~/.discobox/wake/.next/`, handed over by whatever next starts or thaws the
sandbox, exactly as a wake that fell due is. It asks for no start.**

1. **Where it lives.** Every regular file in `~/.discobox/wake/.next/` whose
   name does not begin with `.` is a note for the next start. Its contents are
   the note and belong to the harness, as a wake's do; its mtime is when it was
   left. `.next` begins with `.`, so it is not a wake and nothing schedules a
   start for it: the sandbox's `wakeAt` and a power-down's confirmation ignore
   it. `discobox-power sleep --note TEXT` with no wake writes `.next/sleep`,
   dated now, before its sleep request. `echo … > ~/.discobox/wake/.next/x`
   does the same by hand. `discobox-power status` lists the notes.
2. **Who reads it.** The next boot sets the notes aside with the due wakes
   (ADR 26-10-16-337 §7), into `~/.discobox/wake/.done/`, and the harness it
   launches gets their paths in `DISCOBOX_WAKE` with the wakes', oldest first.
   It does not matter what started the box: a person attaching, a request, the
   manager, or a wake. A thaw sets them aside too (ADR 26-10-16-420 §4). That
   thaw is the start the note was left for, so a frozen harness reads it in
   `.done/`, as it reads its due wakes, rather than having it reach a later
   boot stale.
3. **Names never collide on the way.** A wake keeps its name in `.done/`. A
   note whose name a wake moved in the same pass already took gets the first
   free `<name>.<n>`. A pending `sleep` wake and a later `sleep` note are both
   handed over.
4. **A note with a wake is unchanged.** It is that wake's contents, and
   nothing is written to `.next/`.

## Alternatives rejected

**Keep refusing it, and tell agents to put their state in tags.** This was the
triage's reading of ADR 26-10-16-337, and it needs no change. The owner
preferred the feature: a done agent's note is worth keeping, and the refusal
surprised the agent that hit it.

**Put the note in the sleep request.** The request is on `/run` and is consumed
when the sleep is confirmed (ADR 26-10-16-337 §1), and its contents are
ignored. Making it survive and be read would give the request a second
meaning and a reader it was designed not to have.

**Have the server show it.** The agent's status would report the note and the
server would store it on the sandbox, so `discobox ls` and a manager could
read it while the box sleeps. Rejected for now: it adds an API field, server
storage and a display surface to carry text that ADR 26-10-16-337 decided
discobox does not read. Revisit if a person or a manager needs to read a
sleeping box's note without starting it.

**Write it as a wake dated now.** A wake already due when the box powers down
asks for an immediate start (ADR 26-10-16-337 §5), so the box would wake as
soon as it slept.

## Consequences

- `sleep --note` with no wake succeeds, and the note survives the stop: it is
  in the run user's home, not on `/run`.
- A sleep that never happens (the box is used before it powers down) leaves the
  note in `.next/` until the next boot or thaw, which hands it over then.
- The thaw response's `wakes` now lists the notes it set aside as well.
