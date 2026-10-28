# 26-10-27-767 — A machine pool runs what every pool runs

- **Status**: Accepted (decided by the maintainer on 2026-10-27, delivering
  #127)
- **Date**: 2026-10-27
- **Supersedes**:
  [26-10-23-551](26-10-23-551-a-vm-pool-bootstraps-its-own-agent-from-a-bundle-and-provisions-its-machines.md)
  §1's "Nothing runs on it but the pool agent and its proxy: no sandbox, no
  sandbox's build, no BuildKit, registry, mediator or `discobox-pool-runc`";
  §2's "Nothing a machine sandbox's pool cannot use"; and §4's "no sandbox
  code and no sandbox build runs on the pool's machine", for a sandbox's
  build, with the bound it gave the key on that machine; and
  [26-10-24-953](26-10-24-953-every-box-is-provisioned-from-an-ordered-list-of-bundles-and-a-pool-is-a-box.md)'s
  deferral of "A pool that turns its features off".
- **Relates to**: [26-10-09-106](26-10-09-106-disco-vm-is-a-pool-runtime-and-the-server-runs-its-machines.md) §1,
  [0044](0044-builds-run-on-a-pool-shared-buildkit.md).

## Context

ADR 26-10-23-551 said a boxd pool's machine runs the pool agent and its proxy
and nothing else, and bounded the credentials that machine holds — the
provisioning key, and boxd's in-VM token, which acts on the org and which the
machine keeps because boxd peers an isolated machine only with machines that
are not — by "no sandbox code and no sandbox build runs on the pool's
machine". ADR 26-10-24-953 then made every pool a box provisioned from the
pool bundle, whose `pool-builder` feature lays BuildKit, the registry, the
mediator and `discobox-pool-runc` beside `pool-agent` (ADR 0044's
pool-shared builds), and deferred a pool that turns features off to the
first pool whose sandboxes are machines (#127), which is this one.

## Decision

A Docker pool and a machine pool provide the same functionality. A machine
pool is provisioned from the pool bundle as a Docker pool is, every feature
of it applied, and its agent prepares and runs the pool-shared builder there:
BuildKit, the registry and the mediator, which serves the pool's sandboxes'
builds as a Docker pool's does. What a pool provides is a property of pools,
not of the host a pool happens to be on. `discobox-provision apply` takes no
list of features to turn off, and no feature of the pool bundle is
conditioned on a pool's runtime.

**The cost, stated.** A build's steps are its sandbox's code, and on a
machine pool they run on the pool's machine, beside the provisioning key and
boxd's in-VM token, both of which reach the provider's org:

- **Escaping BuildKit's containment** reaches what those credentials reach:
  every pool of the provider and their sandboxes, and any other machine of
  the org (551 §4's account of the key, now without its bound).
- **Without escaping anything,** a build step has the network BuildKit gives
  it (`--oci-worker-net bridge`), routed out through the pool's machine, and
  a pool machine has a route off-box of its own (to the control plane, the
  driver, the bundle registries). Only the proxy settings the mediator
  injects keep a step on the proxy, and a step that ignores them goes around
  the proxy's policy, secret swapping and audit. Whether boxd serves its
  in-VM token, or its integrations, to a request from inside the machine is
  not known (551 §4 asks boxd); if it does, a build step reaches the org with
  no escape at all. BuildKit's own pulls of base images are not on the
  proxy either: buildkitd is given the agent's inherited proxy environment,
  which on a pool machine is usually none, so they leave over the machine's
  route. A Docker pool's builds are kept on the proxy the same
  way, by the settings and not by the network: its container has a route
  out too, which its own proxy uses, so a build step there that ignores the
  settings likely leaves the same way. That predates this ADR, and is not
  changed by it. Holding a build's network to the proxy is not required
  before a machine pool schedules.
- **The pool's services** — the proxy, the mediator (17081) and the
  registry (5000, plaintext, unauthenticated) — listen on every interface
  of the pool's machine, as they do in a Docker pool's container, and are
  reached by the pool's sandboxes over the pool's network, which no machine
  outside the pool is on. The machine's one public URL is the driver's
  service, which the pool agent alone listens on (disco-vm ADR 0005), so
  none of them is reached from outside; a driver that exposed more would
  break this. On a machine pool their hop crosses the driver's network
  between the pool's machines rather than staying on one host.

This is accepted for the same functionality. A sandbox itself still never
runs on the pool's machine: every sandbox is a machine of its own (106 §1),
and only its builds come here. Pools that must not reach each other are
given providers, and orgs, of their own, as 551 §4 says.

The pool's services are named by the proxy's server name
(`discobox-pool-proxy`), which a Docker pool's container answers to as an
alias on its network. Every pool's host names itself so in its hosts file,
never by DNS, for its own builder and a build's forwarder; a sandbox machine
reaching its pool by that name is the pool hop #129 makes.

## Alternatives rejected

- **A machine pool serves no builds** (as this ADR first said, before
  anything shipped against it): its sandboxes build on their own machines.
  It keeps 551 §4's bound, but a machine pool would provide less than a
  Docker pool, and its sandboxes would share no solver cache.
- **A builder machine of the pool's own**, isolated and holding no key, which
  the mediator forwards to. The same functionality with the bound kept, at
  the price of a second machine per pool and a forwarding path built for it;
  revisit if the cost above proves too high.
- **A step selector on the pool's runtime**, marking `pool-builder` for
  Docker pools alone. It makes the bundle know a runtime, and a machine pool
  a different kind of pool.
- **`apply --without <feature>`**, chosen by the maker. The same split, made
  by each provider instead of the bundle.
- **A second pool bundle for machine pools**, named by its own role in the
  release. Two pools to release, which drift apart, as the image twins
  423 retired did.

## Consequences

- The VM provider makes a boxd pool's box with `poolruntime.ProvisionBox`,
  the Docker pool's own path, with nothing turned off.
- The pool agent prepares the builder's configuration on every pool, and a
  machine sandbox is given the pool's BuildKit endpoint as any sandbox is.
- A compromise of a machine pool's build reaches the provider's org; the
  pool's `hostCredential` says what its machine holds.
