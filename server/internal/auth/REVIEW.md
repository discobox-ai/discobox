# Auth Review Notes

- Keep credential parsing and validation in `internal/auth` authenticators, not in handlers or services.
- Authenticators should return `ok=false` only when they do not apply to the request; malformed credentials for an applicable route should return an error.
- Authorizers should return `ok=false` only when they do not apply to the request; denied access for an applicable route should return an error.
- Authorizers should assert what a route is, not what it is not. Avoid negative checks like "not a pool agent route" as authorization criteria because new or misspelled routes can be unintentionally allowed.
- One successful authorizer is sufficient to allow the request; order authorizers from most specific to broadest.
- Avoid authorization by exclusion. General authenticated authorization must use an explicit exact-path or path-prefix allow-list.
- Do not authorize from request-body fields. Use principal, method, path parameters, query parameters, headers, and resource ownership loaded from those attributes. The only exception is `POST /api/pools/register`, which may redeem body-provided bootstrap identity plus a one-time bootstrap token before a pool principal exists.
- Worker authentication must derive `WorkerID` from the validated credential, never from the URL or request body.
- Pool status authorization must compare the authenticated pool principal to the path `poolId` before updating state.
- Keep the request context limited to identity/authorization metadata; do not store mutable resource payloads or credentials in context.
- Preserve public-path bypass behavior only for docs/openapi/health routes,
  plus `GET /ssh/host-key` (ADR 0024): it serves the server's SSH host
  *public* key, which is not a credential — publishing it is the point, the
  same as a `known_hosts` line — and, like docs/openapi, it must be
  fetchable (`disco ssh-config`) before any other credential exists. Do not
  widen this list further without the same "must work pre-auth and reveals
  nothing sensitive" justification.
