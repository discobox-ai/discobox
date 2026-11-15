# 26-11-15-294 — A report after the reconcile started the pool's agent is the new agent's

- **Status**: Accepted (supersedes [26-11-14-624](26-11-14-624-after-an-upgrade-the-server-is-not-ready-until-its-pools-are.md) §1's "its agent has sent a ready status report since that reconcile settled. A report from before the reconcile may be the old agent's, so it does not count", for a pool whose agent this run's reconciles started, and the Consequences line that puts a plain restart's hold at about one heartbeat interval)
- **Date**: 2026-11-15
- Accepted by the owner by merging #1195, 2026-11-15
- **Issue**: [#1194](https://github.com/discobox-ai/discobox/issues/1194)
- **Preserves**: 26-11-14-624 §1's set of watched pools, its kept-box rule (`sandbox.ReportBoxKept`) and its clean, current-generation settle; §§2–5 whole; [26-10-24-953](26-10-24-953-every-box-is-provisioned-from-an-ordered-list-of-bundles-and-a-pool-is-a-box.md) §5; the pool agent's 30s status heartbeat and its first report before it serves ([0030](0030-pool-agent-polls-and-pushes-sandbox-agent-status.md)).

## Context

After a plain server restart, `/healthz` held `upgrading_pools` for about a
minute on a libkrun pool whose VM was down: about two heartbeats, not the
one 26-11-14-624's Consequences expect (#1194). The run's log
(discobox-ai/release-validation, `runs/upgrade-v0.19.5-rc.1/`) shows
each restart take the same shape: the pool VM booted and its reconcile settled
20–54s after the server came up, and the server then waited another 28–30s
before `every pool is upgraded`.

The second wait is structural. It holds whenever a reconcile starts or replaces
the pool's agent. Checked against `main` at
`bd4db631fc9c558b78c7633759817e6b4f7465d5`:

- `upgradeWaitingOn` (`server/internal/resources/pools/upgrade.go`) counts a
  ready report only when `StatusReportedAt` is not before the settle's
  `ReconciledAt`. That is 26-11-14-624 §1's guard against the old agent's
  report.
- The agent sends its first report synchronously at start, before it serves
  (`startStatusReporter` in `pool-agent/agent.go`, called before `Serve`). It
  then reports every `statusReportInterval` (30s), on a ticker that starts at
  boot.
- The pass that starts the agent is usually not the pass that settles. On a
  VM pool, `poolMachine.converge` (`server/providers/discovm/poolmachine.go`)
  starts a stopped machine (`h.engine.Start`, then `markStarted`). It then
  needs `h.engine.Guest`, which a booting machine does not yet answer. The
  pass fails `ErrPoolNotReachable` and requeues. A later pass finds the
  machine `Running`, starts nothing and settles, by which time the new agent
  has sent its boot report. That report is before `ReconciledAt`, so it is
  discarded, and the pool waits for the next tick.
- An in-place upgrade restarts the agent (`poolruntime.RestartPoolUnits`,
  from `poolMachine.converge` and the Docker engine's
  `applyMovedPoolBundles`), and the settle follows within moments. Whether
  the new agent's boot report lands before or after the settle is a race.
  #1143's own verification lost it: the agent restarted at +9s and the pool
  settled at 39s.

A Docker pool whose agent stays up across the restart is the case
26-11-14-624 described: its next tick after the settle clears it, at most
30s later. CI's `ux-smoke` restart (16–17s) is that case, which is why it
never showed this.

So the rule "only a report after the settle counts" rejects the first report
the new agent sends whenever this run's reconciles made, started or
restarted it. The settle time stood in for "not the old agent's", and it
answers wrongly whenever the reconcile itself replaced the agent.

## Decision

### 1. A reconcile says when it started the pool's agent

A provider that makes or starts the pool's box, or restarts the pool's agent
in it, from a pool reconcile reports that to the reconcile, with the moment
after which no report can come from the agent that was there before. The
report goes through the reconcile's context, the way a kept box is reported
(`sandbox.ReportBoxKept`):

| The provider | The moment |
| --- | --- |
| makes the box, or starts a box that was not running (`poolMachine.converge`'s `made \|\| started`; the Docker engine's new pool container) | just before it makes or starts it: no agent ran there, so none can report from it |
| restarts the agent in a running box (`RestartPoolUnits`) | once the restart has returned: the old process has exited |

The reconciler forwards each report to the upgrade watch **when it is
made**, not at the settle. The pass that started the agent is often not the
one that settles (Context), and its start must still count. The watch keeps
the latest start per pool for this process's run, as it keeps a kept box. A
later start replaces it. It is never persisted: it describes this run's
agents.

### 2. A ready report after the latest recorded start counts

26-11-14-624 §1's last condition becomes: the agent has sent a ready status
report since the later of

- the latest agent start this run recorded for the pool, if any, and
- otherwise, the settle (`ReconciledAt`), as today.

Every other condition stays as it was. The reconcile of the pool's current
generation must still have settled cleanly in this run, and a settle that
kept the previous box (`ReportBoxKept`) still holds the pool waited on,
whatever start was recorded.

A report after a recorded start is the new agent's. Either no agent ran in
the box before the start, or the old one had exited when the restart
returned. A pool with no recorded start (its agent stayed up through the
restart) keeps today's rule. Its agent was not replaced in this run, so the
settle is the only boundary there is.

### 3. Scope

Only the upgrade watch reads the record. Pool health, `Ready`, `markStarted`
and the agent's heartbeat are unchanged, and the agent changes in no way.

## Alternatives rejected

- **(b) The agent reports again as soon as it serves.** Pool-agent only, but
  it still races the settle: on a VM pool the settle can come many seconds
  after the agent serves (the requeues on `ErrPoolNotReachable`), and a
  second early report is discarded like the first. It shortens the wait
  sometimes. It never removes it.
- **Keep the start on the pool row (or stamp it at the settle).** Recording
  it only on the pass that settles misses the start, which an earlier pass
  made. Persisting it gains nothing: a server that restarts begins a new run,
  whose starts are its own, and 26-11-14-624 already keeps the kept-box
  record in process for the same reason.
- **Identify the agent in its report (a boot nonce, a start time, a release
  digest).** The server would still need to know which identity is the new
  one. An agent-side start time would compare across clocks. A release
  digest is the version comparison 26-11-14-624 rejected.
- **Have the reconcile wait for the new agent's report before it settles.**
  It holds the reconcile's lease on an agent's timing, and stalls the
  level-triggered engine on a pool that boots slowly. Readiness is the
  watch's question, not the reconcile's.
- **The server asks the agent to report at once after the settle.** A new
  control route and a round trip on every settle, to work around a boundary
  the server can know without asking.
- **Shorten the heartbeat.** It only scales the extra wait, and costs every
  pool more reports for the whole of its life.

## Consequences

A restart that boots a pool's host, and every real upgrade, clears once the
reconcile settles and the new agent has reported. Neither waits an extra
heartbeat. In #1194's run the warm-stopped restart would go from about 57s
to about 27s. What remains is the VM booting.

A restart whose pool agent stayed up still waits up to one heartbeat after
the settle (26-11-14-624's Consequences, unchanged for that case).

A restart's boot report that lands before the restart has returned over the
guest exec is still missed, and that pool waits for its next tick, as it
does today. The boundary is placed late on purpose: one placed early could
count a report the old agent sent while it was stopping.

## Open

Left to implementation, not decided here:

- The name and shape of the context reporter (beside `ReportBoxKept`, or one
  reporter that carries both facts).
- Finding every Docker engine path that makes or replaces a pool's container
  (a stopped container removed and made again, the pool VM replaced). §1
  covers each one: wherever a reconcile starts an agent, it reports it.
