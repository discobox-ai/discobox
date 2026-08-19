# 0059 — A rejected swapped credential is retried once, and the delivered file is restored

- **Status**: Accepted
- **Date**: 2026-08-19

## Context

A sandbox never holds a credential. It holds a sentinel, the proxy swaps the
real value in on the way out, and the control plane keeps that value fresh —
for an OAuth secret, by refreshing it about five minutes before it expires
(`server/internal/resources/secrets/oauth.go`). The sentinel itself does not
expire, which is why the credential file a harness reads (`harness.SecretDeliveryFile`)
is written once, at terminal create and revive, with `expiresAt` set to
2100-01-01 and a placeholder where the refresh token would go.

That arrangement has a gap at exactly one moment: the rotation. The proxy
caches a resolved value per `(clientID, sentinel, host)` and re-checks it on a
30s soft interval, so for a short window after the control plane mints a new
access token, different sandboxes are sending different tokens — some the new
one, some the one it displaced. Either can be the wrong one:

- the cached value is the old token and the upstream has stopped accepting it;
- the cached value is the new token and the upstream has not started accepting
  it yet.

Both surface identically, as a single `401` on a request the sandbox did
nothing wrong to make.

On 2026-08-19 the second variant is what happened. The control plane rotated
the credential at 22:52:24.874Z. Within fifteen seconds three sandboxes each
took exactly one `401` on `POST /v1/messages` — the only Anthropic 401s in the
pool's audit database in two days — while five other sandboxes on the same
credential kept getting 200s throughout, including 13 seconds after the
rotation. Each 401 arrived roughly 250ms after a 200 from the same sandbox: two
near-simultaneous requests, the first served the cached value, the second the
one a background refresh had just stored.

What made a transient blip permanent is the harness's reaction. Claude Code
reads a 401 as an expired login: it rewrote `~/.claude/.credentials.json` with
`accessToken`, `refreshToken` and `expiresAt` emptied, then tried to refresh —
which cannot work, because the refresh token it was handed is a placeholder by
design. One second later each of the three sandboxes was logged out for good,
with a perfectly good credential still sitting behind its sentinel and no path
back to it. Nothing in the sandbox rewrites that file, so every subsequent
launch was logged out too.

## Decision

Two changes, at the two layers that own the two halves of the failure.

**The proxy retries a rejected swapped credential once, with a different one.**
When a request the proxy swapped a credential into comes back `401`, it sends
the request again, choosing the credential in the order a rejection makes
likely: a freshly resolved value (invalidating the cache entry first), and
failing that, the value the last rotation displaced, kept for two minutes. If
neither differs from the value that was rejected, no retry is attempted — there
is nothing new to send. The 401 is audited as the exchange it was, and the
retry is audited as its own.

Only header swaps qualify, and only when the request body fit in the 8 MiB the
retry holds in memory; anything else is forwarded as a stream and its 401 is
passed through.

**The sandbox agent restores a delivered credential file the harness cleared.**
A templated harness file that rendered a sentinel must still contain it. A
30-second loop re-renders any that does not, so a harness that cleared its own
credential is signed in again on its next launch. Nothing else about the file
is reconciled, and `createOnly` files are exempt.

## Alternatives rejected

- **Do nothing at the proxy and rely on the file restore.** The restore fixes
  the next launch, not the running session: a harness that has decided it is
  logged out keeps that decision in memory. The session the user was in the
  middle of is exactly what is worth saving.
- **Retry with the same credential after a delay.** It is the obvious cheap
  version and it does not hold up. On 2026-08-12 one sandbox took three 401s in
  1.5 seconds on the same cached value; a re-send of a value the upstream just
  rejected buys a duplicate upstream request and the same answer. A retry is
  worth making only when there is a different credential to make it with.
- **Delay adopting a rotated value until the old one nears expiry.** This
  prevents the observed failure outright and needs no retry machinery, but it
  is the wrong default in the mirror case: when a rotation *revokes* the old
  token, deliberately holding onto it converts a two-second blip into five
  minutes of failure. The previous-value fallback gets the same protection
  without betting on which way a given rotation behaves.
- **Give the sandbox a real refresh token so the harness can recover itself.**
  This is the one thing the whole sentinel design exists to prevent. A refresh
  token in a sandbox is a credential in a sandbox.
- **Have the control plane verify a newly minted token before adopting it.**
  There is no provider-agnostic way to verify one, and a probe request per
  rotation is a provider-specific dependency in the layer that most needs to
  stay generic.
- **Reconcile the whole delivered file set rather than the sentinel.** It would
  clobber settings a sandbox legitimately changed. The narrow invariant — the
  sentinel that was delivered is still there — is the part that belongs to the
  control plane rather than to the sandbox.

## Consequences

- A 401 on a swapped request can cost one extra upstream request, bounded at
  one per request, and only when a different credential is available to send.
- A swapped request with a body now holds up to 8 MiB in memory for the life of
  the request. Larger bodies stream as before and are not retryable.
- The audit gains a row: a retried request appears as a 401 exchange followed by
  the retry's own exchange, both against the same spooled request body.
- `Installer` grows `RestoreSecretFiles`; every implementation carries it, and
  the ones with no files return nothing.
- A harness that deliberately rewrites a delivered credential file *without* the
  sentinel gets it put back within 30 seconds. That is the intent — an in-sandbox
  login was never a supported path — but it means such a login cannot stick.
