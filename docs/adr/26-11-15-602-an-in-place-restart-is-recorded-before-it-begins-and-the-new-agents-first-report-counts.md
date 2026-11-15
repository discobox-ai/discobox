# 26-11-15-602 — An in-place restart is recorded before it begins, and the new agent's first report counts

- **Status**: Proposed (would supersede [26-11-15-294](26-11-15-294-a-report-after-the-reconcile-started-the-pools-agent-is-the-new-agents.md) §1's restart row, "once the restart has returned", §3's "the agent changes in no way", and the Consequences line that a boot report landing before the restart returns is still missed)
- **Date**: 2026-11-15
- **Issue**: [#1203](https://github.com/discobox-ai/discobox/issues/1203)
- **Preserves**: 26-11-15-294 §1's other row (making or starting the box) and its in-process, per-run record; §2's later-of rule and every other condition of [26-11-14-624](26-11-14-624-after-an-upgrade-the-server-is-not-ready-until-its-pools-are.md) §1 (the clean, current-generation settle in this run, the kept-box rule); the agent's first report before it serves and its 30s heartbeat ([0030](0030-pool-agent-polls-and-pushes-sandbox-agent-status.md)); the single `try-restart` that restarts the agent and its proxy together.

## Context

26-11-15-294 counts a pool's ready report toward its upgrade only after the
reconcile's recorded agent start. For an agent restarted in a running box,
that start is recorded "once the restart has returned", so that no report
from the old agent can count. #1203 measured that the new agent's first
report always lands **before** the restart returns. It is not a race. So
every real upgrade (moved bundles) and every moved placement still holds
`upgrading_pools` for a full heartbeat (about 30s) after the reconcile
settles. Four runs on a Docker pool gave +29–30s every time. The agent's
journal for one: `Stopping` at .633, `Started` at .661, `pool marked ready`
at .665.

Why, checked against `main` at `292a7021d`:

- `discobox-pool-agent.service` is `Type=notify`. The agent sends `READY=1`
  (`agentsystemd.NotifyReady` in `pool-agent/agent.go`) before its first
  status report, which `startStatusReporter` then sends synchronously.
  That is milliseconds after systemd started it.
- Both in-place restarts run one `systemctl try-restart` on the agent **and**
  `discobox-proxy.service`: the apply's
  `pool-agent/bundle/features/pool-agent/restart.sh`, and
  `poolruntime.RestartPoolUnits` (`server/providers/poolruntime/box.go`, from
  `bundle.BootstrapUnits`). The proxy is ordered after the agent, so the job
  returns only once the proxy has started, after the agent's first report.
  The result then has to come back to the server over the guest exec.
- Both providers restart the agent in two places, each after placement is
  closed: `poolruntime.ProvisionBox` (the apply's `restart.sh`), then
  `RestartPoolUnits` when the placement moved. These are
  `poolMachine.converge` in `server/providers/discovm/poolmachine.go`
  (after `gate`) and the Docker engine's moved-bundles path in
  `server/providers/dockerworker/engine.go` (after `begin`).
- The store stamps `StatusReportedAt` with the server's clock
  (`UpdatePoolStatus` in `server/internal/store/pools.go`). The status
  request (`StatusRequest` in `pool-agent/bootstrap.go`) does not say which
  of a process's reports it is.

A boundary that is never earlier than the old agent's last report, and never
later than the new agent's first, does not exist on the server's side of the
restart. The restart happens inside one guest command, and the apply's
restart sits inside `discobox-provision apply`. The boundary has to come
from the report itself.

## Decision

### 1. An in-place restart is recorded before it begins

A provider that may restart the pool's agent in a running box, from a pool
reconcile, records the agent start **just before the first step that can
restart it**: once placement is closed (`gate`, `begin`), before
`ProvisionBox`. One record covers both restarts that may follow (the
apply's and `RestartPoolUnits`). It goes to the upgrade watch through the
reconcile's context, as 26-11-15-294 §1 has it, and replaces the row "once
the restart has returned".

The record now carries what kind of start it was:

| The provider | The moment | What it starts |
| --- | --- | --- |
| makes the box, or starts a box that was not running | just before it makes or starts it (26-11-15-294 §1, unchanged) | a box with no agent |
| may restart the agent in a running box | just before `ProvisionBox`, once placement is closed | a box whose old agent is still running |

A reconcile whose apply then restarts nothing (no moved bundle, the same
placement) has still recorded a restart start. That is harmless. No first
report follows it, so §3 falls back to an ordinary report after the
boundary, which the next heartbeat gives.

### 2. The agent marks its first report

The status request gains one optional boolean, `first`. The agent sets it on
the report `startStatusReporter` sends at start, and on no other. The
heartbeat never sets it. A first report that fails ends the process (today's
behaviour), so each agent process sends at most one accepted first report.
An agent from before the field sends none. The server reads an absent field
as false.

The server records, in process for this run, when it last received a ready
first report for each pool, on the clock it stamps `StatusReportedAt` with.
The upgrade watch reads it beside the recorded start. It is not persisted,
for the reason 26-11-15-294 keeps the start in process: it describes this
run's agents.

### 3. Which report counts after a restart

For a pool whose latest recorded start is a restart in a running box, the
last condition of 26-11-14-624 §1 holds once either is true:

- a ready **first** report was received after the recorded start; or
- any ready report was received after the settle (`ReconciledAt`), as
  before 26-11-15-294.

The old agent's first report was received when it started, long before the
recorded start, so it never passes the first test. Its heartbeats are not
marked, so one it sends while it stops never passes the first test either.
The new agent's first report is marked and received after the start, so it
passes, however soon it lands. The second test is the fallback: it covers a
restart that restarted nothing, and a first report that was lost.

For a pool whose latest start made or started its box, 26-11-15-294 §2 is
unchanged. No agent ran there, so any ready report after the start counts.
For a pool with no recorded start, the settle is the boundary, as today.

The clean, current-generation settle and the kept-box rule still apply in
every case. A report that passes before the settle only waits for it.

### 4. Scope

Only the upgrade watch reads the first-report record. Pool health, `Ready`,
`RegisteredAt`, `markStarted` and the heartbeat are unchanged. The agent
changes in the one field it sends. The restart stays one `try-restart`
transaction in `restart.sh`, the provision runner and `RestartPoolUnits`.

## Alternatives rejected

- **Split the restart into stop, record, start.** It covers
  `RestartPoolUnits` only. To cover the apply, `restart.sh` and the
  provision runner would have to give up the single `try-restart`
  transaction that orders the proxy with the agent, and the server would
  have to reach into the middle of `discobox-provision apply`. That is a
  bigger change, to the riskiest step of an upgrade, for the same result.
- **Have the apply report when it stopped the agent.** It needs a new
  channel out of `discobox-provision apply` back to the reconcile, kept in
  order with the restart it reports. It is the hardest of the three to keep
  correct, and it still leaves `RestartPoolUnits` to be split.
- **Identify the agent in its report (a boot nonce, a start time, a release
  digest).** 26-11-15-294 rejected it because the server would still need to
  know which identity is the new one. A first-report mark needs no identity.
  It says only "this process has just started", which, received after the
  recorded start, can only be an agent started after it.
- **Send `READY=1` after the first report.** The restart would still return
  only after the report, so the report still lands first. It also delays
  the proxy behind a round trip to the control plane.
- **Record the start before the restart, and count any report after it.**
  The old agent's heartbeat can land between the record and the restart.
  That is the early boundary 26-11-15-294 refused, for the right reason.

## Consequences

An upgrade that applies moved bundles, or moves a running pool's placement,
clears once the reconcile settles. The new agent's first report has always
already landed by then. In #1203's runs, `every pool is upgraded` would
follow the settle by moments, not by about 30s.

The status request gains a field the server must accept from new agents and
not require from old ones. The OpenAPI document describes it as what it
means (the process's first report), with no ADR reference.

A narrow case stays open on purpose. An old agent that crashes, and that
systemd starts again from the old binary between the recorded start and the
apply's swap, sends a marked first report after the boundary. If the new
agent then also fails its own first report after `READY=1`, that old report
clears the pool's upgrade. The settle still waits for the restart to return
cleanly, the old agent was running moments before, and the pool's health
then tracks the failing agent on its own. Closing this would need the
agent's identity, rejected above.

A restart whose pool agent stayed up still waits up to one heartbeat after
the settle (26-11-14-624's Consequences, unchanged for that case).

Delivering this depends on #1194's implementation of 26-11-15-294 landing
first. This changes one of its boundaries and adds the first-report test
to its watch.

## Open

Left to implementation, not decided here:

- The wire name of the field (`first` is a placeholder) and its OpenAPI
  description.
- How the agent service hands a first report to the upgrade watch (a call
  beside the status store update, or the store returning it), and how the
  recorded start carries its kind (a field on 26-11-15-294's reporter, or
  two reporters).
- Whether the Docker engine has a restart path besides the moved-bundles
  one. Wherever a reconcile may restart a running pool's agent, it records
  the start before it, as §1 says.
