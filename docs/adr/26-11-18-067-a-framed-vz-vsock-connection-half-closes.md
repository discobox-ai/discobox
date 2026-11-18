# 26-11-18-067 — A framed vz VSOCK connection half-closes

- **Status**: Accepted
- **Date**: 2026-11-18
- Decided by the owner on 2026-11-18, answering a review finding on #1352
  ("fix now, publish vm/v9"); it lands with its implementation, on the
  owner's fast track for #1334.
- **Supersedes in part**: [26-11-17-182](26-11-17-182-a-vz-vsock-connection-is-always-drained-and-the-guest-sends-only-what-the-host-has-granted.md),
  Consequences: "`CloseWrite` stays unimplemented … exposing it is a separate
  decision". The rest of 182 stands.
- **Issue**: #1334

## Context

ADR 26-11-17-182 frames every vz VSOCK connection as one yamux stream, and
left the framed connection without `CloseWrite`: yamux's stream close is a
half-close, but exposing it was deferred. ADR 0062's vz `DESIGN.md` forbids a
`CloseWrite` that does nothing, so callers that probe for the method took
their full-close fallback.

Ports 3001–3004 carry the engine's own traffic, none of which needs a
half-close. The outbound relay ports, 3005 and 3006, do not: they carry a
sandbox's own TCP to the pool's proxy through `pooloutbound`'s splice, which
half-closes a direction when the connection lets it. A client that half-closes
after its request (`nc -N`, some CONNECT tunnels) had its relay connection
closed whole on vz, and lost the upstream's reply. On libkrun, whose plain
VSOCK connections have `CloseWrite`, the same client works.

## Decision

1. A framed connection's `CloseWrite` is its stream's half-close: it sends
   the stream's end, the peer reads EOF after everything written before it,
   and this side reads on until the peer ends its own side, as over TCP.
   `Close` still ends both directions and the session.
2. yamux's stream close timeout is off. Its five-minute default resets a
   half-closed stream whose peer is still sending, which would cut off a
   container's output streamed after its client closed stdin. A session ends
   when its connection is closed, as an unframed connection does.
3. Both ends move together: the guest's services and the host's driver use
   the same `pkg/vsock`, so the guest image is published again and re-pinned
   in the same change (182 §5's order).

## Consequences

- Every framed vz connection now half-closes where a caller asks:
  `discobox-vsock-guest`'s Docker proxy, the pool agent's HTTP server, the
  outbound relays, and the Docker client's hijacked attach. vz behaves as
  libkrun already does.
- A peer that half-closes and then never closes holds its connection, and
  what the host holds for it (one 256 KiB window), until it does, as over
  libkrun's VSOCK or TCP. An idle bound after a half-close would belong in
  the splices, for every backend, and is not added here.
- 0062's rule stands as it was meant: never a `CloseWrite` that does nothing.

## Alternatives

- **Document the limit and expose half-close with the next guest publish.**
  Offered to the owner and not chosen. The relay ports carry arbitrary
  sandbox TCP, and until the next publish a client that half-closes would
  lose its reply on vz and not on libkrun.
- **Keep yamux's close timeout and raise it.** Any bound resets a legitimate
  long stream after its client's half-close; the session's own life is the
  right bound.
