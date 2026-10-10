# disco-vm Provider Design

`discovm` is the `poolruntime.RuntimeProvider` for pools whose sandboxes are
disco-vm machines ([github.com/discobox-ai/vm](https://github.com/discobox-ai/vm)),
beside `dockerworker.Engine` rather than under it: a discovm pool has no
pool-agent container, BuildKit, registry or sandbox network of the engine's
(ADR 26-10-09-106 §1), whatever its driver runs a machine as. This package is the skeleton: the provider kind,
the embedded engine and its shim, and pool hosting with its console, log, and
image build. The driver is configuration it passes to disco-vm; how a pool is
hosted follows from what that driver reports. The machine seam a pool agent
drives sandboxes through (#122), and the pool agents themselves (#64, #127),
come next.

```mermaid
flowchart TD
    pool["poolruntime.Provider"]
    runtime["discovm.Runtime\nengine · BuildGuestImage"]
    engine["disco-vm pkg/engine\non machine.New(config driver)"]
    host{"driver's Capabilities.Remote"}
    machine["poolMachine\npool agent in a Linux machine"]
    agent["hostAgent\npool agent on this host"]

    pool --> runtime
    runtime --> engine
    runtime --> host
    host -- remote --> machine
    host -- local --> agent
```

## The Engine Is the Server's

- One state root per driver per host (`stateRoot`, under the platform's data
  directory), shared by every provider instance on that driver; it is not
  configurable. Each provider instance opens an engine on it. The engine's state
  is its files, so a cap it enforces at `Start` — vz's two running macOS guests
  — counts every instance's machines, which is what makes it the host's (ADR
  §4). The root is per driver because a root is one tag namespace, and the
  twins (below) name their parents by tag. The server is where the engine, its image store, its
  shims, and the boxd credential live (ADR §2).
- The module is imported by commit (a pseudo-version), never a tag.
- A local machine's shim is this binary re-executed as `__discovm-shim`
  (`RunShimIfInvoked`, called from `server.RunVMLauncherIfInvoked` before
  anything a server does) with the engine's root and driver, so the server
  ships no disco-vm binary. A remote driver (boxd, docker) runs no shim: the
  engine attaches to the service's machine on every call, so a running one is adopted across
  restarts, which "VM Lifetime" in [../DESIGN.md](../DESIGN.md) does not forbid
  for a machine that is not the host's.
- A shim does not yet die with the server; that is discobox-ai/vm#8, and it
  gates vz sandboxes, not this skeleton.

## Drivers

The driver is configuration, passed to disco-vm as it is: `newDriver` is
`machine.New(cfg.Driver)` on disco-vm's own registry, and validation asks the
same registry. Nothing in this package is written per driver. A build links
the real drivers its OS has (`drivers.go`: boxd and docker everywhere,
`drivers_darwin.go`: vz, `drivers_windows.go`: hcs), as imports and nothing
else. disco-vm's docker driver runs Linux guests as containers that boot
systemd; it stands beside the `docker` provider (dockerworker), which is
unchanged. It never links disco-vm's `fake` driver, whose guest agent is the
running binary serving exec on loopback; only tests do.

`driver` is `Immutable`: it cannot change while the provider has pools,
because only the driver that made a pool's host can remove it.

Where a pool's agent runs follows from what the driver reports
(`newPoolHost`, `machine.Capabilities.Remote`), as ADR 26-10-09-106 §1 places
it:

| driver reports | pool host | console | log |
| --- | --- | --- | --- |
| remote (boxd, docker) | `poolMachine`: a Linux machine named `discobox-pool-<pool>` | `bash -l` in it, through disco-vm's guest exec | its journal for this boot, through the same exec |
| local (vz, hcs) | `hostAgent`: a pool agent process under `<stateRoot>/pools/<pool>` (#64) | refused, `sandbox.ErrPoolConsoleUnsupported`: the host is the user's own machine | the agent's `pool-agent.log` |

- `poolMachine` reaches the host through disco-vm's own guest agent, never
  through the pool agent, for the console's reason ([../DESIGN.md](../DESIGN.md#pool-host-console)).
- A pool machine is created from the engine's `discobox/pool-agent` image, started,
  and recorded on the pool row as registering. Repair stops and starts the same
  machine, keeping its disk; remove deletes it.
- A remote machine whose service does not answer reads `Unknown`, which is
  neither running nor stopped: ensure and repair answer
  `sandbox.ErrPoolNotReachable` rather than booting a second time, or stopping a
  healthy pool, for a network fault.
- Tests host pools in machines of the `fake` driver (a local driver, so they
  build that `poolMachine` themselves), which runs the engine, its shim, and the
  guest protocol as a real machine does.
- disco-vm's boxd driver reads `BOXD_API_KEY` from the server's environment,
  its only way today. Passing driver options (the key) through the provider's
  configuration needs `machine.New` to take them, and lands with the boxd pool
  (#127).

## Images

A disco-vm image is a Dockerfile's twin: `<driver>.yaml` beside the
Dockerfile, built from the same context and tagged `discobox/<name>`, with each
twin naming its parent's tag in its `from:` (see the root
[DESIGN.md](../../../DESIGN.md)). `BuildGuestImage` builds a driver's twins with
disco-vm's builder into the engine's store, from the checkout it is given,
parents first: base, the pool agent (`discobox/pool-agent`, the pool machine's
image, once #123 adds its twin), the sandbox agent, then the harnesses. The
chain is `twins` in `build.go`, the same one `build:boxd-images` builds for
boxd, which `TestTwinsMatchTheTaskfile` holds it to. A driver builds the twins it
has a spec for and skips the rest, and one with none answers
`ErrGuestImageBuildUnsupported`. One chain builds at a time per state root,
across every provider instance and every process that shares it (`lockChain`,
a file lock at `<root>/build.lock`). The root is the user's, not one server's
data directory, so two servers can share it. A chain resolves each parent by a
tag the chain itself moves, and disco-vm locks only each tag update, so two at
once would build one's children on the other's parents. A second build waits,
says so in its output, and gives up when its caller does.

The image is the server's, not the pool's, so the pool named only says where
the operation was asked from. `RestartHost` is refused: a machine is cloned
from its image, so a new image reaches a pool only when its machine is
replaced, and the build says so (`GuestImageBuild.Adoption`, which the CLI
prints in place of its restart hint).

disco-vm bakes a guest agent into every image it installs: the disco-vm
binary built for the guest, which on the docker driver is also the guest's
init. Left to disco-vm's default it would be the running binary, which here is
the server, so a build needs the provider's `agent` option (a path on the
server's machine) and is refused without it. Where that binary comes from in a
release is the images' to settle (#123, #126).

## Not Yet

- `EnsurePool` mints no bootstrap and `AcquirePoolAgentClient` answers that
  there is no pool agent: no driver starts one yet.
- No pool size fields: a machine's size is the driver's, decided with the pool
  images.
- The pool agent's lease will be the machine's endpoint: an image declares
  `service: {port: N}` and `Engine.Endpoint` returns its one address, boxd's
  public HTTPS URL or a transport into the guest (disco-vm ADR 0001 §2). That
  URL has no access control, so the pool agent authenticates every request.
  It lands with the pool agents (#122, #127).
- disco-vm ADR 0001 §1 moves exec, copy and shutdown onto `machine.Machine` and
  retires the guest client above the driver. `poolMachine`'s console and log go
  through that client (`Engine.Guest`) and move with it when it is implemented.
