# 26-11-03-815 — A pool waits on a sandbox's ports, and reports a change at once

- **Status**: Accepted (narrows [0046](0046-listening-ports-are-polled-and-probed-in-the-background.md)
  §1's procfs discovery and its scan interval on Linux, and [0030](0030-pool-agent-polls-and-pushes-sandbox-agent-status.md)
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

### 1. Listening sockets are rescanned every half second on Linux, with `sock_diag`

The Linux scanner runs every 500 ms, the interval configure boxes already ran
at. darwin keeps 5 seconds: its scanner runs `lsof`, a process per scan.

At that rate procfs's cost is no longer small. Reading `/proc/net/tcp` walks
the kernel's whole established-socket hash, which is sized by the host's
memory, not by the sockets in the box: on a 512 GB, 64-core host each of
`tcp` and `tcp6` took about 2.7 ms, every box's scan paying for the host —
about 1% of a core per box at 500 ms. ADR 0046 rejected `sock_diag` because
the scale it solves "does not exist here"; it does, in the host's hash.

So listening TCP sockets are listed with a netlink `sock_diag` dump
(`SOCK_DIAG_BY_FAMILY`, `inet_diag_req_v2`) asking for `TCP_LISTEN` only,
which walks only the listening hash — about 0.2 ms for both families on the
same host — filtered by uid as procfs's rows were. It is unprivileged and,
like procfs, scoped to the agent's network namespace, which is the sandbox's.
Its request and reply are a stable kernel ABI (`include/uapi/linux/inet_diag.h`,
unchanged since Linux 3.3); `x/sys/unix` does not define the structs, so their
layout is this repository's, and a test checks the dump lists exactly what
procfs lists on the kernel it runs on.

The UDP tables stay on procfs: they are small, and `sock_diag` has nothing
cheaper for them.

A kernel without `CONFIG_INET_DIAG` and its TCP module refuses the dump —
the libkrunfw kernel VM boxes boot does, as can a host that cannot load
modules, or gVisor. The scanner then reads procfs for the rest of the agent's
life. That costs little there: a VM guest's hash is sized by the guest's
memory. Any other failure falls back for that scan only.

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

The pool agent keeps one wait open per running sandbox, started the moment
the runtime reports the sandbox running and stopped when it leaves running;
the standing tick starts and stops them too, as a backstop, but never undoes a
start or stop newer than the list it read. The first call carries no `after`
and is answered at once; each call after names the last version seen. When a
call returns a different version — the first answer included, since a port
may have been bound before the waiter connected — the waiter wakes the
standing loop, which polls that sandbox's status and pushes it then, on the
same loop as the 15-second tick — so reports still reach the control plane in
the order they were observed.

Wakes for one sandbox are at least two seconds apart. The version is the
box's to move: a test run starting and stopping servers moves it twice a
second, and a hostile agent on every answer, and each wake costs a status
computation and a control-plane write. A change inside the spacing is not
lost; the next wait names the version before it and is answered at once.

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
- **Keep procfs alone, and slow the scan on a host where it is costly.** It
  gives back the latency this decision is for, on exactly the large hosts that
  run the most boxes. `sock_diag` costs a second scanner on Linux, and procfs
  stays as the fallback, tested beside it.
- **Enable `INET_DIAG` in the VM kernel too.** Not needed: a VM guest's procfs
  read is cheap, and the kernel is a release of its own. Revisit if a VM box's
  scan shows in a profile.
- **Debounce in the agent** (publish a port only once it has survived two
  scans). It adds a scan's latency to every port, the sign-in's included, and
  bounds nothing a hostile agent does; the pool's spacing bounds both.
- **Wake on any status change.** See §4.

## Consequences

- A port reaches the server within about a second of being bound, and a
  person's machine within about three — what configure had — unless the same
  box was checked in for a change in the two seconds before: then it waits out
  the spacing, up to about 2.5 seconds. A new box's first answer is such a
  check-in, so a sign-in callback bound in its first two seconds is the case
  that waits; the waiter's first answer still saves it the 15-second tick.
- A container box's scan costs about a tenth of a millisecond on any host,
  where procfs's grew with the host's memory. A VM box's reads procfs.
- A pool holds one idle connection per running sandbox. A machine reached at
  its public URL holds it through whatever proxy fronts it; 25 seconds is
  under the idle timeout of the common ones.
- A sandbox with an agent older than this keeps the 15-second check-in.
