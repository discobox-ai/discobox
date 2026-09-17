# 0124 — A terminal is active while what it shows changes

- **Status**: Accepted
- **Date**: 2026-09-17
- **Supersedes**: [ADR 0108](0108-a-sandbox-stops-itself-when-its-terminals-go-quiet.md)
  §2's first activity, a title change on any exec. The rest of §2 —
  connections, leases, the agent's start, and remembering the latest activity
  seen — and §§1, 3–5 stand unchanged.

## Context

ADR 0108 made an exec's title changing the sign that the program in it is
busy, on the grounds that coding harnesses animate their title while they work.
Not all of them do. A harness that leaves its title alone reads as idle for the
whole of a long task, and the sandbox powers off under it while it streams
output — the one case the idle stop exists never to hit.

What every program at work does, harness or not, is show something: a spinner
frame, a streamed response, a build log. The shim's screen emulator already
sees every byte of it (`shimruntime/screen.go`).

## Decision

### 1. An exec's activity is its screen or its title changing

The shim records `screenChangedAt`, replacing `titleChangedAt`: when the text on
the exec's screen changed, or its title changed to a different value. The
policy reads it where it read the title change, and logs it as
`screen change on exec <id>`. As before, it is recorded at the source, survives
a sandbox-agent restart with the shim, and exists only for TTY execs — pipe
execs, which is what declared services are, have no screen and still need a
lease.

### 2. A change is to the text, not the bytes

The shim compares a hash of the screen's cell content — no styles, no cursor,
no scrollback — plus whether the alternate screen is up. Output that leaves all
of that as it was is not activity:

- a cursor the program draws itself and blinks by toggling a cell's reverse
  video, as Bubble Tea's text inputs do;
- cursor motion and visibility, and mode changes;
- a redraw of the screen the program already has, including one that clears
  the whole screen first;
- a client's resize, which lays the same text out again, and is taken as the
  new baseline without counting.

A cursor the *terminal* blinks sends nothing at all.

The scrollback is left out because this emulator copies the screen into it on
every full-screen erase (`CSI 2J`), so a program that clears and redraws the
same frame would grow it each time.

### 3. The screen is compared when a burst of output ends

A frame reaches the emulator in as many pieces as the PTY was read in, and a
frame that erases before it draws is blank part-way through. So the screen is
compared once output has paused for 50ms — by the next write after the pause,
or by the next `/status` read — and a change is dated to the burst's last write,
not to whatever settled it. Output that never pauses is compared at least every
second, so a program streaming without a break still reads as busy. A 200×60
screen hashes in about 20µs.

## Alternatives rejected

**Any output byte is activity.** Simplest, and what "terminal activity" first
suggests. Rejected because programs write while idle: a self-drawn blinking
cursor writes twice a second forever, TUIs re-render unchanged frames on
timers and focus events, and shells re-send their title on redraw — which
ADR 0108 already refused to count. Any of them would hold an idle sandbox up
for good.

**Screen changes including styles.** Catches a reverse-video cursor blink as
a change, for the same result as counting bytes.

**Keep the title change as its own activity beside the screen's.** Two fields
and two log labels for one fact — the program showed something new. The title
is folded into the one time.

**Compare on a write, throttled to a fixed interval.** Cheaper to reason
about, but the write that opens an interval is often the first piece of a
frame, so an idle program redrawing a large frame on a timer is compared
half-drawn and counts as busy on every redraw.

**Include the scrollback length, so identical lines scrolling past count.**
Rejected for the full-screen erase above: clear-and-redraw would count until
the scrollback filled.

**Compare only the lines the emulator marks touched.** Cheaper per compare,
but it leans on the emulator's damage bookkeeping, which is cleared on screen
switches, resets, and resizes for its own rendering purposes. A full hash —
about 20µs for a 200×60 screen, at most once per burst and so at most 20 times a
second — is cheap enough not to need it.

## Consequences

- A harness that never sets a title keeps the sandbox up while it prints or
  animates anything.
- Anything that changes its screen text on a timer holds the sandbox forever
  while its terminal exists: `watch`, `top`, a clock in a prompt, tmux's status
  line (tmux is in the image). That is the claim a lease makes, honored the same
  way — ADR 0108 already accepted it for a title animating forever.
- A program printing the same line over and over, with nothing else on the
  screen changing, does not count once the screen is full of it.
- Harnesses that sit on a still screen while waiting on the user still stop
  after the idle timeout, as before.
