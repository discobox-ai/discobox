# 26-11-23-445 — A box may ask for a CPU and memory size, and its pool's size is the default

- **Status**: Proposed (would supersede [0029](0029-sandboxes-have-no-per-sandbox-resource-requests.md)
  §1 for `cpuVcpus`/`memoryBytes` on sandbox create and §3, and its
  Consequences' "a future need starts from a clean slate"; 0029 §2, no
  capacity gating, and its removal of `storageBytes` and `timeoutSeconds`,
  stand)
- **Date**: 2026-11-23
- **Issue**: [#1601](https://github.com/discobox-ai/discobox/issues/1601)
- **Relates to**: [0003](0003-promote-pool-to-a-first-class-primitive.md) §1
  (the pool envelope, which stays); [26-10-16-423](26-10-16-423-a-harness-is-a-provisioning-bundle-and-the-agent-provisions-any-box.md)
  §8 and [26-11-03-986](26-11-03-986-a-machine-pool-snapshots-a-plan-by-committing-a-vm-it-provisioned-and-shut-down.md)
  (snapshots by plan); [26-11-17-316](26-11-17-316-a-pool-reports-its-snapshots-and-each-box-what-it-started-from.md)
  §3 (a plan as a person reads it); [26-11-11-080](26-11-11-080-each-disco-vm-driver-is-a-provider-type-of-its-own-name-configured-by-its-own-fields.md)
  (a disco-vm driver is a provider type).
- **Depends on**: a discobox-ai/vm change that lets a driver say whether it
  sizes a machine (§6); filed from this ADR's implementation.

## Context

ADR 0029 removed every per-sandbox resource request. Its case for CPU and
memory was that the one place they had teeth was a *nested* Docker limit
inside the pool's worker container, sharing that container's cgroup, and
that coding sandboxes are bursty and never meant to reserve a slice of a
pool. The owner now wants a box sized on create (issue #1601, 2026-11-23):
there is no way to get an Archil box bigger than Archil's default.

What is true at `97b228184`:

- **A box takes no size.** `SandboxCreateConfig` (`api/openapi/server.yaml`)
  and `PoolSandboxCreateRequest` (`pool-agent/api/openapi/pool.yaml`) have no
  CPU or memory field, and the `sandboxes` table no such column.
- **A pool's size is declared per provider.** `ProviderDefinition.PoolSizeFields`
  (`server/internal/sandbox/capabilities.go`) lists the fields a provider
  acts on, and `checkPoolSize` (`server/internal/resources/pools/size.go`)
  refuses any other non-zero one. libkrun, vz and wslc declare CPU and memory
  (`vmsize.PoolSizeFields`) and size the pool's VM from them (`VMSpec`,
  `server/providers/dockerworker/driver.go`). Docker, execvm, digitalocean and
  every disco-vm provider declare none, so **an Archil pool refuses a size
  today** rather than ignoring one.
- **Two sandbox runtimes.** Every provider but disco-vm runs its boxes as
  containers of the pool agent's `DockerSandboxRuntime`; 0029's premise no
  longer holds there: on the docker provider a box is a sibling of the pool
  container on the host daemon, and under libkrun, vz and wslc it is a
  container on the pool VM's own daemon. Either way a limit on the box's
  container is its own cgroup, not a slice nested in the pool's. `boxConfig`
  (`pool-agent/sandboxruntime/runtime.go`) sets none, citing 0029, and is
  shared by snapshot builds.
- **disco-vm pools run `MachineSandboxRuntime`.** `createOptions`
  (`pool-agent/sandboxruntime/machine.go`) makes every machine, a box's and a
  snapshot build's, with no `CPUs`/`Memory`, though `engine.CreateOptions`
  carries both. The pool's own machine (`poolMachine.create`,
  `server/providers/discovm/poolmachine.go`) is unsized too.
- **Drivers differ.** At the pinned vm (`v0.0.0-20261121233619-4035d8ffe365`),
  archil and boxd send the size to their API, vz and hcs size the VM they
  boot, and the disco-vm docker driver only labels the container with it
  (`pkg/machine/docker/docker.go`): it would accept a size and not apply it.
  `machine.Capabilities` has no field saying which.
- **A snapshot fork keeps its source's size on archil.** `archil.go` refuses
  a fork at another size with `machine.ErrUnsupported`; `createMachine` falls
  back to a fresh machine only on `ErrNotFound`, so a sized box whose plan has
  a snapshot would fail with the raw driver error. A machine snapshot's key
  (`snapshotPlan`, `pool-agent/sandboxruntime/snapshot.go`) has no size.

## Decision

### 1. A box may ask for `cpuVcpus` and `memoryBytes` at create

`SandboxCreateConfig` gains `cpuVcpus` (number, vCPUs, fractional allowed)
and `memoryBytes` (integer), each optional; absent or 0 is unset. The API
documents that each overrides the pool's (§2) and that unset means the
pool's, or the provider's default where the pool has none. `discobox run`
(and the root CLI's box create) gains `--cpu-vcpus` and `--memory`, spelled
and parsed as `discobox admin pool`'s are (`cli/internal/cli/pool.go`: `8g`,
`512m`). A negative value is refused, as a pool's is.

A box's size is fixed at create. Resizing a box is not part of this
decision (Deferred).

### 2. The server resolves box-else-pool, per field, and sends the result

At create the server resolves each field on its own: the box's value if set,
else the pool's, else 0. The resolved size is stored on the box (new columns
on `sandboxes`, added by migration; existing rows read as 0) and returned on
the box's read, so a box shows the size it was made at. A later change to the
pool's size reaches new boxes only.

`PoolSandboxCreateRequest` gains `size {cpuVcpus, memoryBytes}`, omitted when
both are 0. The agent never looks its pool's size up: it applies what it is
sent and records it in the box's tree, so a machine or container made again
for the same box (a new root, a recreate) gets the same size.

### 3. A pool's CPU and memory are its boxes' default size

On every provider that acts on a box's size (§6), a pool's `cpuVcpus` and
`memoryBytes` mean the default size of each box in it. That is in addition
to, not instead of, what they already mean on libkrun, vz and wslc: the pool
VM is still sized from them (ADR 0003 §1), and each box in it defaults to the
whole of it. So docker, execvm, digitalocean and the sizing disco-vm drivers
begin to declare CPU and memory as pool size fields, and `checkPoolSize`
keeps refusing them everywhere else. `storageBytes` is unchanged.

The CLI's pool flag help, which says "for the pool's VM", says both.

### 4. A docker worker box gets its size as Docker limits; unset is no limit

`DockerSandboxRuntime` sets a box container's `NanoCPUs` from `cpuVcpus` and
its `Memory` from `memoryBytes`, each only when non-zero; 0 sets no limit, so
nothing is restricted by default. `MemorySwap` is set equal to `Memory`
where Docker accepts it, so the limit is not doubled by swap. The `/dev/shm`
tmpfs (`ShmSize`) counts against a memory limit; it stays as it is.

A snapshot build's container is not limited: it is the pool's work for a
plan, not a box, and a Docker snapshot image carries no limit, so one image
serves boxes of every size and the size is not in a Docker snapshot's key.

The pool container (`poolContainerSpec`, `server/providers/dockerworker/engine.go`)
stays unlimited.

### 5. A machine box is made at its size, and so is its plan's snapshot

`MachineSandboxRuntime` passes the box's size to the engine as `CPUs` (vCPUs
rounded up to a whole one, as `vmsize.FromPool` rounds) and `Memory` (bytes);
0 leaves each to the driver, as today, so an unsized archil box keeps
Archil's default.

A machine snapshot is keyed by size. `snapshotPlan` gains the resolved size,
omitted when 0 so every key held today is unchanged, and a snapshot build is
made at its plan's size. A box asking for a size gets a snapshot built at
that size behind its first create and starts from it afterwards, as an
unsized box does today. `SnapshotPlanName` (ADR 26-11-17-316 §3) gains the
size, so a person can tell two keys apart that differ only in it.

A create from a snapshot that the driver refuses with `ErrUnsupported` is
made fresh, at the asked size, and logged, as a snapshot gone between lookup
and create is today. With the size in the key this is not expected; it keeps
a mismatch from failing the box with the driver's raw error.

The pool's own machine on a disco-vm provider is not sized: it runs the pool
agent and its proxy, not boxes, and the pool's size is its boxes'.

### 6. A provider says whether it sizes a box, and one that does not refuses

`ProviderDefinition.PoolSizeFields` becomes `SizeFields`, the CPU and memory
fields the provider acts on for a box and as a pool's default, plus
`storageBytes` for a pool where declared. One list serves both because no
provider acts on one and not the other (§3). The sandbox create path checks a
box's non-zero fields against it before the box is stored, and refuses one
the provider does not act on with a 400 naming the field, the provider
instance and its type, as `checkPoolSize` words a pool's refusal.

Every docker worker provider declares CPU and memory (§4). A disco-vm
provider declares them only for a driver that sizes a machine. That is the
driver's fact, so it is declared in disco-vm, next to the code that honors
it, and read by `discovm.Definition`: the vm change this ADR depends on. Until
that pin lands, no disco-vm provider declares them, and an archil box asking
for a size is refused clearly rather than made at Archil's default. The
disco-vm docker driver, which labels a size and does not apply it, declares
none unless the vm change makes it apply one.

### 7. Validation is static; there is no capacity gating

0029 §2 stands: nothing is refused for the pool's reported `available*`,
which is a stale snapshot of an overcommitted share. A box's size is checked
only against bounds that are fixed:

- not negative (§1);
- on a provider that sizes a VM per pool, not more than the pool's own
  `cpuVcpus` or `memoryBytes` where the pool sets them, since no box can be
  larger than the VM it runs in. Where the pool leaves its VM to the host
  default, the server does not know the VM's size and does not check.

A size past what the driver or Docker can give (Archil's own limits, more
CPUs than the host has) fails the box's create with the driver's or Docker's
message, reported as a create failure is today.

## Open, for implementation

- **The vm seam's shape for §6**: a field of the driver's registry definition
  (static, so `discovm.Definition` can read it without a driver) is
  preferred; whether it is one flag or a per-resource list is vm's call.
- **The disco-vm docker driver**: whether vm makes it apply the size it
  labels, which would let a disco-vm docker provider declare it.
- **Column names** on `sandboxes` and the migration's form.
- **Docker's `MemorySwap`** where the pool VM's daemon has no swap
  accounting: set it or leave it, whichever Docker accepts there.

## Alternatives rejected

- **Leave 0029 standing and size only the pool.** The issue's need is a
  bigger Archil box, and an archil pool has no VM to size: each box is its own
  machine. A pool-wide size would be the same thing with no way to have two
  sizes in one pool.
- **The agent resolves box-else-pool.** The agent does not know its pool's
  size (it only reports `available*`), and teaching it would put a
  server-owned setting in the bootstrap and re-place every pool on a size
  change. The server already holds both values at create.
- **Resolve at every send rather than store the resolved size.** A box's
  machine or container would change size when its pool's did, on a recreate
  only, and an archil machine cannot change size at all; a box made at one
  size keeps it.
- **Refuse a sized box whose plan has a snapshot at another size.** Only
  unsized boxes would ever start from a snapshot; every sized box would fail
  or provision from scratch, minutes each on archil.
- **Always make a sized box fresh, skipping snapshots.** Correct, but every
  sized box pays a full provision, and the snapshot machinery already keys by
  plan; size is one more input to the key.
- **Gate on `availableCpuVcpus`/`availableMemoryBytes`.** 0029 §2's reasons
  hold: a one-shot comparison against a stale, overcommitted figure refuses
  honest boxes and catches nothing real.
- **Size the disco-vm pool machine from the pool's size.** It holds no box;
  an 8-vCPU pool would pay for an 8-vCPU proxy. Revisited if a pool machine's
  proxy or agent is shown to need more than the driver's default.
- **Keep a list in discobox of which disco-vm drivers size a machine.**
  `PoolSizeFields` exists so that the declaration lives next to the code that
  honors it; a list in discobox goes stale the first time a driver changes.
- **A separate `SandboxSizeFields` beside `PoolSizeFields`.** Two lists that
  are equal for every provider; one is kept.
- **Limit snapshot builds.** A Docker image carries no limit, so it buys
  nothing for the boxes made from it and would slow the build.

## Deferred

- **Resizing a running box.** Docker can update a container's limits; an
  archil machine cannot change size. Revisited when someone asks to resize
  without recreating.
- **A size on a box made by another box** (the child create path) beyond what
  `SandboxCreateConfig` carries, such as a parent's ceiling on its child's
  size. Revisited with the first case of a box asking for more than its
  parent should give.

## Consequences

- `discobox run --cpu-vcpus 4 --memory 8g` on an archil pool makes a
  4-vCPU, 8 GiB machine once the vm pin carries §6; before that, it is
  refused saying the provider does not act on a box's size.
- An archil pool may set a size, and its boxes are made at it.
- A docker worker box has Docker CPU and memory limits equal to its size or
  its pool's, and none when both are unset.
- On libkrun, vz and wslc a pool's size now also limits each box to the whole
  VM, which changes nothing in practice; a smaller box size limits that box.
- A machine pool holds one snapshot per size its boxes ask for, each a held
  machine on the provider's bill.
- `boxConfig`'s and `createOptions`' comments citing 0029, and the pool
  agent's and providers' `DESIGN.md`, change with the code.
