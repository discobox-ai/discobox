# 0005 — Kubernetes backend is a worker driver

- **Status**: Proposed
- **Date**: 2026-07-17

## Context

Production targets Kubernetes; development uses the local Docker backend
heavily (with k3s-in-docker as the default way to exercise the k8s path).
The dominant constraint is divergence: any k8s design that reimplements
sandbox creation forks source delivery, manifest delivery, proxy topology,
sandbox-agent connectivity, and auth into a second implementation that must
stay functionally in sync with the docker/VM path forever.

The provider stack already isolates backend differences at one seam:
`dockerworker.Driver` is "pure VM CRUD plus two connection leases"
(`server/providers/DESIGN.md`), with everything above it — workerpool
provider, engine, worker-agent, sandboxruntime, source delivery, proxy,
sandbox-agent API — backend-independent. `execvm` exists as proof the seam
needs nothing Docker-shaped on the driver side.

ADR-0003 makes the pool the unit of sharing (cache, overcommit envelope,
isolation boundary) that discobox itself must schedule; ADR-0004 removes the
privilege objections to running a nested runtime in a pod.

## Decision

### 1. The k8s backend is a `dockerworker.Driver`

A pool's worker runs as **one pod**: worker-agent + rootless nested runtime
(per ADR-0004), with a PVC for `/var/lib/discobox` (pool cache, sandbox
volumes, proxy material). Sandboxes are containers inside that pod's runtime,
exactly as on every other backend.

- `EnsureVM` / `DeleteVM` / `InspectVM`: create/delete/inspect the worker pod
  and PVC, keyed by worker ID.
- `AcquireDockerClient`: dial the in-pod daemon — `NewDockerClientForDialer`
  adapts a pod-IP or port-forward `net.Conn` dialer, the same shape as
  `sshdocker` for cloud VMs.
- `AcquireWorkerAgentClient`: pod IP + harness port in-cluster; API-server
  proxy or port-forward when the control plane is outside the cluster.

Nothing above the driver seam changes. Bootstrap identity travels as
container env (`dockerworker.BootEnv`) like every backend; the pod spec is
generated from the pool's isolation profile (ADR-0004), so hardening is
configuration of one driver, not new architecture.

### 2. Two-level scheduling

Kubernetes schedules worker pods; discobox schedules sandboxes into pools.
This is deliberate: kube-scheduler has no concept of "this group shares one
overcommitted envelope and one writable cache," and the pool is exactly that
concept. Capacity pressure maps naturally — a worker pod Pending past a
deadline is `ErrNoSandboxCapacity`, and cluster-autoscaler handles node
supply.

### 3. Storage placement

The nested runtime's image/graph store lives on local ephemeral storage, not
the PVC: nested overlayfs on network volumes is the known performance
landmine, and image state is reconstructible. Durable pool state (cache,
sandbox home/source volumes, proxy material) lives on the PVC so
`RepairWorker` — replace the pod, keep the PVC — preserves sandboxes exactly
as container replacement does on the Docker backend.

## Rejected

- **Native provider: one k8s pod per sandbox.** Initially the recommended
  design, reversed when the pool requirements surfaced — recorded because the
  reversal is non-obvious. Per-sandbox pods make each sandbox visible to
  Kubernetes but cannot express a shared writable cache (forces RWX volumes
  or hostPath + node-affinity reconstruction of the pool) or a shared
  overcommit envelope (burstable QoS approximates it per pod, invisibly to
  users). Decisively, it forks the entire sandbox-creation path (init
  containers for source materialization, manifest delivery, per-pod proxy
  sidecars, a provider-neutral lease URL space) into a second implementation
  needing permanent functional sync with the worker path — the maintenance
  cost this architecture exists to avoid. Per-sandbox k8s-native visibility
  (NetworkPolicy, metrics) is not needed: egress control lives in the
  discobox proxy and per-sandbox resources are reported by the worker.
- **Implementing `sandbox.Provider` directly, bypassing workers.** Same fork
  as above one layer higher, and it abandons the pool/worker machinery
  (scheduling, repair, registration, drift) that k8s still needs — a pod is a
  worker host like any VM, not a reason for a new control path.
- **A new backend seam other than `dockerworker.Driver`.** The driver seam
  was sized exactly for "add a backend without reading the engine"; inventing
  a parallel seam for k8s would duplicate the engine contract. If the driver
  seam ever fits k8s poorly, the fix is evolving the one seam, not adding a
  second.

## Deferred

- **Sandboxes as first-class k8s objects** (pod-per-sandbox). Revisit only if
  a hard requirement emerges for per-sandbox kube-native policy (NetworkPolicy,
  per-sandbox service mesh) that the proxy cannot satisfy.
- **Worker pod disruption tuning** (priority class, eviction behavior,
  graceful drain on node maintenance). Needs real cluster experience; the
  repair path bounds the blast radius to one pool meanwhile.
- **Out-of-cluster control plane transport choice** (API-server proxy vs
  long-lived port-forward vs an in-cluster gateway) — decide during
  implementation with latency data; the lease abstraction hides the choice.
