# 26-11-02-902 — A project's secrets are sealed with its own data key, wrapped by the configured provider

- **Status**: Accepted
- **Date**: 2026-11-02

## Context

The server seals everything it stores encrypted with one AES-256-GCM key,
`encryptionKey` (`DISCOBOX_ENCRYPTION_KEY`), through `secrets.Sealer`:

| Purpose | Bound to | Today's resource ID |
| --- | --- | --- |
| `secrets.value` | a project | `projectID/secretID` |
| `sandboxes.secret_state` | a project | `projectID/sandboxID` |
| `sandbox_inventories.document` | a project | `projectID/sandboxID` |
| `sandbox_access_issuer_keys.private_key` | a project | `projectID/userID` |
| `sandbox_provider_instances.encrypted_config` | a project | `projectID/providerID` |
| `worker_agent_request_issuer.private_key` | the server | `worker_agent_request_issuer` |

Whoever holds that key reads every project's secrets, and a hosted discobox
run by its operators puts it in their configuration. A hosted server needs
the key that protects secrets to live in a KMS that never releases it, a key
per project so one project's exposure is not every project's, and a KMS audit
log that names the project behind each unwrap. A self-hosted server has no KMS
and must keep working with a key of its own. With no key set, values are
stored unsealed, which stays supported.

`github.com/discobox-ai/envelope` (its ADR 26-10-12-681) provides this: a
`Keyring` that seals a value with its scope's data key, stores data keys
wrapped by a `Provider`, and has providers for a local key, AWS KMS, Google
Cloud KMS, Azure Key Vault, and Vault or OpenBao transit.

This is step one of keeping the operator out. It protects secrets at rest,
from a database copy, a backup, or someone with database access but not KMS
access. It does not stop an operator who controls the server process and its
KMS credentials; see Deferred.

## Decision

### 1. The server seals through an `envelope.Keyring`

`secrets.Sealer` and `AESGCMSealer` are replaced by the `Keyring`. Every
sealed value names a **scope** and its **associated data**:

- The four project-bound purposes use the project ID as the scope, and
  `purpose/resourceID` as the associated data, with today's resource IDs. A
  value still cannot be moved to another row, field, or project.
- `worker_agent_request_issuer.private_key` uses the reserved scope `server`.
  Project IDs carry the `proj_` prefix, so the two cannot collide.

Data keys are created on first use and kept unwrapped for
`encryption.cacheTTL` (five minutes by default). A secret resolved for the
proxy costs one provider call per project per TTL, not one per request.

### 2. Wrapped data keys live in an `encryption_keys` table

The table has columns `scope`, `id`, `provider`, `provider_key_id`,
`wrapped`, and `created_at`, with primary key `(scope, id)`. The store
implements `envelope.KeyStore` over it. A row holds nothing usable without
its provider.

Deleting a project deletes its rows (`Keyring.Destroy`) in the same flow that
deletes its secrets. Copying a project already opens each value and re-seals
it under the copy (ADR 0023), which now means under the copy's own data key.

### 3. The provider is configured in `server.yaml`

Configuration goes under one `encryption` block. Each setting also has a
`DISCOBOX_ENCRYPTION_*` environment variable, as every server setting does.

```yaml
encryption:
  provider: awskms     # local | awskms | gcpkms | azurekeyvault | vaulttransit
  previous: local      # optional; unwraps but never wraps, during a move
  cacheTTL: 5m
  local:
    key: file:/run/secrets/discobox-key   # base64 of 32 bytes
    retiredKeys: []                       # earlier local keys, unwrap only
  awskms:
    key: arn:aws:kms:us-east-1:111122223333:alias/discobox
    region: ""         # taken from an ARN; required for a bare ID or alias name
  gcpkms:
    key: projects/P/locations/L/keyRings/R/cryptoKeys/K
  azurekeyvault:
    vaultURL: https://example.vault.azure.net
    key: discobox
  vaulttransit:
    address: https://vault.internal:8200
    namespace: ""      # Vault Enterprise / OpenBao namespace
    mount: transit
    key: discobox
    auth: token        # token | kubernetes
    token: file:/run/secrets/vault-token
    role: discobox     # auth: kubernetes
```

- **Credentials** for the clouds come from each SDK's standard chain,
  never from `server.yaml`. A server on EKS, GKE, or AKS with a workload
  identity sets nothing beyond the key. Elsewhere it sets that cloud's own
  variables (`AWS_*`, `GOOGLE_APPLICATION_CREDENTIALS`, `AZURE_*`).
- **Vault and OpenBao** are configured fully here, because neither has an
  ambient identity. `auth: kubernetes` logs in with the pod's service-account
  token and renews the resulting token. A static token is accepted for
  development and for deployments that rotate it externally.
- **`file:`** is accepted by `encryption.local.key`,
  `encryption.local.retiredKeys`, and `encryption.vaulttransit.token`. The
  server reads the file at startup. Kubernetes, systemd, and Docker all
  deliver secrets as files.
- **A misconfigured provider fails startup.** Each provider's `New` checks
  that its key exists and can wrap before the server serves anything.
- **`encryptionKey` is retired** into `encryption.local.key`. A server that
  sets only `encryptionKey` runs as `provider: local` with that key, and logs
  a warning naming the new setting. Setting both is a startup error. No
  provider and no key keeps today's behaviour: values are stored unsealed.

### 4. Existing values are re-sealed at startup, once

`discobox:v1:` values are opened with the old AES key and re-sealed as
`envelope:v1:` values. Unsealed values are sealed if a provider is now
configured.

- The old AES key is `encryption.local.key`. A server moving straight to a
  KMS configures `previous: local` with the local key set until the re-seal
  is done.
- The re-seal runs after `DB.Migrate`, one table at a time, in batches, each
  batch in a transaction. Each row is selected by its prefix, so a re-run
  skips what is done and an interrupted run resumes.
- Until it finishes, reads accept all three forms: `envelope:v1:`,
  `discobox:v1:` (with the local key), and unsealed. Afterwards the server
  logs that the old format is gone. Opening `discobox:v1:` is removed in the
  release after this one.
- If a `discobox:v1:` value is found and no local key is configured, the
  server refuses to start and names the setting. It does not skip the value.

### 5. A change of provider or wrapping key is a rewrap, also at startup

When `previous` is set, or a stored key's `provider_key_id` differs from the
current provider's key ID, the server runs `Keyring.Rewrap` for every scope
with such keys. Data keys do not change, so no sealed value is touched. The
work is the number of scopes, not the number of secrets.

`previous` is removed from the configuration once the log reports that no
key remains on it.

### 6. The server links every provider

`discobox-server` and `discobox-secret-dump` import all five provider
packages, so one binary runs on any of them.

`discobox-secret-dump` builds the same `Keyring` from the same configuration.

## Alternatives rejected

- **A KMS key per project.** The key policy could name each project, and a
  customer could later be handed theirs. Rejected: KMS keys cost money and
  quota per key per month, and envelope's per-key `provider_key_id` already
  allows a different wrapping key for one project later (Deferred) without
  paying for that now.
- **One server-wide data key, wrapped by the KMS.** Rejected: the KMS log
  would no longer name the project, and deleting a project could not make its
  values unreadable.
- **Keep `secrets.Sealer` and add a KMS-backed implementation of it.** The
  interface takes `purpose` and `resourceID`, and a project-aware
  implementation would have to parse the project out of the resource ID.
  Rejected: every caller already knows its project. Passing the scope
  explicitly is the honest shape, and a parsing shim is the kind of
  compatibility layer this repository avoids.
- **Cloud credentials in `server.yaml`.** Rejected: every hosted deployment
  this is for has an ambient workload identity, and a long-lived cloud key in
  a config file is the very thing step B (Deferred) is meant to remove.
- **Build tags to leave out unused SDKs.** This would make a smaller
  `discobox-server`. Rejected for now: one binary that runs anywhere is
  simpler to ship and test, and the size cost is a few megabytes. Revisit if
  binary size becomes a release constraint.
- **Re-sealing lazily, on each value's next write.** Rejected: a secret that
  is never written again would stay under the old key forever, and the old
  key could never be retired.
- **Keeping `encryptionKey` as a separate setting beside `encryption.local`.**
  Rejected: it would be two names for one key, and the question of which one
  wins.

## Consequences

- `github.com/discobox-ai/envelope` must be published before the server
  can depend on it.
- The server's `go.mod` gains the AWS, Google Cloud, Azure, and Vault SDKs,
  and the binary grows accordingly.
- An existing self-hosted server keeps working unchanged. Its key becomes the
  local provider's key, and its first start re-seals its values.
- Destroying a project's keys, or revoking the server's KMS access, takes up
  to `cacheTTL` to reach every server process.
- A server whose KMS is unreachable cannot open a project's secrets once that
  project's key leaves the cache. The proxy's credential swap fails closed.
- `secrets.Sealer`, `AESGCMSealer`, and `store.WithSealer` are removed. The
  store, `auth/sandbox`, and `auth/poolagent` take the `Keyring`.
- This is at-rest protection. An operator who controls the server process and
  its KMS credentials can still read secrets.

## Deferred

- **Keeping the operator out entirely.** Wrapping keys would be released only
  to an attested enclave running a reproducible, published build: an AWS
  Nitro Enclave, GCP Confidential Space, or a confidential VM. Pools would
  decrypt on the customer's side. Revisit when the hosted discobox must show
  that its operators cannot read secrets. The `Provider` interface is where
  that attaches.
- **A project's own wrapping key**, held in the customer's KMS. Revisit
  when a customer asks for it. `encryption_keys.provider_key_id` already
  records the wrapping key per data key; what is missing is per-project
  configuration of the provider.
