# 26-10-14-482 — A sentinel the control plane could not resolve is never sent

- **Status**: Accepted
- **Date**: 2026-10-14
- **Relates to**: [ADR 0059](0059-a-rejected-swapped-credential-is-retried-once.md),
  whose retry this leaves as it is, and
  [ADR 26-09-22-838](26-09-22-838-a-dedicated-pool-harness-judges-commands-and-credential-bearing-requests.md)
  §4, whose authorization still runs first.

## Context

The pool proxy swaps a sentinel for the credential it stands for, resolved
through the control plane and cached per `(client, sentinel, host)`. Its design
said: fail closed on the secret, fail open on the request. A sentinel that did
not resolve, for any reason, stayed in place and the request went upstream.

A control-plane restart showed what that costs (issue #205). One request from a
running Claude Code discobox reached `api.anthropic.com` with the raw sentinel.
The upstream answered `401 OAuth access token is invalid`. Claude Code read that
as a dead login, tried an OAuth refresh, failed, and dropped its `Authorization`
header until its next turn. One second of restart cost the discobox minutes of
unauthenticated requests.

Two things combined:

1. **The cache did not survive a publish.** `ApplyConfig` built a new
   `Swapper`, and so a new, empty cache, on every policy publish. The pool agent
   publishes on every host-trust refresh (every 30 s while the control plane
   answers), every `secrets.json` write, and every agent-credential mint or
   expiry. A cached value seldom lived 30 s. The stale-while-revalidate design,
   which keeps a value serving through an outage until its hard expiry, rarely
   had a value to serve.
2. **A transient failure was treated like a denial.** With nothing cached, a
   resolve that failed because the control plane was unreachable left the
   sentinel in place, and the request was sent.

A `401` and a `503` mean different things to a client. A `401` says the
credential is bad, and clients start refresh or logout handling. A `503` says
the service is out, and clients wait and retry. Sending the sentinel turns an
outage into the first kind.

## Decision

### 1. One `Swapper` for the life of the proxy

The `Swapper` is built once, with its resolver, and `ApplyConfig` reconfigures
it in place (`Swapper.Reconfigure`). The new sentinel set and swap tuning are
installed. A cached value, a cached denial, and a rotation's previous value are
dropped only for a `(client, sentinel)` pair that left the set. A publish that
changes only trusts, or another sandbox's sentinels, leaves every entry intact.

A sentinel removed from a client stops resolving at the publish that removes it.
A resolve already in flight when it is removed does not store its answer.

Everything else about the cache is unchanged: the hard expiry (the grant, an
activation window, `PositiveTTLSeconds`), the background refresh past the soft
bound, `Invalidate` after an upstream rejection, and the negative cache for a
denial.

### 2. A transient failure is retried, then refused

When a sentinel the request carries cannot be resolved for any reason but an
authoritative denial, and nothing usable is cached:

- the proxy asks again, with backoff, for a bounded window
  (`ResolveRetrySeconds`, 5 s by default). That outlasts a control-plane
  restart. Each ask is bounded by the window too, so a control plane that
  hangs holds the request no longer than one that refuses.
- If the window passes without an answer, the request is not sent. The proxy
  answers `503 Service Unavailable` with `Retry-After` and
  `x-should-retry: true`. It audits the request once, as blocked, with the
  reason: the credential could not be resolved.

This is per request, not per sentinel. A request carrying two sentinels, one
resolved and one not, is refused whole. Sending it would put a raw sentinel on
the wire again.

The `401` retry (ADR 0059) never sends a freshly resolved candidate that left a
sentinel unresolved. It falls back to the previous value only when that covers
every sentinel the first attempt swapped, and otherwise passes the upstream's
own `401` through, as it did when nothing differed.

What counts as transient is the resolver's to say: any error but `ErrDenied`.
The pool agent's resolver returns `ErrDenied` for a status below `500` other
than those below, for a resolve that is not approved, and before it has a
control-plane credential at all. A `5xx`, a `408`, a `425`, a `429`, or no
response at all is transient.

### 3. A denial is unchanged

`ErrDenied` is the control plane's answer, including the pending-approval case.
The sentinel stays in place and the request is sent, so the upstream rejects
it. That is a statement about the credential, and a `401` is the right thing for
the client to see.

## Alternatives rejected

- **Only keep the cache across publishes.** This fixes the common case. It does
  not cover a cold cache: a new discobox, a value past its hard expiry, or an
  entry `Invalidate` just dropped. Any of these during a restart would still
  send the sentinel.
- **Only refuse on a transient failure.** Without a cache that lives through a
  publish, every request after a 30 s trust refresh resolves synchronously. A
  restart would then turn into a burst of `503`s across every running discobox,
  where the cache could have served them.
- **Answer `502 Bad Gateway`.** A `502` is what the gate uses when the upstream
  failed after the request was let in, and the call may have been acted on.
  Here nothing was sent. `503` with `Retry-After` is the status clients already
  treat as "try again shortly".
- **Wait without a bound.** That holds the request open for as long as the
  control plane is down. A client's own timeout would then fail it with less
  information than a `503` gives.

## Consequences

- A running discobox keeps working through a control-plane restart until each
  value's hard expiry, which is what the cache always promised.
- While the control plane is down, a discobox with nothing cached sees `503`s
  from its proxy instead of `401`s from its upstream. Clients retry; none of
  them log out.
- A request with an unresolvable sentinel can be held for up to the retry window
  before it is answered.
- The proxy's reporter still lives on the proxy, not the `Swapper`. It no longer
  needs to, but it is the proxy's state about the response path, not the
  cache's.
