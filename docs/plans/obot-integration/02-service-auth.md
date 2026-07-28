# WI-02 — Service authentication and project-scoped service authorization

**Goal:** let a remote service (Obot) authenticate to the Discobox REST API and
project event streams with its own credential, authorized to a specific project.

Read `00-CONTEXT.md` first. **This item is independent and can start now.**

## Why

Obot runs as a separate deployment and calls Discobox over the network. Today
every ordinary REST request is authenticated as the configured default user, so
there is no way to express "this caller is the Obot service, and it may act only
within project X". Without this, the integration has no trust boundary.

This is the only workstream with no close existing analogue to copy, which is
why it is worth starting early and independently.

## Current state

- `server/internal/auth/authenticators.go:56` — `DefaultUserAuthenticator`
  authenticates *every* request as the configured default user.
- `server/internal/auth/poolagent/` — pool agents authenticate with a bearer
  PASETO assertion verified against the pool's stored Ed25519 public key, with
  signed `project_id` and `pool_id` claims that must match the route. This is
  the closest existing model for a non-human principal.
- `server/internal/auth/authorizers.go` — `ProjectAuthorizer` authorizes
  project-scoped routes by authenticated *user membership*;
  `PoolRouteAuthorizer` handles pool principals; `AuthenticatedAuthorizer`
  covers a hard-coded allow-list.
- `server/internal/auth/context.go` — `Principal` is the only request identity
  in context; `PrincipalTypeUser` and `PrincipalTypePool` exist today.
- `server/internal/projectstream/project_stream_socket.go` — the project
  websocket/SSE streams. These need the same credential, not just REST.

Read `server/internal/auth/DESIGN.md` and `REVIEW.md` before designing. Two
rules there bind this work directly:

- Authorization must be decidable from request attributes available *before*
  body interpretation.
- Do not authorize by broad exclusion. Add exact paths or prefixes to the
  allow-list only when a route intentionally has no narrower resource
  authorizer.

## Scope

1. A new principal type for a remote service, carrying the project(s) it is
   scoped to. Keep `Principal` the single request identity — do not introduce a
   parallel identity mechanism.
2. An authenticator that recognizes the service credential.
3. An authorizer that authorizes project-scoped routes for a service principal
   whose scope covers the route's project, alongside the existing
   membership-based user path. `ProjectAuthorizer` resolves the `default`
   project alias for users; a service principal always supplies a concrete
   project ID and must not use the alias.
4. Credential storage, issuance, and revocation.
5. The same credential must work on `server/internal/projectstream` websocket
   and SSE connections, not only on REST.
6. The generated client must be able to attach the credential to both normal
   REST requests and stream requests.
7. Update `server/internal/auth/DESIGN.md` in the same change.

## Out of scope

- Multi-tenant user management, roles, or groups.
- The managed routes themselves — WI-03 owns those. Build this so a service
  principal is authorized for project-scoped routes generally; managed routes
  then inherit it.
- Per-sandbox identity for the agent runtime. Obot issues its own OAuth refresh
  tokens under a project OAuth client and stores them as Discobox `oauth`
  secrets, which already works (`server/internal/resources/secrets/oauth.go`).

## Design questions for the engineer

- **Credential form.** A long-lived opaque API token stored hashed is the
  simplest thing that works and is easy to revoke. A signed assertion in the
  style of the pool-agent PASETO scheme is more consistent with existing
  non-human auth and avoids storing a verifier secret, but is more machinery.
  Bring a recommendation rather than an open question.
- **Scope granularity.** One project per credential, or a set? The Obot
  deployment mapping is one project, but a set is not obviously harder.
- **Where administrators manage it.** New REST routes, CLI, config file, or a
  bootstrap flow like `/api/pools/register`?
- **Does the service principal need to act as a user** for resources that key
  off `created_by_user_id`? `model.Sandbox.CreatedByUserID` is `not null`
  (`server/internal/model/model.go:558`). Answer this early — WI-03 needs it.

## Done when

- A remote caller with a service credential can perform project-scoped REST
  operations and open a project stream for its project, and is rejected for
  another project.
- The default-user path is unchanged for local single-user use.
- Tests cover: valid credential, wrong project, revoked credential, missing
  credential, and stream authentication.
- `server/internal/auth/DESIGN.md` reflects the new pipeline.
- `go tool task check-hooks` passes.
