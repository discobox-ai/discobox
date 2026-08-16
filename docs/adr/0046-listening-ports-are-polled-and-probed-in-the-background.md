# 0046 — Listening ports are discovered by a standing poller and probed for HTTP

- **Status**: Accepted
- **Date**: 2026-08-16
- **Relates to**: [ADR 0030](0030-pool-agent-polls-and-pushes-sandbox-agent-status.md),
  whose status payload this extends, and
  [ADR 0024](0024-ssh-is-a-control-plane-ingress-onto-execs.md), whose
  `tcp/attach` route is the transport a future port forward would ride.

## Context

Nothing outside a sandbox knows what its user processes are serving. A harness
runs `npm run dev` and binds `127.0.0.1:5173`; the control plane, the CLI, and
the UI all learn nothing about it. `sandbox-agent` already has a
raw-TCP ingress (`GET .../tcp/attach`, ADR 0024) that dials `host:port` from
inside the sandbox's network namespace, so a forward *could* be set up onto any
local listener — but only by a client that already knows the port number and
already knows whether to speak HTTP to it. Today a user has to know and type
both.

Discovering the listeners is cheap: `/proc/net/tcp` and `/proc/net/tcp6` name
every socket in `TCP_LISTEN` along with the uid that owns it, so "ports bound
by the sandbox user" is one procfs read and a uid comparison — no `ss`, no
`lsof`, no walk of `/proc/*/fd`.

Deciding what is *behind* a listener is not cheap in the same way. There is no
declaration to read: the only way to know whether port 5173 speaks HTTP, HTTPS,
or a wire protocol of its own is to connect to it and see what comes back.
That is an outbound TCP connection into a user's process, and for a non-HTTP
service it means writing bytes it will reject.

`sandbox-agent/DESIGN.md`'s status rule says the status endpoint is *"answered
fresh on every request … never cached"*, which is what git status and session
state do today. Following that rule for ports means probing every listener on
every status request — and ADR 0030's poll loop makes a status request every
15 seconds, per sandbox, forever.

## Decision

**A standing poller inside sandbox-agent scans procfs for the sandbox user's
listening TCP sockets, probes each newly observed socket once to classify it,
and caches that classification for the socket's lifetime. The status endpoint
reports the poller's latest snapshot rather than computing one.** Concretely:

1. **Discovery is a procfs read, filtered by uid.** `/proc/net/tcp{,6}`, state
   `0A`, uid equal to the identity execs and terminals resolve to
   (`execs.Manager.ResolveUser`, ADR 0025 — the agent's own uid when the
   manifest names nobody). Everything system services own is excluded by
   construction, and sandbox-agent's own listener is excluded explicitly for
   the case where the sandbox user *is* root.

2. **Classification is one probe per socket, cached by inode.** A listener is
   probed the first time it is seen: connect, send a minimal HTTP/1.1 `GET /`,
   and classify the answer as `http`, `https`, or `tcp` (reached, speaks
   something else). The result is keyed by the socket inodes backing the port,
   so a restarted server is re-probed and a long-lived one is not. A probe that
   fails to connect yields `unknown` and is retried on the next tick — the port
   may have been closing, or the process may not have been ready.

   The same request is repeated inside a TLS handshake when, and only when, the
   plaintext answer leaves TLS open: a TLS record, a hang-up without a word, or
   an HTTP `400`. That last case is not a detail — Go's `net/http`, nginx, and
   Apache all answer a plaintext request to an HTTPS port with a *plaintext*
   `400`, so "the reply began with `HTTP/`" is not on its own proof that a port
   is not TLS. Any other status, `404` very much included, is proof, and costs
   the one connection.

3. **The status endpoint reads the snapshot.** `GET .../status` gains a `ports`
   array beside `sources` and `sessions`, and it is the one part of that
   response that is not computed at request time.

## Alternatives rejected

**Probe on every status request, keeping the "never cached" rule intact.**
This is the consistent-with-everything-else option and it is why this ADR
exists. Rejected on three grounds:

- *It repeats an intrusive act on a fixed schedule.* Probing means connecting
  to a user's process and writing bytes at it. Doing that once when a port
  appears is a reasonable cost for the information. Doing it to every listening
  port every 15 seconds, for the entire life of the sandbox, turns the control
  plane's telemetry into a permanent low-rate scan of the user's own services —
  in their logs, in their connection counters, in their metrics.
- *It puts a user process in the latency path of a control-plane poll.* Git
  status shells out, but to `git`, with a bounded budget, against files. A
  probe's response time is whatever a user's half-started dev server decides
  it is. The existing budget shape (per-item timeout plus a total cap) would
  contain it, but containing it means the status response is truncated by
  whichever service is slowest — trading a stale answer for a missing one.
- *The answer it recomputes does not change.* What protocol a listening socket
  speaks is fixed for the socket's lifetime. Recomputing it is not freshness,
  it is repetition; the inode key already detects the only event that
  invalidates it.

**Discover ports on demand and probe in the background.** A middle option:
scan procfs in the handler (genuinely cheap, genuinely fresh) and consult a
background probe cache for the protocol. Rejected because it splits one fact
across two clocks — a port could be reported in the same response as a
protocol belonging to the socket that previously held it — for the sake of
shaving at most one poll interval off a value the consumer already treats as
15 seconds stale (ADR 0030's Consequences).

**Report the process behind each port** (name, argv), by walking `/proc/*/fd`
for the socket inode. Genuinely useful for display — "port 5173, vite" reads
better than "port 5173". Rejected for now as a separate concern with a
materially different cost: it is a scan of every process's descriptors on every
tick rather than two file reads, and it reaches into processes the status
payload otherwise never describes. See Deferred.

**Have pool-agent probe the sandbox's ports instead.** Pool-agent can already
reach a sandbox's container IP, so it could scan and probe from outside.
Rejected because it cannot see what the sandbox sees: a listener bound to
`127.0.0.1` — the default for most dev servers — is not reachable from the
pool host at all, so the common case would be invisible. Enumeration is
impossible from outside regardless; procfs is inside.

**A `netlink` socket-diag query instead of procfs.** The kernel-blessed
replacement for `/proc/net/tcp`, and the one that scales when a host has tens
of thousands of sockets. Rejected as unjustified for a single sandbox's
listener set: it is a binary protocol requiring a raw netlink socket where the
alternative is two text files, and the scale problem it solves does not exist
here.

## Consequences

- Sandbox-agent gains its first cached, background-computed status component.
  The DESIGN rule is amended rather than quietly broken: the status endpoint is
  computed fresh *except* for `ports`, which is a snapshot, and the exception
  has this record behind it. A future component wanting the same exemption
  needs the same kind of argument, not this precedent.
- Sandbox-agent now originates outbound TCP connections to sandbox-local
  addresses on its own initiative. This does not touch the boundary rule it
  must not break — it never calls the pool host or the control plane — but it
  is new behavior, bounded to loopback and the sandbox's own directly-bound
  addresses, one connection per socket lifetime.
- A non-HTTP service is written to once with an HTTP request line. It will log
  a malformed-request error. This is the unavoidable cost of classification
  without a declaration, and it happens once, not per poll.
- Port data is as stale as the poller's interval plus ADR 0030's poll and push
  latency. A port that appears just after a scan is reported up to one poll
  cycle late, which is acceptable for a "what could I forward?" listing and is
  explicitly not a low-latency signal.
- HTTP/2-with-prior-knowledge servers (h2c, and HTTPS servers that will only
  negotiate `h2`) classify as `tcp`, since the probe speaks HTTP/1.1 and
  offers no ALPN. Correcting that is a probe change, not a shape change.

## Non-goals

This ADR does not build the port forward itself. It makes the ports and their
protocols visible on the sandbox record so that a forward can be offered
without the user first knowing the number; the CLI surface, the proxy path
(most likely ADR 0024's `tcp/attach`), and its authorization are separate
decisions.

It also does not add any control-plane behavior keyed on ports — no automatic
exposure, no URL minting, no reconciliation. The control plane stores what was
reported, as it already does for the rest of ADR 0030's payload.

## Deferred

- **The process behind each port** (name and argv, via `/proc/*/fd` inode
  lookup). Revisit when a UI or CLI surface actually displays ports and the
  bare number proves too anonymous to act on.
- **Unix domain sockets and UDP.** Only listening TCP is reported. Revisit if a
  concrete forwarding case needs either; neither is forwardable through
  ADR 0024's TCP ingress as it stands.
- **Push-on-change rather than poll.** A netlink socket-diag subscription or an
  inotify-shaped signal could report a new port in milliseconds instead of one
  poll interval. Revisit if a feature needs a port to appear the instant it is
  bound — a browser preview that opens itself would qualify; a listing does
  not.
