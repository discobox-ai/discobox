# DigitalOcean warm worker pools

DigitalOcean provider instances use the Phase 7 Option B warm-pool model: a provider instance owns a pool of prewarmed worker droplets, and sandbox creation claims an already-registered, schedulable worker instead of creating one droplet per sandbox.

## CLI flow

Create a DigitalOcean provider and bootstrap its warm pool:

```bash
export DIGITALOCEAN_ACCESS_TOKEN=...
disco2 provider create \
  --name do-warm \
  --type digitalocean \
  --control-plane-url https://discobot.example.com \
  --do-region nyc3 \
  --do-size s-1vcpu-1gb \
  --pool-size 1
```

Inspect provider support and manage instances:

```bash
disco2 provider catalog
disco2 provider list
disco2 provider get <provider-id>
disco2 provider update <provider-id> --name do-warm-2 --do-size s-2vcpu-2gb
disco2 provider delete <provider-id>
```

Then create a sandbox against that provider and wait for reconciliation:

```bash
disco2 sandbox create --name dev --provider-instance <provider-id> --wait
```

If the project has no default provider, the first created provider instance becomes the default. Later sandbox creates can omit `--provider-instance`.

## Config

Provider config is stored on the tenant-scoped `sandbox_provider_instances` table. Non-secret config is accepted as JSON with keys such as:

- `tokenEnv` (preferred) or `token`
- `controlPlaneUrl`
- `apiBaseUrl` (useful for tests/fake DigitalOcean servers)
- `region`, `size`, `image`, `sshKeys`, `tags`
- `poolSize`

## Worker model

Workers are tenant/project/provider-instance scoped and do not have `sandbox_id`. One worker can host many sandboxes. Scheduling is controlled by columns:

- `ready`
- `schedulable`
- `degraded`

Detailed worker-reported health is stored in opaque `conditions` JSON. The control plane prefers ready/schedulable/non-degraded workers and falls back to degraded workers.

## Worker authentication

Worker registration returns a short-lived auth token. Workers must send it on status updates as:

```http
Authorization: Bearer <auth-token>
```

The control plane stores only token hashes in the tenant-scoped `worker_auth_tokens` table and rejects missing, invalid, expired, or revoked tokens before accepting `ready`/`schedulable`/`degraded` updates.

## Tenant/global schema separation

Only global identity tables live in the global schema/database. Provider instances, workers, worker bootstrap/auth tokens, sandboxes, project events, and jobqueue tables are tenant-scoped and are included only in tenant migrations.
