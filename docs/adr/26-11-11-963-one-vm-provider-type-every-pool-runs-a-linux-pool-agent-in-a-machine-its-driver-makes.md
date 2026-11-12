# 26-11-11-963 — One vm provider type: every pool runs a Linux pool agent in a machine its driver makes

- **Status**: Accepted. It supersedes, and each superseded ADR's status line
  says so:
  - [26-10-09-106](26-10-09-106-disco-vm-is-a-pool-runtime-and-the-server-runs-its-machines.md):
    - §1's "Where the pool agent runs, and which agent it is, is the
      driver's" and its two bullets;
    - §2's engine in the server alone, its machine seam, "the one process
      that can hold a boxd key", "A local machine is held by its shim, and
      dies with it" and "A local machine lives no longer than its server";
    - §3's `vz` bullet whole, its transport that "never leave[s] the
      server" included;
    - §4's count of vz's running guests by the provider across its vz pools;
    - §5's 0144 lines for a local driver, and its macOS build spec ("A
      non-Linux sandbox image is a disco-vm image, built by the provider
      with disco-vm's builder from a discobox build spec");
    - its rejection of "The pool agent embeds the engine", for every driver;
    - its Deferred "Local machines that outlive their server";
    - its Consequences' machine seam, Docker-free composition,
      sandbox-agent for darwin "as assets with digests", and disco-vm's debt
      of "a shim that dies with its embedder".

    That covers every driver, so 551's supersession of these "for a remote
    driver" now has no exception.
  - [26-10-31-206](26-10-31-206-a-local-vm-pool-runs-its-agent-in-a-linux-vm-the-server-runs-and-its-sandboxes-reach-it-only-over-vsock.md):
    - §1, except its fence step, its Linux base's contract and its posture
      bullet, which §§6 and 14 below keep;
    - §2 whole;
    - §3, except a sandbox with no network device and each agent listening
      where `Service.Listen` says;
    - §5's server relay on hcs;
    - §6's issue list;
    - its rejections of "the seam as a disco-vm driver the pool agent
      opens", "the sandbox speaks the 719 tunnel itself" and "disco-vm
      relays vsock between its guests";
    - its Deferred "Linux machine sandboxes on a local pool", which §1 takes
      up because no separate local pool shape is left to defer it from;
    - its Consequences' seam, relay and "a shim that dies with its
      embedder".

    Its §4, its Deferred tun device or system proxy, and its Deferred hcs
    and Docker-pool entries stand. Its narrowings of 481, 767 and 345 lapse
    with it; this ADR's own, below, replace them.
  - [0144](0144-a-pool-of-host-vm-sandboxes-runs-its-agent-on-the-host.md),
    whole, except:
    - §2's authority argument, which 0017 §§9–10 carries;
    - §4's sandbox endpoints as `wire` URLs verified as `ServerName`;
    - its Deferred "Sharing anything between a host pool's sandboxes",
      which stands for every machine pool.

    Its Deferred Windows helper goes to §13. Its export deferral is 106's,
    which stands.
  - [26-10-23-551](26-10-23-551-a-vm-pool-bootstraps-its-own-agent-from-a-bundle-and-provisions-its-machines.md):
    - every "for a remote driver" and "nothing of a local one", in its
      Status, §§3–5 and Consequences;
    - §3's "The machine seam (#122) … for a local driver";
    - §4's "No key" reason;
    - §5's "macOS (vz) stays as 106 has it".
  - [26-10-27-345](26-10-27-345-a-machine-sandbox-is-made-again-in-place-and-its-bootstrap-names-where-its-agent-listens.md)
    §2's "A local driver's vsock stays behind the machine seam (551 §3)".
- **Narrows**:
  - [26-10-29-481](26-10-29-481-a-machine-sandbox-and-its-pool-reach-each-other-at-their-private-urls-over-tls-the-pools-ca-issues.md):
    - §1's three hops at two URLs "on every runtime", and "A driver whose
      `PrivateURL` is empty cannot carry a machine pool";
    - §2's endpoints derived from the private URL, with a third row;
    - §4's fence through `Egress`, and its scheduling rule requiring
      `Isolation`, `Egress` and a private URL;
  - [26-10-30-875](26-10-30-875-a-remote-pool-reaches-its-control-plane-over-a-channel-the-server-opens.md)
    §§1 and 4, from "a remote driver" to every `vm` pool, and §6's
    server-to-agent hop "still the provider's URL";
  - [26-10-24-953](26-10-24-953-every-box-is-provisioned-from-an-ordered-list-of-bundles-and-a-pool-is-a-box.md)
    §2 and [26-10-27-767](26-10-27-767-a-machine-pool-runs-what-every-pool-runs.md),
    for §6's fence;
  - 345 §2's "Only TCP is taken", for the pool agent too;
  - [26-10-16-423](26-10-16-423-a-harness-is-a-provisioning-bundle-and-the-agent-provisions-any-box.md)'s
    Deferred "macOS and Windows", for macOS (§9).
- **Date**: 2026-11-11
- Accepted by the owner by merging #1003, 2026-11-11
- **Issue**: #42
- **Relates to**:
  - [0017](0017-resource-state-is-desired-and-observed-with-no-operations.md)
    §§9–10;
  - [0126](0126-a-sandbox-does-not-share-a-host-or-a-filesystem-with-its-pool.md)
    §§5–7;
  - [26-10-23-719](26-10-23-719-a-sandbox-may-reach-its-pool-through-a-websocket-tunnel-with-its-mtls-inside.md);
  - [26-11-01-122](26-11-01-122-a-harness-is-only-a-bundle-and-a-sandbox-takes-its-kind-and-platform-from-its-pool.md);
  - [26-11-03-986](26-11-03-986-a-machine-pool-snapshots-a-plan-by-committing-a-vm-it-provisioned-and-shut-down.md);
  - disco-vm ADRs 26-10-31-990, 26-11-03-247, 26-11-03-530, 26-11-03-606,
    26-11-03-868 and 26-11-04-412.

## Context

The `vm` provider has two pool shapes, chosen by `isLocal`
(`server/providers/discovm/runtime.go`), which is a type check for a
`*local.Driver`:

- **A remote driver (boxd).** `poolMachine` (`poolmachine.go`) makes the
  pool's machine from the driver's base, in the provider's disco-vm scope
  (`remotePool`), on a network named for the pool. It makes the machine the
  pool's box with `poolruntime.ProvisionBox` from the pool bundle, so it runs
  the ordinary Linux pool agent.
  - That agent opens disco-vm's engine itself, in the pool's own scope
    (`machinePool`, `bootstrap.MachinePool`, `pool-agent/machines.go`), with
    a provisioning key the provider issues or delivers (551 §4).
  - It makes, provisions and snapshots its sandboxes there (986).
  - The server reaches the agent at the machine's URL, or over the
    control-plane channel it opens there (875).
- **A local driver (vz, libkrun, later hcs).**
  - Every VM sits in one scope, `localPool = "discobox"`.
  - The pool host is `hostAgent` (`hostagent.go`), whose `ensurePoolHost`
    answers `errHostAgentNotStaged`. 206's pool VM, seam and relay were never
    built.

The owner decided on 2026-11-10:

> For the discovm driver we need to drop the idea of local vs remote. There
> is no such thing; they are just one driver type. The local was supposed to
> run the pool agent on the host, whereas now we always want to run a pool
> agent, and the pool agent is Linux platform.

disco-vm (discobox pins `5682ac6`) has moved under 206:

- **Linux guests on vz** (its ADR 26-10-31-990). They boot from a base, with
  the guest agent appended from `$DISCO_VM_AGENT`. libkrun is a local driver
  for Linux guests too (26-11-03-530), and runs from a runtime of its own
  (`$DISCO_VM_KRUN_RUNTIME`).
- **Guests with no network device** (`Egress{}`).
- **`Dial` of a VM's agent port (7300) and service port (7302)**
  (26-11-03-247), through the shim's owner-only `shim.sock`.
  `Driver.Service` still returns the shim's loopback URL, which any local
  process can reach.
- **Peer forwards** from a guest port to another VM's agent or service port,
  carried by the shim and resolved by name in the shim's own scope
  (`pkg/machine/local/shim.go`, `dialForward`).
- **Shims outlive their caller by design** (990 §4).
- **There are only VMs** (26-11-03-606, 868). vz has no base for macOS, and
  a macOS guest's agent is baked in at install.
- **Scope is per pool**, and `MaxRunning` is counted across every pool in a
  driver's root (26-11-04-412 §1). The implementation is vm#63, still open.

## Decision

### 1. One pool shape

**Every `vm` pool, on every driver, runs the ordinary Linux pool agent in a
pool machine the provider makes from the driver's base and bootstraps from
the pool bundle**, as 551 §1 does for boxd today.

- There is no host pool agent, no Docker-free composition and no host roots,
  and the provider has one pool host: `isLocal`, `hostAgent` and `localPool`
  go.
- **A pool still hosts one platform**, which its sandboxes take (122).
  - **The pool names the platform of its sandboxes** when it is created:
    `linux` on every driver, or `darwin` on vz. Linux is the default, and the
    platform does not change after creation.
    - It is the pool's, not the provider instance's, because one provider
      instance backs several pools (ADR 0003), so a vz provider can back a
      linux pool and a darwin one.
    - It is the pool's intent, stored in a column of its own, which a
      migration adds. Empty means linux, which is every existing pool.
  - **Only the OS is named.** The architecture is the pool machine's, which
    on every driver is its sandboxes' too: a held driver's guests run on the
    host's CPU, and boxd's on the machine type's.
  - **The provider writes the OS into the pool's bootstrap**, and the pool
    agent declares that OS with its own architecture. It no longer declares
    `platform.Current()`, which is always Linux.
  - **The agent's report becomes a check, not the source**, for a pool that
    names an OS.
    - Today `recordPoolPlatform` (`server/internal/store/pools.go`)
      overwrites `model.Pool.Platform` on every registration and heartbeat.
    - A report whose OS is not the pool's named OS is not recorded, and the
      pool is not schedulable until its agent declares the named platform.
      So an agent that still reports its own platform, during an upgrade or
      under a provider-override bundle, never turns a darwin pool into a
      linux one, and no sandbox is placed by the wrong platform.

**"Local" names only where a driver's machines run**: on the server's host
or in a provider's account. It changes how things are reached (§§3–5), never
the pool's shape. The provider tells drivers apart by what disco-vm reports,
never by a driver's name:
- **A held driver** is one this server's engine holds under shims. It lists
  `Capabilities.Ports` and issues no key (`Keys: none`): vz, libkrun, hcs.
- **Any other driver** is reached at its URLs, and called with a key: boxd.

### 2. The scope layout is boxd's, on every driver

- **The pool machine** is in the provider's scope, named for the pool, as
  `poolMachine.create` makes it today.
- **The pool's sandboxes and its snapshots** are in the pool's own scope
  (`machinePool`), which the pool agent's engine is opened on, as
  `pool-agent/machines.go` opens it today.

A boxd pool's layout does not change, so no persisted machine moves. What
fences a pool differs by driver:

- On a held driver, the served engine (§3) is opened on the pool's scope.
  Nothing it can name is the pool machine or another pool's VM, so the fence
  is the scope.
- On boxd, the fence is 551 §4's key, whose bounds that ADR states: "The
  project is a naming convention, not a fence, where the key reaches
  further."

### 3. Where the pool agent's engine reaches its driver

- **Any driver but a held one: its own service**, with the key the provider
  issues or delivers (551 §4). This is unchanged.
- **A held driver: the server's engine, served to that pool alone.** The
  served engine is opened on the pool's scope (§2) and uses disco-vm's own
  engine API, so the pool agent's machine runtime, provisioner and snapshots
  are the same code on every driver. The bootstrap names the served engine
  as the driver. No disco-vm key is issued.
  - **Only on the pool's own channel.** It answers only on a connection the
    carrier hub tags with that pool: 875 §3's tagged stream, claimed by
    `CarrierPoolAuthenticator`. It refuses every untagged connection,
    whatever pool credential the connection carries. A pool assertion minted
    with a key taken off the pool machine therefore reaches nothing from
    anywhere else. The access exists only over the channel the server opens
    to that machine, and ends when the server closes it.
  - **An allow-list**, refusing every operation and field it does not list.
    It is written against the machine runtime's calls:
    - **Allowed:** Create, Start, Stop, Remove, Get, List, State,
      `Capabilities`, a VM's `Service` (where its agent listens), the guest
      operations (exec, copy), `Dial` (which disco-vm limits to the agent
      and service ports), Commit, and `Upgrade` (which vz and libkrun answer
      as unsupported until they attach volumes).
    - **Refused, among the rest:** `Install`, `Boot` (which runs a VM inside
      the serving process), a GUI window, `Shares` (host directories),
      volumes and caches a held driver does not attach, and every field not
      listed.
    - **Forced on every VM the pool creates:** no network device, whatever
      the VM is for (a sandbox and a snapshot build look alike), and a size
      within the provider's bounds.
    - **Names:** every name a call carries is in the pool's scope. That is
      the target VM, Create's name, and `From`.
    - **Forwards:** the pool sets none. The only forward a pool's VM has is
      the one the server sets (§5), and the only `From` outside the pool's
      scope is the one the server sets (§9).
  - **Version.** The served protocol carries disco-vm's engine version. When
    a pool agent's version is not the server's, the call is refused with a
    version error, and the pool is upgraded, as a pool agent older than the
    server is waited out today.
    - A pool whose pool-agent bundle is pinned by a provider override (551
      §2) to an engine version this server does not serve is never upgraded
      to match.
    - It is reported **blocked**, naming both versions and the override, and
      not as an upgrade in progress.

**The cost, stated.** The pool machine holds guest operations, exec and copy,
on its own sandboxes. 206 rejected that and kept them in the server. A boxd
pool machine already holds exactly that (551 §4), and one shape is one
posture. Over boxd's, a held driver's served engine adds nothing beyond the
pool's scope, and nothing beyond its channel.

### 4. How the server reaches a pool machine

- **On a held driver**, by `Engine.Dial` of the machine's service port,
  through the shim's owner-only socket. The shim's loopback URL
  (`Driver.Service`) is never used.
- **On any other driver**, at the machine's URL.
- **Over that**, the server opens 875's control-plane channel to every `vm`
  pool with no `controlPlaneUrl`, and a held driver's pool has none.
- **What does not change.** "Local providers … are unchanged" (875 §4) keeps
  holding for the Docker-pool providers.
- **Where the pool agent listens** is where the driver's `Service.Listen`
  says, through `pkg/wire`: a vsock port on a held driver.

### 5. How a sandbox and its pool reach each other

How a sandbox machine is fenced, and which row of 481 §2's table it takes,
are the driver's:

- **A driver that isolates on networks (boxd).** The sandbox is isolated on
  the pool's network (551 §4, 481 §4), as today. It reaches the pool at the
  pool's private URL, or through an edge's tunnel.
- **A held driver.** The sandbox machine has **no network device**, on every
  boot and from every VM it is made from.
  - It reaches its pool through **one peer forward**, which the server sets
    at create: from a fixed guest vsock port to the pool machine's service
    port, across the two scopes (§2). The shim carries it.
  - Through that forward it speaks the WebSocket tunnel of 719, over a
    vsock dialer in `pkg/wire`. That is 481 §2's new row: **the forward
    tunnel**.
  - Unlike an edge, nothing on the path terminates TLS. So the sandbox
    agent's TLS stays the pool's setting (481 §3), as on the direct row, and
    not the edge row's plain HTTP.
  - A sandbox's way out never runs through the server's process.

The pool reaches a sandbox's agent by `Dial` of the sandbox's service port,
through its engine: on a held driver, through the served engine.

**A driver carries a pool only if it does one of the two**: isolate a
sandbox on a network it shares only with its pool machine, or make it with
no network device and the server's peer forward. This replaces 481 §1's
`PrivateURL` rule, 481 §4's scheduling rule, `poolMachine.converge`'s
`Isolation` check and `requireMachineFence` (`pool-agent/poolhop.go`).

Inside a sandbox with no network device, 206 §4 stands: proxy settings, the
DNS stub as the resolver, macOS's stated costs, and its Deferred tun device
or system proxy.

### 6. A pool machine's network is fenced first

On a held driver, the pool machine keeps a network device, for its proxy's
way out (206 §1). Whatever else shares that NAT or passt can reach it, so
206 §1's fence step stands: it is the first step of 953 §2's contract,
carried by `poolruntime.ProvisionBox`.

- It loads a rule set that admits nothing inbound, because every path in is
  vsock, and reads it back before anything of the pool starts.
- The pool machine boots the driver's Linux base, pinned by digest, with
  206 §1's contract, `nftables` included.

On boxd, the pool's network bounds the machine, as 767 has it, and this ADR
does not change a boxd pool's fence. Whether the same step also runs there is
deferred (Deferred).

### 7. A machine lives as long as its driver keeps it

**On every driver, a `vm` pool's machines live as long as their driver
keeps them**: a boxd machine as boxd keeps it, a held one as its shim does
(990 §4).

- **On start**, the server finds its machines again by name in its scopes,
  as `discovm/DESIGN.md` says it finds a boxd machine. It reopens the
  channel to each pool machine (§4), and the pool agent reconciles its
  sandboxes (0017 §§9–10). There is no adoption protocol: no recorded
  process, no lock, no "still mine" check.
- **An older shim** met by a newer engine after an upgrade is disco-vm's to
  handle. It either works with it, or stops it and boots the VM again on its
  disk, and never leaves it half-attached.
- **Stopping them without a server.** A held driver's VMs outlive the
  server, so the user is given a way to stop every one of them that needs no
  running server.
  - It is a **`discobox-server` subcommand**, which embeds the engine as the
    shim subcommand does. It stops each VM in the driver's state root
    through its shim's owner-only socket.
  - The CLI runs it through the server binary it already stages (ADR 0099),
    so the CLI takes no dependency on disco-vm. Uninstalling discobox runs
    it too.
  - The server, on start, shows the held machines it finds running.
  - The subcommand's name is open, for implementation.
- **The cost, stated.** After a server crash, or a quit without that stop,
  a held driver's sandboxes keep running. Their egress keeps working through
  the pool's proxy, under the last policy the pool had, with nothing to show
  it but the next server start or that command. That is the price of a
  sandbox that survives a server restart, and it is boxd's already.
- **What it supersedes.** This replaces 106 §2's local lifetime and its
  Deferred entry. `server/providers`' VM Lifetime rule ("the VM dies with
  the server") stays the rule of the Docker-pool providers (vz, wslc,
  libkrun), as does the REVIEW.md note against adopting their VMs.

### 8. Snapshots are 986's, on every driver

The pool agent builds, commits and reuses plan snapshots on its own engine,
in its own scope, as 986 says, on every driver that reports `Commit` (boxd,
vz, libkrun). No server route builds one. On a held driver a snapshot build
has no network device (§3) and reaches its proxy as a sandbox does (§5).

### 9. A darwin pool

- **The darwin agent** reaches a sandbox in its pinned bundles at every
  boot, as on Linux: a darwin/arm64 agent bundle and platform bundle, with a
  launchd unit. No VM bakes it in. This replaces 106 §5's build spec.
- **The darwin base VM is the host's.** vz has no base for macOS, and an
  install needs the host's network and a network device, which no pool is
  given (§3). So the server installs macOS once per host and macOS version,
  in a scope of its own, adds the single account (uids from 501), and
  commits it.
- **A darwin pool's sandboxes and snapshots** are created `From` the darwin
  base VM, a reference only the server sets (§3).
- **A macOS guest's disco-vm agent** is baked in at install, and vz checks
  the agent protocol of Linux guests only. disco-vm owes two things before a
  darwin pool is enabled:
  - the same protocol check for a macOS guest;
  - a way to move a committed or running macOS guest's agent to the running
    disco-vm's protocol without installing again.

  Without them, a protocol change strands the base, its snapshots and every
  darwin sandbox.

### 10. A Linux guest's disco-vm agent is pulled by digest

The server's engine boots a held driver's Linux guests, pool machines and
sandboxes, with `$DISCO_VM_AGENT`: a static linux `disco-vm` of its module's
protocol. That agent is a guest artifact, which `server/providers/REVIEW.md`
puts on a release line of its own, pulled by digest:

- disco-vm publishes it per module version as an OCI artifact;
- discobox pins its digest beside the module version, and pulls it as it
  pulls `vm-image`, into an owner-only cache;
- the server ships no disco-vm binary beside itself, as
  `server/providers/discovm/shim.go` says today.

The pool agent embeds disco-vm as a library and needs no binary.

### 11. libkrun is a `vm` driver like any other

Where it is linked (linux/amd64), libkrun is enabled for a `vm` pool once
disco-vm delivers two things:

- its runtime pinned by digest (`$DISCO_VM_KRUN_RUNTIME`, 530);
- passt that maps no host loopback and forwards no ports, as discobox's own
  libkrun provider sets it by hand (`krunvm`: `--map-host-loopback none
  --no-map-gw --tcp-ports none --udp-ports none`).

A driver that gives a pool machine a path to the host's loopback carries no
pool. libkrun does not replace `server/providers/libkrun`, which hosts a
Docker pool. vz has the same pairing (`vz` and `vm` on vz).

### 12. Capacity is the driver's limits

A pool reports its driver's limits as its engine reports them: on a held
driver, the host's, less the pool machine. vz's two running macOS guests are
a host limit, and §2 puts a host's VMs in several scopes. disco-vm counts
them across every scope of a driver's root (412 §1, vm#63), and a create past
the cap is a capacity wait. Linux guests do not count.

### 13. Windows

Windows (hcs) is the same shape on hcs, deferred with what 206 §5 lists and
0144's Windows helper.

### 14. The posture

206 §1's posture holds for every pool machine:

- The MITM CA, sentinels, origins and audit database live in the pool
  machine, not as files on the user's machine.
- The CA is trusted inside sandboxes only, never by the host.
- Each sandbox machine is its sandbox's isolation boundary.
- A build's steps run in the pool machine (767), which on a held driver
  holds no disco-vm key.

## Alternatives rejected

- **206's coarse seam**, discobox routes that create, provision and connect
  while guest operations stay in the server.
  - It makes a second machine source in the pool agent, a second
    provisioning path in the server, and a second trust posture.
  - The server would be in every sandbox's create.
  - The served engine (§3) confines a pool by scope, channel and an
    allow-list, with one code path.
- **The pool agent on the host** (0144, 106). It is a second composition of
  the pool agent, host roots, a host-native proxy and a `git` on the host,
  for one platform. The owner's direction ends it.
- **The pool machine in the pool's own scope, with its name refused by the
  served engine.** That is a fence by deny-list, and it is not boxd's
  layout. Scope fences it (§2).
- **The served engine on any listener that takes a pool assertion.** A
  pool's signing key taken off its machine would reach the user's
  hypervisor from anywhere, and keep it until the pool is revoked. The
  tagged channel (§3) holds it to the machine the server opened it to.
- **The server relaying a sandbox's bytes to its pool** (206 §3).
  - It puts the server's process in every byte, and a sandbox's way out dies
    with the server.
  - disco-vm's peer forward does the same in the shim, set by the server at
    create.
  - Its price is the WebSocket tunnel over a vsock dialer in `pkg/wire`.
- **Peer forwards a pool may set.** Within the pool's scope a forward may
  target another VM's agent port, which is exec in that sandbox. Only the
  server's one forward exists (§3).
- **A disco-vm key for a held driver.** A held driver has no key system, and
  the channel already authenticates the pool. A key would be a second
  credential to bound and revoke.
- **Held machines that die with the server** (106 §2, a lifeline as vm#8
  asks).
  - It gives two lifetimes by driver kind, which is the split the direction
    removes, and it kills every sandbox on each server restart.
  - It is the stronger stop for a user who quits the server: a lifeline
    stops every VM, where §7 needs its CLI stop to be run.
  - §7's stop, and uninstall running it, are what close that gap.
- **libkrun as a test-only driver**, linked under a build tag. It would be a
  held driver with a different status from vz's. §11's conditions already
  say when it is safe.

## Deferred

- **Windows (hcs)**, as §13 lists, until a darwin pool runs on vz.
- **A tun device, or a system proxy, in a macOS sandbox** (206 Deferred,
  standing).
- **The Docker pools on disco-vm** (106 Deferred, standing). Revisited as its
  condition says, and taken up by #608 under disco-vm's ADR 26-11-04-412.
- **Sharing anything between a pool's sandboxes** (0144 Deferred, standing).
- **§6's fence step on a boxd pool machine.** Until then, a boxd pool's
  fence is its network (551 §4, 767). Revisited once it is known whether
  boxd's default image (551 §1) carries `nft` and its `nftables.service`,
  checked before the next change to the boxd pool machine's base.

## Consequences

- **`server/providers/discovm`.**
  - `poolMachine` serves every driver, and `isLocal`, `hostAgent` and
    `localPool` go.
  - `placement` stops refusing a driver with no private URL, and writes the
    pool's sandbox platform into its bootstrap. `agentListenURL` takes
    vsock. `acquireAgentClient` and the channel dial by `Engine.Dial` on a
    held driver. `converge`'s `Isolation` check becomes §5's rule.
  - The server serves its engine on a held driver's pool channel, under
    §3's rules, and makes the darwin base VM.
  - `discobox-server` gains §7's stop subcommand, which the CLI and
    uninstall run.
  - Creating a `vm` pool takes its sandbox OS (§1), stored in a new column
    by a migration, and `recordPoolPlatform` checks a pool's report against
    it rather than overwriting it. The store's `REVIEW.md` gains that rule
    with the code.
- **`server/providers/DESIGN.md` and `REVIEW.md`**, when the code lands:
  - VM Lifetime and the adoption note are scoped to the Docker-pool
    providers;
  - REVIEW.md gains the served engine's tagged-channel and allow-list rules,
    the rule that a pool sets no forward, and the rule that a driver change
    never undoes a network restriction discobox set by hand.
- **pool-agent.**
  - The machine runtime takes its driver from the bootstrap, the served
    engine included, and declares the bootstrap's sandbox platform.
  - `requireMachineFence` and the `Isolate` and `Networks` set on every
    create give way to §5's rule.
  - The sandbox agent's listen address takes vsock.
  - The pool reaches a sandbox by `Dial`, not `engine.PrivateURL`, on a held
    driver.
  - The hop gains the forward-tunnel row, with the agent's TLS kept.
- **sandbox-agent.**
  - The tunnel over a vsock dialer.
  - On a box with no network device, the DNS stub as its resolver (206 §4).
  - `pkg/vsock` in a darwin guest.
- **disco-vm owes discobox:**
  - the Linux base released and pinned by digest, with `nftables`;
  - its linux agent published by digest per module version (§10);
  - an engine served over a stream, with an embedder's allow-list and a
    version, and a driver that is its client (§3);
  - forwards and `From` across scopes, set only by the embedder (§§5, 9);
  - 412 §1's count across scopes, implemented (vm#63);
  - an engine that meets an older running shim (§7);
  - libkrun's runtime pin and its passt (§11);
  - the macOS guest agent's protocol check and its move (§9);
  - the base version an install resolved, for the darwin pin.

  vm#8, a shim that dies with its embedder, is no longer wanted.
- **Issues.** #64, #122, #423, #424, #425, #67 and #128 were written for
  206's local shape, and are rewritten from this ADR once it is accepted.
