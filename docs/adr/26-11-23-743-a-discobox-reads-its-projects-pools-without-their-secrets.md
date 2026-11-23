# 26-11-23-743 — A discobox reads its project's pools, without their secrets

- **Status**: Accepted
- **Date**: 2026-11-23

  The owner asked for this amendment in #1616, to land with its
  implementation; merging that PR accepts it.
- **Supersedes**, for reading a pool only:
  [0140](0140-a-discobox-reaches-the-discobox-api-through-its-pool-with-a-fixed-role.md)
  §4's refusal of pools. Its refusal of every other pool route (create,
  update, delete, default, cache, reconcile, logs, audit) and of projects
  stands.
- **Relates to**:
  [26-10-27-880](26-10-27-880-access-to-a-secret-is-a-binding-held-to-static-routes-and-no-judge-decides-it.md)
  §9, which gates the role behind `Sandbox.DiscoboxAPI`;
  [0145](0145-a-sandbox-declares-its-platform-and-a-non-linux-one-is-a-vm-template.md) §6,
  why a client reads a pool before it creates on it.

## Context

A discobox creates discoboxes in the sandbox role (ADR 0140 §4), which reads
no pool. A create may name its pool (`poolId`), and the server places it there
if the caller may create; but the CLI must read the pool first. It resolves
`--pool` from a name or short ID against the pool listing, and it reads the
pool's platform, because every path the create places in the new discobox is
spelled that platform's way (ADR 0145 §6). Refused both reads, a discobox can
create only on the project's default pool, whose platform it guesses.

The manager discobox overflows workers onto the project's other pools when its
host is busy. Every such create fails, so past the load limit nothing launches.

A pool's response carries more than placement needs: its provider instance and
that instance's configuration, the pool agent's public key, the credential its
host holds, its outbound proxy, its egress level, and agent-reported
conditions, resources and errors.

## Decision

### 1. The role lists and gets the project's pools

The sandbox role gains `GET pools` and `GET pools/{id}`, in the caller's own
project, behind `Sandbox.DiscoboxAPI` as the rest of the role is. Every other
pool route stays a person's.

Whether the caller may create on a pool is still the create's question,
answered by the server as for any create; reading a pool grants nothing.

### 2. A discobox reads only what placement needs

A pool read by a sandbox principal is built from a fixed list of fields:
identity (`id`, `projectId`, `name`, `providerInstanceId`), what it hosts
(`platform`, `sandboxOs`, `imageKind`), whether it takes sandboxes and how
much room it has (`health`, `ready`, `schedulable`, `degraded`, the sizes and
`available*` capacity), and its lifecycle (`desiredState`, `state`,
`stateChangedAt`, `generation`, `observedGeneration`, `createdAt`,
`updatedAt`). Nothing else is sent: not the provider instance or its
configuration, the agent's key, the host credential, the proxy, outbound or
egress settings, conditions, resources, provisioning progress, or error
messages. A field added to `Pool` later is not sent to a discobox until it is
added to that list.

## Alternatives rejected

**Send the whole pool, as a person reads it.** The provider configuration is
already stripped of secret fields, and nothing else in it is a credential
value. But a discobox would learn the project's provider accounts, proxies and
policies, which it does not need to place a create, and every field added to
`Pool` would reach it without anyone deciding it should.

**Remove fields a discobox must not see** rather than listing those it may.
The same objection: a new field reaches it by default.

**Let the server resolve a pool name in the create** and skip the CLI's read.
The CLI still needs the platform before it builds the create, and the create
API takes a pool ID; a second resolution path beside the listing buys nothing.

## Consequences

- `discobox run --pool <name or ID>` works from a discobox with the API as it
  does from a person's client.
- A discobox learns the names, platforms and capacity of its project's pools.
- A create with no `--pool` still reads no project, so a discobox places it on
  the default pool with an undeclared platform, as before.
