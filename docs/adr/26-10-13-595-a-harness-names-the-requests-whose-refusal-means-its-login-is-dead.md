# 26-10-13-595 — A harness names the requests whose refusal means its login is dead

- **Status**: Accepted (§2's "the retry itself is untouched" narrowed by [26-11-11-539](26-11-11-539-a-replaced-secret-value-reaches-the-proxy-cache-and-a-rejection-report-names-the-value-it-refused.md): a `403` inside a declared scope is retried)
- **Date**: 2026-10-13
- **Supersedes**, in part:
  [0132](0132-a-credential-rejected-after-its-retry-is-recorded-against-its-secret.md)
  §1's "all three are reported" for a harness credential whose declaration
  names a scope, and its consequence that "an upstream that answers `401` for a
  reason that is not the credential will mark a live credential as rejected"
  for the same. 0132's judgment (§3), record (§4) and band (§5) stand.

## Context

ADR 0132 reports every `401` on a swapped credential, per
`(client, sentinel, host)`, and the control plane turns the ones it cannot
renew away into the sign-in band.

A host is too coarse a unit for that. One credential is sent to many endpoints
on one host, and they do not all answer for the same thing:

```
POST 401 https://api.anthropic.com/v1/code/runners/self-hosted/spawn-hints/poll
POST 200 https://api.anthropic.com/v1/messages?beta=true
POST 200 https://api.anthropic.com/api/event_logging/v2/batch
```

The login is alive: inference works. The runner endpoint refuses it for a
reason of its own — what the login may do, not whether it exists — and it is
polled every few seconds. Under 0132 that is:

- a `rejected` report a minute, each forcing an OAuth renewal that rotates a
  refresh token to fix nothing;
- the second one inside `rejectionRefreshCooldown` recorded as
  `rejected-after-refresh`, which draws the band;
- an `accepted` report from the next `/v1/messages`, which clears it, and the
  poll setting it again — a band telling a signed-in person to sign in, flapping.

0132 named this ("an upstream that answers `401` for a reason that is not the
credential") and accepted it because the next accepted request clears it. That
holds for a stray `401`. It does not hold for an endpoint that refuses on every
call beside one that accepts on every call.

Nothing in the system knows which endpoint is the authority on a login. The
harness does: it is a fact about the CLI and its provider, the same kind of
fact as which variable the CLI reads the credential from.

## Decision

**A harness's secret declaration may name the requests that answer for the
credential, and only those requests report on it — a refusal and an acceptance
alike.**

### 1. The declaration is the harness's

A `secrets` entry in the image manifest (`harness.secrets[]`, beside
`required`, `oneOfGroup` and `delivery`) gains `rejectionScope`: a list of
`{host, paths}`.

```json
{
  "name": "CLAUDE_CODE_OAUTH_TOKEN",
  "required": true,
  "oneOfGroup": "auth",
  "delivery": "file",
  "rejectionScope": [{ "host": "api.anthropic.com", "paths": ["/v1/messages"] }]
}
```

- `host` matches the way `Secret.Host` does (`hostscope`): that host and the
  hosts beneath it. It is required in an entry.
- `paths` are prefixes on a segment boundary: `/v1/messages` covers
  `/v1/messages` and `/v1/messages/count_tokens`, never `/v1/messagesX`. The
  query is not part of the match. Empty means every path on the host, which is
  how a harness restricts by domain alone.
- No `rejectionScope` is every request, which is 0132 unchanged — every
  harness that names none, and every secret that is not a harness's.

It travels where the rest of the declaration does: `harness.Secret`,
`model.HarnessConfigSecret`, the API's harness-config secret. A declaration is
re-read from the image a config resolves to, so a shipped login gains a scope
when its harness image does, with no reconfigure and no migration.

### 2. The proxy is told the scope, and stays silent outside it

Resolving a sentinel returns the scope beside the value
(`secrets.ResolveResult`, the pool agent's resolve response,
`ResolveSandboxSecretResponse`), derived on the control plane from the
declaration the sandbox's harness config makes for the variable the sentinel is
bound to. A sentinel bound to no declared variable has none.

`retryRejectedSwap` and the accepted path consult it before
`credentialReporter`: a request outside a sentinel's scope reports nothing for
that sentinel, in either direction. The retry itself (ADR 0059) is untouched —
it is about a rotation the two ends have not converged on, which any endpoint
can expose.

Acceptance is scoped for the same reason refusal is. An endpoint that is not
the authority on the login being dead is not the authority on it being alive:
an event-logging `200` must not retract what `/v1/messages` said.

### 3. The control plane's half does not change

A report that arrives is judged, recorded, expired and drawn exactly as 0132
says. The row is still per `(secret, host)`; the scope decides which exchanges
are evidence, not what the evidence is recorded as.

## Alternatives rejected

- **Send the path up and filter on the control plane**, where the declaration
  already lives and where 0132 put every other judgment. The reporter coalesces
  per `(client, sentinel, host)` at one a minute, and that is what breaks: the
  runner poll takes the slot every minute and the `/v1/messages` refusal that
  matters is dropped behind it, indefinitely. Keying the cooldown by path fixes
  that and unbounds the key — paths carry IDs — on a queue that holds 64 and
  drops. The scope is not what 0132 withheld from the proxy either: that was
  what kind of credential it is and whether it renews. Which URLs speak for it
  is neither, and is no secret.
- **A scope on the secret**, set by the configure command's output or by a
  person. It is the same list one hop later and it is frozen at the last
  sign-in: every login already stored would keep drawing the band until
  somebody reconfigured, which is the remedy the false band was already
  wrongly asking for. Any secret, not only a harness's, can be given one later
  if a case for it appears; this decision does not need it.
- **Bind the secret to a host** (`Secret.Host`). That restricts where the
  credential may be *used*, and a path cannot be said in it at all. The runner
  endpoint is a legitimate use that happens to be refused.
- **Report only what the endpoint says is an authentication failure**, by
  reading the `401`'s body. Every provider words it differently, the proxy does
  not read bodies it relays, and Anthropic's own runner refusal is a `401`
  whose wording is not a contract.
- **Require a `rejectionScope` of every harness.** Most harnesses talk to one
  API with one answer, and for them host-wide is right. A required field would
  be filled in with "everything", which says nothing and has to be maintained.
- **Scope only the refusal.** Then any `2xx` on the host clears a real
  rejection within a minute of the band going up, which is the flapping with
  the roles reversed.

## Consequences

- A harness that names a scope is not told its login is dead by an endpoint
  outside it, and is not told it is alive by one either. A provider that moves
  its inference endpoint needs a harness image that says so; until then a dead
  login there draws no band, which is the failure 0132 started from.
- An out-of-scope `401` no longer forces a renewal, so it no longer spends a
  refresh token.
- The resolve contract carries a second non-secret fact beside `secretId`. A
  control plane that predates it sends none, and a proxy that predates it
  ignores it; both are 0132's behavior.
- A scope changes with the harness image, and reaches a running proxy at its
  next resolve of the sentinel (`RefreshIntervalSeconds`).
- Claude Code's two credentials declare `api.anthropic.com` `/v1/messages`.
  The other built-in harnesses declare nothing until one shows the same split.
