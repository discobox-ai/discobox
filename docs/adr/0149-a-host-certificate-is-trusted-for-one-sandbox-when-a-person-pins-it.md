# 0149 — A host's certificate is trusted for one sandbox when a person pins it

- **Status**: Accepted
- **Date**: 2026-09-24
- **Relates to**: [ADR 0031](0031-agent-credentials-are-a-portable-protocol-with-ephemeral-sentinels.md),
  the protocol this adds a verb to;
  [ADR 0131](0131-the-launcher-answers-every-servers-credential-requests-and-names-the-server-its-config-screens-edit.md),
  the inbox this widens;
  [ADR 0140](0140-a-discobox-reaches-the-discobox-api-through-its-pool-with-a-fixed-role.md),
  whose judge this extends past swapped credentials.

## Context

The pool proxy MITMs every allowed `CONNECT`, then dials the origin itself.
That upstream TLS connection is verified against the pool host's system roots
(`proxy/upstream.go`: a bare `http.Transport`), and nothing else. A host whose
certificate chains to a private CA is therefore unreachable from every sandbox,
and the sandbox is told nothing useful: the proxy drops the connection and
`curl` reports `Empty reply from server`.

This is the ordinary case for Kubernetes. Found on 2026-09-23/24:

- **GKE.** A cluster's public IP endpoint (`https://34.70.64.109`) serves a
  certificate from the cluster's own CA (`masterAuth.clusterCaCertificate`).
  Through the proxy it is an empty reply before any credential is involved.
  The cluster's DNS endpoint (`*.gke.goog`) has a public certificate and
  verifies, but is off by default (`allowExternalTraffic: false`). Auth is a
  Google OAuth bearer token, which sentinel swapping already handles — the CA
  is the only thing missing.
- **A desktop cluster** at `192.168.1.161:6445`: the same failure, with a
  client-certificate kubeconfig on top.

The same holds for any self-signed or internal-CA service a user asks an agent
to reach. Trusting a CA for a host is a security decision, not a convenience: a
pin for a host the project sends a credential to decides who that credential
is handed to. So it is a person's decision, made per host, as credentials are.

What *can* be decided without a person is where trust applies. The sandbox
only ever talks to the proxy, whose MITM CA its trust store already holds. The
proxy's upstream verification is the one that fails, and the proxy serves many
sandboxes.

A client that carries its own CA is the exception on the sandbox side, and
kubectl is one: a kubeconfig names the cluster's CA and trusts nothing else, so
it refuses the proxy's certificate whatever the proxy does upstream. Such a
client is pointed at the sandbox's MITM CA
(`kubectl config set-cluster … --certificate-authority=/etc/discobox/proxy/mitm-ca.crt`).
That moves the cluster's verification to the proxy, against the pin; it
removes none of it, and the agent is told so, and told never to skip
verification instead.

## Decision

**An agent asks for a host to be trusted with `discobox-access trust`. The
pool proxy connects to the host and records the chain it is shown; a person
sees that chain and the uses the agent gave, and approves a pin. The pin is
held by that one sandbox, for that exact `host:port`, and is enforced by the
proxy on that sandbox's traffic alone. Every request the sandbox then sends
to the host is judged against the uses the trust was approved for. Trust
requests are their own resource; the window reads them in the same poll as
credential requests.**

### 1. The protocol gains a `trust` verb

Beside `/v1/credentials`, same transport, error codes, and TTL rules
([protocol doc](../agent-credentials-protocol.md)):

```
POST /v1/trusts/requests
{ "host": "34.70.64.109:443",
  "justification": "GKE cluster shared-env-obot; its API server uses a cluster-private CA",
  "uses": [{ "description": "Read-only kubectl: get/list/describe pods, deployments, events" }],
  "suppliedCA": "-----BEGIN CERTIFICATE-----…",      // optional
  "grantTTLSeconds": 14400 }                         // optional, 1..2592000

→ 202 { "requestId": "treq_…", "status": "pending", "observedChain": [ … ] }
→ 200 { "status": "unneeded", "reason": "the certificate … already verifies", "observedChain": [ … ] }

GET /v1/trusts/requests/{id}  → { status: pending|granted|denied|unneeded, pin?, uses? }
GET /v1/trusts                → { trusts: [ { host, pin, uses: [{useId, description, expiresAt}] } ] }
```

There is no `use` route. A trust hands nothing to a process; it changes what
the proxy does for the sandbox as a whole. `discobox-access trust <host>`
and `discobox-access trusts` are the CLI.

`host` is always `host:port`, `:443` filled in when absent. A trust covers
exactly that endpoint — unlike a secret's host scope (`hostscope`), never the
names beneath it. A pin is about one server's certificate, not a site.

### 2. The pool proxy fetches the chain; the sandbox's word is not used

The sandbox-agent relays the ask to the pool as it does a credential ask. The
pool agent asks its proxy to connect to the host — through the proxy's own
dial path, so an upstream proxy and `NO_PROXY` apply exactly as they will to
the traffic — completes a TLS handshake with verification off, and attaches
the chain it was shown to the request it opens on the control plane. The chain
the approver sees is the one the proxy will meet.

A chain that already verifies against the system roots for that name is
answered `unneeded` at once, and no request is opened: the person is never
asked, and a pin can never replace public-CA verification for a host that has
it.

A pool whose egress goes through an upstream proxy — a pool running inside a
discobox — sees what that proxy presents, which for a discobox's proxy is its
own certificate, and that verifies. `unneeded` is then this proxy's honest
answer, and says which upstream it went through: the host's real chain is seen,
and the host trusted, only at the outermost proxy.

### 3. The approver pins one certificate from what was observed

| Observed | Pin | Verified by |
| --- | --- | --- |
| The chain includes a self-signed CA | that CA (`ca`, SHA-256 of the DER) | chain to the pinned CA, and hostname/IP SAN as usual |
| Only a leaf | the leaf's public key (`leaf-spki`) | exact SPKI match |
| `suppliedCA` given, and the observed chain verifies against it | the supplied CA (`ca`) | as the first row |

A `suppliedCA` that the observed chain does not verify against is refused at
request time: the approver is only ever asked about what the proxy actually
met. GKE is the case `suppliedCA` exists for — the agent reads the CA from
`gcloud container clusters describe`, over a connection that already verifies,
and the proxy's observation confirms it.

The approval dialog shows the host, the uses and justification, each observed
certificate (subject, issuer, SANs, validity, fingerprint), which one will be
pinned, and — when any of the project's secrets is bound to a scope covering
the host — that those credentials will be sent to the pinned server.

### 4. Trust is scoped to one sandbox, in the resource's shape

```
GET    /projects/{p}/trust-requests[?status=pending]
GET    /projects/{p}/trust-requests/{id}
POST   /projects/{p}/trust-requests/{id}/approve   { pin?: {kind, sha256}, uses?, grantTTLSeconds? }
POST   /projects/{p}/trust-requests/{id}/deny
GET    /projects/{p}/sandboxes/{s}/host-trusts
DELETE /projects/{p}/sandboxes/{s}/host-trusts/{trustId}
```

```yaml
HostTrustRequest: id, projectId, sandboxId, host, justification, uses[],
                  grantTTLSeconds?, observedChain[], suppliedCA?,
                  status: pending|approved|denied, trustId?
HostTrust:        id, projectId, sandboxId, host, pin: {kind: ca|leaf-spki, sha256, pem},
                  uses[]: {useId, description}, expiresAt, grantedBy
```

A host trust lives under its sandbox, so no other scope can be expressed, and
it goes when the sandbox does. It expires as a grant does: the approver picks
the lifetime, the agent's ask is the default, never forever, and a
`leaf-spki` pin also ends with the leaf's `notAfter`. A leaf rotated to a new
key fails the pin, and the agent asks again (§6 tells it to).

The proxy unit reads its pool's live trusts from the control plane
(`GET /api/pools/{poolId}/sandbox-host-trusts`) — at start, every 30 seconds,
and, before it answers, on an agent's poll that reads `granted` — and applies
them through `ApplyConfig` as client-scoped rules, the tenant boundary every
proxy rule already uses. A sentinel set reaches a pool with its sandbox's
spec; a trust is approved while the sandbox runs, and the poll is what lets
the agent's very next request find the pin in force. An `unneeded` ask opens
no request, so the stored statuses are a secret request's: pending, approved,
denied.

Only a person approves a pin. A discobox answering the inbox
([ADR 0140](0140-a-discobox-reaches-the-discobox-api-through-its-pool-with-a-fixed-role.md))
is refused: a pin decides who the sandbox's credentials for that host are
handed to.

### 5. Every request to a trusted host is judged

A pin only makes a host reachable; whether the sandbox can authenticate there
is still a credential grant. Reachability is what the agent asked for, so its
uses govern the traffic: the proxy hands every request to a pinned host to
`Resolver.Judge` with the trust's uses, whether or not it carries a credential.
A request that also carries a swapped credential is judged against both sets
of uses. A request the judge refuses is a 403 from the proxy, audited once as
blocked, as today.

Upgraded streams (`kubectl exec`, `port-forward`) are judged once, at the
upgrade request. What the judge caches and how it keeps chatty clients
affordable (`kubectl` discovery calls, `watch`) is the judge's design, not
this ADR's.

### 6. An untrusted upstream is a 502 that names the remedy

When upstream verification fails and the sandbox holds no pin for the host,
the proxy answers `502` with `X-Discobox-Untrusted-Host: <host:port>` and a
body telling the agent to ask with `discobox-access trust <host:port>`, audited
as `upstream certificate not trusted`. A pin that no longer matches is the same
answer. The empty reply is gone: the failure an agent meets is the one that
names the fix, as a `401` names `request`.

### 7. The inbox is one read across both kinds

```
GET /projects/{p}/approval-requests?status=pending
→ { "requests": [ { "kind": "credential", "credential": { …SecretRequest… } },
                  { "kind": "trust",      "trust":      { …HostTrustRequest… } } ] }
```

A read model over the two resources, and nothing else: answering goes to each
resource's own `approve`/`deny`. The window polls it in place of
`secret-requests?status=pending`, on the same terms as today
([ADR 0131](0131-the-launcher-answers-every-servers-credential-requests-and-names-the-server-its-config-screens-edit.md)
§1), so each tick still makes one inbox call per server. The TUI's inbox item
becomes a sum of the two kinds; the `!` mark and the banner do not change.
`discobox secret request ls` keeps reading `secret-requests`;
`discobox trust request ls` reads `trust-requests`.

## Alternatives rejected

- **A trust as a kind of `SecretRequest` and `SecretGrant`.** One inbox and
  one poll for free, but both types would carry two shapes with half their
  fields meaningless for each (`secretId`, `envVar`, `purpose`, `secretType`,
  `wellKnownId` against `observedChain` and `pin`), `secret-grants` would list
  grants of no secret, and every credential path — environment building,
  rejections, the proxy resolver — would branch on `kind` to ignore trusts.
  §7 gets the single poll without it.
- **Trust requests as their own resource, polled separately.** One more call
  per server on every five-second tick, next to `secret-rejections`, for a
  list that is empty nearly always. §7's read model costs one route.
- **Project scope, or an approver option to widen to it.** Sandbox scope is
  what a person reasons about when approving one agent's ask, and a project
  pin would outlive the sandbox that asked. Putting trusts under the sandbox
  in the URL makes that a property of the resource rather than a check.
- **Trusting a chain the sandbox reports.** The sandbox cannot reach the host
  and could say anything. Only the proxy's observation is shown.
- **Per-host passthrough, or verification off per host.** Passthrough loses
  audit and the judge, the reason the proxy MITMs at all; verification off
  lets anyone on the pool's path answer for the host, credentials included.
- **Adding the CA inside the sandbox.** The sandbox's trust store is not the
  one that fails. It trusts the proxy already; the proxy's upstream check is
  the one this is about.

## Deferred

- **Client certificates.** A `tls-client` secret (cert, key, CA) and a
  sentinel certificate the proxy requests on the sandbox leg and exchanges for
  the real one upstream, so a client-certificate kubeconfig works without the
  key entering the sandbox. Revisit when a user needs an endpoint that accepts
  nothing else — the desktop cluster above is the first. It builds on this
  ADR's pin as the upstream trust.
- **A kubeconfig as a credential shape** that splits into host, pin, and
  token or client-certificate secret. Revisit with client certificates.
- **A project event stream for the inbox.** Revisit if the inbox grows more
  kinds or its poll shows up in a server's load.
- **TLS that is not HTTPS from its first byte** (Postgres, MySQL `STARTTLS`
  over SOCKS). The proxy does not terminate it, so there is nothing to pin.

## Consequences

- **Protocol** (`agentcreds`, `access`, `docs/agent-credentials-protocol.md`):
  the `/v1/trusts` routes and `discobox-access trust`/`trusts`. The protocol
  stays Discobox-free; `observedChain` and `unneeded` are part of it.
- **Sandbox agent** (`credentials`): relays the new routes over the mTLS hop.
- **Pool agent and proxy**: a control route that probes a host through the
  proxy's dial path and returns the chain; `HostTrust` rules in `Config`
  applied per client; upstream verification that consults the requesting
  client's pins. The proxy's upstream `http.Transport` is shared across
  clients and pools connections by host, so a connection verified under one
  sandbox's pin must never be reused for another sandbox's request to that
  host — pinned connections are pooled per `(client, trust)`, not shared.
- **Proxy** `Judge`: called for every request to a pinned host with the
  trust's uses, not only for swapped requests. The 502 in §6.
- **Server**: `trust-requests`, `host-trusts` under the sandbox,
  `approval-requests`, their store tables and a migration; the pool's read of
  its live trusts; deleted with their sandbox.
- **CLI/TUI**: the inbox reads `approval-requests`; an approval dialog for a
  trust that shows the chain and the pin choice; `discobox trust request ls`
  and `discobox trust ls`.
