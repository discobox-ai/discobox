# Auth Review Notes

- Keep credential parsing and validation in `internal/auth` authenticators, not in handlers or services.
- Authenticators should return `ok=false` only when they do not apply to the request; malformed credentials for an applicable route should return an error.
- Authorizers should return `ok=false` only when they do not apply to the request; denied access for an applicable route should return an error.
- Authorizers should assert what a route is, not what it is not. Avoid negative checks like "not a pool agent route" as authorization criteria because new or misspelled routes can be unintentionally allowed.
- One successful authorizer is sufficient to allow the request; order authorizers from most specific to broadest.
- Avoid authorization by exclusion. General authenticated authorization must use an explicit exact-path or path-prefix allow-list (`authenticatedAllowedPaths`).
- A new pool agent route must add its action to `poolRuntimeActions` in the same change. Unlisted routes are not pool routes and answer 403 to the real agent, which no service-level test notices. Mark an action as taking a trailing resource ID only if it does; accepting one everywhere lets an unlisted subroute inherit pool access.
- Do not authorize from request-body fields. Use principal, method, path parameters, query parameters, headers, and resource ownership loaded from those attributes. The only exception is `POST /api/pools/register`, which may redeem body-provided bootstrap identity plus a one-time bootstrap token before a pool principal exists.
- Pool authentication must derive the principal's `PoolID` from the validated assertion, never from the URL or request body; the URL pool ID is only checked against it.
- Pool runtime operations must compare the authenticated pool principal's `PoolID` to the path `poolId` before updating state.
- Keep the request context limited to identity/authorization metadata; do not store mutable resource payloads or credentials in context.
- Preserve public-path bypass (`IsPublicPath`) only for `/healthz`,
  `/openapi.yaml`, `/docs`, `/docs/*`, `/ssh`, and `/ssh/connect`. `GET /ssh`
  (ADR 0024) serves the SSH endpoint discovery document — the advertised
  address and the server's SSH host *public* key, neither of which is a
  credential; publishing the key is the point, the same as a `known_hosts`
  line — and, like docs/openapi, it must be fetchable
  (`discobox admin ssh-config`) before any other credential exists.
  `/ssh/connect` carries an SSH connection over HTTP and needs no HTTP auth for
  the same reason the SSH TCP listener does not: SSH authenticates by public
  key inside its own protocol before any channel exists. Do not widen this list
  further without the same "must work pre-auth and reveals nothing sensitive,
  or authenticates in-band" justification.
