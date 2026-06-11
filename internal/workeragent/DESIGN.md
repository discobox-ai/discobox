# Worker Agent Design

This package implements the in-guest startup behavior for VM-backed sandbox
workers.

## Startup Flow

1. The VM receives bootstrap settings from cloud-init, kernel command-line args,
   or environment variables.
2. The worker agent reads the settings into `Bootstrap`.
3. The agent generates or loads an Ed25519 worker keypair.
4. The agent registers with the control plane using tenant ID, worker ID,
   bootstrap token, and public key.
5. The control plane validates the tenant-scoped bootstrap token, stores the
   worker public key, and returns a short-lived runtime auth token.
6. The worker uses the runtime token for work subscription and status updates.

After registration, the worker periodically reports scheduling status and local
pressure details. It sets `ready`, `schedulable`, and `degraded` booleans for
control-plane scheduling. Richer pressure/condition details can be sent as an
opaque JSON blob for display. The worker should set `schedulable=false` when
local policy says no additional sandbox work should be accepted. It may set
`degraded=true` when it can accept fallback work but should not be preferred.

## Tenant Requirement

Tenant ID is required during bootstrap. For SQLite sharding, the control plane
needs the tenant ID before it can open the correct tenant database and validate
the worker bootstrap token. For Postgres, the same tenant ID is used as the
row-level authorization/query boundary.

## Package Boundary

The package is intentionally independent of VM drivers. VM drivers only need to
pass the bootstrap settings into the guest. The worker agent package owns reading
those settings, generating worker identity keys, and calling the registration
client.
