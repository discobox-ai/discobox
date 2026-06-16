# Sandbox Provider Design

This module contains concrete sandbox provider implementations, factory
registration, and reusable VM-backed provider adapters. It consumes the root
`sandboxprovider` package for the Go-level provider interface, provider manager,
and shared provider types. It also consumes root contracts such as `model` and `workerbootstrap` for
resource models and worker boot metadata.

Providers own runtime mechanics only; services own persistence, authorization,
orchestration, and API shape.

## Runtime References

`SandboxRef` carries `project_id` and `sandbox_id`.

- `project_id` scopes provider placement, shared caches, VM selection, resource
  settings, and cleanup.
- `sandbox_id` identifies the managed runtime resource.

## VM Provider Abstraction

`providers/sandbox/vm` is the generic VM-backed provider implementation layer.
It adapts the root `sandboxprovider.Provider` interface to a smaller VM-driver
interface:

- create a VM from an `InstanceSpec`,
- start/stop/delete/inspect a VM by instance ID,
- optionally provide an HTTP client lease to reach the sandbox agent.

Drivers should be thin platform integrations for KVM, HCS, Apple
Virtualization, AWS, Azure, GCP, or similar VM backends. The generic VM provider
owns Disco-specific boot metadata, worker bootstrap parameters, provider state
serialization, and conversion to `sandbox.Sandbox` runtime state.

## Worker Agent HTTP Routing

VM-backed providers must be able to send provider/runtime requests to the
worker agent running inside a worker VM. The generic VM provider obtains that
connectivity through the optional driver method represented by
`HTTPClientDriver.AcquireHTTPClient`. The returned `sandbox.HTTPClientLease`
contains an `http.Client` and any lease cleanup needed for the driver's routing
mechanism.

The provider-facing logical URL space for a worker agent is:

```text
https://worker/api/project/{project_id}/worker/{worker_id}/...
```

The `https://worker` authority is a stable logical authority, not necessarily a
real DNS name. VM drivers are responsible for translating requests for that
logical worker endpoint into something that reaches the in-guest worker-agent
HTTP server:

- local Docker or local VM drivers may rewrite the request to a localhost port,
  Unix socket, container network address, or other host-local endpoint;
- editor/dev drivers may return an `http.Client` whose `RoundTripper` dials a
  VS Code-managed socket while the request URL remains `https://worker/...`;
- remote VM drivers may use direct private networking, a reverse tunnel, SSH
  port forwarding, a provider proxy, or another short-lived transport;
- drivers that require a temporary tunnel should open it in `AcquireHTTPClient`
  and close it from the lease cleanup function.

The lease may either include a concrete `BaseURL`, such as a local forwarded
HTTP endpoint, or omit it and rely on the returned client's transport to handle
the logical `https://worker` authority. Callers must not assume that the worker
endpoint is reachable by the default network stack.

The worker agent should validate that `{project_id}` and `{worker_id}` match its
bootstrap identity before performing any operation. Worker-local sandbox routes
must also require an authenticated request, such as a bearer token provisioned
through bootstrap or registration and carried by the driver's HTTP client lease.
Transport security depends on the driver path: remote paths should use TLS or an
authenticated tunnel; localhost-only Docker paths do not need to expose HTTPS as
long as the driver rewrites the logical URL to the local transport.

## Sandbox Worker API

The worker-agent HTTP server owns sandbox runtime operations inside one worker.
The control plane still owns persistence, authorization, events, and desired
state. Calling the worker API must therefore be done from reconciliation or
provider operations after intent has already been accepted and stored.

The logical API under the worker route is:

| Method | Path | Meaning |
| --- | --- | --- |
| `GET` | `/api/project/{project_id}/worker/{worker_id}/sandboxes` | List sandboxes hosted by the worker. |
| `POST` | `/api/project/{project_id}/worker/{worker_id}/sandboxes` | Create and usually start a sandbox on the worker. |
| `GET` | `/api/project/{project_id}/worker/{worker_id}/sandboxes/{sandbox_id}` | Inspect one sandbox. |
| `PATCH` | `/api/project/{project_id}/worker/{worker_id}/sandboxes/{sandbox_id}` | Update mutable runtime settings supported by the worker. |
| `DELETE` | `/api/project/{project_id}/worker/{worker_id}/sandboxes/{sandbox_id}` | Delete the sandbox runtime and local resources. |
| `POST` | `/api/project/{project_id}/worker/{worker_id}/sandboxes/{sandbox_id}/start` | Start a retained sandbox. |
| `POST` | `/api/project/{project_id}/worker/{worker_id}/sandboxes/{sandbox_id}/stop` | Stop a retained sandbox. |

The worker API intentionally does not expose a restart endpoint. Public restart
intent is accepted by the control plane as `restartGeneration` and reconciled as
the required worker-local stop and start operations. Keeping restart orchestration
in the control plane preserves supersedable desired-state semantics and avoids a
separate worker-local restart operation with its own lifecycle behavior.

Request and response bodies should stay provider/runtime-level DTOs, not API
models from the public control-plane package `internal/api`. They should include
sandbox ID, image/source, working directory, resource requests, environment or
mount settings, and opaque runtime state as needed by the worker implementation.
Responses should return the provider/runtime sandbox shape needed by
`sandbox.Provider` operations: provider runtime ID, sandbox ID, status,
image/source summary, timestamps, addresses/ports, metadata, and opaque state
updates. The worker-agent generated client and schema types represent the
worker-local API contract, not the public control-plane API DTOs, and may be used
by VM providers and worker-local runtimes that call or implement that API.

Operation endpoints should be synchronous from the worker's perspective: when a
worker endpoint returns 2xx, the worker has completed the local runtime action or
has made the requested runtime state observable. The control plane may still
return `202 Accepted` to external clients because the public sandbox API is
intent-based and reconciliation-driven.

The public control-plane sandbox API remains project scoped and desired-state
oriented:

| Method | Path | Accepted intent |
| --- | --- | --- |
| `GET` | `/projects/{projectId}/sandboxes` | List persisted sandbox resources. |
| `POST` | `/projects/{projectId}/sandboxes` | Accept create intent; desired state becomes `running`. |
| `GET` | `/projects/{projectId}/sandboxes/{sandboxId}` | Return persisted desired and observed state. |
| `PATCH` | `/projects/{projectId}/sandboxes/{sandboxId}` | Update persisted sandbox configuration. |
| `DELETE` | `/projects/{projectId}/sandboxes/{sandboxId}` | Accept delete intent; desired state becomes `deleted`. |
| `POST` | `/projects/{projectId}/sandboxes/{sandboxId}/start` | Accept start intent; desired state becomes `running`. |
| `POST` | `/projects/{projectId}/sandboxes/{sandboxId}/stop` | Accept stop intent; desired state becomes `stopped`. |
| `POST` | `/projects/{projectId}/sandboxes/{sandboxId}/restart` | Accept restart intent; increment restart generation. |

Provider operations bridge the two APIs: they use the VM driver's HTTP client
lease to call the worker API, then persist the resulting runtime state through
the normal reconciler flow. The worker API is registered with Huma so it can be
exported as OpenAPI and used with a generated client. Any generated client must
accept an injected `http.Client` so VM-driver transports such as VS Code sockets,
Unix sockets, tunnels, or proxies remain behind the lease abstraction.

## VM Boot Metadata

VM drivers receive boot metadata in multiple common forms:

- environment variables,
- kernel command-line arguments,
- cloud-init user-data,
- cloud-init meta-data.

Drivers should pass the form their backend supports. The boot metadata is built
from the root `workerbootstrap` contract and includes the control plane URL,
project/sandbox identity, worker ID, bootstrap token, and agent port. This
allows the in-guest worker agent to register itself with the control plane after
the VM boots.

## DigitalOcean Driver

`providers/sandbox/vm/digitalocean` implements the VM driver contract with one
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
