# 26-11-21-395 — A machine pool freezes a sleeping sandbox where its driver can, and stops it where it cannot

- **Status**: Accepted
- **Date**: 2026-11-21
- Accepted by the owner by merging #1519, 2026-11-21
- Amended before anything shipped against it, 2026-11-22: §5's cache clear
  leaves a suspended machine with its cache, as a stopped one (the owner's
  choice, on #1274's implementation), where it had said a cache clear acts
  on it as on a running one.
- Amended again before anything shipped against it, 2026-11-22: a suspend of
  a machine its provider paused records the pool's freeze (§§3–4; the
  owner's choice, on #1274's implementation), where §3 refused it as not
  running and §4 had the next reconcile resume it. Since #1523 such a
  machine reads `suspended` too, so the refusal answered "not running" for a
  box shown paused, and the resume then undid what was asked.
- **Issue**: #1274
- **Extends** [26-10-16-420](26-10-16-420-a-docker-pool-freezes-a-sleeping-sandbox-and-a-person-can-suspend-one.md)
  §§1–5 (as amended by
  [26-11-17-583](26-11-17-583-a-sleep-may-leave-a-note-for-the-next-start.md))
  from the Docker runtime to the machine runtime. Nothing 420 decides for
  the Docker runtime changes.
- **Supersedes in part**
  [26-11-16-619](26-11-16-619-a-pool-on-a-driver-with-no-networks-fences-each-sandbox-at-the-providers-edge-and-starts-systemd-in-the-guest.md)
  §5's first bullet: "a sleep is a stop on archil, as on every machine
  pool", and "discobox never asks Archil for a pause". The machine runtime
  that stops every sleep has shipped, so this supersedes rather than amends.
  The rest of §5 stands: a pause discobox did not ask for is undone. This ADR
  decides 619's Deferred **A machine pool that freezes a sleep and a
  suspend**.
- **Relates to**:
  - [26-10-16-337](26-10-16-337-a-discobox-asks-to-sleep-and-names-when-to-wake.md)
    §5 (the runtime chooses freeze or stop) and §6 (an outside stop cancels
    the wake);
  - [26-11-16-372](26-11-16-372-a-sandbox-powers-down-when-its-primary-terminals-shell-exits.md)
    (power-down reason `exit`, which stops);
  - disco-vm ADR 26-11-03-868 (Commit is a capability a driver reports),
    the shape followed here;
  - discobox-ai/vm#196 (a machine its provider paused reads as paused, and
    `Start` resumes it). This ADR relies on it.

## Context

On a Docker pool, a confirmed sleep runs `docker pause` and stops only if the
pause fails. `discobox admin box suspend` makes the same freeze, asked for by
a person (420 §§2–3; `DockerSandboxRuntime.PowerDownSandbox` and
`SuspendSandbox`, `pool-agent/sandboxruntime/power.go`). The harness,
terminals, memory and `/run` all survive.

A machine pool cannot do this:

- **disco-vm has no freeze.** `machine.Machine` offers `Guest`, `Kill`,
  `Done`, `Err` and `Dial` (`pkg/machine/machine.go`, pinned at
  `a1fd6c39f632`). `engine.State` returns `stopped`, `running` or `unknown`
  only (`pkg/engine/engine.go:74-81`).
- **The machine runtime stops every sleep and refuses every suspend.**
  `MachineSandboxRuntime.PowerDownSandbox` calls `stopLocked` whatever the
  reason. `SuspendSandbox` answers `ErrUnsupported`, "a machine sandbox
  cannot be frozen whole" (`pool-agent/sandboxruntime/machinepower.go`).
- **The providers can freeze, but disco-vm never asks them to:**
  - boxd has `SuspendVm` and `ResumeVm`. The driver calls neither. Its
    `settle` turns boxd's auto-suspend and auto-hibernate off, and its boot
    wait resumes a `suspended` or `standby` machine and wakes a `hibernated`
    one (`pkg/machine/boxd/boxd.go`, `untilRunning`).
  - Archil pauses a sandbox at its 24-hour session limit. The driver's
    `Start` resumes it (`POST .../resume`), and `Attach` marks it to resume
    at its first exec (`pkg/machine/archil/archil.go`). The driver has no
    call that pauses a sandbox, and nothing here confirms that Archil offers
    a caller one.
  - Virtualization.framework pauses and resumes a VM (`VirtualMachine.Pause`
    and `Resume` in Code-Hex/vz v3.7.1). The vz driver holds its VM in a
    shim, and nothing calls either method.
  - HCS pauses and resumes a compute system. The Docker daemon behind
    disco-vm's docker driver pauses a container.
  - libkrun's API has no pause.

So a box that sleeps on boxd or archil loses its running processes, and the
same box on Docker keeps them. The owner asked for the freeze on PR 1270
(2026-11-16): "make discobox sleep/pause fire an actual pause like the docker
containers do today."

## Decision

**disco-vm gains a freeze, reported as a capability. Resume is `Start`. The
machine runtime freezes a confirmed sleep, and a suspend, on a driver that
reports the capability. A sleep stops where the driver cannot freeze or the
freeze fails. A suspend is refused there. The pool records each freeze it
makes, so a sandbox is reported `suspended` only when the pool froze it. A
pause the provider made is still undone (619 §5).**

### 1. The freeze is a capability of disco-vm's contract

disco-vm's contract gains the following. disco-vm chooses the names, and
records the contract in an ADR of its own:

- **A capability bit.** A driver that can freeze a running machine whole
  reports it, as it reports Commit (disco-vm ADR 26-11-03-868). A caller
  checks it before trying.
- **A required freeze method on `machine.Machine`**, beside `Kill`. The freeze
  acts on a running guest, and the attached machine already reaches it,
  through the provider's API or the shim that holds it. Every driver
  implements the method. A driver that does not report the capability
  returns `ErrUnsupported` and changes nothing. A freeze that fails leaves
  the machine running. Freezing a machine that is already paused does
  nothing.
- **An engine call** that attaches and freezes, as the engine's `Stop`
  attaches and stops.
- **`Start` resumes a paused machine.** This is discobox-ai/vm#196's rule:
  boxd and archil already resume this way. There is no separate resume
  call.
- **The paused state from discobox-ai/vm#196.** A machine that discobox
  froze reads the same as one its provider paused: `engine.State` reports
  it paused, and reading it wakes nothing. disco-vm does not record who
  paused a machine. The pool records that (§4).
- **What other operations do to a paused machine.** Stop and Destroy act on
  it. Commit, Upgrade and SaveCache refuse it, since it is not stopped. A
  paused machine counts against `MaxRunning`, because it still holds what
  its hypervisor gave it.
- **The clock after a resume.** When `Start` resumes a paused machine, the
  guest's wall clock is correct by the time `Start` returns. A container
  shares its host's clock and a VM does not: a guest left behind would read
  its wakes and certificates against a stale time. How each driver corrects
  the clock is open (below).
- **Conformance cases**, for every driver that reports the capability:
  - freeze, then read the state as paused;
  - resume with `Start`, then check that a process started before the
    freeze is still running under the same PID and the guest clock is
    current;
  - stop a paused machine.

### 2. Which drivers freeze

| Driver | Freeze | Resume (`Start`) |
|---|---|---|
| boxd | `SuspendVm` | `StartVm`, then `ResumeVm` from the boot wait (today's path) |
| archil | a caller pause, if Archil offers one (open) | `POST .../resume` (today's path) |
| vz | `VirtualMachine.Pause`, through the shim's own route | `Resume`, through the same route |
| hcs | pause the compute system | resume it |
| docker (disco-vm's) | `docker pause` | `docker unpause` |
| fake | yes, through discobox-ai/vm#196's test hook | yes |
| libkrun | none: libkrun has no pause | — |

If Archil turns out to offer no caller pause, archil reports no freeze, and
a sleep on archil stays a stop under §3's fallback. Nothing else in this
ADR changes.

### 3. The machine runtime freezes a sleep and a suspend

`MachineSandboxRuntime` does what 420 §§2–3 have the Docker runtime do:

- **Sleep.** A sleep the sandbox confirms freezes the machine when the driver
  reports the capability. If the driver does not report it, or the freeze
  fails, the machine stops, as Docker stops when a pause fails. A power-down
  for `idle` or `exit` still stops (337, 372).
- **Suspend.** `SuspendSandbox` freezes the machine. A driver without the
  capability is refused with `ErrUnsupported`, as every machine pool is
  refused today. A freeze that fails returns an error, and nothing changes.
  A suspend never turns into a stop (420 §3).
- **Shared preconditions.** A sandbox that is still booting, or not running,
  is refused. One already suspended is a no-op. A suspend of one its
  provider paused (§4) is taken as asked: the pool records the freeze, and
  the machine, already paused, stays so. An explicit suspend stages
  its completion receipt before the freeze, as the Docker runtime does
  (`suspendWithCompletion`, shared through `sandboxCore`).

### 4. The pool records each freeze, so a provider's pause is never reported as a sleep

The machine record gains a field saying that the pool froze the machine.
Under the power lock, the pool writes it before the freeze and clears it if
the freeze fails. When the pool reads a machine's state, the field decides
what the state means:

| Engine state | Pool froze it | The sandbox is |
|---|---|---|
| paused | yes | `suspended` (420 §1) |
| paused | no | a pause by the provider: resumed at the next reconcile, never reported as a sleep or counted as idle (619 §5, unchanged), unless a suspend asks for it first, which records the pool's freeze (§3) |
| running | yes | thawed by something other than the pool. Example: Archil resumes a paused sandbox when a request with its port token arrives. The pool runs 420 §4's after-thaw steps (§5), then clears the field |
| stopped | yes | `stopped`. The field is cleared |

Without the field, a sandbox Archil paused at its session limit would read as
a sleep that no wake undoes. That is 619 §5's rule, kept.

### 5. Every start thaws a frozen machine

An explicit start, the scheduled wake's start, and the start every use makes
(`EnsureSandboxRunning`) all resume a suspended machine with `Start`. Each
holds the same power lock and start gate it holds today, and nothing boots.
After the resume, the pool runs 420 §4's after-thaw steps, as amended by 583,
detached from the caller:

- it delivers the runtime-config document;
- it calls `POST .../autostop/resumed`, which sets aside the wakes now due
  and hands over the next start's notes;
- it counts the thaw as a start for the idle clock.

Once thawed, the machine is a running machine. The rule that places what a
running machine boots on, and restarts it onto that (`placementCurrent`),
applies to it as to any other running machine. A thaw is given no exception
to it.

Restart, stop, archive and delete act on a suspended machine as on a running
one. disco-vm's stop of a paused machine stops it (§1). A cache clear leaves
a suspended machine with its cache, as it leaves a stopped one: on a machine
pool it stops nothing and asks each running sandbox's agent to empty its own
cache, which a frozen agent cannot answer, and thawing every sleeping box to
ask would undo the sleeps it was asked to keep.

### 6. A frozen machine sandbox is read like a frozen container

420 §5 applies unchanged. The pool polls no status from a frozen machine
sandbox and delivers it no document. Reads that never start a sandbox are
refused as `suspended`. Its tree is not exported. An attached client is
detached, and the client's next attach resumes the sandbox.

The pool's own machine, which runs the pool agent, is never frozen by
discobox. 619 §5's resume of a paused pool machine stands.

## Alternatives rejected

- **A freeze method on `machine.Driver`, taking an `Instance`.** The guest
  operations and `Kill` live on the `Machine` that `Attach` returns. That
  machine already reaches the guest through the provider's API or the shim
  that holds it (vz's machine dies with its process). A driver method would
  repeat `Attach`'s lookup in every driver.
- **An optional interface** that a caller type-asserts (a `Pauser`). The
  repository's rule is a required method plus a capability bit, so a caller
  can plan without trying. Commit already works this way (868).
- **A resume call of its own.** `Start` already resumes a machine its
  provider paused on boxd and archil, and discobox-ai/vm#196 makes it the
  one way to undo a pause. A second way would be one that every start in
  discobox had to choose between. This is 420's reason for having no
  resume instruction.
- **Every paused machine reads as `suspended`, with no record.** A sandbox
  that Archil paused at its 24-hour limit would then read as asleep with
  nothing to wake it. 619 §5 forbids reporting a pause the provider made as
  a sleep.
- **Freezing inside the guest**, with the cgroup freezer on the sandbox's
  units, driven by the sandbox agent. It would work on every driver,
  libkrun included. But the machine keeps running on its provider: it
  counts against boxd's machine and running caps and vz's two-guest cap,
  and it is billed as running. Only a whole-machine freeze gives the
  provider back the CPU.
- **Hibernating on boxd** (`hibernated`, woken by `WakeVm`) instead of
  `SuspendVm`. Holding memory on disk would cost less, but waking would be
  slower. The owner asked for the freeze Docker makes, which keeps memory
  resident. Deferred below.
- **Freezing libkrun by stopping its launcher process (`SIGSTOP`).** libkrun
  has no pause. Stopping the VMM's process stalls the guest without warning,
  and disco-vm cannot vouch that its vCPU state, timers and vsock peers
  survive that. Deferred below.
- **Freezing only on archil, with Archil's pause.** 619 already rejected
  this: the freeze belongs in disco-vm's contract and the machine runtime,
  for every driver that can freeze.

## Consequences

- **A sleep on a driver that freezes keeps the box.** On boxd, and on any
  other driver that reports the capability, a sleep keeps the box's harness,
  terminals and `/run`. On waking, the box continues where it froze, as on
  Docker. `discobox admin box suspend` works on these drivers. On a driver
  without the capability, a sleep is still a stop and a suspend is still
  refused.
- **disco-vm** (discobox-ai/vm):
  - the capability, the freeze method and the engine call, recorded in a
    disco-vm ADR;
  - each implementation in §2;
  - a resume that leaves the guest clock current;
  - the conformance cases;
  - discobox-ai/vm#196's paused state, which this relies on.
- **pool-agent**:
  - `MachineSandboxRuntime.PowerDownSandbox` and `SuspendSandbox` freeze
    where the driver can (§3);
  - the machine record gains the freeze field (§4);
  - `status` maps paused to `StatusSuspended` or to a provider pause (§4);
  - `startLocked` and `EnsureSandboxRunning` thaw, then run the after-thaw
    steps (§5);
  - the suspend-completion receipt is shared with the Docker runtime.
- **No API or server change.** `suspended`, the `suspend` route and
  `resume-sandbox-autostop` already exist (420). The server's provider for
  disco-vm does not freeze its pool machine.
- **Docs.** The machine-runtime sections of `pool-agent/DESIGN.md` (its
  "Refused: suspend" line among them) and
  `pool-agent/sandboxruntime/DESIGN.md` describe the freeze. The provider
  docs say which drivers freeze.
- **A frozen remote machine may still be billed.** It holds its memory on
  the provider until it is resumed or stopped, and nothing stops it on its
  own (420). The provider may bill for that memory.
- **The box's connections drop.** Its tunnels to the pool, and its
  connections out, may be gone when it thaws. The sandbox agent and the
  bridges reconnect, as they do after a Docker freeze.

## Deferred

- **Hibernating instead of freezing**, on boxd or any provider that can.
  Revisit when the cost of a frozen machine's memory is reported.
- **A freeze on libkrun.** Revisit when libkrun gains a pause, or a spike
  shows that stopping the launcher is safe.
- **Stopping a frozen machine after a time limit.** Revisit with
  420's own Deferred items, when a held machine's cost is reported.

## Open, for implementation

- The names of disco-vm's capability, freeze method, engine call and paused
  state (§1), and the machine record's field (§4).
- Whether Archil offers a caller pause (§2). If it does, two more
  questions: whether a paused sandbox still runs down its 24-hour session,
  and whether Archil stops a sandbox that stays paused too long.
- How each driver keeps the guest clock current at resume (§1). Options
  include a step the driver runs through the guest, or the hypervisor's own
  clock handling. The conformance case decides whether each driver passes.
- Whether boxd keeps a suspended machine across its own host maintenance.
  If it loses one, the machine reads as stopped, the field is cleared, and
  the sandbox is stopped. That is the fallback anyway.
- Whether boxd bills a suspended machine's memory.
