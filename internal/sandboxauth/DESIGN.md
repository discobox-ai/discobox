# Auth Design

This package currently manages sandbox access trust keys. It also documents the
planned worker identity pattern because both flows should use the same high-level
shape: long-lived key identity -> proof or issuer use -> short-lived scoped token.

## Worker Auth: Workload Identity

Workers have their own identity. The worker private key stays on the worker.

```text
1. Control plane creates Worker + one-time WorkerBootstrapToken.
2. Worker boots with worker ID, bootstrap token, and control plane URL.
3. Worker generates a keypair locally.
4. Worker registers its public key using the bootstrap token.
5. Control plane stores the public key and marks the bootstrap token used.
6. Worker proves possession of its private key with a signed challenge.
7. Control plane issues a short-lived WorkerAuthToken.
8. Worker uses the token to subscribe for work and report status.
```

Notes:

- Bootstrap tokens are short-lived, one-time use, and stored only as hashes.
- `WorkerAuthToken` may be stateless.
- Worker authorization should be scoped to assigned work and provider/sandbox scope.
- Worker bootstrap and runtime auth must identify the tenant before database
  lookup. For SQLite this is required to resolve the correct tenant database
  file before validating the token or worker key. A worker should present
  `tenant_id` with its worker ID/bootstrap token, and issued worker runtime
  tokens must include a tenant claim.

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
- Issued sandbox access tokens include `tenant_id`, `project_id`,
  `sandbox_id`, and `user_id` claims. The tenant claim is derived from the
  sandbox's project and is the database shard key for later authenticated
  requests.

## Tenant Binding

Authentication must always establish tenant before resource access:

- User auth maps the user/session to a tenant membership.
- Sandbox auth maps the sandbox token to the tenant that owns the sandbox's
  project.
- Worker auth maps bootstrap/runtime credentials to the tenant that owns the
  worker's sandbox/provider scope.

For Postgres, the tenant is used as an authorization and query boundary within
the shared database. For SQLite, the tenant is also required to choose the
database file before loading resource rows.

## Key Ownership

| Flow | Private key owner | Purpose |
| --- | --- | --- |
| Worker auth | Worker | Proves workload identity to the control plane. |
| Sandbox auth | Control plane | Issues delegated sandbox access tokens. |
