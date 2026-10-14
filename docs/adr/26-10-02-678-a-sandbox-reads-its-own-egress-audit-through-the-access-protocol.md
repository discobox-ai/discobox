# 26-10-02-678 — A sandbox reads its own egress audit through the access protocol

- **Status**: Accepted
- **Date**: 2026-10-02
- **Relates to**: [ADR 0130](0130-an-audit-record-is-read-where-it-was-written-and-names-its-attestor.md),
  whose pool trail this makes readable from inside the sandbox it is about, by
  the route §4 there did not need: the pool proxy reading its own rows.
  [ADR 0031](0031-agent-credentials-are-a-portable-protocol-with-ephemeral-sentinels.md)
  owns the protocol this adds a verb to, and its §2 the identity the read is
  scoped by.

## Context

The pool proxy refuses requests for reasons an agent can act on: a host the
policy denies, a call to the discobox API the gate does not admit, a swap the
judge refuses (`judge: <reason>`), a host whose certificate is not trusted. It
says so in the response it sends back, and the proxy records it as an HTTP
audit row with `blocked` and `blockedReason` (ADR 0130 §1).

The response only helps when the program that made the request passes it on.
Many do not: an SDK that retries a 403 and then reports "request failed", a
build tool that prints "could not fetch dependency", a language server that
logs nothing. The agent sees the failure without the reason, and the reason
it would act on — ask for a credential, ask for a trust, narrow the command —
sits in a row it cannot read.

ADR 0130 made that row readable to people through `discobox admin audit http`,
which needs the control plane and a project member. From inside the sandbox
the same read is two hops of authorization away: a use of
`ai.discobox.sandbox` a person approved, judged per call, and the sandbox role
does not include the project audit routes at all.

The read needs no authority beyond the sandbox's own identity. Every row was
written by the pool about a request this sandbox sent, keyed by the client ID
its mTLS certificate carries; the URL is recorded before any sentinel is
swapped into it and the headers are redacted where they are written (ADR 0130
§3). Nothing in a row is something the sandbox did not already hold, except
the proxy's account of what it did with it — which is the point — and one
thing §3 takes out: an ephemeral sentinel the URL carried.

## Decision

### 1. The protocol gains a read of the caller's own egress

```
GET /v1/audit/http?host=&blocked=&since=&limit=
```

```json
{ "exchanges": [ { "id": "http_812", "createdAt": "…", "method": "POST",
  "url": "https://api.github.com/repos/o/r/pulls", "host": "api.github.com",
  "status": 403, "blocked": true, "blockedReason": "judge: …",
  "durationMs": 41, "useIds": ["use_7f3c…"] } ] }
```

Newest first; `limit` defaults to 20 and is capped at 200. `blocked=true`
keeps the refused, `false` the let through, absent both. `since` is RFC 3339.
`useIds` are the approved uses whose sentinels the request carried — spent, or
refused under when blocked — the join to `discobox-access
run --use`.

It is in the protocol, not beside it, because the protocol is already the
sandbox's one channel to an authority about what it may send, and an
implementation that serves `request` and `trust` has to answer "why was my
request refused" to make either usable: the refusal is what tells an agent
which of the two to ask for. An implementation with no egress record answers
an empty list.

### 2. The pool proxy serves it from its own rows, scoped by the certificate

The credentials endpoint (`pool-agent/proxyagent`, `:17083`) is the proxy
process. It reads the audit store in process, narrowed to the sandbox ID of
the verified client certificate — the same structural scoping every other
verb there has: the broker is built per request for one sandbox, and the
query has no field that names another.

Not through the control API on loopback. That route exists for the pool agent,
which is another process and needs a token; the proxy unit signing a token to
call itself would add a key read and a hop to restate a narrowing the broker
already has.

Not through the control plane. A read the sandbox's own identity fully
authorizes has nothing to gain from a party that would only check that
identity again, and it would cost the read the control plane's availability
and a person's approval of `ai.discobox.sandbox`.

### 3. The summary, not the record

The verb returns the fields that say what happened and why: no headers, no
bodies, no cache or rewrite detail. Headers are redacted where they are
written, but what a redaction rule misses in a header the upstream set — a
session cookie, a signed URL — the program kept from the agent on purpose or
by accident, and the agent needs none of it to learn why a request failed.
Bodies are unbounded and are the same question at a larger size.
`discobox admin audit get` remains the full record, for a person.

Because the URL is recorded as sent, it can carry an ephemeral sentinel a
command put in it, which the proxy does not swap in a URL. A live one is a
bearer token for its window, and the sandbox held it only inside the command
it was minted for; handing it back here would be the unjudged way to take a
value [ADR 0092](0092-the-cli-has-no-unjudged-way-to-take-a-value.md) rules
out. The broker redacts the sandbox's live sentinels from the URL and the
reason. A page also stops, newest first, before it outgrows what a client
decodes, since field bounds cannot promise that once JSON escapes them.

Adding a field later is cheap; taking one back from a protocol is not.

### 4. `discobox-access audit`

```
discobox-access audit [--host H] [--blocked|--allowed] [--since 10m] [--limit N] [--json]
```

The bundled skill points an agent at it when a request fails without a reason.

## Deferred

- **SOCKS connects and DNS queries.** Both are recorded and narrowed the same
  way, and a refused SOCKS host is the same question. Add them when an agent
  is seen failing on one; the HTTP trail is where every refusal the agent can
  act on is written today.
- **Following.** `--follow` reads forward by row id in `admin audit`; an agent
  asks after a failure, not during one. Add `after` when a skill needs to watch.

## Consequences

- `agentcreds.Service` gains a required method; the relay, the pool broker, and
  every fake implement it.
- An agent learns the proxy judge's reason for refusing a swap, which it could
  already read from the response when its program passed it on. The judge is a
  boundary on the request, not on the agent's knowledge of the decision.
- A sandbox can read its rows for the retention window (48h) after anything it
  started has exited, and only while it runs: the endpoint is reached over its
  own certificate.
