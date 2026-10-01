# 26-10-01-874 — A build's registry credentials are sandbox sentinels

- **Status**: Proposed
- **Date**: 2026-10-01
- **Relates to**: [ADR 0044](0044-builds-run-on-a-pool-shared-buildkit.md)
  §6, which accepted that buildkitd's own fetches are attributed to the pool;
  [ADR 26-10-01-032](26-10-01-032-a-sandboxs-egress-is-denied-when-any-of-its-levels-says-so.md)
  §7, which records that they are not filtered at all;
  [ADR 0031](0031-agent-credentials-are-a-portable-protocol-with-ephemeral-sentinels.md),
  the sentinels this reuses.

## Context

A sandbox's `docker build` runs on its pool's shared buildkitd. `RUN` steps
reach the proxy as the sandbox, through the per-build forwarder. buildkitd's
own fetches — `FROM` pulls, `ADD <url>`, git contexts, cache import — do not
reach the pool proxy at all: buildkitd dials origins directly, or through an
outer proxy when the pool is nested in a discobox. Nothing records whose build
a pull served or applies that sandbox's egress policy to it (ADR 26-10-01-032
§7).

Two further costs follow from the same gap:

- A real registry credential can only be handed to buildkitd by the sandbox's
  build client, so it is in the sandbox. Discobox does not manage registry
  credentials today; when it does, it will want them swapped at the proxy like
  every other credential, never present in the sandbox.
- buildkitd may reuse a cached registry token for another session that
  presents the same credentials (`util/resolver`, `VerifyTokenAuthority`), so
  one sandbox's token can serve another's build.

What BuildKit gives to work with (v0.20.2): registry credentials come from the
client side of the build's session, asked per host (`session/auth`). Once
buildkitd's fetches go through the pool proxy (§0), every credential-bearing
exchange on a pull passes through it, MITM'd:

- **Token registries** (Docker Hub, GHCR, most). `GET /v2/` answers `401`
  naming a token realm. The credential goes to the realm, as Basic or an OAuth
  POST, from the sandbox's build client (`authprovider.FetchToken`) or from
  buildkitd, depending on the session's token authority. The registry then
  sees only the issued `Bearer` token.
- **Basic registries.** buildkitd sends the credential on every request.
- **Blob redirects.** Layers are often redirected to a CDN with no
  `Authorization` header at all.

## Decision

**Every build's session answers every registry host with a credential that is
a sentinel bound to the building sandbox. The proxy treats that sentinel as it
treats any other: it identifies the sandbox, the sandbox's egress policy is
applied, and the sentinel is swapped for a real registry credential when one
is bound, or removed so the request goes anonymously when none is. The proxy
carries the attribution onto the token the exchange issued, and onto the
redirect a response it attributed sends buildkitd to.**

### 0. buildkitd's own fetches go through the pool proxy

buildkitd is pointed at the pool proxy through a pool-local forwarder holding
a client certificate of the pool's own, and trusts the pool's MITM CA. The
pool registry stays in `NO_PROXY`. Unattributed buildkitd traffic is then the
pool's, governed by the project and pool levels of ADR 26-10-01-032 (§5).

### 1. The mediator answers registry auth, not the sandbox

A sentinel the sandbox's own Docker config supplied would be the sandbox's to
omit, and an omitted one is an anonymous, unattributed pull: the attribution
would bind only a willing sandbox. The mediator already sits on each build's
session and knows the sandbox from its mTLS peer, so it answers the session's
`Auth` service itself — every host, the building sandbox's registry sentinel
— and forwards the rest of the session (filesync, secrets, ssh) unchanged.
The client's own answers are never reached.

### 2. One stable registry sentinel per sandbox

Minted with the sandbox and pushed to the pool with its other sentinels, so it
is in the proxy's set before the first build. One per sandbox, not per
registry: what it names is the sandbox; which credential it becomes is
decided at swap time by the destination, as for any sentinel. Distinct per
sandbox, which also ends cross-sandbox token reuse in buildkitd.

### 3. Swap, or strip

At the proxy, a registry sentinel in `Authorization` (Basic, or the OAuth
form's password):

- **attributes** the request to its sandbox, which is the client ID the
  egress filter, audit, and the judge then see;
- is **swapped** for a registry credential bound to that sandbox for that host,
  once Discobox manages them;
- is otherwise **removed**, and the request sent anonymously. A registry
  answers a wrong credential with `401` rather than falling back to anonymous,
  so a sentinel left in place would break every public pull.

### 4. Attribution follows the token and the redirect

- A token-realm response to an attributed request: the proxy records the
  issued token's digest against the sandbox, until its `expires_in`. A later
  `Bearer` request carrying that token is attributed to the sandbox.
- A `3xx` response to an attributed request: its `Location` is attributed to
  the sandbox for a short window, matched by exact URL.

### 5. Unattributed build traffic

buildkitd traffic that carries no sentinel, no recorded token, and no recorded
redirect stays the pool's, under the project and pool levels of ADR
26-10-01-032 alone: §2's rule over just those two, the levels known for every
build on a pool. With
§1 in place, that is traffic no build asked for by name.

## Alternatives rejected

- **The sandbox's Docker config supplies the sentinel.** Simple — a credential
  helper answering every host — but optional for the sandbox (§1).
- **Source policy as the per-sandbox control.** The mediator can set a
  per-build source policy from the sandbox's allowlist, and it is enforced
  before a connection opens. It matches image names, not destinations, so a
  permitted pull's redirects escape it, and it attributes nothing for audit or
  a credential swap. It may still be layered on as an early refusal.
- **A per-build proxy for buildkitd.** BuildKit has no per-solve proxy for its
  own fetches; the daemon's environment is shared by every build.

## Consequences

- buildkitd's fetches gain per-sandbox audit rows, and a restricted sandbox's
  pulls are held to its own effective policy rather than the project's.
- Registry credentials can be managed and swapped as other secrets are, never
  entering a sandbox.
- The mediator becomes a party to the session protocol rather than a byte
  relay for it, and has to track BuildKit's `session/auth` service.
- A registry whose layer redirect does not exactly match what was recorded
  (re-signed URLs, multi-hop redirects) falls back to the pool's attribution.
