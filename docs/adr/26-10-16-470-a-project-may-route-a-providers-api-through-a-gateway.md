# 26-10-16-470 — A project may route a provider's API through a gateway

- **Status**: Accepted (§§1, 4 and 6 narrowed by [26-10-17-271](26-10-17-271-the-server-configuration-file-names-project-gateways.md):
  `valueFrom` is the server configuration file's alone)
- **Date**: 2026-10-16
- **Relates to**: [26-10-01-032](26-10-01-032-a-sandboxs-egress-is-denied-when-any-of-its-levels-says-so.md),
  whose per-sandbox push this rides;
  [26-10-02-924](26-10-02-924-a-secret-may-carry-routes-and-every-request-it-is-sent-in-must-fall-on-one.md),
  whose routes are judged before a gateway applies;
  [0126](0126-a-sandbox-does-not-share-a-host-or-a-filesystem-with-its-pool.md)
  §6, which makes the pool proxy a sandbox's only route out.

## Context

Teams put an LLM gateway (LiteLLM, Cloudflare AI Gateway, Portkey, Helicone,
an in-house one) in front of model providers, for spend, logging, and policy.
Almost all of them proxy the provider's API as is: the same request, sent to
another base URL, with a header or two of the gateway's own — its key,
a team tag — and sometimes the provider's key replaced by the gateway's.

A discobox reaches a provider from many clients: the harness, SDKs in the
user's code, `curl`. Each has its own way to change a base URL
(`ANTHROPIC_BASE_URL`, `OPENAI_BASE_URL`, a config file), and some have none.
Every byte already leaves through the pool proxy, which MITMs it and applies
per-sandbox policy; that is the one place a reroute applies to all of them.

The values a gateway needs — its key above all — are setup, not something a
user hands an agent. Discobox secrets are for the latter: they are requested,
approved, granted, and scoped to what a person decided an agent may use.

## Decision

**A project holds named gateways. Each matches requests by origin and path
prefix, rewrites them onto a target origin and prefix, and modifies their
headers. The pool proxy applies them to every discobox in the project after
all other policy has judged the request as the discobox sent it. A header
value may name an environment variable of the server, which the server
resolves for the proxy on demand; nothing else of the server's environment is
reachable.**

The vocabulary is the Kubernetes Gateway API's `HTTPRoute`: a `PathPrefix`
match, a `URLRewrite` with `ReplacePrefixMatch`, and a
`RequestHeaderModifier` — the same operations as Envoy's `prefix_rewrite`,
`host_rewrite_literal`, `request_headers_to_add` and
`request_headers_to_remove`.

### 1. Shape

A gateway is an API resource, `ProjectGateway`; no file holds one. As the
body of `PUT /projects/{projectId}/gateways/anthropic`:

```json
{
  "match": "https://api.anthropic.com/v1",
  "target": "https://gateway.example.com/anthropic/v1",
  "requestHeaders": {
    "remove": ["x-api-key"],
    "set": [
      {"name": "cf-aig-authorization", "valueFrom": {"env": "DISCOBOX_GATEWAY_CF_TOKEN"}},
      {"name": "x-team", "value": "platform"}
    ],
    "add": []
  }
}
```

- `name` is the path's last segment: a DNS label, unique in the project.
- `match` is an `http` or `https` URL with a host and nothing after the path:
  no user, query, or fragment. The host is exact; no wildcards. The path is a
  prefix, `/` when absent.
- `target` is the same kind of URL. Its path replaces the matched prefix.
- A header entry has a `name` and exactly one of `value` or `valueFrom.env`.
  `remove` is names. A name that is hop-by-hop
  ([proxy: Hop-by-hop Headers](../../proxy/DESIGN.md#hop-by-hop-headers)) or
  `Host` is refused.
- Validated when written, never when applied: a proxy skips a route it cannot
  apply rather than refusing every other policy that arrives with it.

### 2. Matching and rewriting

- **A prefix matches on segment boundaries**, as Gateway API's `PathPrefix`:
  `/v1` and `/v1/` both match `/v1` and `/v1/messages`, never `/v1beta`. A
  trailing slash is dropped when stored.
- **The request is matched as it will be read.** The host as policy reads
  it, without a trailing dot; the path with its dot and empty segments
  resolved, and that cleaned path is what is forwarded — a discobox can
  neither spell its way around a gateway nor out of its target's prefix. A
  path under a gateway's origin with an encoded slash or a backslash is
  refused, since a server may read it as another path after the match.
- **The longest matching prefix wins.** Two gateways with the same `match`
  are refused, so a request matches at most one.
- **The rest of the path and the query are kept.** `/v1/messages?beta=true`
  under the gateway above goes to
  `https://gateway.example.com/anthropic/v1/messages?beta=true`.
- **Headers:** `remove`, then `set` (replacing any value), then `add`
  (appending). `Host` becomes the target's.
- The target is reached as any other destination is: through the proxy's
  transport and upstream proxy, verified with the proxy's trust and the
  sandbox's host trusts, over HTTP/2 when the target offers it. Streaming
  (SSE) responses pass through unchanged.

### 3. Every other policy judges the request as the discobox sent it

A gateway is how the proxy forwards a request, not what a discobox may do.
In order:

1. The egress filter, the tunnel's `Host` check, sentinel authorization and
   routes, and the secret swap all see the original request — host
   `api.anthropic.com`, path `/v1/messages`. A restricted discobox must be
   allowed the matched host, as it would be without a gateway; the target is
   never filtered, since the discobox never names it.
2. The gateway rewrites the URL and the headers.
3. The request is forwarded and audited. The audit row records the URL as
   sent by the discobox and, beside it, the gateway's name and the URL
   forwarded; every header the gateway touches is redacted as a swapped
   secret is. The response is never cached: it is the gateway's, which the
   project can change or remove, and a key made from the request as sent
   would outlive either.

So a provider key swapped in by its sentinel — granted for
`api.anthropic.com` — is sent to the gateway. That is deliberate: a gateway
that passes the provider's key through needs it, and the project naming the
gateway is the project vouching for it. A gateway that uses its own key
removes or `set`s the provider's header; the credential then never reaches the
upstream, and a `401` is not read as its refusal.

### 4. Values from the server's environment, resolved on demand

`valueFrom.env` names a variable of the `discobox-server` process. Only names
beginning `DISCOBOX_GATEWAY_` are accepted: a project gateway can point at any
host, so a reference to any other variable — a database URL, a signing key —
would send it there.

The server never pushes a value. The pool's proxy asks for one when a request
first needs it — `POST /api/pools/{poolId}/resolve-gateway-headers`, beside
`resolve-sandbox-secret`, with the same pool assertion — naming the sandbox
and the gateway. The server checks the sandbox is on that pool and in the
gateway's project, and answers the resolved headers. The proxy keeps each
value, per client, gateway, and variable, fresh for five minutes, and drops it
when the gateway is removed. A stale value is asked for again under the
outage rules ADR 26-10-14-482 gives a credential: asked again within the
retry window, and served stale while the control plane does not answer.

A variable that is not set, or set empty, is not an empty header. The proxy
refuses the request with `502`, saying which variable, and audits it as
blocked: sending it without the gateway's key would fail anyway, less
legibly, and sending it to the original host would bypass the gateway the
project asked for. A control plane that does not answer, with no value held,
is a `503` with `Retry-After`, for the same reasons.

Any project member may name any `DISCOBOX_GATEWAY_*` variable, and so may
send its value to a target of their choosing. That is accepted: Discobox has
no role below a project member, and a member already holds the project's
secrets and harness configs. When project roles exist, writing a gateway
belongs to the role that administers the project.

### 5. Delivery

A project's gateways are part of each of its sandboxes' proxy policy, pushed
the way the egress allow list is: the sandbox's policy reconcile sends them in
the pool's `UpdateSandbox`, and the pool agent writes them to the proxy's
policy file per client. Changing a project's gateways marks its sandboxes for
that reconcile. A value from the environment is sent as its reference only.

### 6. API and CLI

- `GET /projects/{projectId}/gateways`,
  `GET|PUT|DELETE /projects/{projectId}/gateways/{name}`. `PUT` creates or
  replaces. A member's; the sandbox role (ADR 0140) is refused every route,
  including reads: a discobox that could rewrite where its own credentials go
  could send them anywhere.
- `discobox admin gateway ls|get|set|rm`, on the project `--project` names,
  as the other project-scoped admin commands. `set NAME` is the `PUT`: it
  creates or replaces the whole gateway from `--match`, `--target`, and
  repeatable `--set-header NAME=VALUE`, `--set-header-env NAME=VAR`,
  `--add-header`, `--add-header-env`, and `--remove-header NAME`. `get` shows
  an environment value as the variable's name; the value is never the
  CLI's to show.
- No console surface.

## Consequences

- One setting covers every client in every discobox of a project; nothing in
  a harness or its image changes.
- A discobox cannot see or bypass the gateway. A client that pins a
  certificate already fails behind the proxy, gateway or not.
- Rotating a gateway key is changing the server's environment and restarting
  it; proxies take the new value within five minutes.
- Every project on a server reads the same `DISCOBOX_GATEWAY_*` variables.
  Per-project values are the server configuration file's concern (below).

## Alternatives considered

- **Set each client's base-URL variable.** Per harness and per SDK, misses
  clients with no such variable and anything the user runs, and the discobox
  could unset it. Rejected.
- **Store the gateway's key as a Discobox secret.** Secrets are what a person
  grants an agent, with requests, approvals and scopes; a gateway key is the
  operator's setup and needs none of that. Rejected.
- **Resolve the environment on the server and push the values.** Simpler, but
  puts credentials at rest in every pool's policy file, keeps a stale value
  until something pushes again, and opens a second way for cleartext to leave
  the control plane alongside the resolve route. Rejected for the on-demand
  route in §4.
- **Match an exact path.** `/v1/messages` alone leaves `count_tokens`,
  `models` and batch calls going around the gateway, silently. Rejected.
- **Judge egress and credentials against the target.** Every grant and allow
  entry for a provider would have to be repeated for its gateway, and a
  restricted discobox would be filtered on a host it never named. Rejected.

## Deferred

- **Projects and their gateways in the server's configuration file**
  (`projects: {default: {gateways: …}}`), with per-project environment
  references. A follow-on ADR, when that file is designed.
- **Gateways at the pool, harness-config, or sandbox level.** Revisit when
  someone needs one discobox routed differently from its project.
- **Wildcard hosts** (`*.amazonaws.com`). Revisit with a provider whose API is
  spread across hosts, when rewriting can say which target each maps to.
