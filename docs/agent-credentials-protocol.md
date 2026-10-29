# Agent Credentials Protocol v1

A small HTTP protocol an agent inside a sandbox uses to **ask a human** for a
credential it was not provisioned with, and to learn when it has it. Two
operations: `list` and `request` (with its poll), and
[environment](#environment--what-is-in-my-variables-now), which a process
started before a binding reads to take it up. A second verb,
[trust](#trust--ask-for-a-host-to-be-trusted), asks for a host whose
certificate the sandbox's egress refuses to be trusted for it. A third,
[audit](#audit--what-became-of-my-requests), reads back what the egress did
with the sandbox's own requests.

The protocol knows nothing about Discobox and never carries a value. It is the
contract between the in-sandbox CLI (`discobox-access`) and whatever serves it,
so the same CLI works against Discobox's sandbox-agent and against any other
implementation.

Decision records:
[ADR 0031](adr/0031-agent-credentials-are-a-portable-protocol-with-ephemeral-sentinels.md),
and [ADR 26-10-27-880](adr/26-10-27-880-access-to-a-secret-is-a-binding-held-to-static-routes-and-no-judge-decides-it.md),
which removed the judge, policies, uses and the `get` operation.

## Transport

- HTTP/1.1, JSON request and response bodies, all routes under `/v1`.
- The client is configured with a **base URL** (`DISCOBOX_CREDENTIALS_URL`,
  defaulting to `http://127.0.0.1:17010`) and an optional **bearer token**
  (`DISCOBOX_CREDENTIALS_TOKEN`). When a token is configured it is sent as
  `Authorization: Bearer <token>` on every request. Implementations reachable
  only over a trusted local transport — Discobox serves the protocol on sandbox
  loopback — may require no token. Discobox sets `DISCOBOX_CREDENTIALS_URL` to
  that default in every exec's environment unless the exec already names one,
  and sets no token.

### Errors

A failure is the HTTP status plus a body carrying a **stable code** and a
message:

```json
{ "code": "denied", "error": "this request is not this sandbox's own" }
```

| Code | Status | What the caller should do |
| --- | --- | --- |
| `invalid` | 400 | The call was malformed. Fix it and retry. |
| `denied` | 403 | You may not do this. Ask for it with `request`. |
| `not_found` | 404 | The id means nothing to this server. |
| `unavailable` | 5xx | The server could not answer. Retry; the same call may succeed. |

The body's `code` wins over the status line. A client reading an error with no
`code` classifies it by status: 400 or 409 is `invalid`, 401 or 403 `denied`,
404 `not_found`, anything else `unavailable`.

The code exists so a caller — an agent, above all — can branch on a token
rather than on the wording of a sentence. The set is deliberately coarse: an id
that is unknown, revoked, or expired all report `denied`, because saying which
would tell an untrusted caller more about the state of an approval than it needs
to know, and the remedy is the same for all three.

The message never repeats the code. They are two halves of one answer.

## A granted credential arrives in its variable

The protocol hands out nothing. An implementation delivers a granted
credential itself, in the variable the request named. Discobox puts a
*sentinel* there — a byte-shaped lookalike of the credential — in the
environment of every command started from then on; its egress proxy swaps it
for the real value on the routes its secret and its binding allow, and
refuses a request on any other. A process already running when the binding
lands does not see it: it reads what is delivered now with `environment`.

## `environment` — what is in my variables now?

```
GET /v1/credentials/environment
200 {"variables": {"GH_TOKEN": "ghp_…"}}
```

Every variable a credential is delivered in, and what is delivered there now.
It is for a process that started before a binding — a harness's tool shell
inherits the environment the harness was launched with, never a later one —
to take the credential up: `eval "$(discobox-access env)"`. It never carries a
value; in Discobox each one is the credential's sentinel, which only that
sandbox's egress exchanges.

## `list` — what am I allowed to use?

```
GET /v1/credentials
```

```json
{
  "credentials": [
    {
      "name": "github",
      "envVar": "GITHUB_TOKEN",
      "hosts": ["api.github.com"],
      "purpose": "use",
      "expiresAt": "2026-08-12T18:00:00Z"
    }
  ]
}
```

`list` never returns values. `envVar` is where the credential is delivered,
`hosts` the hosts of the routes it may be sent on, and `expiresAt` when the approval lapses; absent,
it does not expire on its own. A credential with `purpose` `delegate` is one
the caller may give the sandboxes it creates, and is not delivered to the
caller itself, so it has no `envVar`. `raw` (`env` or `file`) says a person
handed over the value itself in `envVar`, or the path of a file holding it,
rather than a placeholder; no request asks for that, it is the approver's choice
([ADR 26-10-27-840](adr/26-10-27-840-a-binding-may-hand-over-the-raw-value-held-in-memory-and-watched-for-at-the-proxy.md)).

## `request` — ask for something new

```
POST /v1/credentials/requests
```

```json
{
  "name": "github",
  "envVar": "GITHUB_TOKEN",
  "routes": [
    "GET api.github.com/repos/discobox-ai/discobox/**",
    "api.github.com/graphql graphql=query"
  ],
  "justification": "The task asks me to read the PR's checks.",
  "grantTTLSeconds": 14400
}
```

```json
{ "requestId": "req_1a2b…", "status": "pending", "purpose": "use" }
```

`name` is the credential asked for and `envVar` the variable it is wanted in.
`hosts` and `routes` are where it will be sent; an ask names at least one of
either, and may name both. A host is shorthand for a route to that exact host
and no host beneath it — `*.github.com` asks for the names beneath
`github.com`, and `*` alone for any host
([ADR 26-10-27-939](adr/26-10-27-939-a-route-may-name-any-host.md)). Several
destinations are for a credential one tool sends to more than one site —
Copilot CLI sends one GitHub token to `api.github.com` and to
`githubcopilot.com`
([ADR 26-10-02-393](adr/26-10-02-393-a-credential-request-and-its-grant-may-name-several-hosts.md)).

A route narrows a destination to what the agent will do there, in one typed
string
([ADR 26-10-27-880](adr/26-10-27-880-access-to-a-secret-is-a-binding-held-to-static-routes-and-no-judge-decides-it.md) §4):

```
[METHODS ]HOSTS[PATHS][ graphql=OPS[:FIELDS]][ mcp=TOOLS][=blocked]
```

- `METHODS`, optional: upper-case methods joined by commas (`GET`,
  `GET,HEAD`). Absent is any method.
- `HOSTS`: comma-separated host patterns as above, up to the first `/`.
- `PATHS`, optional: from the first `/`; several are joined by `,` followed
  by `/` (`api.github.com/a/**,/b`). Each is matched against the whole
  request path, segment by segment: `*` is exactly one segment, `**` (only as
  the last) zero or more, anything else literal, compared ignoring case
  (`/repos/acme/**` also allows `/Repos/ACME/...`). Absent is any path. A
  `:port` in a host is dropped, so a host is every port of it.
- `graphql=OPS[:FIELDS]`: the GraphQL operation types allowed (`query`,
  `mutation`, `subscription`, comma-separated), and optionally the root fields
  every operation may select, each optionally ending in `*`. The document is
  parsed; `operationName` is never read.
- `mcp=TOOLS`: the MCP tools a `tools/call` may name, each optionally ending
  in `*`; the protocol's own methods (`initialize`, `ping`, `tools/list`,
  notifications) pass.
- `=blocked` at the end marks a carve-out: the route refuses what it is the
  most specific route for.

`api.github.com`, `*`, `GET api.github.com/repos/*/*/pulls/**`,
`api.github.com/graphql graphql=mutation:createPullRequest` and
`api.github.com/repos/*/*/pulls/*/merge=blocked` are all routes. In Discobox
each host and route becomes a route of the binding asked for, which the
approver may edit; a route that does not parse makes the ask `invalid`.

`name` and a destination are required, and `envVar` to use the
credential — an ask to delegate names none; an ask without them is `invalid`. A body that still
names a destination as `host`, as a client that predates `hosts` sends, is
`invalid` rather than read without it; an empty `host` is no destination.
`justification` is shown to the person answering; nothing evaluates it.

`grantTTLSeconds` is optional: how long the agent asks the approval to last. It
is a suggestion, not a term — Discobox shows it to the approver as the answer
already chosen, and they may choose any other. Absent or `0` asks for nothing in
particular, so forever cannot be asked for. An ask runs from 1 second to
**2592000 (thirty days)**; outside that range it is `invalid`. The ceiling is
part of the contract, because an ask is only worth anything if it can be shown
to a human as an answer: a ten-year ask is one keystroke from a credential
that outlives the work.

`purpose` is optional: `use`, the default, asks to use the credential;
`delegate` asks to give it to the sandboxes the caller creates. One or the
other, never both. Only a person answers an ask to delegate. Any other value is
`invalid`. The answer, and every poll of it, carries the `purpose` the
implementation recorded.

A second ask for the same `envVar`, hosts and routes (each in any order) and `purpose` while
one is still pending returns that pending request rather than a new one.

Approval is human-latency, so `request` is **asynchronous**: it answers
`202 Accepted` immediately and the client polls.

```
GET /v1/credentials/requests/{requestId}
```

```json
{ "requestId": "req_1a2b…", "status": "granted", "purpose": "use",
  "credential": { "name": "github", "envVar": "GITHUB_TOKEN", "hosts": ["api.github.com"], "expiresAt": "2026-08-12T18:00:00Z" } }
```

`status` is one of `pending`, `granted`, `denied`. `credential` is present once
granted, as `list` reports it — the hosts and lifetime the approver chose,
which need not be the ones asked for, and `raw` when the approver chose to
hand over the value itself rather than a placeholder. In Discobox a request may also be
answered by the sandbox that created the caller, when it holds a delegate
binding of that credential; the protocol is the same either way. Discobox
reports an approval whose binding has since been removed as `denied`, and a
request id that is not the calling sandbox's own as `not_found`.

Blocking is the client's job (`--wait` on the CLI), built on this poll. There is
no long-poll and no synchronous request primitive.

## `trust` — ask for a host to be trusted

An implementation's egress verifies every host it connects to. A host whose
certificate chains to a private CA — a Kubernetes API server, an internal
service — is refused, and there is no credential that fixes that. The trust
verb asks a human to pin one certificate from the chain the implementation
observed, for the caller's sandbox alone. Decision record:
[ADR 0149](adr/0149-a-host-certificate-is-trusted-for-one-sandbox-when-a-person-pins-it.md).

```
POST /v1/trusts/requests
```

```json
{
  "host": "34.70.64.109:443",
  "justification": "The user's GKE cluster; its API server uses the cluster's own CA.",
  "suppliedCA": "-----BEGIN CERTIFICATE-----…",
  "grantTTLSeconds": 14400
}
```

`host` is `host:port`, `:443` when no port is named; a trust covers exactly that
endpoint and nothing beneath it. A trust says which certificate to accept there
and nothing about what may be sent: a credential sent to a trusted host is held
to its own binding and routes like any other
([ADR 26-10-27-880](adr/26-10-27-880-access-to-a-secret-is-a-binding-held-to-static-routes-and-no-judge-decides-it.md) §10).
`justification` is shown to the person answering. `suppliedCA` is
optional: a CA the caller already has from a source it trusts. It is refused as
`invalid` unless the chain the implementation observes verifies against it.
`grantTTLSeconds` is read as a credential request's is.

The implementation connects to the host itself and answers with what it was
shown. A chain that already verifies needs no one, and settles at once:

```json
{ "requestId": "", "status": "unneeded", "host": "34.70.64.109:443", "reason": "…", "observedChain": [ … ] }
```

Otherwise the ask is recorded — `202`, `pending` — and polled as a credential
request is:

```
GET /v1/trusts/requests/{requestId}
```

```json
{ "requestId": "treq_…", "status": "granted", "host": "34.70.64.109:443",
  "pin": { "kind": "ca", "sha256": "…" } }
```

`status` is `pending`, `granted`, `denied`, or `unneeded`. Each certificate in
`observedChain` carries `subject`, `issuer`, `dnsNames`, `ips`, `notBefore`,
`notAfter`, `sha256` (of the DER, what a `ca` pin names), `spkiSha256` (of the
public key, what a `leaf-spki` pin names), `isCA`, `selfSigned`, and `pem`.

```
GET /v1/trusts
```

```json
{ "trusts": [ { "host": "34.70.64.109:443", "pin": { "kind": "ca", "sha256": "…" },
                "expiresAt": "2026-09-25T00:00:00Z" } ] }
```

A trust hands nothing to a process. It changes what the
implementation's egress accepts, for every process in the sandbox.
A client that carries its own CA rather than the sandbox's trust store still
has to be pointed at whatever the egress presents; in Discobox that is the
MITM CA at `/etc/discobox/proxy/mitm-ca.crt`.

In Discobox the pool's proxy unit probes the host through the proxy's own
upstream path, so the chain a person is shown is the one the proxy will meet,
and a poll that reads `granted` has already put the pin in the proxy. A host
the proxy refuses answers `502` with `X-Discobox-Untrusted-Host: <host:port>`,
which is how an agent learns to ask.

## `audit` — what became of my requests

An implementation's egress refuses some requests — a host it does not trust,
a credential sent somewhere its routes do not allow — and says why in the
response. A
program that does not pass that on leaves the agent with a failure and no
reason. The audit verb reads the reason back. Decision record:
[ADR 26-10-02-678](adr/26-10-02-678-a-sandbox-reads-its-own-egress-audit-through-the-access-protocol.md).

```
GET /v1/audit/http?host=api.github.com&blocked=true&since=2026-10-02T12:00:00Z&limit=20
```

```json
{ "exchanges": [ { "id": "http_812", "createdAt": "2026-10-02T12:00:01Z",
  "method": "POST", "url": "https://api.github.com/repos/o/r/pulls",
  "host": "api.github.com", "status": 403, "blocked": true,
  "blockedReason": "route: …", "durationMs": 41 } ] }
```

Newest first. Every parameter is optional: `host` keeps one destination (a
host named without a port at any port, so `api.github.com` also finds the
refused `CONNECT` to `api.github.com:443`),
`blocked` (`true`/`false`) the refused or the let-through, `since` (RFC 3339)
a window, and `limit` is 20 when absent and at most 200 — more is read as 200.
An implementation may return fewer than the limit, the newest, to keep the
response within 1 MiB; a narrower read reaches the rest.
A value that does not parse is `invalid`. `blocked` means the implementation
refused the request rather than the destination, and `blockedReason` says why.
A refusal by the
destination itself, a 401 for a missing or wrong credential, is an ordinary
exchange with that status, not a blocked one. An exchange is a summary: no
headers and no bodies, and a value swapped into a URL is recorded as the
placeholder the caller sent. A service that predates the verb answers 404, which a client reads
as the verb missing, not as an empty record.

The read is the caller's own: the transport names whose exchanges these are,
and no parameter can name another caller. An implementation that keeps no
egress record answers an empty list.

In Discobox the pool's proxy unit answers it from its own audit trail, keyed
by the sandbox ID the client certificate carries, for the trail's retention
window.

## The client shape that fits it best

The reference client is `discobox-access`. Its primary consumer is an LLM
agent. **Asking takes JSON**, because the ask carries free text through a shell
that reads quotes and apostrophes as syntax:

```
discobox-access request --json <<'JSON'
{
  "name": "github",
  "envVar": "GITHUB_TOKEN",
  "routes": ["GET api.github.com/repos/discobox-ai/discobox/**"],
  "justification": "the user's task asks me to read the repository's issues",
  "wait": true
}
JSON
```

`--json` means "talk to me in JSON" in whichever direction a command has:
structured output everywhere, and a structured body on stdin for `request`.
Results go to stdout and failures to stderr, always.

## Implementing the server side

An implementation owns the decisions the protocol does not make (and, for
the trust verb, what its egress verifies and how a pin reaches it):

1. **Who may call it.** The protocol carries no identity beyond the optional
   bearer token; the transport decides whose credentials these are.
2. **How a request reaches a human.** The protocol only says a request has an
   id and a status that eventually settles.
3. **How a granted credential is delivered.** The protocol never carries one.

In Discobox: sandbox-agent serves the protocol on sandbox loopback
(`127.0.0.1:17010`) and relays each call to the pool over the sandbox's mTLS
client certificate, whose common name is the sandbox ID and is the identity.
The pool's proxy unit (`pool-agent/proxyagent`, on `:17083`) answers it by
calling the control plane, where bindings and cleartext live; an approved
request becomes a binding on the asking sandbox, and a sentinel in the
variable asked for. See
[`pool-agent/DESIGN.md`](../pool-agent/DESIGN.md) and
[`sandbox-agent/DESIGN.md`](../sandbox-agent/DESIGN.md).
