# 26-10-27-767 — A machine pool runs what every pool runs

- **Status**: Accepted (decided by the maintainer on 2026-10-27, delivering
  #127)
- **Date**: 2026-10-27
- **Supersedes**:
  [26-10-23-551](26-10-23-551-a-vm-pool-bootstraps-its-own-agent-from-a-bundle-and-provisions-its-machines.md)
  §1's "Nothing runs on it but the pool agent and its proxy: no sandbox, no
  sandbox's build, no BuildKit, registry, mediator or `discobox-pool-runc`",
  and §2's "Nothing a machine sandbox's pool cannot use"; and
  [26-10-24-953](26-10-24-953-every-box-is-provisioned-from-an-ordered-list-of-bundles-and-a-pool-is-a-box.md)'s
  deferral of "A pool that turns its features off".
- **Relates to**: [26-10-09-106](26-10-09-106-disco-vm-is-a-pool-runtime-and-the-server-runs-its-machines.md) §1.

## Context

ADR 26-10-23-551 said a boxd pool's machine runs the pool agent and its proxy
and nothing else. ADR 26-10-24-953 then made every pool a box provisioned
from the pool bundle, whose `pool-builder` feature lays BuildKit, the
registry, the mediator and `discobox-pool-runc` beside `pool-agent`, and
deferred a pool that turns features off to the first pool whose sandboxes
are machines (#127), which is this one.

## Decision

A machine pool is provisioned from the pool bundle as a Docker pool is, every
feature of it applied. What a pool runs is a property of pools, not of the
host a pool happens to be on, so a pool's machine is not a narrower pool.
`discobox-provision apply` takes no list of features to turn off, and no
feature of the pool bundle is conditioned on a pool's runtime.

551 §1's sentence is what this replaces; its point stands that no sandbox,
and no sandbox's code, runs on the pool's machine: every sandbox is a machine
of its own (106 §1). So a machine pool's BuildKit mediator serves no builds:
a build's steps are a sandbox's code, and on the pool's machine they would
run beside the provisioning key and the driver's machine token (551 §4's
bound). The refusal is the pool's — every sandbox of it holds a certificate
the mediator would otherwise accept, at an address that is no secret — and a
machine sandbox, told no BuildKit endpoint, builds on its own machine.

## Alternatives rejected

- **A step selector on the pool's runtime**, marking `pool-builder` for
  Docker pools alone. It makes the bundle know a runtime, and a machine pool
  a different kind of pool.
- **`apply --without <feature>`**, chosen by the maker. The same split, made
  by each provider instead of the bundle.
- **A second pool bundle for machine pools**, named by its own role in the
  release. Two pools to release, which drift apart, as the image twins
  423 retired did.

## Consequences

- The VM provider makes a boxd pool's box with
  `poolruntime.ProvisionBox`, the Docker pool's own path, with nothing turned
  off.
- A machine pool's units for what only a Docker pool's sandboxes use run
  idle: its mediator binds nothing, and a machine sandbox is not given the
  pool's BuildKit.
