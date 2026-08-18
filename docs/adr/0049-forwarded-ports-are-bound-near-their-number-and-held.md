# 0049 — Forwarded ports are bound near their own number, and held once given

- **Status**: Accepted
- **Date**: 2026-08-18

## Context

A sandbox already reports what its own processes are listening on: the
sandbox-agent's poller discovers the ports and probes them for HTTP
(ADR 0046), and the listing reaches the control plane on the same push that
carries the git state. `disco ls` and the launcher draw those numbers on a row.

Nothing reaches them. A port a person can see and cannot open is a worse
outcome than not showing it, and the two existing ways in do not close the gap:

- `/projects/{p}/sandboxes/{s}/http/{port}/*` reverse-proxies from the pool
  agent to the container IP. It is HTTP-only, and it addresses the sandbox from
  outside it, so a dev server bound to `127.0.0.1` — which is the default for
  most of them — is not there.
- `ssh -L` reaches loopback inside the sandbox correctly, over the tunnel
  ADR 0024 §3 built. It needs an enrolled key, an `ssh` binary, and a list of
  forwards fixed when the connection is made. A sandbox announcing a new port
  five minutes in cannot be added to a running `ssh -L`.

What is wanted is the thing a person means by "open my sandbox's ports": every
port it announces, reachable locally, appearing as it appears, with the local
numbers matching the sandbox's numbers closely enough to be guessable.

## Decision

### 1. The tunnel is exposed at the HTTP edge, and the CLI does the forwarding

`GET /api/projects/{p}/sandboxes/{s}/tcp/attach?host=&port=` forwards the
websocket upgrade to the sandbox-agent's `/tcp/attach`, gated by `tcp:connect`.
It is ADR 0024 §3's tunnel with the SSH session in front of it removed: the
same lease chain, the same pool-agent route, the same endpoint inside the
sandbox's network namespace, and the same `execstream/frame` half-close
(ADR 0024 §4). `disco proxy` opens one per forwarded connection.

Driving `ssh -L` from the CLI was the alternative, and it is the reason the
tunnel exists at all. It was rejected because a forward set that changes while
the command runs is not something `ssh -L` can express — the forwards are
argv — and because it would make an everyday CLI command depend on key
enrollment and a working `ssh` binary to reach a sandbox the CLI is already
authenticated to. The tunnel is a control-plane primitive; SSH was the first
caller, not the owner.

Generalizing `/http/{port}` instead was rejected for the reason ADR 0024 §3
already gives: the pool agent dials the container IP, and the ports people
want forwarded are usually not on it.

### 2. A port is bound near its own number, not at an ephemeral one

The local listener for a sandbox port starts its search at that same number and
takes the nearest free port above it: a sandbox serving 8080 is
`localhost:8080`, or `localhost:8081` when something local already has 8080. A
privileged port gets one attempt at its own number and then the whole search at
its number plus 8000 — 80 becomes 8080, 443 becomes 8443.

Binding an ephemeral port and printing it was the simpler alternative. It was
rejected because the number is the interface: a person who knows their sandbox
serves 3000 will type 3000, and being right almost always is worth more than
being consistent. The privileged offset exists because an ordinary user cannot
bind 80 at all, and scanning 80..144 would be 65 guaranteed failures on the way
to an ephemeral port — 8080 is where a person would look anyway.

Failing when the exact port is taken was also rejected. Two sandboxes serving
8080 is the normal case, not an error, and a forward that refuses to start is
useless precisely when a second sandbox is what you have.

### 3. A local port is held once it has been given out

A sandbox port that stops being announced does not release its local port. The
binding is marked inactive and reported, connections to it fail while it is
gone, and the same local number is reused when the port comes back.

Releasing it was the obvious alternative and is wrong for the case that
dominates: a dev server restarting drops off the listing for a moment, and a
local port that moved in that moment would break the URL the user has open in a
browser — the one thing the feature exists to keep working. Holding the port
costs a listener on a number nothing is behind, which is recovered when the
command ends.

This makes the forward set grow-only for the life of a `disco proxy`. That is
acceptable for a foreground command bounded by a session. If a long-lived
holder — the launcher — finds the accumulation matters, revisit with an
explicit release rather than a timeout: a grace period would reintroduce the
same race at a less predictable moment.

### 4. The listing is polled

`disco proxy` re-reads the sandbox on an interval rather than subscribing to
the project event stream. The stream carries resource identities and actions,
not bodies, so an event would be followed by exactly the read the loop already
does; and the ports reach the control plane on the sandbox-agent's own poll
cadence (ADR 0046) either way, which is what actually bounds freshness. A
subscription would add a second failure mode for no reduction in latency that a
user could perceive.

### 5. The mechanics are frontend-independent

`cli/internal/portforward` owns local port selection, accepting, splicing, and
status reporting, against a `Dialer` interface and a set of targets. It knows
nothing about sandboxes, websockets, or the control plane; `disco proxy`
supplies both halves and prints the events.

The alternative — writing it inside the command and lifting it later — was
rejected because the launcher is a known second caller with a different
consumer of the same events (a pane, not a line of text), and a status stream
that only exists as `fmt.Fprintln` has to be rebuilt to have a second
frontend.

## Consequences

- A forwarded port is an unauthenticated door onto something inside a sandbox
  for anything that can reach it, so bindings are loopback-only unless
  `--address` says otherwise. The authorization boundary is the sandbox, as
  ADR 0024 already states for `direct-tcpip`: whoever may exec there can
  already reach those services.
- Every forwarded connection is one websocket through the control plane and one
  pool-agent hop. This is a developer-scale path, not a load-bearing proxy.
- A local port number is not stable across runs of `disco proxy`, only within
  one. What is stable is the rule that produces it.
- `-R`-style forwarding remains out of scope, per ADR 0024 §8.
