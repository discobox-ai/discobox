# 26-09-25-428 — The judge may let its allow stand for a route

- **Status**: Accepted
- **Date**: 2026-09-25
- **Supersedes**: [26-09-22-838](26-09-22-838-a-dedicated-pool-harness-judges-commands-and-credential-bearing-requests.md)
  §7's rule that only an endpoint rule may let an allow stand. The rest of §7,
  and every other section, is unchanged.

## Context

Every request that spends an approved use waits for a model (ADR 26-09-22-838
§4). That is seconds per request, which is fine for a single `gh pr create`.
It is not fine for a client that pages through a listing, polls a job, or
uploads a pull request's review comments one call at a time. Each of those
calls is the same operation on the same target, and the judge answers each one
the same way.

ADR 26-09-22-838 §7 foresaw this, and gave the power to let an allow stand to
endpoint rules: trusted code, written per API, that ships with the pool. None
has been written. There will never be one for every API a use can name, and the
judge is the only part of the system that reads every request. It already
knows, when it allows one, which neighbouring requests are the same operation.

## Decision

### 1. An allow may carry a standing route

An allow may also say which requests it covers and for how long:

```json
{"allow": true, "reason": "...",
 "standing": {"route": "GET /repos/org/repo/pulls/{number}/comments", "seconds": 600}}
```

`route` uses Go's `net/http` pattern syntax: exactly one method, a space, and
an absolute path. `{name}` matches one segment, and a final `{name...}`
matches the rest of the path. Nothing else is accepted: no host, no
method-less pattern, no implicit prefix match from a trailing slash, no `{$}`.
`Decode` refuses a malformed route the way it refuses any other malformed
verdict, and refuses a standing route on anything that is not an allow.

The route covers the method and the path. It does not cover the query or the
body, and the system prompt tells the judge so: a standing allow covers every
request matching the route, whatever it carries. A route whose operation lives
in the body, such as `POST /graphql`, is one the judge is told never to name.

### 2. Discobox, not the judge, decides whether it stands

The control plane keeps the route only when all of these hold:

- the answer decided on the first round, before any body was shown, because
  an allow that needed the body was about that body;
- the route names its target in at least one non-empty literal segment, because
  `POST /{rest...}` is the host rather than a target on it;
- it stands for a positive number of seconds;
- the route matches the request it was granted on, because a route that does
  not cover its own evidence was not derived from it.

The duration is capped at **15 minutes**, whatever the judge names. The origin
is never the judge's to choose: a standing allow covers only the scheme, host,
and port of the request it was granted on, and only for the same discobox and
use.

A route that fails these checks is dropped and the allow stays. The request
was allowed, and the only thing refused is the shortcut for the next one.

### 3. It is matched in the control plane, and liveness is still checked

A pool asks the control plane exactly as it does now. Before routing the ask to
the judge, the control plane reads the use from the live grant, which it
already does for every ask, and then looks for an unexpired standing allow for
the same discobox, use, and origin whose route matches. On a match it answers
allow without asking the model.

Revoking the grant, or letting the activation lapse, therefore ends every
standing allow on that use with the next request. A project whose judge is gone
or failed refuses as it does today, standing allow or not: an allow stands only
while the judge that could be asked instead is there. A request whose path has a
`.` or `..` segment, an empty one, or a segment that unescapes to a slash or a
backslash, matches no route. It goes to the judge, because the upstream may
resolve such a path to a target the route never named.

### 4. Every request covered by a standing allow still leaves a verdict

The standing allow is recorded on the verdict row that granted it: the route
and the time it expires. A request the standing allow covers writes its own
request verdict, with origin `judge`, that names the verdict that decided it.
Every request that carried a credential still has a verdict, and the audit
shows which ones no model read.

## Alternatives rejected

**Match in the pool.** It saves the hop to the control plane, which is
milliseconds next to the seconds a model takes. The pool cannot see a revoked
grant, so a standing allow would outlive a revocation unless revocations were
pushed to every pool. It would also split the verdict trail between the
control plane and each pool's audit.

**Keep standing allows for endpoint rules only.** That is ADR 26-09-22-838 §7
as written, and no rule has been written. It gives the benefit only to APIs
somebody wrote code for, and takes it from every other use.

**Cache the verdict by exact request.** Paging, polling, and per-item calls
differ in exactly the part an exact key includes, so almost nothing would hit
the cache.

**Let the route name a host, or any method.** The use already fixes the host,
and a route that could widen it would let the judge grant what the use never
approved. A method-less route would put a delete under an allow granted for a
read.

**Allow standing only on safe methods.** It is simpler, but repeated writes to
one target, such as review comments on one pull request, would each still wait
for the model. What makes a write dangerous to let stand is an operation that
lives in the body, and the judge is told to leave those out.

**Let the standing allow outlive the grant, or last longer.** A standing allow
is a model's judgment about requests it has not seen. Fifteen minutes covers a
burst of work, and every request still passes the deterministic checks.

## Consequences

- A burst of calls to one operation costs one model call instead of one per
  request. A covered request costs a store read, and a store write for its
  verdict.
- The model can now allow requests it never read. The route, the host, the
  use, the 15-minute cap, and the liveness check bound what it can grant, but a
  route drawn too wide is allowed until it expires. The audit names every
  request that was allowed that way.
- The judge's schema and system prompt change, so `PromptVersion` changes.
  Harness wrappers keep enforcing the schema they are given. A wrapper that
  cannot express the optional field only ever produces an allow without one.
- Endpoint rules keep the rest of §7: explaining an endpoint, refusing without
  the model, adding trusted facts, naming an upgrade. A rule that should always
  refuse a route still refuses before any standing allow is consulted, once
  rules exist.
