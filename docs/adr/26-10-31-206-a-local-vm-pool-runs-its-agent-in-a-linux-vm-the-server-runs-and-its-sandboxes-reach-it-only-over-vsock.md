# 26-10-31-206 — A local VM pool runs its agent in a Linux VM the server runs, and its sandboxes reach it only over vsock

- **Status**: Accepted (by the owner, 2026-10-31). For a local driver (vz,
  later hcs) only, it supersedes:
  - [0144](0144-a-pool-of-host-vm-sandboxes-runs-its-agent-on-the-host.md):
    §1 whole; §3 whole; §4's "the agent dials each sandbox's guest socket",
    "Whether an interface exists is the driver's decision", and "the
    socket's identity is a second check"; §5's pool services as host
    processes; §6's pool state as files on the user's machine and the agent
    as a host process; its rejection of "Boot a Linux VM whose only job is
    to host the agent"; and its Consequences of no relay, a per-platform
    signed pool-agent asset, and a `git` on the host.
  - [26-10-09-106](26-10-09-106-disco-vm-is-a-pool-runtime-and-the-server-runs-its-machines.md):
    §1's local-hypervisor bullet (the Docker-free agent as a native host
    process); §2's per-instance "return the address of the agent's port";
    §3's `vz` bullet's server-owned route given out as the address, and its
    forwards "to Unix sockets the host pool agent serves"; §5's "§1 stands
    for a local driver" (host roots, git on a host, staging and supervising
    the agent) and "§§3–6 stand, for a local pool" for §§3, 5 and 6; its
    rejection of "disco-vm as a sandbox runtime inside an existing Docker
    pool" for its pool VM on every Mac and its relay for every byte (not for
    a pool of several platforms, which stays refused); and its Consequence
    of a Docker-free composition on darwin.
  - [26-10-23-551](26-10-23-551-a-vm-pool-bootstraps-its-own-agent-from-a-bundle-and-provisions-its-machines.md):
    §4's reason for "No key" ("whose pool agent runs beside the server");
    §5's "macOS (vz) stays as 106 has it", its pool agent a host process,
    and "For a local driver, all of 106 stands".

  It narrows
  [26-10-24-953](26-10-24-953-every-box-is-provisioned-from-an-ordered-list-of-bundles-and-a-pool-is-a-box.md)
  §2's steps of making a box, with a first step that fences a local pool's
  network;
  [26-10-27-345](26-10-27-345-a-machine-sandbox-is-made-again-in-place-and-its-bootstrap-names-where-its-agent-listens.md)
  §2's "Only TCP is taken";
  [26-10-27-767](26-10-27-767-a-machine-pool-runs-what-every-pool-runs.md)'s
  pool's services reached over "the pool's network, which no machine outside
  the pool is on"; and
  [26-10-29-481](26-10-29-481-a-machine-sandbox-and-its-pool-reach-each-other-at-their-private-urls-over-tls-the-pools-ca-issues.md)
  §1's three hops at two URLs "on every runtime" and "A driver whose
  `PrivateURL` is empty cannot carry a machine pool", §2's two endpoint
  forms and the bootstrap's answer for the pool's name, and §4's fence
  through `CreateOptions.Egress`.
- **Date**: 2026-10-31
- **Issue**: #418, part of #42. Its sibling is discobox-ai/vm#27 (Linux
  guests in disco-vm), decided beside this one.
- **Relates to**:
  [0017](0017-resource-state-is-desired-and-observed-with-no-operations.md)
  §§9–10 (the observer reports),
  [0062](0062-macos-pools-run-vz-vms-with-an-independently-released-guest-image.md)
  (the Docker pool's Linux VM on vz),
  [0126](0126-a-sandbox-does-not-share-a-host-or-a-filesystem-with-its-pool.md)
  §6 (no egress but the proxy),
  [26-10-23-719](26-10-23-719-a-sandbox-may-reach-its-pool-through-a-websocket-tunnel-with-its-mtls-inside.md)
  (the tunnel, mTLS inside),
  [26-10-24-953](26-10-24-953-every-box-is-provisioned-from-an-ordered-list-of-bundles-and-a-pool-is-a-box.md)
  (a pool is a box),
  [26-10-30-875](26-10-30-875-a-remote-pool-reaches-its-control-plane-over-a-channel-the-server-opens.md)
  §3 (a carrier stream reaches only its pool's routes).

## Context

ADRs 0144 and 26-10-09-106 put a local disco-vm pool's agent on the host: a
Docker-free composition of the pool agent (#58), staged and supervised beside
the server (#64), with host roots, a `git` on the host (#60), a host-native
proxy, and the server serving it a machine seam (#122) for its sandbox VMs.
ADR 26-10-23-551 kept that for vz while a remote driver's pool became a
machine bootstrapped from the pool bundle, running the ordinary Linux agent
and its own engine. The provider already splits the two
(`discovm.isLocal`): `poolMachine` for boxd, `hostAgent` for vz, whose
`EnsurePool` answers that it is not staged. None of the local path has
shipped: no Docker-free agent, no staging, no seam.

Since then the remote path has become the one every pool shares. A pool is a
box provisioned from the pool bundle (953), a machine pool runs every feature
of it (767), a sandbox reaches its pool's services over mTLS at endpoints its
bootstrap names, directly or through the 719 tunnel at the pool's one service
(481), and every carrier stream into the server's hub reaches only its own
pool's routes (875 §3). The host-agent plan shares none of that: it is a
second agent composition, a second proxy deployment and a second trust
posture (0144 §6), for one platform.

The owner decided on 2026-10-31 to redesign macOS and Windows support:

- A local-driver pool runs the **ordinary Linux pool agent and proxy in a
  dedicated Linux VM**, made the pool's box from the same pool bundle a boxd
  pool machine is.
- For a local driver **the server creates and runs every VM**, the pool VM
  and the sandbox VMs alike. The pool agent asks nothing of the hypervisor.
  Local and remote flows stay different.
- **The egress fence is vsock.** A sandbox VM has no network card; its only
  way out is vsock, relayed to the pool VM's proxy, so 0126 §6 holds by
  construction.
- **macOS first**; Windows later, ideally the same design.

What exists to build on:

- **disco-vm's local driver** (vz, hcs) runs each instance under a shim the
  embedder starts as its own subcommand. The shim serves the instance's one
  service as a loopback URL that needs no credential (`Service.URL`), and
  splices guest-to-host connections on a vsock port into a host Unix socket
  (`CreateOptions.Forwards`). A local driver has no private URL, no
  isolation and no egress fence (disco-vm ADR 26-10-29-129). vz launches only
  macOS guests, always with the framework's NAT (discobox-ai/vm#5), and
  `MaxRunning` caps it at two.
- **Linux VMs on vz and vsock control planes are discobox's already.** ADR
  0062's vz Docker pool boots `vm-image`'s kernel and root, and its agent
  reaches the server over vsock port 3001, which the driver pushes into
  `carrierhub` tagged with the pool. libkrun's guest reaches the server over
  vsock without the hub, and wslc relays a Unix socket into it. `pkg/wire`
  dials and listens on `vsock://`, and a sandbox's endpoints are wire URLs
  verified as `ServerName` when the URL has no host to take it from
  (`sandboxconfig.PoolEndpoints`, ADR 0144 §4).

## Decision

### 1. A local pool's agent and proxy run in a Linux pool VM

For a local driver the `vm` provider makes the pool a **pool VM**: a disco-vm
instance of a Linux base on that driver, in the server's engine, made the
pool's box with `poolruntime.ProvisionBox` over the driver's guest operations,
exactly as a boxd pool's machine is (953 §2, 767). It runs the ordinary Linux
pool agent as root on `layout.Container()`'s paths, its proxy as a systemd
unit, and every feature of the pool bundle. There is no Docker-free agent, no
host process, no host roots and no `git` on the host.

- **Its guest is a disco-vm Linux base with the pool bundle applied** (551's
  model), not `vm-image`. The base is disco-vm's for the driver
  (discobox-ai/vm#27 decides how it is made and cached). The server names it
  by digest, a release pin with a provider override, as it names `vm-image`
  today, never a floating tag. Of the base discobox asks: 423 §9's platform
  contract (a Debian-family systemd Linux with root), a disk kept across stop
  and start, a kernel with what BuildKit needs (767) and with nf_tables,
  the `nft` tool and a unit that loads a saved rule set before the network
  comes up (Debian's `nftables.service` and `/etc/nftables.conf`), vsock,
  one network device, and a clock it keeps to the host's across a host sleep with no
  network (ADR 0062's reason: every credential is time-bounded).
- **It keeps a network device, for going out only.** Its proxy is its
  sandboxes' way out, so it keeps the driver's NAT, as a boxd pool machine
  keeps its internet (481 §4). That NAT is shared with the host and every
  other guest on it, so it is not the pool's network 767 bounds the pool's
  services by: the registry (plaintext, unauthenticated), the mediator and
  the tunnel listener listen on every interface. So making a local pool's
  box gains one step, first, in 953 §2's contract and carried by
  `poolruntime.ProvisionBox`: **fence the box's network**. It loads a rule
  set that admits nothing inbound on the NAT device but replies to the VM's
  own connections, saved where the base's unit loads it before the network
  comes up on every boot, and reads it back. The same step places a
  systemd drop-in for each of the pool's units that requires that unit and
  is ordered after it, so on no boot does a pool service start without the
  rule set. Nothing but the rule set and the drop-ins is placed, and no unit
  of the pool starts, until the rule set is read back as asked; one that
  does not load or read back fails the box, as a fence that does not read
  back fails a create (481 §4), and the provider never makes such a pool
  ready. The rule set and the drop-ins are the provider's, placed by a step
  of the box contract it runs, and the pool bundle's own units are the same
  on every runtime, so no feature is conditioned on a runtime (767). A pool's
  services are reached only over vsock (§3).
- **It holds no provider credential.** A local driver has none (551 §4's "No
  key"), now because there is none to give, not because the agent runs
  beside the server.
- **It is the server's.** It is named for the pool in the local project, dies
  with the server and keeps its disk (106 §2's "A local machine lives no
  longer than its server", discobox-ai/vm#8), and is started again by the
  pool's reconcile. It is a Linux guest, so it does not count against vz's
  two macOS guests.
- **Its posture is a pool container's.** The MITM CA, sentinels, origins and
  audit database live in the pool VM, not as files on the user's machine;
  the CA is trusted inside sandboxes only, never by the host; and each
  sandbox VM is its sandbox's isolation boundary. A build's steps run in the
  pool VM (767), which holds no key.
- **Its capacity is the driver's limits on the host** (106 §1's rule for a
  machine pool): the host's CPU, memory and disk less the pool VM's own
  size, and vz's two running macOS guests. The pool's services are in the
  pool VM, not host processes (narrows 0144 §5).

### 2. The server creates every VM; the pool agent asks through the seam, on its own control-plane stream

**The pool VM reaches the server over vsock.** Its control-plane URL is
`vsock://2:<port>`, a forward the server names, which splices into a Unix
socket the server serves in its owner-only data directory. Each connection on
it is pushed into the carrier hub tagged with the pool (875 §3), as the vz
Docker pool's port 3001 is. A local pool needs no `controlPlaneUrl` and no
channel (875 §4: local providers are unchanged).

**The machine seam (#122) is a set of routes there**, served to that pool
alone and reaching only that pool's instances. Every seam request, connect
included, carries the pool agent's own credential, and 875 §3's rule applies
as to any route on a tagged stream: `PoolAuthenticator` must claim it, for the
pool the stream is tagged with, and anything unclaimed is refused. The tag
says which socket a connection came in on; any process in the pool VM can
open one, so the tag is never the credential. It is 106 §2's seam as 551 §3
widens it for a local driver:

- **Per instance**: create, start, provision, stop, remove, inspect and list
  with power state, and connect (§3).
- **Per pool**: the platform, image kind and limits the driver offers, the
  host's running-guest cap counted across every pool on the host (106 §4),
  and making or confirming the sandbox image.

**Every host-side value is the server's.** The pool names a sandbox and
nothing about the host. On create the server derives the instance's name, the
base or image, its size within the provider's bounds, no network device, and
its forwards: one per pool service of §3, each to a Unix socket the server
makes for that sandbox under its owner-only directory and serves itself. No
path, port, share, network or boot option is taken from a request. A sandbox
image is made by the server from what its release pins and the driver's base,
never from anything a pool sends, so no pool can change an image another
pool's sandboxes are made from.

**Provision places only what the server can check.** The pool sends the
sandbox's bootstrap and its agent's TLS keypair (481 §3), and the sandbox's
own pins: the platform and harness bundles it was made with, by digest,
which the pool keeps per sandbox and moves only on a re-pin. The server
places a pinned bundle only if its own asset service serves that digest, and
reads it from there, never from the pool; it extracts the agent layer and
places it with the units and pins (`agentlayer.Archive`), the bootstrap and
the keypair at their fixed paths, with the shared provisioner, and runs the
agent's `install`. It runs no command the pool names. What is placed is what
the pool recorded, so a start is a move only when the pins moved.

The pool agent stays the runtime authority (0017 §§9–10): its machine runtime
decides every create, start, stop and delete, and reports power state as the
seam answers it, which is the engine's (a local machine's shim). Its machines
come from its own engine when its bootstrap names a remote driver, and from
the seam when it names a local one. The provider holds no sandbox intent and
publishes no sandbox state. No guest operation of a local machine (exec, copy,
the shim's `/guest` relay, its loopback URL) reaches the pool VM; the seam is
those operations' only use on the pool's behalf.

### 3. Every hop is a vsock connection; a sandbox VM has no network device

| hop | path |
| --- | --- |
| server → pool agent | the pool VM's service endpoint, which only the server can reach |
| pool agent → sandbox agent | the seam's connect, an upgrade on the pool's control-plane stream, which the server splices into the sandbox VM's service endpoint |
| sandbox → its pool's services | `vsock://2:<port>` per service, forwarded to the sandbox's Unix socket, from which the server opens a 719 tunnel stream at the pool VM's service endpoint |
| pool → control plane | §2 |
| server → sandbox agent | none: through the pool agent, as on every pool |

- **Connect is an authenticated route, then a splice.** The server checks the
  pool's credential and that the sandbox is the pool's, answers the upgrade,
  and splices the stream into the sandbox VM's service endpoint. The pool
  agent's TLS to the sandbox agent (481 §3) runs end to end through it.
- **Toward the pool, the server is a tunnel client.** The pool VM keeps one
  service, so the pool's services are reached through it as for a pool behind
  an edge (481 §2): `/tunnel/<service>`, carried by the pool agent to its
  proxy unit's tunnel listener. For each connection on a sandbox's socket the
  server opens a tunnel stream (pkg/wstunnel's client, as `bridge.Dialer`
  is) over the pool VM's service endpoint and copies the bytes into it. The
  server terminates the WebSocket, not the sandbox's mTLS inside it (0126 §7),
  and sees ciphertext. The tunnel listener's bounds (`maxTunnels`, its wait
  for a first stream) are sized for every stream of the pool's sandboxes, not
  an edge's.
- **A sandbox reaches its own pool and nothing else.** Each socket is that
  sandbox's, so the server knows where every connection came from and opens
  its tunnel only to its own pool's VM. At the proxy every tunnel stream
  comes from the tunnel listener's source address, so the client
  certificate is a sandbox's identity there, as behind an edge. 0144 §4's
  second check by the socket is not carried to the proxy; the server's relay
  holds each sandbox to its own pool in its place.
- **A sandbox's endpoints are vsock URLs verified as the pool's name.** The
  pool gives a local sandbox `PoolEndpoints` of `vsock://2:<port>`, one guest
  port per service (the proxy, credentials, DNS, BuildKit), with `ServerName`
  `discobox-pool-proxy` and no `Address`: the form that contract already has
  for a URL with no host (ADR 0144 §4). The bridges dial them through
  `pkg/wire`, and verify the pool as they do today. This is a third row in
  481 §2's table, beside the direct path and the edge.
- **Each agent listens where the driver's `Service.Listen` says**, a vsock
  port on a local driver, through `wire.Listen`: the sandbox agent in its
  bootstrap and the pool agent in the pool's (narrows 345 §2's "Only TCP").
- **The fence is the VM's construction.** A sandbox VM is created with no
  network device, on every boot and from every image; whether one exists is
  no longer the driver's choice (0144 §4). Its forwards and its service are
  its only paths off the guest, and the server names all of them (§2). No
  filter is set and read back (481 §4): there is nothing to filter. Making an
  image may use a network; a sandbox's VM never has one. A local driver that
  cannot run a guest with no network device carries no pool.
- **The relay is the server's, not disco-vm's.** Which connection goes where
  is discobox's policy, and disco-vm's forwards and one service per instance
  are enough to route it. What disco-vm still owes is in the Consequences.

### 4. Inside a sandbox with no network device, programs use the proxy settings

Loopback exists without a network device, on Linux and on macOS. The sandbox
agent's bridges (the proxy bridge, the credentials relay, the DNS stub)
listen on loopback as on every runtime, and dial their `vsock://` endpoints.
A program reaches the network through the proxy settings the agent gives
every process it starts. A program that ignores them reaches nothing and
fails closed, as it does on a Docker pool's internal network. There is no tun
device.

**The DNS stub is the box's resolver.** A box with no network device has no
provider's resolver to keep, so on a local pool's sandbox the stub is made
the system's resolver: on Linux in `resolv.conf`, on macOS through the
system's resolver configuration with its address aliased on `lo0`. This
reverses, for that runtime, the sandbox agent's rule that the stub never
touches a machine's resolver. Names resolve through the pool's DNS; the
addresses they answer are reachable only through the proxy.

macOS is the harder guest, and what it costs is said:

- With no network service, macOS has no system proxy to configure
  (`networksetup` and `scutil` set proxies per network service), so a program
  that reads only the system's proxy settings through CFNetwork, not the
  environment, does not reach the proxy.
- Reachability APIs (`NWPathMonitor`, `SCNetworkReachability`) report no path,
  and a tool that checks them before trying will not try.
- The macOS resolver configuration that holds with no network service is
  proven on a guest when the darwin agent is built (#59). If none holds,
  names do not resolve for a program that resolves before it uses the proxy.
- The harnesses and the command-line tools a sandbox runs (node, curl, git,
  python, go, Homebrew) read the environment and hand names to the proxy.
  They work.

### 5. Windows (hcs) inherits the same shape

A Windows local pool is the same design: a Linux pool VM on hcs, Windows
sandbox VMs with no network device, and the server's relay. hvsocket takes
vsock's place (disco-vm's forwards and service are already hvsocket on hcs,
and a Linux guest on Hyper-V speaks AF_VSOCK to the host). Still deferred
with Windows (#68): Linux guests on hcs (discobox-ai/vm#27's Windows half), a
Windows guest with no network device, the elevation hcs needs, a Windows
sandbox agent with an hvsocket dialer in `pkg/wire`, and how WinHTTP's proxy
setting and the resolver are given. wslc stays the Linux pool on Windows.

### 6. What changes in the issues

- **#58 closes.** No Docker-free composition is needed.
- **#60 closes.** The pool VM's `apply` installs `git` as a boxd machine's
  does.
- **#64 is rewritten**: the vz driver makes, provisions, starts, repairs and
  removes the pool VM, with its console and log through the driver's guest
  operations, as `poolMachine` does for boxd.
- **#122 is rewritten**: the seam of §2 on the pool's carrier stream, with
  connect and the sandbox relays of §3.
- **#128 is rewritten** against §§2–4.
- **#67 is rewritten** against §1's capacity.
- **#68 is rewritten** against §5.
- **discobox-ai/vm#5** becomes a macOS guest with no network device, rather
  than "no route off the machine".
- **#59, #63, #121, #125, #126, discobox-ai/vm#6 and discobox-ai/vm#8**
  stand.

## Alternatives rejected

- **The Docker-free agent on the host** (0144, 106 as accepted). It is a
  second composition of the pool agent, a host-native proxy, host roots, a
  `git` to stage, and a darwin pool-agent release asset to sign and
  supervise, all for one platform, and it puts the pool's secrets in files on
  the user's machine (0144 §6). The pool VM costs a Linux VM per pool and a
  relay for every byte between a sandbox and its pool, which 0144 and 106
  rejected this design for. The relay runs in a process that already holds
  both ends, and the VM brings the pool to the same shape as every other
  pool.
- **`vm-image` as the pool VM's guest.** It is a Docker host: a read-only
  root shared by every pool, data and cache disks, and a Docker daemon the
  pool's container runs on (0062). Using it means either the Docker pool's
  shape, a pool container and its engine, with the seam and relays added to
  it, or provisioning a bundle onto a root that is read-only by design. A base
  with the pool bundle applied is how a boxd pool is made already. disco-vm
  may build its Linux base from `vm-image`'s kernel; that is its choice
  (discobox-ai/vm#27).
- **The seam as a disco-vm driver the pool agent opens**, each method a call
  to the server, so the pool's machine runtime and provisioner run unchanged
  over it. It hands the hypervisor's guest operations, root exec and copy in
  every machine of the pool, to the guest that runs the pool's builds (767),
  and turns each provisioning step into a round trip across the seam. The
  coarse seam keeps guest operations in the server, where 106 §3 keeps them.
- **The pool names a sandbox's forwards and image.** It would let whatever
  runs in the pool VM point a sandbox VM's forward at any socket the
  server's user can open, the host's Docker or SSH agent among them, or
  replace an image another pool's sandboxes start from.
- **The pool agent dials sandboxes over a host-only network**, with a network
  device in each sandbox VM fenced by the host. It brings back address
  assignment and a per-OS firewall that must be right for the fence to hold
  (0144's reason against it), where no device holds the fence by
  construction.
- **disco-vm relays vsock between its guests.** It would need discobox's
  routing (which sandbox reaches which pool, on which port), and the server
  would still own the pool's side. Forwards and one service per instance are
  enough.
- **The sandbox speaks the 719 tunnel itself, over one vsock port**, so the
  server's relay is a plain splice. Every sandbox's dialer would change, and
  `pkg/wire` would need a WebSocket over vsock where today it has only
  `wss` over TLS through an edge. A tunnel client in the server, which
  already holds both ends, keeps the sandbox's contract as it is.
- **A forward per service on the pool VM**, so the server splices each
  sandbox port into a pool port with no tunnel. disco-vm gives the embedder
  one service per instance and no other guest port (its ADR 0001), and the
  tunnel at that service already exists for an edge.
- **The pool's services bound to loopback and vsock in a pool VM**, in place
  of the firewall. It makes the pool bundle's features differ by runtime,
  which 767 refuses.
- **A tun device over vsock in each sandbox**, routing every packet to the
  pool. It would serve programs that ignore proxy settings. On Linux it is a
  userspace network stack per sandbox; on macOS a `utun` needs root and
  routes on a guest with no network service, or a Network Extension. No
  harness needs it (Deferred).

## Deferred

- **A tun device, or a system proxy, in a macOS sandbox.** Revisit when a
  harness or tool a sandbox must run reads only the system's proxy settings,
  or refuses to try because the system reports no path.
- **Linux machine sandboxes on a local pool**, and **the Docker pool's VM on
  disco-vm** (106 Deferred, whose condition discobox-ai/vm#27 may meet).
  Revisit when one Mac running both a vz Docker pool and a local pool costs
  too much in VMs.
- **Windows**, as §5 lists, with #68.

## Consequences

- disco-vm owes discobox, before a local pool is enabled
  (discobox-ai/vm#27 and the issues it names):
  - a Linux guest on vz with forwards and a service, from a base the embedder
    names by digest;
  - a guest created with no network device, macOS's included
    (discobox-ai/vm#5), which its driver seam does not offer today;
  - Linux guests outside `MaxRunning`;
  - a shim that dies with its embedder (discobox-ai/vm#8);
  - a clock kept to the host's across a host sleep in every local guest;
  - **a local instance's service endpoint that only the embedder can
    reach**, such as a Unix socket in the driver's owner-only directory, in
    place of a loopback port any local process reaches.
- The `vm` provider's local path makes a pool VM the way `poolMachine` does,
  with `poolruntime.ProvisionBox`, whose first step fences its network, and gains the
  seam's routes on the pool's carrier streams, connect, and a relay per
  sandbox that is a tunnel client toward the pool VM. `hostAgent` goes.
- pool-agent's machine runtime takes its machines from its own engine or from
  the seam, by its bootstrap's driver, and sends a sandbox's pins with its
  bootstrap. `resolvePoolHop` gains the vsock row. The Docker-free
  composition is never built.
- The sandbox agent's DNS stub becomes the resolver of a box with no network
  device.
- `pkg/vsock` dials and listens in a macOS guest (it is `mdlayher/vsock`,
  Linux only, today), so the darwin sandbox agent can reach its pool.
- A Mac runs a Linux VM per local pool beside its macOS sandboxes. It does
  not take a macOS slot.
- `server/providers`, `server/providers/discovm`, `pool-agent` and
  `sandbox-agent` `DESIGN.md` describe the new state when it lands, and
  `server/providers/REVIEW.md` gains the rule that every host-side value of a
  seam request is the server's. The issues in §6 are rewritten or closed once
  this is accepted.
