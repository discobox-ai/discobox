# 26-10-07-005 — discobot asserts who a person is, and discobox keeps no users

- **Status**: Proposed
- **Date**: 2026-10-07
- **Relates to**: [ADR 0011](0011-oauth-secrets-refresh-server-side-on-resolve.md),
  whose server-side refresh this keeps and extends with a client secret;
  [ADR 0140](0140-a-discobox-reaches-the-discobox-api-through-its-pool-with-a-fixed-role.md),
  whose sandbox principal and fixed role stay as they are; and the pool agent
  assertions in `server/internal/auth/poolagent`, the precedent for the token.

## Context

discobot (its own repository, `github.com/discobox/discobot`) is a
team-facing control plane for the agents that run in
discoboxes: a conversation with an agent, a live map of which discoboxes reach
which systems, and the credentials and approvals behind it. Building it showed
that most of what it needs already exists in the discobox API (sandboxes,
terminals, secrets, grants and their uses, requests and approvals, the judge,
the audit trail, OAuth refresh), and that what is missing is about people:
signing in, users, which projects they are in, what they may do, which systems
exist and how to sign in to them.

The discobox server is single-user. `DefaultUserAuthenticator` answers every
request that is not a pool's or a forwarded sandbox's as the configured default
user with every scope; `ProjectAuthorizer` checks project membership rows. A
team product in front of it needs each request to carry a real person, and
discobox's records (`createdByUserId`, `grantedBy`, the audit trail) to name
that person.

Two products could each own people. Only one should.

## Decision

### 1. Two products, split by what they are for

- **discobox is execution and enforcement**: discoboxes and their terminals,
  secret storage and the sentinel swap, grants and their uses, requests and
  approvals, the judge, delegation between discoboxes, the audit trail, and
  OAuth refresh.
- **discobot is people and meaning**: signing in, users, project membership and
  roles, the systems registry (hosts, operations, resource patterns, how to sign
  in to each), the OAuth authorization flow and its first code exchange, the
  agent, its conversations and proposals, routing approvals to people, and the
  live feed its clients see.

discobox gains no user, membership, role, or OAuth-flow management.

### 2. discobot asserts the principal in a signed token

discobot signs, per request, a **PASETO `v4.public`** token (Ed25519) that
discobox verifies with discobot's public key, which the server is configured
with. Claims: `iss` discobot, `aud` discobox, `sub` the user's ID, the user's
display name, the project the request acts in (`project_id`, as pool assertions
carry theirs), a short `exp`, and a `jti`.

PASETO over JWT because discobox already issues and verifies `v4.public`
assertions for pool agents with `aidanwoods.dev/go-paseto` (in the root,
server, pool-agent, and sandbox-agent modules): the same library, key type, and
verification shape, and no algorithm field to get wrong. Signing on discobot's
Node side is Ed25519 over PASETO's pre-authentication encoding, which
`node:crypto` does with no dependency if no maintained library fits; JWT with
EdDSA through `jose` is the fallback only if that proves untenable, with the
algorithm pinned on verify.

An asserted-principal authenticator joins the chain after
`SandboxForwardAuthenticator` and `PoolAuthenticator` and before
`DefaultUserAuthenticator`. Like the sandbox one, it **fails rather than
stepping aside** once a request carries an assertion: a bad assertion is a 401,
never a fall-through to the default user. It yields a `PrincipalTypeUser`
principal with the asserted user ID, and `ProjectAuthorizer` takes the signed
`project_id` claim as the membership check for it, instead of member rows.

discobox records and propagates the asserted principal where it records one
today: `createdByUserId`, `grantedBy`, the audit trail, and on the discobox it
creates, so what that discobox does later is attributed to the person.

discobox's other callers keep their own authentication: discoboxes by their
forwarded identity (ADR 0140; `discobox-access`, delegation), pools by their
assertions, and the CLI by the default user in single-user mode.

### 3. Who may do what is discobot's, for now

discobox trusts discobot's assertion as it trusts a pool's: discobot decides
whether a person may approve, grant, or launch, and discobox enforces the rest
(grants, uses, the judge, the proxy). The asserted user is attribution, not a
second authorization.

Deferred: the discobot agent moves into a discobox assigned to the person
(Codex or any harness, reached over discobox's exec API), so its own access is
governed the way every discobox's is, by natural-language rules on that box's
grants, enforced by the proxy and the judge. One rule holds then as now:
**the agent proposes under its box's grant; a person's apply runs as that
person**, asserted by discobot from their session. The agent never holds the
person's authority. Revisit when the agent runs in a discobox, or when
discobot's authorization grows past what one service should be trusted with.

### 4. OAuth: discobot signs in, discobox refreshes

discobot runs the authorization code flow (PKCE) and the first code exchange,
then creates the secret in discobox as raw OAuth values: access token, refresh
token, client ID, token URL, token request encoding, scopes, expiry. discobox
refreshes it (ADR 0011) and never runs an authorization flow.

`SecretValue` gains an optional **client secret**, stored sealed like the
refresh token and sent on refresh when present. A confidential client must
authenticate on refresh as well as on the code exchange (RFC 6749 §6), and
`refreshOAuthToken` sends only `client_id` today, so without it a credential
from a confidential client (most web-app registrations: Google, GitHub Apps,
Slack) stops working when its first access token expires.

### 5. No change feed in discobox yet

discobot polls the discobox API and fans changes out to its clients over
server-sent events. Revisit when polling's cost on the server, or the delay it
puts on what a person sees, matters.

### 6. discobot has no API of its own

discobot's interface is its agent (text) and its pages. What it keeps (systems,
members, conversations, proposals) is an internal TypeScript layer behind its
own pages and the agent's tools, not a REST or OpenAPI contract.
Its draft `api/discobot.yaml` goes.

## Alternatives rejected

- **Users, membership, and roles in discobox.** discobox would carry a second
  identity model next to discobot's, kept in sync, for a product whose only
  question about a person is who to record. Its enforcement is about what a
  discobox may do, not what a person may.
- **discobox runs OAuth authorization flows.** It would need registered OAuth
  apps, redirect URIs, and a browser-facing flow on a server that otherwise has
  no person in front of it. discobot already has the person and the page.
- **An unsigned trusted header** (the `X-Discobox-User` stand-in the proof of
  concept uses). Anything that can reach the listener can claim to be anyone,
  and the server cannot tell a misrouted request from a forged one.
- **JWT.** No better on either side for this: discobox already verifies
  PASETO `v4.public`, and JWT reintroduces the algorithm field to pin.
- **A change feed in discobox now.** A durable, resumable event stream is a
  real design (ordering, retention, per-principal filtering) for one consumer
  that polling serves today.
- **A formal discobot API.** It would be a second contract to version for an
  agent whose interface is text; nothing else calls it.

## Consequences

- The discobox server gains an issuer key setting and one authenticator, and
  `ProjectAuthorizer` an asserted-project branch. No user or member tables are
  written for asserted users.
- A server exposed to discobot must not also answer as the default user
  with every scope. That is a setting, `authRequired`, not a property of a
  transport: set, nothing is answered as the default user without a proven
  identity, on any listener; unset (single-user mode), the CLI keeps its own
  access. An enrolled iroh peer is a proven identity: its request is checked
  against the enrollments on every request and authenticated as the
  operator's own client, which is how the CLI reaches such a server.
- `/ssh/connect` is authenticated over HTTP before SSH sees a byte, and
  admitted only for the CLI's own user, its one client: not a pool agent, a
  sandbox, or a person discobot asserted. With `authRequired` that is the CLI
  over iroh, as its enrolled peer. These are two layers, not one replacing
  the other: discobox's authentication decides whether the CLI's
  `ProxyCommand` may open the tunnel at all, and SSH's own key
  authentication, which every SSH client needs, still decides what the
  session may do (ADR 0024 §5's file and project layers, unchanged). This
  supersedes only [ADR 0024](0024-ssh-is-a-control-plane-ingress-onto-execs.md)'s
  consequence that SSH is an authentication surface "reachable before any
  HTTP authorization runs".
- `SecretValue` gains a sealed client secret; refresh sends it when present.
- discobot becomes security-critical for people: a compromised discobot key can
  assert anyone. Its key is rotated by reconfiguring the servers that trust it.
- Once accepted and built, these design docs change: the root `DESIGN.md`
  (the two products and the boundary), `server/internal/auth/DESIGN.md` (the
  asserted-principal authenticator and the project claim),
  `server/internal/resources/secrets/DESIGN.md` (the client secret on refresh),
  and discobot's own `DESIGN.md` (identity, the internal data layer in place of
  `discobot.yaml`).
