# 26-11-11-539 — A replaced secret value reaches the proxy cache, and a rejection report names the value it refused

- **Status**: Accepted (written after the fact: it records what main does
  since #1024 and #1029, plus §2's renewal memory, which #1140 implements;
  the owner accepts it by merging its PR)
- **Date**: 2026-11-11
- **Issue**: [#1022](https://github.com/discobox-ai/discobox/issues/1022)
- **Supersedes**, in part:
  [0059](0059-a-rejected-swapped-credential-is-retried-once.md)'s `401` as
  the only refusal retried (§3 here), and its previous-value fallback for a
  value a write replaced rather than a rotation (§1);
  [26-10-14-482](26-10-14-482-a-sentinel-the-control-plane-could-not-resolve-is-never-sent.md)
  §1's "dropped only for a `(client, sentinel)` pair that left the set" (§1).
- **Narrows**:
  [0132](0132-a-credential-rejected-after-its-retry-is-recorded-against-its-secret.md)
  §3, whose judgment now applies only to a report about the value the secret
  holds, and whose renewal memory is per value (§2);
  [26-10-13-595](26-10-13-595-a-harness-names-the-requests-whose-refusal-means-its-login-is-dead.md)
  §2's "the retry itself is untouched": a declared scope also widens what is
  retried (§3).
- **Split out**: retrying *and reporting* the statuses a harness declares
  (#1141).

## Context

A secret's value can be replaced behind a sentinel that stays the same. Saving
a harness login over the secret its config already owns does it (save-back's
update-in-place, `harnessconfigs/capture.go`), and so do `secret update` and a
client answering a refresh request (`secrets/refresh.go`).

Before #1024 nothing told the pool:

1. **The cache kept the replaced value.** The proxy caches a resolved value
   per `(client, sentinel, host)` and kept it across every publish that left
   the sentinel in place (26-10-14-482 §1). Only a change to a secret's
   routes marked its holders for re-delivery, so a replaced value was never
   published at all. The first request after the save carried the dead token,
   and a later soft refresh filed the dead token as the "previous" value,
   ADR 0059's retry fallback, for two more minutes.
2. **A report was judged against whatever the secret held now.** A report
   named a sentinel, never the value it saw refused. A report about the old
   login arriving after the save forced a renewal of the *new* login,
   spending its rotating refresh token.
3. **Only a `401` was retried.** Anthropic refuses a revoked OAuth token on
   `/v1/messages` with `403 permission_error`, which Claude Code reads as a
   dead login. The proxy handed it through with a fresh value in hand.

Together these made the loop in #1022: `/login`, save, "login expired" a few
seconds later, `/login` again. Only deleting the binding and the secret broke
it, because the next save minted a sentinel the pool had never cached.

## Decision

**A secret's value has a generation that a person's replacement moves and a
control-plane renewal does not. A replacement publishes it to every pool
holding the secret, and the proxy drops what it holds of another. A report
names the generation it saw refused, and the control plane judges only the
value it holds. Inside a harness's declared rejection scope, a `403` is
retried like a `401`, and not reported.**

### 1. A replaced value is published, and the pool drops the old one

**The generation is the value's write stamp.** `Secret.ValueGeneration` is
`ValueUpdatedAt` in microseconds, base 36: opaque outside the control plane,
and with no column of its own. It moves on every write the store stamps
(`ValueWritten`): a value replaced through `store.UpdateSecret` (`secret
update`, save-back's update-in-place), an answered refresh request (even one
that answers with the same value: the client vouched for it again), and an
expiry given with a value. A control-plane renewal
(`UpdateSecretValueIfUnchanged`) does not stamp, so it does not move.

**Every stamp moves the generation (#1029).** `ValueWritten` advances the
stamp to at least one microsecond past the previous one when the clock
repeats or goes backwards. A coarse clock (Windows) otherwise let a
replacement land in its creation's tick and keep the same generation.

**A replacement marks the holders.** Each writer that moves the generation
marks the sentinel delivery of every sandbox holding the secret, in the same
transaction, as a route change already did (`MarkSecretHoldersDirtyTx`):
`UpdateSecret` when the generation or routes changed, `RefreshSecret` always,
and save-back's update-in-place when the generation or routes changed.

**The delivery carries each sentinel's generation.** The sentinel delivery
sends `sentinelGenerations` (sentinel → generation), the pool agent keeps it
in the secrets file (`generations`, beside the routes), and the proxy is
handed it as `secrets.Config.Generations`. The pool API decoder refuses a
field it does not know, so it is sent only to an agent that declared it takes
it (`Pool.SentinelGenerations`, declared on every status report, as
`sentinelGrants` was). A delivery that left a stale generation is marked again
(`markSentinelsIfStale`), as a stale grant is.

**The proxy drops a value of another generation.** The resolve answer carries
`valueGeneration`; the proxy keeps it on the cache entry and on the previous
value. `Reconfigure` drops a cached value, and the value it displaced, whose
generation differs from the one delivered for its sentinel. An answer of a
replaced generation that arrives after the delivery (a resolve already in
flight) is used once, never cached, and is never filed as a previous value.

"Differs" is enough because nothing but a replacement moves the generation:
a control-plane renewal resolves to the same generation, so a delivery and a
fresh resolve never evict each other.

The previous value goes with the entry. ADR 0059 keeps it because a
rotation's displaced token is usually still accepted; a value someone
replaced is the one they replaced on purpose, often because it stopped
working.

A sentinel delivered with no generation (a control plane or agent from
before) drops nothing, which is 26-10-14-482 §1 unchanged.

### 2. A report names the generation it saw refused

**The proxy reports the value, by generation.** A report carries the
generation of the value the upstream answered: the first attempt's, or after
a retry, the retried value's (`ReportRequest.Generation`,
`ReportSandboxSecretRejectionBody.valueGeneration`). Like the sentinel, it is
no secret, and is sent only when the control plane handed one out.

**The control plane judges only the value it holds.**
`RecordSandboxSecretRejection` compares the reported generation with the
secret's before anything else. A report or an acceptance of another
generation is dropped with a `204`: it renews nothing, records
nothing and clears nothing. A report with none (an agent from before) is
judged as 0132 §3 says.

**The renewal memory is per value.** The forced renewal a rejection spends is
remembered under the generation it was spent on, not only the secret. A
refusal of the same generation inside the cooldown is
`rejected-after-refresh`, as 0132 §3 intends; a replaced value is a
generation no renewal was spent on, and is judged afresh. A control-plane
renewal keeps the generation, so the loop 0132 §3 guards against is guarded
as before. Main still keys it by secret alone; #1140 implements this.

### 3. A `403` inside a declared rejection scope is retried, not reported

`retryRejectedSwap` retries a `403` the way it retries a `401` when the
request carried a sentinel whose resolve named a rejection scope
(26-10-13-595) that covers the request (`secrets.Result.Declared`). A
harness that declared a scope has said which requests answer for its login,
and a refusal of one of them strands that harness whichever status says so.
Everywhere else, only a `401` is retried, as ADR 0059 decided.

A `403` is never reported, on the first attempt or on the retry. Elsewhere a
`403` is what the credential may not do, and a report spends a renewal on a
credential nothing showed was dead. A `401` on the retry of a `403` is
reported `rejected-after-retry`, as any `401` after a retry is.

Whether a harness should name the statuses that mean a refusal, and have
those reported too, is #1141.

## Alternatives rejected

- **A value revision column, compared "greater than".** The first draft of
  this ADR. It needs a migration and a backfill, and its point, ordering a
  stale delivery against a renewed value, does not arise once a renewal does
  not move the generation. #1029's monotonic stamp gives the same guarantee
  for the writes that do move it.
- **Use `Secret.Revision`.** It moves on a rename, a route change and a
  control-plane renewal: a rename would make every in-flight report look stale
  and drop it, and every renewal would be published to every pool.
- **Publish every control-plane renewal.** That is ADR 0059's rotation: the
  displaced value is usually still good, the soft refresh catches up within
  `RefreshInterval`, and the retry covers the moment between. Publishing it
  would cost a delivery per box per renewal for nothing the retry does not
  already do.
- **Evict on every publish, or flush the cache on a secret write.** One
  person's save would turn into a resolve storm across every box on the pool,
  which 26-10-14-482 §1 was written against.
- **Mint a new sentinel on every replacement.** It is what deleting the
  binding did, and it works. But a sentinel is delivered into the sandbox's
  environment and files, and a running process that read the old one keeps
  it; sentinels are kept stable across rebinds for that reason.
- **Shorten the positive TTL, or re-resolve every request.** The first
  narrows the window without closing it; the second puts the control plane
  on every request's path.
- **Rely on the `401` retry.** It recovers only when the upstream says `401`
  and the request can be retried, and still spends an upstream refusal on the
  first request after every save.
- **Retry any status inside the scope.** A `429` or a `5xx` is load, not a
  dead login: another value changes nothing, and a report would force a
  renewal for an outage.
- **Retry a `403` everywhere.** For a `gh` token a `403` is a rate limit, SSO
  enforcement or a missing scope, none of which another value fixes.

## Consequences

- A login saved over an existing one is what the next request carries once
  the delivery reaches the pool. A pool that cannot be reached is retried by
  the reconciler; until it takes the delivery, the `401` retry and the
  positive TTL are what they were.
- Every replacement costs one sentinel delivery per box holding the secret
  and one resolve per `(box, sentinel, host)` it drops. A renewal costs
  nothing new.
- A late report about a replaced login no longer spends the new login's
  refresh token or clears what is recorded about it.
- Reports about a value the control plane renewed away keep its generation
  and are still judged: two boxes refused just before a renewal can still
  record `rejected-after-refresh`. That is 0132's behavior, unchanged here.
- A dead login refused with `403` on both attempts reaches the harness and
  draws no band (#1141).
- Every side that predates generations sends none, or is sent none, and
  behaves as before.
