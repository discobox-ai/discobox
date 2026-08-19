# 0051. The pool host console attaches through the driver's Docker client

Status: Accepted

## Context

Backends that are hard to debug are the ones where the pool host is a machine
we do not otherwise log into: a WSL guest (`wslc`), a libkrun microVM, a
DigitalOcean droplet, and soon a macOS VM. When one of those does not work, the
symptom the control plane sees is a pool that never registers, and the thing an
operator needs is a shell on that host — its processes, its network, its mount
table, its Docker daemon.

Discobox already has an attach protocol (`execstream`, ADR 0008) and a route
shape for it: the sandbox exec attach, where the control plane authorizes the
project, mints a scoped pool-agent token, and reverse-proxies the websocket to
the pool agent, which owns the process. Reusing that shape for a host console
was the obvious first design, and it comes with a real benefit — a
`pool:console` scope, so the capability is explicitly granted rather than
implied by provider access.

## Decision

The console is opened by `dockerworker.Engine` through
`Driver.AcquireDockerClient`, not by the pool agent:

1. `sandbox.PoolRuntime.OpenConsole` is a required method on the pool runtime
   interface, implemented by `poolruntime.Provider` over a new required
   `RuntimeProvider.OpenConsole`, implemented by the engine.
2. The engine creates one privileged container per pool host from the pool
   image, in the host's PID/IPC/network/UTS/cgroup namespaces, with `/` at
   `/host` and the host Docker socket bound, and attaches to its TTY.
3. The container is persistent and reattachable, and is removed only by
   `RemovePool`.
4. The control plane serves it at
   `/api/projects/{projectId}/pools/{poolId}/console`, terminating the
   websocket itself and pumping `execstream/frame` to that TTY.

## Consequences

- The console answers whenever the host's Docker daemon is reachable, including
  when the pool agent never registered, exited, or is the thing being debugged.
  This is the whole reason the feature exists, and it is what the pool-agent
  design could not offer.
- There is no pool-agent request, so there is no pool-agent scope. The
  capability's authorization is the control-plane route's alone.
- The engine now creates a container no reconcile accounts for. It is kept out
  of drift detection by label (`LabelPoolConsole`, never `LabelPoolAgent`) and
  out of the pool agent's reaping by not carrying `discobox.sandbox.managed`,
  and pool teardown removes it explicitly.
- A backend that is not Docker-hosted — the Kubernetes driver of ADR 0005 —
  will have to implement `OpenConsole` in its own terms (an exec into a
  privileged pod on the node) rather than inheriting it. That is the right
  shape: "a shell on the host" is meaningful on every backend, while "the pool
  agent runs a container for you" is not.

## Alternatives

**A pool-agent endpoint with a `pool:console` scope.** Rejected: the console
would then require the pool agent to be healthy and registered, which is false
in most of the situations that motivate a console. The scope is a real loss —
see the open issue below.

**Both, with the driver path as a fallback.** Rejected for now: two code paths
for one capability, where the fallback is the one that always works. If a
future need appears for a console on a *healthy* pool that the control plane
cannot reach directly, this is the design to revisit.

**A provider-scoped console (`providers console <id>`).** Rejected: on every
VM-backed driver the Docker daemon is per pool, so a provider-addressed console
has no unambiguous target. The pool is the runtime host (ADR-0006), so the pool
is what a console attaches to.

## Deferred: administrative authorization

The console is a root shell on the pool host, and the control plane has no
administrative role today: `ProjectAuthorizer` grants it to any member of the
project. This is accepted deliberately and revisited as soon as the server
grows a role or permission model — at which point the console route must
require the administrative role rather than project membership, and this ADR is
superseded by the one that introduces it.
