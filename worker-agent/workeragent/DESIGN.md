# Worker Agent Design

This package implements the in-guest startup behavior for VM-backed sandbox
workers.

## Startup Flow

1. The VM receives bootstrap settings from cloud-init, kernel command-line args,
   or environment variables.
2. The worker agent reads the settings into the root `workerbootstrap.Bootstrap`
   contract re-exported by this package.
3. The agent generates or loads an Ed25519 worker keypair.
4. The agent registers with the control plane using worker ID, bootstrap token, and public key.
5. The control plane validates the bootstrap token, stores the worker public key, and returns a short-lived runtime auth token.
6. The worker uses the runtime token for work subscription and status updates.

After registration, the worker periodically reports scheduling status and local
pressure details. It sets `ready`, `schedulable`, and `degraded` booleans for
control-plane scheduling. Richer pressure/condition details can be sent as an
opaque JSON blob for display. The worker should set `schedulable=false` when
local policy says no additional sandbox work should be accepted. It may set
`degraded=true` when it can accept fallback work but should not be preferred.

## Worker-Local HTTP Server

After registration, the worker agent should run an HTTP server for provider
runtime operations against sandboxes hosted on that worker. The logical route
shape is:

```text
/api/project/{project_id}/worker/{worker_id}/...
```

The generic VM provider reaches this server through a VM-driver-provided HTTP
client lease. Callers use the logical base URL `https://worker`; individual
drivers may rewrite that URL to localhost, a Unix socket, container networking,
private VM networking, or a temporary tunnel. The Docker driver does not need to
serve HTTPS when the rewritten endpoint is localhost-only.

The worker agent must reject requests whose `{project_id}` or `{worker_id}` do
not match its bootstrap identity. It must also reject sandbox operation requests
without a valid worker-local bearer token provisioned through bootstrap or
registration. Sandbox operation handlers under this route own only local runtime
work; control-plane persistence, user authorization, project events, and
desired-state orchestration remain outside this package.


## Package Layout

The root `workeragent` package owns the boot contract, registration flow, and
high-level command orchestration. Runtime-heavy or platform-specific concerns
live in subpackages to keep the root package small:

- `server`: worker-local HTTP server, health/metadata endpoints, and sandbox API
  route/auth handling. Worker-local sandbox routes are registered with Huma, the
  same API style used by the public control-plane server, so the worker API can
  have its own OpenAPI document and generated client.
- `sandboxruntime`: local sandbox runtime implementations used by the worker
  server, including Docker-backed and in-memory runtimes.
- `systemd`: Linux/systemd namespace startup and child reaping helpers, with
  non-Linux stubs.

## Package Boundary

The package is intentionally independent of VM drivers. VM drivers only need to
pass the bootstrap settings into the guest. The worker agent package owns reading
those settings, generating worker identity keys, and calling the registration
client.
