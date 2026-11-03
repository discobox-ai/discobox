# 26-11-03-815 — A pool waits on a sandbox's ports, and reports a change at once

- **Status**: Accepted (narrows [0046](0046-listening-ports-are-polled-and-probed-in-the-background.md)'s
  scan interval on Linux, and [0030](0030-pool-agent-polls-and-pushes-sandbox-agent-status.md)
  §1's standing poll as the only way a sandbox's status reaches the pool)
- **Date**: 2026-11-03
- **Relates to**: [26-11-02-152](26-11-02-152-a-harness-is-set-up-in-the-box-it-runs-in-and-configure-is-removed.md),
  which removed configure and with it the fast poll its sign-in callbacks rode
  ([26-10-09-867](26-10-09-867-a-configure-port-may-be-ephemeral-and-is-still-bound-at-its-own-number.md)).

## Context

A port a program opens in a box reaches a person's machine in three timed
hops: the sandbox agent rescans its socket table every 5 seconds (ADR 0046),
the pool agent polls each sandbox's status every 15 seconds (ADR 0030), and
the console reads the server's port list every 2 seconds. That is about 11
seconds on average and up to about 22.

That was fine for a dev server. It is not for a sign-in. A harness signing in
through a browser listens on a loopback callback port and sends the browser to
it; the console forwards that port at its own number when it is free, so the
redirect lands on the box — once the port is bound. A provider that redirects
at once, because the person is already signed in there, beats the forward and
the code is lost. Configure hid this with a half-second scan and a one-second
poll of configure boxes only. With configure gone (ADR 26-11-02-152), every
sign-in happens in an ordinary box.

Linux has no notification for "a socket started listening". procfs sends no
inotify events; netlink `sock_diag` answers queries and multicasts only socket
destruction; the `sock:inet_sock_set_state` tracepoint needs eBPF, which a
container box does not have the privileges for and a VM box's kernel may not
carry. Detection inside the box is a poll whatever is done.

The sandbox agent may not call out to the pool or the server (ADR 0030,
`sandbox-agent/DESIGN.md`), so it cannot push a change either.

## Decision

### 1. The socket table is rescanned every half second on Linux

The Linux scanner — the procfs tables — runs every 500 ms, the interval
configure boxes already ran at. darwin keeps 5 seconds: its scanner runs
`lsof`, a process per scan.

### 2. The sandbox agent answers a wait on its ports

`GET .../status/wait?after=<version>`, under `status:read` like the status it
belongs to, answers with `portsVersion`, an opaque token that changes whenever
the published port snapshot does: a port appearing or going, a probe
classifying one, its addresses changing. It is unique to the agent's process,
so a restarted agent is never mistaken for an unchanged one.

Waiting is the caller's to ask for. A call without `after`, or with an `after`
that is not the current version, answers at once. Only a call naming the
current version is held, until the snapshot changes or 25 seconds pass,
whichever is first; then it answers with the version it has, changed or not.
The hold is the agent's bound, not the caller's: nothing a caller sends holds
a connection longer.

The status route itself is unchanged and never waits.

### 3. The pool waits on every running sandbox and checks a changed one in at once

The pool agent keeps one wait open per running sandbox, started and stopped
by the standing tick as sandboxes come and go. The first call carries no
`after` and only learns the version; each call after names the last version
seen. When a call returns a different version, the waiter wakes the standing
loop, which polls that sandbox's status and pushes it then, on the same loop
as the 15-second tick — so reports still reach the control plane in the order
they were observed.

The 15-second tick stays the check-in for everything else and the fallback
for ports. A sandbox whose agent answers the wait with 404 predates it; its
waiter stops asking and tries again in five minutes, since an agent can be
upgraded in place. Any other failure backs off a few seconds.

### 4. Only ports wake the pool

Program status, git status and the rest keep the 15-second check-in. Ports
are the one fact a person is racing against; the others change constantly
while a harness works, and waking on them would turn the poll into a stream of
single-sandbox pushes without anyone waiting for one.

## Alternatives rejected

- **The sandbox agent pushes a change.** It is the boundary ADR 0030 keeps: an
  untrusted box originating writes to trusted infrastructure. A held request
  gives the same latency with the pool still the only side that dials.
- **Hold the status call itself.** The check-in would then be late by however
  long the wait ran, and the pool could not tell a slow agent from a quiet one.
- **Poll every sandbox's status each second.** Each status computes git status
  for every source; a pool of boxes would spend that every second to catch the
  rare port.
- **Scan with `sock_diag` filtered to listening sockets.** Cheaper than procfs
  in a box with thousands of sockets, but a kernel without the diag modules
  (a VM box's minimal kernel) needs procfs anyway, and procfs at 500 ms is what
  configure already ran. Revisit if a profile shows the scan costing a busy
  box.
- **Wake on any status change.** See §4.

## Consequences

- A port reaches the server within about a second of being bound, and a
  person's machine within about three — what configure had.
- A pool holds one idle connection per running sandbox. A machine reached at
  its public URL holds it through whatever proxy fronts it; 25 seconds is
  under the idle timeout of the common ones.
- A sandbox with an agent older than this keeps the 15-second check-in.
