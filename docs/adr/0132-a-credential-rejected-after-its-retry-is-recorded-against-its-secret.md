# 0132 — A credential rejected after its retry is recorded against its secret

- **Status**: Accepted
- **Date**: 2026-09-18

## Context

[ADR 0059](0059-a-rejected-swapped-credential-is-retried-once.md) made the proxy
retry a rejected swapped credential once, with a different value, and made the
sandbox agent put back a delivered credential file a harness cleared. Both
halves treat a `401` as a *blip* — a rotation the two ends had not converged on
yet — because that is what the incident it was written for was.

The other case is a credential that is simply dead: a subscription that lapsed,
an API key revoked in the provider's console, a refresh token spent or
invalidated by a sign-out somewhere else. The retry runs, finds nothing
different to send or sends the displaced value and is refused again, and the
`401` reaches the sandbox. From there:

- The harness concludes its login is gone. Claude Code clears
  `~/.claude/.credentials.json` and tries to refresh, which cannot work — the
  refresh token it holds is a placeholder (ADR 0059).
- The sandbox agent restores the delivered file within 30 seconds, so the
  sentinel is back and the next launch is "signed in" again — onto the same dead
  credential.
- The user sees a harness that will not stay logged in, in a box where the only
  visible remedy is `/login`, which is the one thing that cannot work: an
  in-sandbox login was never a supported path, and the restore loop overwrites
  it within half a minute.

The fix is out on the user's own machine — reconfigure the harness, which runs
its real sign-in and stores the new credential in the control plane — and
nothing in the product says so, because nothing knows.

The facts needed to say it exist and are all in the wrong places.

**The proxy knows the whole shape of the failure and records it as an
anonymous 401.** `retryRejectedSwap` (`proxy/http.go`) is the one place that
sees a request the proxy swapped a credential into, its rejection, and what the
retry made of it. The audit row it writes carries the status, the host and the
sandbox, and the swapped header names inside `RedactRequestHeaders`, and —
since [ADR 0130](0130-an-audit-record-is-read-where-it-was-written-and-names-its-attestor.md)
§3 — the *use* a protocol credential spent, by ID. It deliberately never names
the sentinel, so a swap from an ordinary injected sentinel names nothing at all:
and that is what a harness's own credential is. The trail is readable from the
control plane now (ADR 0130 §4, relayed by the pool agent), but a 401 on a
harness credential is still a row that cannot be attributed to a secret. The
one place that records the retry's *outcome* as a single fact is the
`proxy.secret_swap.retry` span — and nothing in this repository ever calls
`SetTracerProvider`, so that span is a no-op today.

**The control plane knows whether the credential can be saved and has decided
not to ask.** `ensureFreshOAuth` (`server/internal/resources/secrets/oauth.go`)
refreshes an access token inside the skew window, and on a failed refresh
serves the token on hand with the comment that "a 401 from Anthropic is the
authority on liveness". `oauthNeedsRefresh` returns false for a credential with
no refresh token for the same stated reason. Both defer to an authority that
has no way to answer.

**The window already has the shape for saying it.** The workspace draws an
attention band for a credential request waiting on a person and for work ready
to apply (`cli/internal/tui/banner.go`), and the harnesses screen already opens
a harness's configure flow as an overlay that returns to whatever was under it
when it ends.

## Decision

**A swapped credential the upstream rejected and the retry could not save is
reported to the control plane, recorded against the secret it came from, and
named in the window — with the remedy that fits it, which for a harness's own
credential is configuring that harness again.**

### 1. The proxy reports the rejection, not the 401

`retryRejectedSwap` already distinguishes the three outcomes, and all three are
reported:

- **rejected, not retried** — no credential different from the rejected one was
  available, so what the control plane would hand out now is what was refused;
- **rejected, retried, rejected again** — a freshly resolved or displaced value
  was also refused;
- **rejected, retried, accepted** — a clearance, which is how a rejection
  recorded earlier stops being true.

`secrets.Result` gains the sentinels it swapped so a report can name one.
Sentinels are non-secret by construction — the pool keeps them in a plaintext
file — and this is the one thing that makes a `401` attributable.

`secrets.Resolver` gains `Report` beside `Resolve`, as a required method rather
than an optional interface: every resolver the proxy is given must answer for a
rejection, and an implementation that silently did not would look identical to
a credential that never failed.

Reporting never touches the request path. It is queued and sent on a background
goroutine with a bounded queue that drops rather than blocks, the rule audit
already follows, and it is coalesced per `(client, sentinel, host)` behind a
cooldown, so a harness spinning on a dead credential sends one report a minute
rather than one per request.

### 2. The pool agent translates the sentinel it minted

The report goes out through the same seam and the same translation as a resolve
(`pool-agent/proxyagent/secrets.go`): an ephemeral sentinel is looked up in the
live activation table and reported as the stable sentinel the control plane
knows, plus the use ID it was minted for. The control plane still never learns
that ephemeral sentinels exist (ADR 0031 §3), and a wrapped command's rejection
is attributable to the use that authorized it (ADR 0079).

`POST /api/pools/{poolId}/sandbox-secret-rejections`, allowlisted in
`poolRuntimeActions`, authorized by the `secret:resolve` scope the resolve
context token already carries: the same principal, about the same credential,
naming nothing it was not already told.

### 3. The control plane decides whether the rejection is terminal

A report is a fact about an exchange; whether it means a human is needed is the
control plane's judgment, because only it can see the credential:

| The secret | The judgment |
| --- | --- |
| `token` | **Terminal.** There is nothing to refresh, and the upstream refused what there is. |
| `oauth`, no refresh material | **Terminal.** `oauthNeedsRefresh` already treats it as unrefreshable; the 401 it deferred to has now arrived. |
| `oauth` with refresh material | **Refresh first**, past `oauthRefreshSkew`, through the existing singleflight. Rotated to a new token → not terminal: the proxy invalidated its cache on the way here, so the next request carries the new value. The endpoint *refusing* to renew → terminal. |
| `oauth` refused again within the cooldown of a renewal | **Terminal.** The renewal was not the answer, whatever the token endpoint said. |
| Anything that is not an answer | **Nothing is recorded.** A token endpoint that is unreachable or answering 500s, a value that will not decrypt, a renewal that joined a non-forced one: none of those say anything about the credential, and recording one would ask a person to redo a sign-in they do not need — then suppress the renewal that would have worked. |

Only a 4xx from the token endpoint is a refusal. Telling that from a bad day at
the endpoint is the difference between this feature and the "mark it on the
first rejection" alternative rejected below.

The forced refresh is the part that earns its keep beyond the banner: an access
token killed mid-life — a sign-out elsewhere, a plan change, a rotation this
control plane did not perform — is refreshable, and today it costs a
reconfigure.

The renewal cooldown is what keeps that from running away, and it is
deliberately not the same length as anything else here. A lapsed subscription
can hold a perfectly good refresh token: every rejection would renew happily,
read as recovered, record nothing, and be refused again a minute later —
forever, spending a rotating refresh token each time, with nothing on screen to
show for it. So a renewal stands for a cooldown, and a credential refused again
inside it is one the renewal did not save. That memory is per-credential and
lives in the process rather than on the row, because the case it exists for is
the one where no row was written.

**The windows are ordered, not equal.** The proxy reports at one minute; a
standing rejection is re-judged no more often than five; a renewal stands for
ten. Two windows the same length as the interval being measured are decided by
jitter rather than by the rule they state, and the one guarding a rotating
refresh token is the one that must not be.

### 4. The record is a row, not a flag

Terminal rejections are stored as their own resource, one live row per
`(secret, host)`: the project, the secret, the host, the sandbox and use that
observed it, when it was first and last seen, how many times, and why it is
terminal (`not-refreshable`, `refresh-failed`, `rejected-after-refresh`).

The harness is *derived*, not stored: a secret the configure flow created is
named by its config's `ConfiguredSecretIDs`, and a harness-scoped grant names
one too. A denormalized flag on the harness config would have two writers and
would have to be cleared by paths that belong to the secret.

A row is cleared by the credential being replaced — the configure flow applying
its output, `secret update` — and by a clearance report. Both matter: the fix
is a reconfigure, but a user who fixes it another way must not be left with a
band telling them to fix it. Replacing a value is enforced in the store, at the
one point a new credential reaches a row, rather than in each service that
writes one.

**And a rejection expires.** A clearance can only come from the proxy process
that reported the rejection and still remembers doing so, so a pool agent
restart or a deleted discobox leaves nobody to send one. A rejection nothing has
re-reported for half an hour is therefore dropped: a credential still refused
and still in use re-reports every minute, so it never expires, and one that has
not been refused all that time is either fixed or unused. It comes back within a
minute of the next failure.

### 5. The window says which credential was refused, and offers the remedy that fits it

`GET /projects/{id}/secret-rejections` is polled on the tick beside the
credential inbox, and indexed by sandbox and by harness config the way pending
requests are indexed by sandbox.

The workspace gains a third band, and it outranks the two it has.

That is not where this ADR first put it. A credential request is a person
blocked on a keystroke right now, so it looked like the more urgent bar. In use
it is the other way round, because the two are usually one event: the agent
takes the `401`, concludes it needs a credential, and asks for one. The request
is then the symptom, and it is the half that cannot be usefully answered —
handing over another credential leaves the dead one bound to the harness and the
new one belonging to nobody. The refusal therefore stays up until it is dealt
with, and names how many requests are queued behind it. Nothing is made
unreachable: both bands' leader keys are bound whichever one is drawn, and
pending requests are still marked on the list and listed on the secrets screen.

**Every terminal rejection gets the band, not only a harness's.** The band's
first job is attribution, and that job is the same whichever credential it was:
from inside the box, a credential the upstream refused looks exactly like
Discobox failing to deliver one — the sentinel is there, the swap happened, the
request went out, and something on the other side said no. Saying *which*
credential was refused, and at which host, is what separates "the product is
broken" from "this token is dead", and that sentence is worth as much for a
`gh` token as for a subscription login.

The remedy is what varies, so the band dispatches on what the secret is:

- **A harness's own credential** opens that harness's configure flow as an
  overlay over the box, so ending the flow puts the user back on the box they
  were watching. Its words say the sign-in has to be done again out here; the
  one thing the band must not read as is an invitation to log in inside the
  sandbox, which cannot stick (ADR 0059).
- **Any other secret** opens that secret's edit dialog over the workspace, the
  same one the secrets screen opens, because replacing the stored value is the
  whole of the remedy.

## Alternatives rejected

- **Record it as a `SecretRequest`, in the inbox that already exists.** It is
  the closest existing shape and it is the wrong one: approving a request mints
  a grant, and a grant cannot fix a credential the upstream refused — the only
  action the inbox offers is the only action that does nothing. The reactive
  species also means something specific ("no grant covers this sentinel"), and
  overloading it with "a grant covers it and the credential is dead" would make
  the inbox's own remedy wrong for some of its rows.
- **Leave it in the proxy audit trail and read it from there.** The rejection is
  already audited, and since ADR 0130 the control plane can read that trail, so
  this looks free. It is not, for two reasons that survive the relay. The row
  names the use a *protocol* credential spent and nothing for an injected
  sentinel — ADR 0130 §3 declines to store sentinels, rightly, since one is a
  live credential for as long as it resolves — so the harness credentials this
  feature is mostly about are exactly the rows that cannot be attributed. And
  the trail is history, read by a fan-out that never waits on a pool: the right
  shape for an operator asking what happened, the wrong one for a window asking
  on every tick what is broken now, and no shape at all for the renewal that
  has to happen *before* anybody is told (§3). A report is the event arriving
  once, at the side that can act on it.
- **Read it from the spans.** `proxy.secret_swap.retry` already records
  attempted, credential source and retry status as one fact. Nothing installs a
  `TracerProvider` anywhere in this repository, so it is a no-op; and telemetry
  is for operators looking at a fleet, not a state the product's own UI depends
  on.
- **Mark the secret on the first rejection, without the forced refresh.** It is
  simpler and it produces a false alarm for the most recoverable case: a token
  that died before its stated expiry, whose refresh token is still good. That
  asks the user to redo a sign-in they do not need, which is the fastest way to
  teach them to ignore the band.
- **Have the harness tell us, or tell the harness.** There is no
  provider-agnostic way to mark a `401` as "not about you" in a response a
  harness will read, and the decision a harness has already made in memory is
  not reachable from outside it. ADR 0059 fixed the durable half of that
  reaction (the cleared file); this ADR covers the half that needs a person.
- **Let the user sign in inside the sandbox.** It cannot stick — the sandbox
  agent restores the delivered sentinel file within 30 seconds (ADR 0059) — and
  a real credential typed into a sandbox is what the sentinel design exists to
  prevent. This is why the band's words are part of the decision.
- **A flag on the harness config.** Denormalizing the derived answer puts a
  second writer on a fact the secret owns, and every clearing path — a value
  written, a rejection cleared — belongs to the secret rather than the config.
- **Band only the harness credentials, where the remedy is a flow we own.** It
  is the tidier feature and it withholds the more valuable half of the message.
  A rejected `gh` or npm token produces the same symptom as a rejected harness
  login — a sandbox that cannot reach something, with a sentinel sitting there
  looking correct — and the band's answer to "is this Discobox or is this my
  credential?" is worth saying whether or not the fix is a flow with a button
  on it.

## Consequences

- The resolver seam stops being read-only: `secrets.Resolver` grows a second
  method, and every implementation — the pool agent's and the tests' — carries
  it.
- A dead credential costs one control-plane call per minute per
  `(sandbox, sentinel, host)`, off the request path, and drops under queue
  pressure like an audit event.
- An OAuth access token killed before its expiry now recovers on its own, at the
  cost of one forced refresh per cooldown. A refresh token rotates on use, so
  that cooldown is what keeps the recovery from spending them.
- A rejection is per host: a credential that answers for one host and is refused
  at another says exactly that, and does not condemn the secret.
- Nothing new is persisted about the value. The report names a sentinel; the
  rejected credential is never sent, logged, or written to an audit row.
- An upstream that answers `401` for a reason that is not the credential will
  mark a live credential as rejected. It clears on the next accepted request
  through the same sentinel, expires on its own within half an hour of the last
  report, and the band is an offer rather than a block.
- Only a 2xx retracts a rejection. A redirect to a sign-in page is how many
  upstreams say a session is *not* good, and reading it as success would use
  that as the evidence the credential is alive.
- The workspace has three bands and can only draw one, so precedence is now a
  rule with a third case in it rather than a pair.
- The third band has two presses behind it — a configure flow and a secret edit
  — decided by what the rejected secret is. A rejection naming a secret the
  window cannot act on at all still draws, because saying which credential was
  refused is the point.
