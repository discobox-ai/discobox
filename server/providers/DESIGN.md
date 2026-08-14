# Sandbox Provider Design

This package contains concrete sandbox provider implementations and factory
registration. It consumes `server/internal/sandbox` for the Go-level provider
interface, provider manager, typed `PoolManager` control-plane surface, and
shared provider types. It also consumes server-owned persistence models and
the pool-agent package for pool host boot metadata.

Providers own runtime mechanics only; services own persistence, authorization,
orchestration, and API shape.

A pool is its own runtime host (ADR-0006): one container, VM, or pod runs the
pool agent and hosts the pool's sandboxes.

## Runtime References

`SandboxRef` carries `project_id` and `sandbox_id`.

- `project_id` scopes provider placement, shared caches, VM selection, resource
  settings, and cleanup.
- `sandbox_id` identifies the managed runtime resource.

## Provider Layers

Docker container management is the invariant: every backend ends with "run the
pool-agent container in some Docker daemon." Backends differ only in VM CRUD
and how to reach that daemon and the pool-agent API.

```mermaid
flowchart TD
    pool["poolruntime.Provider\nimplements sandbox.Provider\nplacement gate · pool-agent API (docker-free)"]
    engine["dockerworker.Engine\nthe one poolruntime.RuntimeProvider\npool-agent container, networks, volumes, drift"]
    driver["dockerworker.Driver\nVM lifecycle + two connection leases"]
    local["docker.LocalDriver\nVM CRUD no-op · host socket ·\npublished loopback agent port"]
    do["digitalocean.Driver\ndroplet CRUD by pool tag ·\ndocker over SSH · agent at public IP"]
    execd["execvm.Driver\ndelegates every op to an external\ncommand (shell-script backends)"]
    libkrun["libkrun.Driver\nlibkrun process · persistent disks ·\nUnix/VSOCK connection leases"]
    future["(later) k8s / ec2 / apple / windows\nsame shape; pool runs as a pod on k8s"]

    pool --> engine --> driver
    driver --> local & do & execd & libkrun & future
```

`server/providers/poolruntime.Provider` is the registered `sandbox.Provider`
for pool-backed provider instances. It owns sandbox placement gating (the
sandbox's pool must be ready and schedulable — there is no candidate search
and no capacity check; sandboxes share their pool's CPU/memory/storage
envelope with no per-sandbox reservation, docs/adr/0029), capacity waits,
bootstrap credential minting, pool runtime convergence
(`sandbox.PoolRuntimeReconciler`), and user-sandbox operations through the
pool-agent API. Docker never appears at this layer or above: the boundary
contract downward is the `poolruntime.RuntimeProvider` interface, and the
runtime contract for sandboxes is the pool-agent HTTP API reached through
`transport.HTTPClientLease`.

`poolruntime.RuntimeProvider` is a five-method interface: `Close`,
`EnsurePool`, `RepairPool`, `RemovePool`, and `AcquirePoolAgentClient`.
`dockerworker.Engine` is its only implementation. The engine owns everything
Docker: launching the pool-agent container with boot env, socket bind and host
mounts, scoped volumes, the per-pool sandbox proxy network, health waits,
config-revision drift detection, container replacement during repair, and
applying the pool envelope (CPU/memory) as the container limit. A sandbox
container created inside it gets no nested CPU/memory limit of its own — it
shares the worker container's cgroup with its siblings (docs/adr/0025). It
obtains Docker access exclusively through the driver.

`dockerworker.Driver` is the backend seam sized for "add EC2 without reading
the engine":

- `EnsureVM` / `StopVM` / `DeleteVM` / `InspectVM`: idempotent VM lifecycle
  keyed by pool ID. `StopVM` preserves driver-owned persistent state for
  repair; `DeleteVM` removes it after pool deletion is authorized. The local
  Docker driver resolves every pool to the host and lifecycle is a no-op.
- `AcquireDockerClient`: a Docker API client lease for the daemon hosting the
  pool's containers — the host socket locally, the in-VM daemon over SSH for
  DigitalOcean, or VSOCK terminated at a private Unix socket for local libkrun
  VMs. `NewDockerClientForDialer` adapts any `net.Conn`
  dialer; `dockerworker/sshdocker` is the shared pure-Go SSH-to-docker-socket
  dialer for cloud VM drivers and for `ssh://` endpoints from the exec driver.
- `AcquirePoolAgentClient`: an HTTP lease reaching the pool-agent API — the
  container's published loopback port locally, `http://<public-ip>:<agent
  port>` for cloud VMs, or VSOCK terminated at a private Unix socket for local
  libkrun VMs.

The engine owns Docker readiness waiting after `EnsureVM` (ping with a
deadline), so drivers never implement boot polling.

## Development Image Convergence

The development image watcher publishes a versioned manifest of
content-addressed pool, sandbox-base, and harness images. When its explicit
environment flag is enabled, server composition gives one shared
`dockerworker.DevelopmentImageSynchronizer` to every engine.

After `EnsureVM` and Docker readiness, both `EnsurePool` and `RepairPool`
converge that manifest before reconciling the pool-agent container. The
synchronizer inspects the destination daemon by reference and image ID, retags
an already-present ID without transferring it, and otherwise streams one
compressed multi-image archive from the developer's Docker daemon into the
driver-provided Docker client. Calls for the same daemon ID and manifest
coalesce. Destination inspection is the durable truth; no synchronized-host
state is persisted.

This is engine behavior, not a driver capability. Local Docker therefore
usually performs no transfer, while libkrun, DigitalOcean, and exec drivers
receive the same images over their existing VSOCK, SSH, or configured Docker
transport. Synchronization failures fail pool reconciliation rather than
allowing a host with incomplete development images to become ready.

## Image Reclamation

The engine also reclaims what it put on a daemon. Discobox images are labeled at
build time (`harness.ReclaimLabel`), and a labeled image is removed once no
container refers to it and it has been on the daemon longer than the retention
window. The rules live in `imagereap`, shared with the pool agent, which applies
the same pass to its own daemon; ADR 0039 covers why local arrival time rather
than the image's `Created` timestamp decides staleness.

The window is 24h, or 15m when `DevelopmentImageSync` is set — the image watcher
supersedes an image every few minutes, so the production window would reclaim
nothing before the disk filled. That field is the whole development signal;
there is no separate mode flag. An explicit `DISCOBOX_IMAGE_RETENTION` overrides
both, and the engine propagates whatever it resolves into the pool container's
environment, so one setting governs the host daemon and every pool daemon under
it. The sweep interval is derived from the window (half of it, clamped to
[1m, 1h]) rather than configured, which is also how a pool inherits the
development cadence: it is handed a retention, not a mode.

Reclamation runs from two places, because the two daemons are reached
differently. `EnsurePool` reclaims on the daemon behind the pool it just
reconciled, throttled to once per interval per pool: a VM backend has no
long-lived host Docker client for a standing loop to hold, and one superseded
pool image per upgrade is exactly what an upgrade's own reconcile then clears.
The local Docker provider additionally runs a standing hourly loop over the host
daemon, where development rebuilds deposit an image per build with no pool
activity required to notice.

`Engine.imageKeepReferences` is what the container check cannot infer: the
configured pool image, which has no container while every pool is stopped, and
the development image set, whose sandbox base is run by nothing yet is the base
every harness image is built `FROM`.

Pool runtime lifecycle is not the same as pool row deletion. The engine
replaces the pool-agent container (and a VM driver may replace the VM) for an
existing pool during reconciliation or repair, but the pool row and pool ID
are preserved: recovery is strictly in-place, and pool-local state survives in
named Docker volumes. Pool row deletion is intent-based and allowed only after
the control plane proves no sandbox is assigned to the pool.

`RepairPool` is the recovery hook for pools whose runtime is known to be
unhealthy, including a runtime whose agent never registered within the
registration timeout. The engine replaces the container and replaces the VM
only when `InspectVM` reports it missing or unhealthy.

The control plane launches the pool-agent container over the VM's Docker
daemon on every backend. Cloud VM images therefore stay generic: DigitalOcean
cloud-init only installs and enables Docker; bootstrap identity travels as
container environment rendered by `dockerworker.BootEnv`.

## Pool Runtime Drift

Runtime drift detection is backend-owned. The local docker provider runs a
watcher over the shared daemon: it lists managed pool containers, compares
them with pool rows, and uses the engine's config revision
(`Engine.ShouldReconcileWorkerContainer`) for drift. For pool rows that still
exist it marks the pool dirty; the only direct side effect allowed is deleting
an orphan managed runtime (the pool container) with no pool row. VM-per-pool
backends get drift detection through `InspectVM` during normal reconciliation.

The server-created runtime (pool container, network, named volume) is the
watcher's / `RemovePool`'s job; the *agent-created* footprint (sandbox
containers and host data/proxy subtrees) is reaped by the pool agent itself.
On each successful `ReconcilePool`, the provider hands the now-ready agent the
authoritative pool set via the `pool-sync` API, so a shared host daemon reclaims
whole orphaned pools without the control plane re-deriving the agent's teardown
logic. This is a no-op on isolated per-pool daemons.

The watcher's initial drift scan and its event loop both run in the
background: provider initialization starts the watcher and returns
immediately. The initial scan is best-effort — its failures are logged, never
fatal. Drift detection is only about pool host runtimes: it must not inspect,
reconcile, or delete user sandbox containers hosted inside a pool; those
belong behind the pool-agent sandbox runtime API. It must never delete a
persisted pool row.

## Pool Failure and Recovery

Terminal failure is reserved for pools whose *initial create* never
succeeded. A pool is stateful once its runtime has completed create and its
agent registered (`Pool.EverCreated()`, keyed off `RegisteredAt`): it may own
runtime volumes and assigned sandboxes, so the control plane makes every
effort to reconcile it back to health instead of abandoning it.

- A failed reconcile of a never-created pool latches the terminal `failed`
  phase; a created pool drops to the non-terminal `offline` phase, not ready
  and not schedulable, and is re-driven (`SchedulePoolRepair` bumps the
  generation so schedulers can tell a pending retry from a settled failure).
- A reconcile failure while sandboxes are assigned repairs the runtime in
  place rather than recording the failure.
- The docker watcher re-marks a created but failed/offline pool even when its
  container is gone, so a missing runtime is recreated.

## Pool Agent HTTP Routing

Transport leasing is represented by `server/internal/transport.HTTPClientLease`.
The provider obtains pool-agent connectivity from
`RuntimeProvider.AcquirePoolAgentClient` and attaches per-request token
providers to the lease so credentials are minted close to use and are not
cached as driver or lease state.

The provider-facing logical URL space for a pool agent is:

```text
https://pool/api/project/{project_id}/pool/{pool_id}/...
```

The `https://pool` authority is a stable logical authority, not necessarily a
real DNS name. Drivers translate it into something that reaches the in-guest
pool-agent HTTP server. The pool agent validates that `{project_id}` and
`{pool_id}` match its bootstrap identity before performing any operation, and
sandbox routes require a short-lived scoped bearer token signed by the control
plane.

## Sandbox Pool API

The pool-agent HTTP server owns sandbox runtime operations inside one pool.
The control plane still owns persistence, authorization, events, and desired
state; calling the pool API must be done from reconciliation or provider
operations after intent has already been accepted and stored. The canonical
contract is `pool-agent/api/openapi/pool.yaml`; operation endpoints are
synchronous from the pool's perspective.

## Control Plane Reachability

A pool agent dials the control plane; the control plane does not dial it. The
transport comes from what the server is actually listening on, because HTTP is
opt-in (see [server](../DESIGN.md#listen-endpoints)):

| Backend | Agent reaches the control plane by |
| --- | --- |
| `libkrun` | VSOCK to the host CID |
| `wslc` | `unix://` socket served by the in-guest relay |
| `docker`, local daemon | `unix://` the control plane's own socket, bind-mounted into the pool container at the same path (`Config.RelaySocketDir`) |
| `docker`, remote daemon | the configured HTTP listener, rewritten to the container-resolvable host-gateway address |
| `digitalocean`, `execvm` | explicit `controlPlaneUrl`, required at config time |

The Docker provider prefers the socket whenever its daemon is local, which is
what lets a default install open no TCP port at all. A remote daemon cannot see
that socket, so it needs an HTTP listener; when none is configured, provider
instantiation fails with that as the message rather than producing a pool that
starts and never registers.

## Pool Boot Metadata

Bootstrap identity — control plane URL, project/pool identity, bootstrap
token, control-plane trust key, agent port — is rendered as container
environment by `dockerworker.BootEnv` and injected into the pool-agent
container by the engine, uniformly on every backend. VM drivers only need
their platform's Docker bring-up; they never carry bootstrap secrets in VM
user data.

## DigitalOcean Driver

`server/providers/digitalocean` implements the `dockerworker.Driver` contract
with one Docker-enabled Droplet per pool, keyed by the
`discobox-pool-<pool_id>` tag. Configuration includes the API token, control
plane URL, region/size/droplet image, pool-agent container image, registered
SSH keys plus the matching SSH private key, VPC UUID, tags, and feature flags.

## Exec Driver

`server/providers/execvm` implements the `dockerworker.Driver` contract by
invoking an external command as `<command> <op> <pool-id>`, so a pool backend
can be a shell script. The protocol is documented in the `execvm` package doc;
it exists both as an escape hatch and as the proof that the driver seam needs
nothing Docker-shaped.

## Placement

Placement is a gate, not a search: `SchedulablePoolForSandbox` verifies the
sandbox's pool is active, ready, schedulable, unrevoked, and that the request
fits the pool's agent-reported capacity. When the gate fails, the provider
kicks the pool reconcile and waits bounded time for the agent to report
schedulable, surfacing a settled pool failure (latest intent attempted and
lost) with its recorded cause instead of a bare capacity error.
