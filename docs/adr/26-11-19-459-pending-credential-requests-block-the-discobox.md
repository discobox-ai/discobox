# 26-11-19-459 — Pending credential requests block the discobox

- Status: Proposed
- Date: 2026-11-19
- Issue: [#1463](https://github.com/discobox-ai/discobox/issues/1463)

## Context

The credential service saves an agent's request as pending against its sandbox
in `server/internal/resources/secrets/agentcredentials.go`. The console already
uses pending requests to derive box standing in `cli/internal/tui/list.go` and
`data.go`. A background `discobox-access request --wait` polls for the answer
(`access/commands.go`). The primary terminal can keep reporting `working`
throughout the approval wait.

`cli/internal/cli/programstatus.go` displays the primary terminal's OSC 7501
report in the AGENT column. `cli/internal/cli/terminal_drive.go` sends
`blocked` and `finished` as program-state predicates, and
`sandbox-agent/shimruntime/text.go` matches reports newer than the terminal
cursor. The saved request can therefore remain pending while both predicates
time out.

## Decision

1. The server derives a box-level permission blocker from the presence of any
   pending secret request whose target is that project and sandbox. The
   persisted request status is authoritative. The blocker applies whether the
   caller waits, runs in the background, or has exited. Approval or denial
   clears the blocker when the last pending request leaves that state. A
   request on another box has no effect. Apply the existing sandbox access
   checks and expose only the blocker, without request contents.
2. On a running box, `discobox ls` displays `blocked:permission` in AGENT
   while the blocker holds, taking precedence over the primary terminal's
   report, including when there is no report. When it clears, the column
   reflects the latest primary terminal report again. The existing display
   rule for stopped boxes and the console's standing rules continue to apply.
3. `wait --until blocked` and `--until blocked:permission` also match the
   box-level blocker, including when an explicit terminal is selected.
   Observe the current state on entry and changes during the wait, within the
   existing deadline and cancellation rules. An already pending request
   qualifies even with `--after` or a later last-input cursor. If it is still
   pending on a subsequent wait, it qualifies again. Return a successful
   `blocked:permission` result with a valid terminal resume point and make
   the source distinguishable in the structured result. A request resolved
   before observation does not satisfy this predicate. Existing box startup
   and stopped-box wait behavior still applies.
4. Terminal predicates remain cursor-based, including terminal-reported
   permission prompts. `finished` still requires a matching terminal report.
   A combined wait can complete on either condition. The request state has
   no effect on the raw program report, terminal quiet time, or power policy.

This amends
[ADR 26-10-16-424 §5](26-10-16-424-a-terminals-status-is-what-its-program-reports-over-osc-7501.md)
for the list column and
[ADR 26-11-04-204 §§3–4](26-11-04-204-help-is-grouped-and-colored-and-a-terminal-is-driven-from-the-top-level.md)
for the additional wait predicate.

## Alternatives considered

- Teach the credential client or each harness to emit OSC 7501 for permission
  waits. A background tool may have no terminal, and a one-time report before
  the cursor would still be missed. A second writer also cannot reliably
  restore the harness status or know whether other requests remain pending
  when its own wait ends.
- Match only request creation events after the wait cursor. This misses
  unresolved approvals that predate the cursor and would allow a historical
  event to stand for a request that has already been answered.

## Consequences

A box can require approval while its harness continues other work. List
output and the permission wait reflect that outstanding decision. The
implementation must cover requests pending before and created during a wait,
approval and denial, multiple requests, and terminal-only waits.

The API field names, query strategy, and how request changes wake an active
wait are open for implementation. Listing should read blockers in bulk and
active waits should observe changes without requiring an extra CLI process or
an attached terminal.
