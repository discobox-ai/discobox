# 26-10-08-447 — A discobox tags the discoboxes it creates

- **Status**: Accepted
- **Date**: 2026-10-08
- **Supersedes**: in [0140](0140-a-discobox-reaches-the-discobox-api-through-its-pool-with-a-fixed-role.md),
  for the discoboxes the caller created and only for the route of §1: §4's
  refusal of everything outside the role, and its rejection of authority by
  creator, which
  [26-09-24-630](26-09-24-630-a-discobox-delivers-the-source-of-the-discoboxes-it-creates.md),
  [26-10-01-397](26-10-01-397-a-discobox-reads-and-types-into-the-terminals-of-the-discoboxes-it-creates.md)
  and [26-10-02-478](26-10-02-478-a-discobox-starts-and-stops-the-discoboxes-it-creates.md)
  lifted for source delivery, terminals, and power only.

## Context

A lead discobox creates workers and drives them (ADRs 0140, 26-09-24-630,
26-10-01-397, 26-10-02-478). When a worker's pull request merges, the lead
marks the worker for its person to clean up — a `to-delete` tag the person
filters on with `discobox ls --tag`. A sandbox's tags are its meta, held in
`~/.discobox/meta.json` inside it and changed from outside by
`PATCH sandboxes/{id}/meta` (ADR 0136), which the role refuses.

So the lead starts each finished worker, types into its harness asking it to
edit its own meta file, waits, and stops it again. That is three judged calls
and a prompt per worker for one tag, and it fails outright when the restarted
harness opens a first-run dialog instead of reading the prompt.

## Decision

### 1. The role changes the meta of a discobox the caller created

The sandbox role gains `PATCH sandboxes/{id}/meta`, allowed only when the
named discobox's recorded creator is the calling sandbox (the `createdSandbox`
ownership of ADR 26-09-24-630 §2). It changes the description and tags, and
nothing else: no name, no config, no runtime.

The sandbox service needs no change for it. The write into the sandbox leases
`exec:write`, which `authorizeRequestedScopes` already admits for a sandbox
caller because typing into a worker's terminal leases it too (ADR
26-10-01-397). Starting a stopped worker to take the change is what the call
already does for a person, and what the lead may already ask for on its own
(ADR 26-10-02-478).

### 2. The CLI command is `discobox tag`

The call gets an everyday root command, `discobox tag DISCOBOX KEY[=VALUE]...
[--rm KEY]... [--description TEXT]`, resolving its argument the way `start`
and `stop` do, so the lead's use names one command and one call.

## Alternatives rejected

**Keep it out: the lead can already have the worker edit its own file.** That
is the workflow this replaces. It reaches the same file through more judged
calls, a harness that may not be listening, and a prompt the worker could
misread; the direct call is narrower than the terminal input the lead already
holds.

**Tags only, not the description.** The route carries both, and the role is
decided by route. Splitting it would need a body check in the authorizer for a
field that is display text the worker's own agent rewrites freely.

**`admin box tag` as well.** The admin box forms are the raw ones that print
the record the API answered with; `admin box get` already prints that record,
meta included, and the change has no `--force` or other raw option to carry.

## Consequences

- A lead tags and describes the workers it created, without starting a
  harness conversation, and its person filters on what it set.
- A discobox a person created records no creator, so no discobox can change
  its meta through the API; a discobox still edits its own file directly.
- The in-box skills and the well-known credential's description say a lead
  may tag its own discoboxes.
