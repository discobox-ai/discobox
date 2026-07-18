# Sandbox Auth Design

`internal/auth/sandbox` owns token and key helpers for sandbox access delegation
and pool identity flows. It should not decide API authorization policy; it
issues or validates credentials that other server layers scope to projects,
sandboxes, and pools.

## Auth Shape

Both sandbox access and pool identity follow the same pattern:

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
body-provided project ID, sandbox ID, one-time bootstrap token, and public key
for the first pool runtime token. This is safe only because the control plane
created the bootstrap token for a preassigned sandbox pool, stores it as a
short-lived one-time hash, and validates it before issuing runtime credentials.
After registration, pool authorization must use the authenticated pool
principal and request attributes, not body fields.

## Sandbox Auth: Access Delegation

Sandbox auth is delegated access. The control plane owns a sandbox access issuer
key for a user/project and signs short-lived tokens accepted by the sandbox side.

The current implementation stores this as `ProjectUserKey`; the design-level
name is `SandboxAccessIssuerKey`.

```text
1. A sandbox is created by a user in a project.
2. When the sandbox starts, the control plane ensures a SandboxAccessIssuerKey
   exists for that project/user.
3. If missing, the control plane generates an Ed25519 keypair.
4. The public key is stored on the issuer key row.
5. The private key is encrypted and stored on the issuer key row.
6. The sandbox runtime receives the public trust key as DISCOBOX_TRUST_KEY.
7. When sandbox access is needed, the control plane decrypts the private key.
8. The control plane signs a short-lived PASETO token with that private key.
9. The token can be used with the sandbox side that trusts the matching public key.
```

Current details:

- Token TTL is 12 hours.
- Signing key type is Ed25519 / PASETO v4 public.
- Key scope is `(projectID, userID)`, not individual sandbox.
- Encryption associated data binds ciphertext to the `projectID/userID` identity.
- Issued sandbox access tokens include `project_id`, `sandbox_id`, and `user_id` claims.

## Pool Auth: Workload Identity

Workers have their own identity. The pool private key stays on the pool host.

```text
1. Control plane creates Pool + one-time WorkerBootstrapToken.
2. Pool boots with project ID, sandbox ID, bootstrap token, control plane URL, and the assigned pool ID for subsequent pool-scoped routes.
3. Pool generates a keypair locally.
4. Pool registers its public key using project ID, sandbox ID, and the bootstrap token in the registration body; the control plane derives the pool ID from the sandbox assignment.
5. Control plane validates the token for that assigned pool, stores the public key, and marks the bootstrap token used.
6. Pool signs each pool-to-control-plane runtime request with its private key.
7. Control plane validates the short-lived assertion against the stored public
   key, project ID, pool ID, route pool ID, and pool revocation state.
```

Rules:

- Bootstrap tokens are short-lived, one-time use, and stored only as hashes.
- Runtime assertions use PASETO v4.public with Ed25519, a short TTL, and a
  backwards `nbf` skew allowance for local VM clocks.
- Pool authorization should be scoped to assigned work and provider/sandbox
  scope.

## Pool-Agent Request Tokens

Control-plane calls to a pool host agent use a separate server-owned issuer key.
The public key is delivered to the pool host in bootstrap metadata as
`DISCOBOX_CONTROL_PLANE_PUBLIC_KEY`; the private key remains on the control
plane and is stored in `server_state` as encrypted-at-rest key material when a
sealer is configured.

The workerpool layer mints short-lived PASETO v4.public bearer tokens when it
builds pool-agent clients or reverse-proxy requests. Tokens are audience-bound
to `pool-agent`, include `project_id`, `worker_id`, optional `sandbox_id`, and
operation scopes, and backdate `nbf` to tolerate local VM clock skew. Driver
HTTP leases should carry routing/connectivity only; they should not cache or
persist pool-agent request tokens.

## Key Ownership

| Flow | Private key owner | Purpose |
| --- | --- | --- |
| Pool auth | Pool | Proves workload identity to the control plane. |
| Sandbox auth | Control plane | Issues delegated sandbox access tokens. |
