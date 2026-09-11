# Sandbox Auth Design

`internal/auth/sandbox` (package `sandboxauth`) owns the sandbox access issuer:
the per-project, per-user Ed25519 trust key and the short-lived sandbox access
tokens signed with it. It does not decide API authorization policy; it issues
credentials that other server layers scope to projects, sandboxes, and users.

This doc also records the sibling credential flows that share its shape but
are owned elsewhere:

- Pool identity: [`pool-agent/poolauth`](../../../../pool-agent/poolauth/assertion.go)
  (assertions), `PoolAuthenticator` in [`internal/auth`](../DESIGN.md)
  (verification), and [`internal/resources/pools`](../../resources/pools/DESIGN.md)
  (registration).
- Pool-agent request tokens: [`internal/auth/poolagent`](../poolagent/auth.go)
  (package `poolagentauth`).

## Auth Shape

Every flow follows the same pattern:

```text
long-lived key identity -> proof or issuer use -> short-lived scoped token
```

Authentication must establish the caller identity before resource access:

- User auth maps a user/session to a user ID.
- Sandbox auth maps a sandbox token to project, sandbox, and user identity.
- Pool auth maps bootstrap/runtime credentials to a pool ID.

Authorization should be possible from request attributes without inspecting the
request body. Use the authenticated principal plus method, route/path
parameters, query parameters, headers, and resource ownership loaded from those
attributes. If a body field is needed to identify the resource being authorized,
move that identity into the URL or another request attribute.

The only intentional exception is pool bootstrap registration. A booting
pool has no runtime principal yet, so `POST /api/pools/register` redeems a
body-provided project ID, pool ID, one-time bootstrap token, and public key
(plus optional key type). This is safe only because the control plane created
the bootstrap token for that pool, stores only its hash with a short expiry,
and validates it before recording the key. After registration, pool
authorization must use the authenticated pool principal and request
attributes, not body fields.

## Sandbox Auth: Access Delegation

Sandbox auth is delegated access. The control plane owns a sandbox access issuer
key for a project/user and signs short-lived tokens for the sandbox side.

The row is `model.SandboxAccessIssuerKey` (table `sandbox_access_issuer_keys`);
this package's `UserStore` interface refers to it by its alias
`model.ProjectUserKey`.

```text
1. A user creates a sandbox in a project.
2. When the sandbox reconciler creates the sandbox runtime, it calls
   Manager.EnsureTrustKey for (project, sandbox creator).
3. If no key row exists, the manager generates an Ed25519 keypair.
4. The public key is stored on the issuer key row.
5. The private key is sealed and stored on the same row
   (CreateProjectUserKeyIfMissing; a concurrent loser reloads the winner).
6. The runtime receives the public key in its create env as DISCOBOX_TRUST_KEY.
7. Manager.CreateToken (via sandboxes Service.CreateSandboxAuthToken) opens
   the sealed private key.
8. It signs a short-lived PASETO v4.public token with that key.
9. A verifier holding the matching public key can check the token.
```

Current details:

- The manager is wired only when the server has a secret sealer. Without a
  store or sealer, `EnsureTrustKey` and `CreateToken` return an empty string
  and no error, and no `DISCOBOX_TRUST_KEY` is injected.
- Token TTL is 12 hours (`TokenTTL`).
- Signing key type is Ed25519 / PASETO v4 public.
- `iat` and `nbf` are both backdated one minute (`clockSkew`) for pool VM
  clock drift; each token carries a random `jti`.
- Key scope is `(projectID, userID)`, not individual sandbox.
- The sealer purpose is `sandbox_access_issuer_keys.private_key`, and the
  resource ID `projectID/userID` binds the ciphertext to that identity.
- Tokens carry `project_id` and `user_id` claims, and `sandbox_id` when set.
- A key row missing its public or sealed private key is an error, never
  regenerated. The row's `rotated_at` and `revoked_at` columns are not read.
- No component in this repository reads `DISCOBOX_TRUST_KEY` or verifies these
  tokens, and `CreateSandboxAuthToken` has no callers. Sandbox-agent requests
  use pool-agent request tokens instead (below).

## Pool Auth: Workload Identity

Pools have their own identity. The pool private key stays on the pool host.

```text
1. When a runtime provider creates a pool runtime, poolruntime mints a one-time
   PoolBootstrapToken for that pool and the bootstrap metadata.
2. The pool agent boots with the control plane URL, project ID, pool ID,
   bootstrap token, and control-plane public key (DISCOBOX_CONTROL_PLANE_URL,
   DISCOBOX_PROJECT_ID, DISCOBOX_POOL_ID, DISCOBOX_POOL_BOOTSTRAP_TOKEN,
   DISCOBOX_CONTROL_PLANE_PUBLIC_KEY).
3. The pool agent generates an Ed25519 keypair locally, or loads the one it
   kept on durable pool storage.
4. With a new key, the agent calls POST /api/pools/register with project ID,
   pool ID, bootstrap token, and public key. With a key loaded from disk it
   skips registration and authenticates directly.
5. The control plane checks that the token hash belongs to that pool and is
   unexpired, unused, and unrevoked, then records the public key and key type,
   stamps the pool registered, and marks the token used.
6. The pool agent signs each pool-to-control-plane runtime request with a
   poolauth assertion (audience discobox-control-plane).
7. PoolAuthenticator verifies the assertion against the stored public key and
   key type, and requires the pool to be unrevoked and the claimed project ID
   and pool ID to match the pool row and the route pool ID.
```

Rules:

- Bootstrap tokens expire after 30 minutes, are one-time use, and are stored
  only as SHA-256 hashes. Spent tokens are purged periodically.
- Runtime assertions use PASETO v4.public with Ed25519, a 5-minute TTL, and
  `iat`/`nbf` backdated 5 minutes for local VM clocks.
- Pool authorization should be scoped to assigned work and provider/sandbox
  scope.

## Pool-Agent Request Tokens

Control-plane calls to a pool agent, and to the sandbox agents behind it, use a
separate server-owned issuer key managed by `poolagentauth.Manager`. The public
key is delivered to the pool host in bootstrap metadata as
`DISCOBOX_CONTROL_PLANE_PUBLIC_KEY`, and the pool agent passes it on to the
sandbox agents it runs. The private key remains on the control plane in
`server_state` under `worker_agent_request_issuer`. It is sealed at rest when a
sealer is configured and stored in plaintext otherwise.

`server/providers/poolruntime` mints short-lived PASETO v4.public bearer tokens
through an auth token provider on each pool-agent client lease. Tokens are:

- audience-bound to `pool-agent` (verified by `pool-agent/server`) or
  `sandbox-agent` (verified by `sandbox-agent/server`; exec, terminal, and TCP
  scopes need one);
- carry `project_id`, `pool_id`, optional `sandbox_id`, and operation `scopes`;
- valid for 15 minutes, with `iat`/`nbf` backdated 5 minutes for local VM
  clock skew.

The pool agent rejects tokens whose project and pool do not match its own
identity and the route. A pool principal can also obtain `status:read`-only
sandbox-agent tokens for sandboxes it hosts, through
`MintSandboxAgentStatusTokens`.

Driver HTTP leases (`transport.HTTPClientLease`) should carry
routing/connectivity and a token provider only; they should not cache or
persist pool-agent request tokens.

## Key Ownership

| Flow | Private key owner | Purpose |
| --- | --- | --- |
| Pool auth | Pool | Proves workload identity to the control plane. |
| Sandbox auth | Control plane (`sandbox_access_issuer_keys`) | Issues delegated sandbox access tokens. |
| Pool-agent requests | Control plane (`server_state`) | Signs control-plane requests to pool and sandbox agents. |
