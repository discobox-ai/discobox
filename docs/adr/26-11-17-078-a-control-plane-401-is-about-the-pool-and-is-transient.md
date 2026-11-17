# 26-11-17-078 — A control-plane 401 is about the pool, and is transient

- **Status**: Accepted (by the owner by merging #1284, 2026-11-17)
- **Date**: 2026-11-17
- **Supersedes**: [ADR 26-10-14-482](26-10-14-482-a-sentinel-the-control-plane-could-not-resolve-is-never-sent.md)
  §2's last paragraph, where a `401` and a resolve made before the pool holds a
  control-plane credential are each `ErrDenied`. It also narrows §2's
  paragraph on the retry of [ADR 0059](0059-a-rejected-swapped-credential-is-retried-once.md)
  (and of a `403`, [ADR 26-11-11-539](26-11-11-539-a-replaced-secret-value-reaches-the-proxy-cache-and-a-rejection-report-names-the-value-it-refused.md)
  §3), which passes the upstream's refusal through when it has nothing else to
  send. 482 §§1 and 3 stand.
- **Issue**: #1282, part B. Part A (re-minting the resolve token by wall-clock
  age) is a bug fix that needs no ADR.

## Context

The pool proxy resolves a sentinel by calling the control plane as the pool,
with a short-lived signed assertion the pool agent mints and writes to the
resolve-context file (`pool-agent/agent.go`, `startResolveTokenRefresher`). The
resolver reads that file again on every call
(`pool-agent/proxyagent/secrets.go`, `readResolveContext`).

ADR 26-10-14-482 §2 split a resolve failure into two kinds. A transient one is
retried for `ResolveRetrySeconds`, and then the request is refused with `503`,
`Retry-After` and `x-should-retry: true`. A denial (`ErrDenied`) leaves the
sentinel in place and sends the request, so the upstream answers `401`. The
resolver took any status below `500` as a denial except `408`, `425` and `429`
(`resolveStatusTransient`), and took a resolve made with no credential on disk
as a denial too.

Issue #1282 shows the cost. A host that sleeps longer than the assertion's
30-minute life wakes with an expired assertion on disk, and on a `vz` pool the
guest's clock lags for up to 30 s after it steps. Every resolve is then answered
`401` by `PoolAuthenticator`. The resolver reads that as a denial. The swapper
replaces a working cached value with a cached denial, and a synchronous resolve
sends the sentinel. `api.anthropic.com` answers `401`. The `401` retry resolves
again, is denied again, has nothing different to send, and passes the `401`
through. Claude Code reads it as a dead login, and the person has to run
`/login` with a credential that never stopped working.

What a `401` on these routes is about, on `main`:

- **It is always about the caller.** `Authentication` (`server/internal/auth/middleware.go`)
  answers `401` for every `PoolAuthenticator` error: no assertion, pool not
  found, pool revoked, a key type it does not take, a signature that does not
  verify, an assertion outside its validity window, an identity that does not
  match the route. The handlers answer `401` only for a missing pool principal
  (`server/internal/handlers/secrets.go`). Nothing about the sandbox or its
  credential is ever a `401`. That answer is `200` with `pending` or `denied`,
  a `403`, or a `404`.
- **A revoked pool has no sandboxes.** `RevokedAt` is set only by the pool
  reconciler's delete (`server/internal/resources/pools/reconciler.go`), only
  after it has refused while any sandbox is still assigned and after the
  runtime's `RemovePool`, and the row is deleted in the same pass. From then on
  the authenticator answers "pool not found", which is a `401` too.

## Decision

**A control-plane answer that is about the pool's own credential, rather than
about the sandbox's, is transient. A `401`, and a resolve made before the pool
holds a credential, are both that. A revoked pool is not told apart.**

### 1. The resolver reads a `401` as transient

`resolveStatusTransient` adds `401`. `Resolve` and `ResolveRaw` both return a
plain error for it, never `proxy.ErrSecretResolveDenied`. From 482 §§1–2 as
they stand, this means:

- a cached value keeps serving until its hard expiry, and a background refresh
  that is answered `401` keeps the value instead of caching a denial;
- a request with nothing usable cached is asked about again for the retry
  window. Each ask re-reads the resolve-context file, so an assertion the pool
  agent re-mints inside the window is picked up and the request goes out.
  Otherwise the request is refused with `503`, and the sentinel is never sent;
- a held raw value keeps standing.

A resolve made before the resolve-context file holds a credential is
transient in the same way. Today `Resolve` makes it a denial, while `ResolveRaw`
and `ResolveGatewayHeaders` already make it an error. Both resolves now agree:
a proxy that serves before the agent has written its first assertion holds the
request for the window rather than sending the sentinel.

`403` and `404` stay denials. They are what the control plane says about the
sandbox or its credential, and 482 §3 keeps them as it was. So do a
`pending` or `denied` answer and any other status below `500`.

`ResolveGatewayHeaders` is unchanged: every answer but `200` is already no
answer.

### 2. A retry that could not resolve answers `503`, not the upstream's refusal

When `retryRejectedSwap` (`proxy/http.go`) has a refusal it retries (a `401`,
or a `403` inside a declared rejection scope, 26-11-11-539 §3), and the
fresh resolve failed transiently for a sentinel the first attempt swapped, and
the previous value does not cover every such sentinel, the proxy does not
pass the upstream's refusal through. It answers the sandbox as 482 §2 answers
a request it could not resolve: `503`, `Retry-After`, `x-should-retry: true`.

- The upstream's refusal was a real exchange and is audited as one, as the
  retry path already does. The `503` is audited as blocked, with the reason
  the credential could not be resolved.
- The refusal is still reported (`OutcomeRejected`): the upstream did refuse
  the value it was sent, which is evidence about that credential whatever
  happened to the resolve after it.
- When the fresh resolve succeeded and resolved to the value that was refused,
  nothing changes: the upstream's refusal passes through, because the control
  plane has said that is the credential.
- A refusal the proxy cannot retry (a body too large to hold, a swap into the
  URL) or whose retry the gate refuses passes through as it does today. No
  resolve was attempted, so there is no transient failure to report.

A harness retries a `503`. It logs out on a `401`.

### 3. The credential broker reports a `401` as unavailable

`controlPlaneError` (`pool-agent/proxyagent/credentials.go`) maps a `401` from
the control plane to the protocol's unavailable code (`agentcreds.Code`'s
default, a `500` to the sandbox) instead of `agentcreds.ErrDenied`. The
sandbox's client then says "try again" rather than "denied". A `403` stays
`ErrDenied`.

The rejection reporter (`proxy/credentialreport.go`) is unchanged: it already
drops every failed send the same way, whatever the status.

### 4. A revoked pool is not told apart

The resolver treats a revoked pool's `401` like any other `401`. Nothing in the
resolve, broker or report path asks why a `401` was given.

- **Its proxy has no sandbox to answer.** Revocation follows the last
  sandbox's removal (Context). A request still reaching such a proxy comes
  from a sandbox the control plane no longer places there, and is held, then
  refused with `503`, instead of being sent with its sentinel and refused with
  `401`. Neither one works, and only the `401` would make a harness drop its
  login.
- **Revocation is enforced by the refusal, not by how the pool reads it.** A
  revoked pool gets no new value either way. A cached value keeps serving only
  until its hard expiry, which is what the same proxy does through any
  outage, and what a pool that ignored the control plane's answer could do
  regardless. Reading the `401` as a denial would add nothing to that bound.

## Alternatives rejected

- **A distinct status, or a header, from the server for an assertion outside
  its validity window.** For example `401` with `WWW-Authenticate: Bearer
  error="invalid_token"` for an expired or clock-skewed assertion, and a
  different answer for a revoked pool. This would let the resolver keep
  reading the rest as a denial. It splits a status the server answers before
  it knows anything about the request, for a distinction no caller acts on:
  the revoked case has no sandbox (§4), and a bad signature (a restored
  database, a pool recreated under an agent that kept its key) also strands
  every sandbox on the pool's credential, not on theirs. It adds an API
  variant that a pool agent and a server of different versions would have to
  agree on, so it buys nothing over §1.
- **Read the `401` body.** The authenticator's message says which fault it
  was, and the pool agent could match on it. The message is for people, and
  matching on it ties the pool agent to wording no contract fixes.
- **Treat every status below `500` as transient.** That turns a real refusal
  (`403` not this pool's sandbox, `404` no such sentinel) into a held request
  and a `503` the harness retries for ever. That is 482 §2's reason for having
  denials at all.
- **Fix only the token refresh (part A).** It narrows the window after a sleep
  to the guest clock's step (about 30 s on `vz`) and to seconds elsewhere. It
  does not close it, and it does nothing for any other way the assertion goes
  bad. A denial on a pool fault is what turns the window into a lost login.
- **Make the proxy ask the pool agent to re-mint on a `401`.** This would
  shorten the window further. It needs a channel from the proxy unit to the
  agent that does not exist, and with part A's wall-clock check and §1's
  re-read of the file on every ask, the retry window already covers a fresh
  assertion. Revisit if pool logs show `401`s on resolve outliving the retry
  window after part A ships.

## Consequences

- A host sleep, a guest clock that lags, or any other fault in the pool's
  assertion costs a running harness `503`s, held for up to the retry window
  each, and never a logout. With part A the assertion is replaced within
  seconds of waking, and the `503`s stop.
- A pool whose key the control plane no longer recognizes, a restored database
  for example, now holds and refuses its sandboxes' requests with `503`
  instead of sending sentinels. The cause shows up as `401`s on resolve in
  the pool's and the server's logs, where it did before; the sandbox sees a
  `503` instead of an upstream `401`.
- The resolver's transient-status test (`TestResolverTellsAnUnansweredResolveFromADenial`)
  gains a `401` row, and the retry path gains a test where the fresh resolve
  fails transiently.
- `proxy/DESIGN.md` and `pool-agent/DESIGN.md` describe the new
  classification when the change lands.
