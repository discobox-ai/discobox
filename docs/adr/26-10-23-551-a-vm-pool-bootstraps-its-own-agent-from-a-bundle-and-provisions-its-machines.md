# 26-10-23-551 — A VM pool bootstraps its own pool agent from a bundle, and the pool agent provisions its machines

- **Status**: Accepted (§3's agent listening where `guest.ServiceFile` says
  narrowed by [26-10-27-345](26-10-27-345-a-machine-sandbox-is-made-again-in-place-and-its-bootstrap-names-where-its-agent-listens.md))
- **Date**: 2026-10-23
- **Supersedes**, for a remote driver (boxd now, disco-vm's
  `docker` driver and others later), and nothing of a local one (vz, hcs):
  [26-10-09-106](26-10-09-106-disco-vm-is-a-pool-runtime-and-the-server-runs-its-machines.md)
  §1's "The boxd pool machine" (its image twin, its bootstrap file "the pool
  image's", its machine runtime of seam instances, and its kind and capacity
  "learned through the seam") and the paragraph after it ("created from that
  image", "no credential of boxd's"); §2 whole — the engine in the server
  alone, the machine seam, and the provider as "the one process that can hold
  a boxd key the pool agent's own machine must not"; §3's "the machine seam is
  served on that connection"; §5's change to 0126's Consequence (a provider
  credential held by the server's provider, not the pool) and its 0144 posture
  bullet's "the boxd key never reaches it"; §6's provider as the backend of
  26-10-09-143 §2; its rejection of "The pool agent embeds the engine"; and its
  Consequences' machine seam served to a pool, machine runtime of seam
  instances, and the pool-agent image's boxd twin.
- **Restores**, for a remote driver,
  [0126](0126-a-sandbox-does-not-share-a-host-or-a-filesystem-with-its-pool.md)'s
  Consequence that a provider-hosted backend's credentials stay in the pool,
  which 106 superseded (§4).
- **Narrows**:
  [26-10-16-423](26-10-16-423-a-harness-is-a-provisioning-bundle-and-the-agent-provisions-any-box.md)
  §7's "a machine gets it copied through the provider, and the provider runs
  one command on its first boot" — for a remote driver it is the pool agent,
  not the provider (§3) — and its Consequence that the pool agent "asks a
  machine's provider to place the agent layer"; and
  [26-10-09-106](26-10-09-106-disco-vm-is-a-pool-runtime-and-the-server-runs-its-machines.md)'s
  deferral of disco-vm's `docker` driver, whose pool machine is bootstrapped
  as §1 here says.
- **Relates to**: [0063](0063-a-pool-agent-keeps-its-identity-key-and-registers-once.md),
  [0144](0144-a-pool-of-host-vm-sandboxes-runs-its-agent-on-the-host.md),
  [26-10-09-143](26-10-09-143-a-vm-sandbox-starts-its-agent-when-its-bootstrap-arrives.md),
  and the `vm` spelling of the provider and its image kind in
  [PR #132](https://github.com/discobox-ai/discobox/pull/132) (its ADR
  26-10-15-629).

## Context

ADR 26-10-09-106 has the server run every disco-vm machine and hold the
provider's credential, and the pool agent ask it for each operation through a
machine seam. A boxd pool's own machine runs the pool-agent image's boxd twin
(§1), and the seam carries 26-10-09-143 §2's boot contract and each machine's
lifecycle (§2). None of that has shipped: PR #132's provider skeleton is
unmerged, the boxd images (#123, PR #180) were closed, and the seam (#122) is
unwritten.

Two things it stands on have moved:

- **ADR 26-10-16-423** has a box boot its provider's own OS, and the pool
  place the platform bundle's agent layer, its units and pins, and run one
  command. Every `boxd.yaml` twin is gone from `main`, so 106's pool machine
  image no longer exists, and 423 does not say what replaces it. A Docker
  pool's runtime places `agentlayer.Archive` with `CopyToContainer` before
  start (`pool-agent/DESIGN.md`, "Delivering the Agent").
- **disco-vm** is now only the layer that boots a machine, copies files and
  runs commands. An instance runs straight from a driver's base, such as
  boxd's default image (its ADR 26-10-15-833); a machine is its guest
  operations, exec and copy (0001); `snapshot` makes an image of a stopped
  instance (26-10-15-882); an instance has one HTTP URL (0005, Proposed); a
  project scopes every resource and is in the name of what a driver makes
  (0004); and converging a guest is the client's job (26-10-15-703).

So a pool machine, like a sandbox machine, is a driver base that something
places files in and runs one command on. What is left to decide is who does
that for each, and who holds the credential to do it.

## Decision

### 1. The VM provider bootstraps a pool machine from the pool-agent bundle

The VM provider (`vm`, PR #132; `discovm` in 106) sits beside `dockerworker`.
The Docker driver, the pool-agent image and the Docker runtime's placement
are unchanged; nothing here is used by any provider but this one.

For a remote driver the provider makes the pool's machine the way the pool
makes a sandbox's (§3):

1. **Boot a base.** It creates the pool's machine from a driver base (boxd's
   default image unless the provider names another), in the provider's
   disco-vm project, named for the pool so a lost create is found by name, on
   a network of the pool's own (§4), and starts it.
2. **Place three things** with disco-vm's copy:
   - the **pool-agent bundle**'s tree (§2), under its digest;
   - the pool's **bootstrap** — `poolagent.Bootstrap` with the pool's runtime
     and driver — as a root-only file, whose path and form are the bundle's;
   - the **provisioning key** (§4), as a root-only file beside it, which the
     agent's engine reads for each call it makes to the driver (a key source,
     not a key fixed at open: the disco-vm issue), so a renewed key is a new
     file and nothing more.
3. **Run one command**: the bundle's `install`. It is idempotent and does, in
   order: install the one package the agent needs that the platform contract
   (423 §9) does not promise, `git`, where it is absent (Debian family only,
   as 423 §3), first, so that a failure there moves nothing; point the
   bundle's stable link at the tree just placed; lay the pool agent's and the
   pool proxy's units, with the proxy's ordered after the agent has written
   its environment, CA and control key (106 §1's ordering, which stands); and,
   unless a record it writes after each successful restart says both units
   already run this tree with these units, reload systemd, and enable and
   restart the agent and the proxy; and, once they run the tree, remove every
   other tree the provider placed beside it, which nothing runs from any
   more. Run again with nothing moved, it changes nothing; an install cut
   short before its restart took is finished by the next.
   The agent serves its API where disco-vm says the instance's service
   listens (`guest.ServiceFile`, written at every start; disco-vm ADR 0005),
   not at a port of its own choosing, so the machine's one URL reaches it.

That machine is then the ordinary Linux pool agent of 106 §1, as root, on
`layout.Container()`'s paths, with its proxy as a systemd unit. It registers
with the bootstrap's single-use token, keeps its identity key on the
machine's disk (0063), and is leased at the machine's one URL (106 §3, which
stands). Nothing runs on it but the pool agent and its proxy: no sandbox, no
sandbox's build, no BuildKit, registry, mediator or `discobox-pool-runc`
(106 §1, which stands).

**Across stop and start** the machine keeps its disk, which the driver must
offer (106 §1 asks it already). On boot, systemd starts the agent from the
units `install` enabled; the agent loads its identity key and spends no token
(0063); its disco-vm store (§3) is on the same disk, so it attaches to its
sandboxes' machines again (a remote driver runs no shim).

On every pool reconcile the provider starts the machine if it is stopped, and
compares its own record of what it placed — the bundle digest and the key's
ID and expiry — with what it now wants, never anything the machine reports.
The record is persisted with the pool in the server's database, since the
machine outlives the server's process (106 §2's "the provider adopts it
again"). A moved bundle is placed again and `install` run again, which
restarts the agent; a renewed key is placed alone, and restarts nothing. A
record that is lost costs a placement, not a key: an issued key is named for
the pool, so the driver finds it again by name and the provider revokes it.

A machine lost outside discobox is made again as a repair, and registers
anew. Its tree — the agent's identity, its sandboxes' records and its disco-vm
store — is lost with its disk, as a Docker pool's is with its volume (0126
Consequences). Its sandboxes' machines are then orphans in the pool's project
(disco-vm ADR 0004), which disco-vm lists and the repaired pool removes; it
does not adopt them, since nothing it holds says what they were.

### 2. The pool-agent bundle is its own bundle, built beside the pool-agent image

The pool agent is a bundle of its own, not a role of the platform bundle:

- **What it carries**: `/features/pool-agent/` with `discobox-pool-agent`
  (which is also the pool proxy, `discobox-pool-agent proxy`) and its units.
  Nothing a machine sandbox's pool cannot use: no BuildKit, registry,
  mediator, `discobox-pool-runc` or Docker configuration.
- **How it is built**: a `FROM scratch` target of `pool-agent/Dockerfile`,
  copying the binary from the same build stage the pool-agent image copies
  it from, so the image and the bundle hold the same binary; a
  multi-architecture index (`linux/amd64`, `linux/arm64`), as the platform
  bundle is (423 §2). `release:images` publishes it beside the image, and the
  watcher builds it as an OCI layout tar (423 §13).
- **How it is pinned**: named by the server's release as the pool-agent image
  is — a compiled-in default a release overwrites at link time, the release
  manifest (`Bundles.PoolAgent`), and a provider override — resolved to a
  digest in the server's blob store (423 §13), from which the provider copies
  it. A pool follows the digest its server's release resolves, as a Docker
  pool follows its pool-agent image; the machine never fetches it.

`install` is the bundle's one command. It applies no `provision.yaml`: the
pool machine needs two units and one package, and the provisioning runner is
the sandbox agent's, which a pool machine does not run.

### 3. The pool agent provisions its sandbox machines, and runs its own engine

For a remote driver, the pool agent opens disco-vm's engine itself, on a
state root on its machine's disk, in a disco-vm project of its own, with the
provisioning key (§4). The project is derived from the pool by the provider,
within disco-vm's project rule (lower-case letters, digits and single
hyphens, at most 32: a pool ID's `_` is not one of them, so it becomes a
`-`), never equal to the provider's own project for pool machines (§1), and
given to the agent in its bootstrap; the provider keeps it in its record
(§1). It names each
sandbox's machine by the sandbox ID, so a lost create is found by name and the
project's orphans are its own. It remains the runtime authority of ADR 0017
§§9–10: it decides and carries out every create, start, stop and delete, and
reports power state from its engine (106 §2's "Power state is the engine's",
now the pool's engine).

Each sandbox machine is provisioned as 423 §§7 and 9 describe a machine: boot
a driver base, copy in the platform bundle's agent layer, its units and pins
(`agentlayer.Archive`) and `sandbox.json`, and run the agent's `install`.
The address the pool reaches the sandbox agent at is the instance's one URL,
and the sandbox agent listens where `guest.ServiceFile` says, as the pool
agent does (§1). Each sandbox machine is created as §4 requires (isolated, on
the pool's network). The pool's kind is the provider's (423 §2); its capacity
is the driver's limits, which its own engine now reports; and a create past
boxd's org quota fails in the pool agent's engine, and is reported as that
failure (106 §4, with "at the provider" now the pool's engine).

**One provisioner** does both: given a machine, a tree to place and a command
to run, it places the tree with disco-vm's copy and runs the command with its
exec. The provider runs it once per pool (§1); the pool agent runs it once per
sandbox. It lives in the pool-agent module, which the server module already
imports for what the two share (root `DESIGN.md`, Module Boundaries), so the
root module takes no dependency on disco-vm; a new kind of machine is a new
tree and command, not a new path. Only what is already a shared package — `agentlayer`, `assetcache`,
the bundle store — is shared with the Docker runtime, whose placement stays
its own.

**The machine seam (#122) carries nothing for a remote driver.** The pool
agent holds a key that does every lifecycle operation, so a seam that kept
lifecycle in the server would split one machine's operations between two
processes holding the same authority. For a local driver (vz, later hcs) the
seam stays as 106 §2 has it, widened as 423 §7 widens 26-10-09-143 §2: the
server places the agent layer and runs the one command with the same
provisioner, behind the seam.

### 4. A provisioning key reaches the pool machine, and is bounded

disco-vm gains a **provisioning key**: a credential its engine is given rather
than reads from its process's environment, and that a driver can issue for
another host. The provider asks its driver for one per pool:

- **An issued key**, where the driver can make one narrower than the
  provider's, which expires and **cannot itself issue keys**: a key that
  could would let its holder make one that outlives every renewal and
  revocation, so a driver that cannot promise it issues none. On boxd it is an
  API key fenced to the provider's org, with an expiry (`CreateApiKey`), of
  the kind that reaches no more than the provider's machines — which kind,
  and whether boxd refuses such a key `CreateApiKey`, the disco-vm issue
  settles before boxd is enabled.
- **The provider's own key**, only where the provider's configuration says
  so, explicitly: an operator opts in to delivering it, and is told what it
  costs when they do. It reaches everything the provider's key reaches, has
  no expiry discobox sets, is never renewed or revoked by discobox, and may
  be able to issue keys of its own; so whoever reads it on any pool machine
  keeps it after the pool is gone, and the operator rotates it, and removes
  any key made from it, when a pool machine is lost or suspected. A driver
  that cannot issue one, or a provider with no opt-in, blocks the pool with
  that reason. **An issue that fails is retried, never downgraded** to the
  provider's own key.
- **No key**, for a local driver, which has no credential and whose pool
  agent runs beside the server (§5).

**What a driver gives a machine by default is switched off at create.** A
provider's machine may come with credentials and peers of the provider's own:
a boxd machine that is not `isolated` carries the boxd CLI and an in-VM token
that acts on the org, a git credential helper and integrations, and reaches
the org's other machines, exec and copy included. So on boxd every sandbox
machine is created `isolated`, which strips those and is immutable and
inherited by every fork and restore, on a network shared only with its pool's
machine; and the pool's machine is on that network and no other, off the
org's default one, so no other machine of the org reaches it. A driver whose
machines cannot be made so is not enabled.

**The pool's machine is the exception, and it is said.** boxd peers an
isolated machine only with non-isolated ones on a shared network, so the
pool's machine is not isolated, and keeps what boxd gives a machine: its CLI,
integrations, and an in-VM token that acts on the org as the provider's user.
That is a second credential reaching the org on the pool's machine, beside
the provisioning key, and it is bounded only by where it is: on the pool's
machine alone, where no sandbox code runs and with which no other machine of
the org peers. discobox sets no expiry on it, never renews or revokes it, and
does not know whether it can issue keys, whether processes other than root
can read it, or whether destroying the machine revokes it; the disco-vm issue
asks boxd those before boxd is enabled. The pool records and shows that its
machine holds one. Until boxd answers otherwise, a compromise of a boxd pool's
machine is costed as one of the provider's own key (above): the issued key's
bounds below do not limit it, and the cost is the org's whatever key is
issued.

This reverses 106 §2's rule that a boxd key never reaches the pool's machine.
What bounds a delivered key:

- **Where it is.** One root-only file on the pool's machine. It is not in the
  pool's reports, logs, exported trees or anything the server reads back, it
  never enters a sandbox, and the proxy never forwards it. Every sandbox is a
  machine of its own (106 §1), isolated as above, and no sandbox code and no
  sandbox build runs on the pool's machine.
- **What it reaches.** The narrowest the driver issues, which cannot issue
  another. An issued key expires. The provider places a new one by half the
  old one's life and leaves the old to expire rather than revoking it, so no
  call in flight meets a revoked key; it revokes every key of the pool when
  the pool is deleted or its machine replaced. These bounds hold for an issued
  key only; the provider's own key has none of them (above).
- **What uses it.** The pool agent alone, through its engine, in its own
  project (§3). The project is a naming convention, not a fence, where the
  key reaches further.
- **That it is seen.** The pool records whether its key is issued or the
  provider's, and its scope, and shows it with the pool, as 0126's
  Consequence asks: a backend with an account-wide key has a larger blast
  radius, stated before it is enabled. The provider's documentation says to
  give discobox an org or account of its own.

**A key wider than the pool is acceptable where the driver offers nothing
narrower**, and what it costs is stated rather than hidden. Whoever controls
the pool's machine already holds everything of its sandboxes that matters:
the proxy's CA and the secrets it substitutes, the origins, the pool's
identity key. What the key adds is everything else it reaches. On boxd that
is the org: **every other pool of the same provider, and their sandboxes,
whichever discobox projects they serve** — the pool's project is a naming
convention, not a fence — and any other machine of the org. Pools that must
not reach each other are given providers, and orgs, of their own. 106 kept
the key in the server at the price of the server in every sandbox start's
path and a seam carrying every guest operation 423 now needs; that is the
price this ADR declines.

### 5. What stays

- **The pool's proxy is the only way out.** Every sandbox reaches the network
  only through its pool's proxy, provisioning included (423 §9.5, 0126 §6). A
  boxd sandbox's pool hop and egress (#129) remain a precondition for the
  first one. The pool agent, holding the key, is now where a sandbox
  machine's driver-side egress controls and network membership are set, at
  create, before anything runs in it.
- **macOS (vz) stays as 106 has it.** A macOS guest cannot host a Linux pool
  agent, so its pool agent remains a host process beside the server, the
  server runs the engine, and the seam serves it (§3). The bootstrap here is
  for remote drivers.
- **disco-vm's `docker` driver stays deferred** (106 Deferred), with its pool
  machine bootstrapped as §1 says rather than from a twin. Its URL is on the
  daemon's host, so how a pool machine on that daemon reaches its sandboxes'
  machines is what is still open.
- **What stays of 106** for a remote driver: §1's one provider with drivers,
  one machine per sandbox named by its ID, one platform per pool, and the
  remote driver's ordinary Linux pool agent as root on `layout.Container()`
  with its proxy as a unit; §1's "what the driver asks of that machine" but
  its image and its "no credential of boxd's"; §2's runtime authority and
  "Power state is the engine's", now the pool's engine; §3 but its seam on
  the pool's connection; §4, with a quota failure now the pool's engine's
  (§3); §5's 0126 and 0145 lines but its credential line, and its 0144 lines
  but "the boxd key never reaches it"; §6's §§1 and 3, and its §2 with the
  pool agent as the backend; and its Deferred items but the `docker` driver's
  pool machine. For a local driver, all of 106 stands. Of 423, everything
  stands but §7's "through the provider".

## Alternatives rejected

- **Keep 106 §2: the server runs every machine, the pool asks through a
  seam.** The key stays off the pool's machine, but the seam must carry every
  guest operation 423 needs — copy the agent layer, run the command, snapshot
  (423 §8), set egress — and every sandbox operation crosses the server and
  its link to the pool machine. §4 says why the key costs less than that.
- **A seam for lifecycle only**, with the pool agent doing copy and exec. The
  pool agent would need a key for copy and exec anyway, which on boxd is the
  same key that does lifecycle.
- **The pool agent as a role of the platform bundle.** The platform bundle is
  every sandbox's, fetched by every box through `assets.discobox.internal`
  and applied by the sandbox agent's runner; it would carry the pool agent
  into every sandbox, and the pool machine would need the sandbox agent to
  apply it.
- **The pool-agent image in a container on the pool machine.** It needs Docker
  on every base and a container around an agent whose sandboxes are machines
  elsewhere.
- **A disco-vm image of the pool machine, built from a spec**, as 106's boxd
  twin was. It is the second implementation of an image that 423 retired
  because it drifted, and a base plus a bundle needs no build.
- **The pool machine provisioned with 423's runner and a `provision.yaml`.**
  Two units and one package do not need it, and it would make the pool agent
  carry the sandbox agent's runner.
- **Always deliver the provider's own key.** Simplest, but it gives up the
  narrower key a driver can issue, and an expiry.
- **Refuse a driver that cannot issue a narrower key, with no opt-in.**
  Where boxd may not issue one, this would refuse the first driver outright;
  §4 lets an operator choose the provider's own key, knowingly, instead.
- **Fall back to the provider's own key when an issue fails.** A transient
  error would silently hand the pool a long-lived, wider key; a failed issue
  is retried instead.

## Deferred

- **A snapshot of a bootstrapped pool machine**, as 423 §8 caches a sandbox's
  plan. One pool machine is made per pool, once; revisit if pool creation
  time matters.
- **A key fenced to the pool's own machines**, which no driver offers yet;
  revisit when one does, through the same provisioning-key abstraction.

## Consequences

- disco-vm gains the provisioning key: an engine given a key source it reads
  on each call to the driver, and a driver able to issue, find by name and
  revoke a narrower key that cannot issue keys, or say it cannot; and the
  switches that isolate a machine at create.
- `pool-agent` gains the pool-agent bundle target and its `install` command,
  and, for a remote driver, its own disco-vm engine and a machine runtime that
  provisions sandbox machines through it. The pool-agent module imports
  disco-vm.
- The pool-agent module gains the provisioner the VM provider and the pool
  agent share.
- The VM provider boots the pool machine from a base, places the bundle, the
  bootstrap and the key, renews and revokes the key, and re-adopts the
  machine, keeping its record of each pool in the server's database (a
  migration adds it). It builds no guest image for a remote driver.
- A provider's pools share what its key reaches; pools that must be kept
  apart need providers and orgs of their own, and a pool shows which key it
  holds.
- The machine seam (#122) is for a local driver only.
- The release manifest and the watcher gain the pool-agent bundle.
- The pool agent's API on a public URL is audited as 106 §3 says, and the
  pool machine now holds a provider credential, so it is part of that audit.
