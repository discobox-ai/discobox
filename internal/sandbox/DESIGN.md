# Sandbox Provider Design

This package defines the runtime provider interface used by service
reconciliation. Providers own runtime mechanics only; services own persistence,
authorization, orchestration, and API shape.

## Tenant-Aware Runtime References

`SandboxRef` carries `tenant_id`, `project_id`, and `sandbox_id`.

- `tenant_id` is the database shard key and must be propagated into VM worker
  boot metadata.
- `project_id` scopes provider placement, shared caches, VM selection, resource
  settings, and cleanup.
- `sandbox_id` identifies the managed runtime resource.

## VM Provider Abstraction

`internal/sandbox/vm` is the generic VM-backed provider layer. It adapts the
common `sandbox.Provider` interface to a smaller VM-driver interface:

- create a VM from an `InstanceSpec`,
- start/stop/delete/inspect a VM by instance ID,
- optionally provide an HTTP client lease to reach the sandbox agent.

Drivers should be thin platform integrations for KVM, HCS, Apple
Virtualization, AWS, Azure, GCP, or similar VM backends. The generic VM provider
owns Disco-specific boot metadata, worker bootstrap parameters, provider state
serialization, and conversion to `sandbox.Sandbox` runtime state.

## VM Boot Metadata

VM drivers receive boot metadata in multiple common forms:

- environment variables,
- kernel command-line arguments,
- cloud-init user-data,
- cloud-init meta-data.

Drivers should pass the form their backend supports. The boot metadata includes
the control plane URL, tenant/project/sandbox identity, worker ID, bootstrap
token, and agent port. This allows the in-guest worker agent to register itself
with the control plane after the VM boots.

## DigitalOcean Driver

`internal/sandbox/vm/digitalocean` implements the VM driver contract with one
DigitalOcean Droplet per sandbox worker.

Provider instances use type `digitalocean`. Configuration includes the
DigitalOcean API token, control plane URL, region, size, image, SSH keys, VPC
UUID, tags, and feature flags such as backups, IPv6, and monitoring. The token
can be supplied directly by provider config or loaded from an environment
variable such as `DIGITALOCEAN_ACCESS_TOKEN`.

The driver passes cloud-init user-data from the generic VM boot contract to the
Droplet create API so the guest worker agent can start and register itself.

## Pull-Based Scheduling and Worker Conditions

VM-backed providers maintain prewarmed workers for each provider instance. Each
provider instance is treated as one homogeneous warm pool; heterogeneous
capacity should be modeled as multiple provider instances.

Workers initiate scheduling by polling or subscribing for work. The control
plane should not rely on rigid slot counts because sandbox resource requests can
be heavily overprovisioned and real pressure depends on local compute, memory,
storage, cache, and runtime state. Instead, workers report three
scheduling-relevant booleans on their worker row:

- `ready=true`: the worker is alive and healthy.
- `schedulable=true`: the worker is willing to pull new sandbox work.
- `degraded=true`: the worker may be used as fallback capacity but should not
  be preferred.

Workers may also report richer pressure/condition details as an opaque JSON blob
for display. The control plane does not interpret that blob for scheduling.

Scheduling preference is therefore:

1. preferred workers: ready and schedulable, not degraded;
2. degraded workers: ready and schedulable, degraded, used only when necessary;
3. unavailable workers: not ready, not schedulable, drained, revoked, or
   deleted.

Pool reconciliation should scale up when pending work is not being claimed by
preferred or degraded workers within policy, and scale down by draining/removing
idle workers above the warm target.
