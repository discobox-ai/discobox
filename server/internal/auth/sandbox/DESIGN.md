# Sandbox Auth Design

`internal/auth/sandbox` (package `sandboxauth`) owns the sandbox access issuer:
the per-project, per-user Ed25519 trust key and the short-lived sandbox access
tokens signed with it. It does not decide API authorization policy; it issues
credentials that other server layers scope to projects, sandboxes, and users.

This doc also records the sibling credential flows that share its shape but
are owned elsewhere:

- Pool identity: [`pkg/poolauth`](../../../../pkg/poolauth/assertion.go)
  (assertions), `PoolAuthenticator` in [`internal/auth`](../DESIGN.md)
  (verification), and [`internal/resources/pools`](../../resources/pools/DESIGN.md)
  (enrollment).
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
- Pool auth maps a pool's signed assertion to a pool ID.

Authorization should be possible from request attributes without inspecting the
request body. Use the authenticated principal plus method, route/path
parameters, query parameters, headers, and resource ownership loaded from those
attributes. If a body field is needed to identify the resource being authorized,
move that identity into the URL or another request attribute.

There is no exception. A pool has no unauthenticated registration route: its
maker records its key before its agent makes a request
([ADR 26-10-24-953](../../../../docs/adr/26-10-24-953-every-box-is-provisioned-from-an-ordered-list-of-bundles-and-a-pool-is-a-box.md)
§4).

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
It is enrolled by the pool's maker, over the channel it made the box with
([ADR 26-10-24-953](../../../../docs/adr/26-10-24-953-every-box-is-provisioned-from-an-ordered-list-of-bundles-and-a-pool-is-a-box.md)
§4); no token is minted for it.

```text
1. When a runtime provider creates a pool's box, it places the agent bundle
   there and runs `discobox-provision identity`, which generates an Ed25519
   key on the box's durable storage (or keeps the one there) and prints its
   public half.
2. The provider records that public key as the pool's (EnrollPool, through
   poolruntime's PoolMaker): the key and key type on the pool row.
3. It places the bootstrap — control plane URL, project ID, pool ID and the
   control plane's public key, no credential — and runs `discobox-provision
   apply`, which fetches the pool's bundles with that key and starts the agent.
4. The pool agent signs each pool-to-control-plane request with a poolauth
   assertion (audience discobox-control-plane). Its first heartbeat stamps
   the pool registered.
5. PoolAuthenticator verifies the assertion against the stored public key and
   key type, and requires the pool to be unrevoked and the claimed project ID
   and pool ID to match the pool row and the route pool ID.
```

Rules:

- Only a pool's maker records its key, over the channel it made the box with,
  each time it provisions the box: at create, at repair, and when it applies
  moved bundles in the running box. `provision identity` keeps a key the box
  already has, so each enrollment after the first records the same key, and
  nothing else writes `Pool.PublicKey`.
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
