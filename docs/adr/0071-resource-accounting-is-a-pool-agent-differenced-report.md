# 0071. Resource accounting is a pool-agent-differenced report

Status: Accepted

## Context

A pool has no answer to "what is using all of this". `Pool.AvailableStorageBytes`
is a single `statfs` of the project's data tree, reported for scheduling, and it
says nothing about where the space went. Nothing at all reports CPU or memory
per sandbox. When a pool runs hot or fills up, the only recourse is to attach to
each sandbox in turn and run `top` and `du` by hand.

Four things about the existing layout constrain any answer.

**Sandbox containers are not capped.** `sandboxruntime`'s create path sets no
`NanoCPUs` and no `Memory` on the container. Nothing bounds a sandbox, so one
really can starve the pool, and there is no limit to report a usage against.

**Cache is pool-shared and keyed by target path.** `prepareSandboxVolumes` binds
`layout.PoolCache(project, pool)` whole onto `/.discobox/cache` in every
sandbox, and `sandbox-agent/boot` lays it out as `/.discobox/cache/<target>` —
by what the harness declared, never by which sandbox wrote it (ADR 0007, ADR
0050). There is no per-sandbox cache figure on disk to read.

**Sandboxes are not the only consumers.** buildkitd, the pool registry, and the
proxy all run inside the pool container (ADR 0044). On a pool mid-build,
BuildKit is plausibly the largest consumer on the host.

**Only the sandbox can see inside itself.** The pool agent runs in its own
container, in its own cgroup and PID namespace. It can read the pool's totals
and walk the pool-owned trees on disk, but the processes inside a sandbox are
visible only to that sandbox's own agent.

## Decision

**Resource accounting is one report, authored by the pool agent, carrying
cumulative counters differenced over a single tick.**

### 1. Cumulative counters travel; rates are computed

`cpu.stat`'s `usage_usec` is a monotonic count of CPU-microseconds. Sandbox-agent
reports the counter, never a rate. The rate is

```
(usage_usec₂ - usage_usec₁) / (observed₂ - observed₁)  =  vCPU-equivalents
```

where `1.0` is one core saturated. That unit is additive across sandboxes and
comparable between them, so it sorts directly into "who is eating the pool" and
the column sums to pool load. Share-of-pool is `vcpus / Pool.CPUVCPUs`.

Shipping the counter rather than a pre-computed rate means a missed or delayed
report degrades into a wider measurement window instead of a wrong number, and
it leaves a server free to difference across any two reports it kept.

Windows are computed from each sample's own `observedAt`, never from the tick's
wall clock, so poll skew inside a tick cannot distort a rate.

### 2. The pool agent differences, over one tick

Every sandbox in a pool is measured over the same tick, by the one component
that polls all of them. Rates computed independently inside each sandbox would
each cover a slightly different window, and a ranking assembled from them would
not be comparing like with like — which is the entire purpose of the report.

The cost is deliberate and bounded: the pool agent now *reads* two fields out of
a payload it otherwise relays untouched. The relay itself stays opaque (ADR
0030) — the payload is forwarded as received rather than re-serialized from a
parsed structure, and the computed numbers travel beside it in the pool agent's
own report rather than being merged into it.

### 3. Attribution goes down to processes

Per-sandbox totals say which sandbox; they do not say what. Sandbox-agent
therefore reports candidate processes — the union of the top by cumulative CPU
and the top by RSS — each with `pid`, `startTicks`, command, and its own
cumulative counters. The pool agent differences them per `(pid, startTicks)` and
reports the top few by CPU rate and by resident size.

`startTicks` is part of the key because PIDs are reused: without it a recycled
PID would inherit its predecessor's counter and difference into a nonsense
spike.

Selecting candidates by *cumulative* CPU rather than by rate is a deliberate
approximation. A process burning four cores for fifteen seconds has accrued a
minute of CPU, which outranks most long-lived idle daemons within a tick or two,
and it keeps sandbox-agent's status genuinely computed-fresh with no sampling
state of its own.

### 4. Memory is reported twice, because both numbers are true

The cgroup's `memory.current` is what the host charges the sandbox — anonymous
memory, page cache, kernel memory. `VmSize`/`VmRSS` summed over processes is what
the processes think they hold, and double-counts every shared page. Neither
substitutes for the other, so both are reported: `currentBytes` for what the
sandbox costs the pool, `virtualBytes`/`residentBytes` for what its processes are
doing. Summed RSS routinely exceeds `memory.current`; that is expected, not a
bug.

### 5. Cache is accounted once, at the pool

Per-sandbox reports carry data, config, sources, secrets, and origins — the
trees that are genuinely per-sandbox. Cache is reported as a single pool-level
figure, because that is what it is.

The rejected alternative was to repeat the shared total on every sandbox to make
the shape uniform. It would make the per-sandbox column stop summing to anything
real: N sandboxes would each claim the whole cache, and any total computed over
the report would be wrong by a factor of N.

Restructuring `PoolCache` into per-sandbox subtrees would make the figure
attributable, and is rejected for a larger reason: cross-sandbox cache sharing
is what the pool is for. A per-sandbox cache means every sandbox in a pool
re-downloads the same module tree.

### 6. Pool services are measured, not derived

The pool container runs the pool agent, buildkitd, the pool registry, the proxy
and the build mediator (ADR 0044). Its own cgroup measures exactly those, and
that figure is reported alongside the per-sandbox ones. The pool's load is
`services + Σ sandboxes` — two disjoint measurements added.

**It is not `pool − Σ sandboxes`, which is what this section originally said.**
That was wrong, and only running it proved it: on a live pool the "pool total"
came back at 0.91 vCPU while its sandboxes summed to 3.10, so the subtraction
went negative and clamped to zero.

The premise was that the pool container's cgroup contains everything on the
host. It does not. Sandboxes run under a nested container runtime, and their
cgroups are not children of the pool container's — walking that subtree on a
live pool finds `buildkit/…` and `system.slice/discobox-*.service` and no
sandbox anywhere in it. The two sets never overlapped, so subtracting one from
the other was meaningless in both directions.

Measuring the services directly is also simply better. It cannot go negative,
it does not depend on how a provider nests its runtimes, and it names a real
thing — on a pool mid-build, BuildKit's own cgroup is the largest consumer in
the container and now shows up as such rather than as a remainder.

The individual services are still not broken out. Locating each unit's cgroup is
more wiring than the aggregate is worth today, and the aggregate already
separates "the pool's machinery" from "the sandboxes". Revisit when a reader
needs to know which service.

### 7. Disk is two measurements at two freshnesses

`statfs` of `/var/lib/discobox` is one syscall, and it is the figure that
answers "is this pool about to run out of disk". It is taken on **every report**.

Per-tree attribution is one pass over every inode the pool owns. It runs on its
**own adaptive schedule**, and the report carries the last completed sweep with
`observedAt`, `durationMillis`, `intervalSeconds`, and `nextScanAt`.

Reporting both at one frequency was the original decision and it was wrong in
both directions: it made the cheap, urgent number as slow as the expensive one,
and the expensive one as frequent as the cheap one.

**The schedule is a duty cycle.** After a sweep costing `d`, the next is due in
`clamp(d / 0.02, 1min, 1h)` — the agent spends at most about 2% of wall-clock
time walking disk. A duty cycle rather than a back-off multiplier because it is
the only form an operator can state and verify: *"never spend more than 2% of a
core walking disk"* is a budget, where *"back off 50×"* is a knob whose meaning
depends on a pool size nobody knows in advance. It also needs no per-pool
tuning — a 400ms pool lands on the one-minute floor, a 30-second pool on a
half-hour interval, from the same constant.

This is not only an economy. On a pool whose trees take longer to walk than the
reporting interval, a *fixed* schedule does not degrade gracefully — sweeps
overlap or the loop falls permanently behind. Deriving the interval from
measured cost means there is no tree size at which the schedule stops making
sense. When the cap binds, the budget is being exceeded anyway, and the agent
logs that rather than absorbing it silently.

**Sweeps are staggered by a hash of the pool ID**, not randomly. One Docker
daemon hosts every local pool (ADR 0003), so after a host reboot every agent on
it would otherwise sweep in lockstep. A hash spreads them while keeping each
pool's own schedule reproducible; a random jitter would make one agent behave
differently run to run, which is harder to reason about and to test.

**This supersedes the original "nothing is cached" rule**, whose stated
objection was that a cached size is right at an unknown moment. Stamping the
sweep with `observedAt` and `nextScanAt` answers that objection rather than
avoiding it: the moment is no longer unknown, and a reader can see both how old
a figure is and how old it is allowed to get.

A canceled sweep is discarded rather than reported partially. A walk that
stopped half way through reports every unvisited tree as empty, which is a wrong
answer rather than a missing one.

**The sweep enumerates trees, not containers.** The two halves of a sandbox do
not end together: archiving drops the container and keeps the durable tree by
intent (ADR 0022 §6), and a sandbox whose container was lost out of band keeps
one until the reaper's retention expires. Both occupy every byte they occupied
while running, and an archived sandbox is exactly the one whose disk somebody
deciding whether to purge it needs to see — enumerated from containers alone it
would be invisible while holding gigabytes.

So the report covers the union of the containers and the trees. A tree with no
sandbox behind it is reported too: it is occupying the disk regardless, and
which trees still answer to a sandbox is the control plane's judgement, against
the rows it holds rather than against a directory listing.

Filesystem quotas would make attribution O(1), and are still rejected for now:
they need project quota IDs assigned per filesystem at volume-preparation time,
and are silently unavailable on filesystems a pool may legitimately land on.
Revisit if the duty cycle pushes real pools to the one-hour cap.

Walking one sandbox per tick instead of sweeping all of them — spreading the
same cost more evenly and keeping each figure fresher — was considered and
deferred. It gives every tree its own timestamp and its own staleness, which is
more machinery than a single stamped sweep, and the pool cache is usually the
largest single tree and is not divisible that way regardless.

## Consequences

- `statfs` of `/var/lib/discobox` measures the whole backing filesystem, which
  may hold more than Discobox. `usedBytes` is the filesystem's, not Discobox's;
  the Discobox share is the walked totals.
- The walked figures are older than the report that carries them, by up to one
  sweep interval. On a large pool that is up to an hour. The filesystem's own
  free space is not, so nothing that needs to be timely depends on the sweep.
- A pool reports no attribution at all until its first sweep completes, which
  is deliberately not zeroes: a pool whose first sweep is still running has not
  been measured, and has not been found empty.
- A pool agent restart loses its previous samples, so the first report after a
  restart carries counters and storage but no rates. This is reported as an
  absent rate, never as zero.
- Summed process RSS exceeds cgroup `memory.current` whenever pages are shared.
- The report is telemetry: it is written outside the generation contract, like
  the status channel it rides beside (ADR 0030), and publishes no project event.
