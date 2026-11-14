# 26-11-14-624 — After an upgrade, the server is not ready until its pools are

- **Status**: Accepted (narrows [0069](0069-staging-pool-images-is-a-condition.md)'s "the server binds, migrates, builds its services and reports ready": readiness now also waits, bounded, on the pools the server had being brought onto its agent; image staging still holds nothing back)
- **Date**: 2026-11-14
- Accepted by the owner in the issue and in the delivering box, 2026-11-13 and 2026-11-14
- **Issue**: [#1134](https://github.com/discobox-ai/discobox/issues/1134); pauses [#1106](https://github.com/discobox-ai/discobox/issues/1106), [#1118](https://github.com/discobox-ai/discobox/issues/1118) and [#933](https://github.com/discobox-ai/discobox/issues/933)
- **Preserves**: the `starting` state and its 503 for every route; [26-11-04-167](26-11-04-167-an-explicit-start-waits-for-its-pool-to-return.md) and [26-11-05-143](26-11-05-143-a-start-waits-for-a-compatible-pool-and-its-pending-upgrade.md) whole: their request waits still cover a client that does not wait on health, and a pool lost after the server settles; [26-10-24-953](26-10-24-953-every-box-is-provisioned-from-an-ordered-list-of-bundles-and-a-pool-is-a-box.md) §5's rule that a running pool is never taken down over its bundles.

## Context

A server upgrade replaces each pool's agent: the release moves, every pool
reconciles, and its box is provisioned again in place (ADR 26-10-24-953 §5).
Until now the server reported `ready` the moment its router took over, so work
arriving while the pools were being replaced met them half-replaced:

- a Docker box's first `start` said `started` before it was usable, and `exec`
  got a 502 (#1106);
- a box stopped mid-provisioning started with its features failed (#933);
- streams the old pool agent served were cut (#1118).

v0.19.4-rc.1's upgrade validation found these
(discobox-ai/release-validation#39). Each had a stopgap of its own in flight.

The owner, on the issue (2026-11-13): "I want to put a blank hold on any issue
where after upgrade while the system is settling we get startup errors. I'm
willing to say at this point that we just have an upgrade process and not
until all pools are upgraded do we pass the healthcheck. so we can open the
port but the healthcheck should not go to a full ready state until the pools
are upgraded. In testing we can wait for that condition and then later we can
figure out HA and other things like that."

In the delivering box (2026-11-14), on what a pool that never upgrades does:
"keep pool upgrading as a 503 for now, with the 5 minute timeout, then ready
with warning until it clears", and on tests: "For our testing we can wait for
all clear."

ADR 0069 took staging pool images out of server startup and rejected a
`warming` health state for it. Its objections were to startup starting pools
nobody asked for, converging them outside the reconcile engine, and the API
not answering. None holds here: the pools waited on are the ones the server
already runs, they converge in the engine as a release move already makes
them, and the API serves throughout.

## Decision

### 1. Which pools are waited on, and when one is upgraded

At startup, after the pools' health checks begin and before anything
reconciles one, the server fixes the set to wait on: every present pool that
has registered (an earlier server ran it). A pool made later has no older
agent and is not waited on.

A watched pool is upgraded once, in this server run, a reconcile of its
current generation has settled without an error, its provider placed this
server's release in its box rather than keeping the previous one, and its
agent has sent a ready status report since that reconcile settled. A report
from before the reconcile may be the old agent's, so it does not count. A
provider keeps a running pool's box when the release does not resolve or
apply (26-10-24-953 §5) and still settles cleanly; it reports that it did, and
the pool stays waited on until a later reconcile places the new box. Startup
marks every pool for a reconcile, so a restart with no release move settles
too.

A watched pool that is deleted, or whose provider instance is removed or
disabled, drops out. Once none is left the wait has cleared, and it stays
clear for the rest of the run: a pool that fails later is that pool's health,
not an upgrade in progress.

### 2. Health reports `upgrading_pools` while it waits, by count

The listeners still bind at once and `starting` is unchanged. Once the router
takes over, the API serves every route — the pools report through it — while
`/healthz` answers:

| State | `/healthz` | When |
| --- | --- | --- |
| `upgrading_pools` | `503` | a watched pool is not upgraded, and the wait has not run out; `poolsUpgrading` counts them |
| `ready` | `200` | none is left; or the wait ran out, and `poolsUpgrading` and a warning count those still upgrading until none is |

"Settled" is `ready` with no `poolsUpgrading`.

`/healthz` answers anyone, before authentication, on every listener, so it
counts the pools and never names them: which pools a server runs, and a
pool's error, are its project's. The server's log names each pool and what it
waits on, whenever that changes.

### 3. The wait is bounded at five minutes

The wait runs at most `health.PoolUpgradeWait`, five minutes from handover,
the same ceiling an explicit start already waits on its pool (26-11-04-167
§2). After it, an offline, unregistering, backing-off or kept-box pool no
longer holds the server's health: the server reports ready, warning how many
pools are still upgrading, until none is. The owner chose this over never
becoming ready "for now".

### 4. Clients wait it out as progress

The CLI's server-start wait treats `upgrading_pools` as progress, on its own
line ("Upgrading pools: <n> left"), never as an error. It gives it a deadline
of its own from when it is first seen: the server's bound and a margin, so
it ends in the server's ready answer, not its own timeout. `admin server
status` shows it as a warning with the count, and still asks the API, which
serves. Commands that autolaunch the server wait out the window like any
start, `admin pool` ones included; `admin server logs` reads the log without
the server.

### 5. Tests wait for settled

Every end-to-end or upgrade test that restarts or upgrades a server waits for
settled health before it uses a box, and records the time to settle as its own
number. The release validation does the same (discobox-ai/manager dc561da).

## Alternatives rejected

- **Ready at once, with a warning while pools upgrade.** The owner weighed it
  and kept the 503 for now: the autolaunching CLI waits on a 503 and not on a
  warning, which is what keeps the first command after an upgrade off a pool
  being replaced.
- **Never ready while a pool cannot upgrade.** One unreachable pool would hold
  every command on every other pool behind it indefinitely.
- **Report it as `starting` with an "upgrading pools" phase.** Every existing
  client already waits on `starting`, but `starting` promises that every route
  answers 503, and clients act on that — `admin server status` stops asking the
  API. Here the API serves, and pools must reach it to finish.
- **Name the pools, and why each waits, in the health answer.** It would
  publish every project's pools and their provider errors to anyone who can
  reach the port.
- **A separate `/readyz` beside `/healthz`.** The conventional split, but a
  second contract every client and test would have to learn, for one state.
- **Decide "upgraded" by comparing the agent's version or bundle digest.**
  26-11-05-143 rejects release labels as a contract. Whether the box was
  placed is the provider's to say, and it says so for the one case its clean
  settle hides.
- **Fail the reconcile when a provider keeps the previous box.** A recorded
  pool failure ends every start waiting on the pool, and a failed reconcile of
  a pool with sandboxes repairs it — what 26-10-24-953 §5 keeps a running pool
  from over its bundles.
- **Keep the per-issue stopgaps (#1106's start wait and its kin).** Each
  closes one race in one request path and leaves the next; settling first
  closes the window for every client that waits on health.

## Deferred

A handover with no window at all — a second server, or a pool agent replaced
without cutting the streams it serves (#1118) — is deferred. Revisit when the
server runs more than one instance or a release must upgrade without
interrupting a running box's streams.

## Consequences

A CLI from before this decision does not know `upgrading_pools`, and treats a
503 it cannot read as an error: for up to five minutes after an upgrade, its
commands against that local server fail rather than wait. A server is upgraded
by a CLI of its own release, which waits; an older CLI on the same machine is
the exception, and is bounded by §3.

A remote client that never reads health is not held back; the request waits of
26-11-04-167 and 26-11-05-143 still cover its explicit starts.

A plain restart reports `upgrading_pools` until its pools reconcile and report
again: about one heartbeat interval (30s), or the rest of a failure backoff a
pool carried over from the last run, at most the bound.
