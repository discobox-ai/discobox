# 0004 — User namespaces are the default isolation

- **Status**: Proposed
- **Date**: 2026-07-17

## Context

Privilege enters the runtime stack in two independent places today:

1. The worker runs a Docker daemon (DinD when the worker itself is a
   container/pod).
2. Sandbox containers are created with `Privileged: true`
   (`worker-agent/sandboxruntime`) for systemd and for Docker inside the
   sandbox.

Enterprise security review is signal-driven: `privileged: true`, root, added
capabilities, or hostPath in a pod spec is rejected before any conversation
about actual isolation happens. The k8s backend (ADR-0005) puts worker pod
specs directly in front of those scanners.

A second forcing function is self-hosting: discobox is developed inside
discobox, nested arbitrarily deep (sandbox → inner discobox → its sandboxes,
k3s-in-docker as the default dev mode). Privileged nesting requires the host
to grant privilege again at every level; rootless nesting requires nothing
from the host — an unprivileged user who can create containers is the same
situation at every depth, so the model is self-similar. The kernel permits 32
levels of userns nesting; practical depth is bounded by performance, not
correctness.

If dev ran privileged while prod ran rootless, the dev/prod code-path parity
this architecture is built around would silently break at the security layer:
rootless changes real behavior (cgroup delegation, overlay storage, no true
root, userspace networking).

## Decision

### 1. Rootless + user namespaces everywhere, dev and prod

- The baseline runtime posture on every backend is: worker runtime rootless
  (rootless dockerd, or a rootless runtime behind the existing
  `sandboxruntime` interface), sandbox containers unprivileged (systemd via
  cgroup v2 delegation, native rootless overlayfs on kernel ≥ 5.13), and on
  k8s `hostUsers: false` pods.
- The local Docker backend adopts this first: the de-privileging pass on
  `sandboxruntime` and the sandbox image happens where iteration is fast, and
  the k8s driver inherits a worker that already runs clean.
- Privileged mode is retained only as an explicit escape hatch, selected per
  pool, never the default anywhere.

### 2. Isolation is a pool profile

Per ADR-0003, the pool carries an isolation/capability profile. Profiles are
pod-template/runtime-flag configuration behind the same driver seam — never a
second code path:

- `userns-rootless` (default): no privileged, no added caps, non-root or
  root-in-userns, PVC-only storage. Known residual signals to document per
  pool: a custom seccomp profile (mount/userns syscalls), possibly `/dev/fuse`
  on pre-5.13 kernels, `procMount: Unmasked` where a level hosts a nested
  runtime. Yellow flags, negotiable; `privileged` is not.
- `sysbox` (opt-in): unmodified DinD with clean signals via RuntimeClass;
  requires installing the sysbox runtime on nodes, so it is only for clusters
  the operator controls.
- `kata` (opt-in): hardware-virtualized pool boundary for hostile
  multi-tenancy; note the inner spec typically still carries `privileged`
  (guest-safe, but scanners key off the spec field), so it is the regulated
  tenant-isolation story, not the pass-the-scan story.
- `privileged` (escape hatch): today's behavior.

### 3. Docker-in-sandbox is a declared pool capability

Nested Docker inside the sandbox is the one feature that forces double
nesting. It becomes a pool capability flag, off in hardened profiles;
pools that need it accept the rootless-nested setup or a sysbox/kata-backed
pool. It is no longer an assumption baked into the sandbox image contract.

### 4. Nesting invariants

Required at every level for self-hosting to work; baked into images and pool
config, not per-level hand setup:

- **Storage roots chain to a real filesystem.** Overlayfs cannot stack on
  overlayfs. Every level's container-storage root (`/var/lib/docker`, k3s
  containerd root) must be a bind-mount chaining down to a non-overlay
  filesystem, provisioned as a discobox volume — then native rootless overlay
  works at every depth.
- **Overlapping subuid ranges at inner levels.** A child userns may map the
  parent's entire UID range 1:1, so depth costs nothing. Disjoint ranges
  (sibling isolation) are an outer/production-profile concern; inner dev
  levels overlap. Images ship a standard `/etc/subuid` layout and setuid
  `newuidmap`/`newgidmap`.
- **One uniform seccomp/proc profile.** A single discobox seccomp profile
  permitting the nesting syscalls (`unshare`/`clone` with `CLONE_NEWUSER`,
  `mount`), applied identically to worker and sandbox at every level, with
  unmasked proc where a level hosts a runtime. The enterprise-clean spec and
  the nesting-capable spec are deliberately the same spec, so dogfooding
  continuously validates the enterprise path.
- **pasta for rootless networking.** Userspace network hops compound per
  level; pasta over slirp4netns, and 2–3 interactive levels is the supported
  comfort zone.

## Rejected

- **Privileged DinD as the default.** Dead on arrival with enterprise
  security teams regardless of actual risk; the signal alone ends the
  evaluation.
- **Sysbox as the default.** Cleanest signals with zero worker-agent changes,
  but requires a third-party OCI runtime installed on every node — impossible
  on managed/serverless node pools and itself a red flag for platform teams.
  Wrong default for software deployed into customer-owned clusters; kept as
  an opt-in profile.
- **Kata as the default.** Strongest real isolation, but the `privileged`
  spec-field signal remains, and it demands nested virtualization support.
  Kept as the opt-in profile for hostile multi-tenancy.
- **Privileged dev, rootless prod.** Recreates exactly the dev/prod
  divergence the worker-as-pod architecture exists to eliminate — the
  behavioral differences of rootless (delegation, storage, networking) must
  be exercised constantly, not discovered at deploy time.

## Deferred

- **Removing privileged mode entirely.** Revisit once the rootless baseline
  has covered the local Docker and k8s backends through a full release cycle
  with nested-docker pools working.
- **Disjoint-subuid enforcement between sandboxes in one pool.** An
  outer-profile hardening knob; revisit with pool isolation-profile RBAC.
