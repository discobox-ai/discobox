# 26-11-26-536 — A disco-vm pool hands over to its new agent, and the old one drains

- **Status**: Accepted
- **Date**: 2026-11-26
- Accepted by the owner by merging #1825, 2026-11-26
- Amended 2026-11-26, while nothing had shipped against it, with what its
  implementation (#1118) found: §2's builder drain, §3's sweeps, §4's lock
  files, control-plane channel and a handover that does not take, and two of
  its open questions answered.
- **Issue**: [#1118](https://github.com/discobox-ai/discobox/issues/1118)
- **Would narrow**, for a pool whose host is a disco-vm pool machine only:
  - [26-10-24-953](26-10-24-953-every-box-is-provisioned-from-an-ordered-list-of-bundles-and-a-pool-is-a-box.md)
    §5's "`apply` run again in the running box, which restarts the units it
    moved" for the agent and its proxy (§4 below), and its "a machine keeps
    its base" for the pool's own machine (§2).
  - [26-11-15-602](26-11-15-602-an-in-place-restart-is-recorded-before-it-begins-and-the-new-agents-first-report-counts.md)
    §4's "the restart stays one `try-restart` transaction": a handover takes
    its place, recorded at the same moment (§4).
  - [26-11-14-624](26-11-14-624-after-an-upgrade-the-server-is-not-ready-until-its-pools-are.md)'s
    Deferred: this takes up its second condition, "a release must upgrade
    without interrupting a running box's streams", for these pools alone.
  - [26-10-29-481](26-10-29-481-a-machine-sandbox-and-its-pool-reach-each-other-at-their-private-urls-over-tls-the-pools-ca-issues.md)
    §2's address in the sandbox's bootstrap: the runtime-config document
    carries the address of a direct path too, as
    [26-11-16-619](26-11-16-619-a-pool-on-a-driver-with-no-networks-fences-each-sandbox-at-the-providers-edge-and-starts-systemd-in-the-guest.md)
    §3 has it for an edge (§5).
- **Depends on**: [26-11-24-077](26-11-24-077-disco-vm-holds-a-pool-discobox-asks-for-its-host-and-members-and-the-driver-fences-them.md)
  (Proposed) giving a pool a successor host (§5).
- **Preserves**: the Docker engine and every driver but disco-vm's remote
  drivers, unchanged; [26-11-15-294](26-11-15-294-a-report-after-the-reconcile-started-the-pools-agent-is-the-new-agents.md)
  and 602's first-report rule; 953 §5's "a running pool is never taken down
  over its bundles"; [0063](0063-a-pool-agent-keeps-its-identity-key-and-registers-once.md)'s
  key, kept across both scenarios; repair, which still stops and starts the
  machine.

## Context

Upgrading a disco-vm pool cuts every stream its agent and proxy serve: an
`exec` or terminal routed through the agent, a sandbox's egress, its DNS, a
build. The owner (2026-11-26, on #1118):

> I primarily want this implemented for disco-vm drivers, and no other
> drivers. In that situation today we already upgrade in place the bundles.
> This means we have two scenarios to consider. 1. Upgrade by provisioning
> and existing pool machine 2. Replace a pool machine. Scenario 1 already
> happens today but just not done in a non disruptive way. We need to find a
> way to send new requests to the new pool proxy while draining the old one.

Before that, the owner (2026-11-20) asked where the pool's clients are, and
suggested that a client be told to take new connections elsewhere ("a special
30x we recognize") while existing ones stay up to a bound ("30 minutes, maybe
10").

**Scenario 1 today.** `poolMachine.converge`
(`server/providers/discovm/poolmachine.go`) runs `poolruntime.ProvisionBox`
in the running machine when bundles move. The bundle's `apply` restarts the
units whose files moved. A moved placement then runs
`poolruntime.RestartPoolUnits` (`server/providers/poolruntime/box.go:89`),
which is `systemctl try-restart` of the agent and proxy units. systemd stops
a unit's whole cgroup, so every connection either process holds ends. The
proxy has no drain either: `proxy.Server.Close` (`proxy/server.go:426`)
closes its listener and every connection together.

**Scenario 2 today.** It does not happen. `converge` never replaces a running
pool machine. A machine that is gone is made again, with nothing left to
drain. A moved base is applied in place: the pool's machine keeps its base,
and only its sandboxes' machines move to the new one (953 §5).

**Who connects to the pool.** There are two clients, as the owner hoped:

| Client | Reaches the pool at | Where it chooses |
| --- | --- | --- |
| The server, for every sandbox operation, stream and the create socket | the machine's ingress URL, one TCP address the driver's `Service.Listen` maps it to (`agentListenURL`, `acquireAgentClient`) | `RuntimeProvider.AcquirePoolAgentClient`, one lease per request. Plus the control-plane channel the server holds for a pool with no `controlPlaneUrl` (`poolruntime/channel.go`). |
| A sandbox's machine: egress, nested Docker, BuildKit, DNS and credentials | `https://discobox-pool-proxy:<port>` dialed at the pool's address, or the edge's tunnel | One dialer, `proxy/bridge.Dialer` (`proxy/bridge/dialer.go:61`), used by the bridge forwarder, the DNS stub and the credentials relay. A direct path's address is fixed in the sandbox's bootstrap (`PoolEndpoints.Address`). An edge's address is in the runtime-config document, renewed with each revision (619 §3). |

**What lives on the pool's machine alone.** The pool's identity key
(`layout.PoolIdentityKey`), which every sandbox's bootstrap pins to check
runtime-config deliveries. The proxy's CAs, which every sandbox trusts. The
durable data tree: sandbox records, origins, source data, the Git cache
(`layout.PoolData`). Only the cache tree rebuilds (`layout.CachePool`). Both
agents and both proxies of scenario 1 share all of it, since they share a
disk. Scenario 2's new machine has none of it.

**What lives in the agent's process.** The per-sandbox power lock
(`sandboxCore.powerLocks`) and boot mark (`booting`), the background loops
(status poll, reapers, sweeps, reports) and the control-plane socket
(`unix:///run/discobox/control-plane.sock`). Suspend receipts are already
on disk (`suspend_completion.go`).

## Decision

### 1. Scope: a disco-vm pool's machine

A **handover** replaces a pool's agent and proxy without cutting what they
serve. It runs only for a pool hosted on a disco-vm pool machine
(`poolMachine`): boxd and archil today. The Docker engine, every one of its
drivers (docker, libkrun, vz, wslc, DigitalOcean, exec) and disco-vm's local
host agent (not staged, #64) are unchanged. Repair is unchanged: it serves a
pool whose agent does not answer, which has nothing to drain.

### 2. Two scenarios, and what starts each

- **In place (scenario 1):** whatever restarts the agent or the proxy today:
  a moved pool bundle whose `apply` would restart them, or a moved placement.
  The machine stays. A new agent and proxy start beside the old ones (§4).
- **A successor host (scenario 2):** the pool machine's base moves. Today the
  machine keeps its old base (953 §5); now a successor host is made from the
  new one, and the pool hands over to it (§5). A machine that is gone is
  still made again as today, since nothing is left to drain.

A moved builder (BuildKit and the registry) is not handed over in either
scenario. Their state is one directory, and two builders cannot share it. A
moved builder restarts as today, but only once the old builder's builds have
ended or the mediator's own build drain (ten minutes) has passed. A new build
waits for it at the mediator's socket and is not refused. *(Amended: this
said the drain bound of §3. The mediator's drain runs inside the apply of every
pool, the Docker engine's included, under its 15-minute bound; after a
handover nothing waits for it.)*

### 3. One agent is active, and the old one drains

At any moment, exactly one agent of a pool is **active**. It accepts new
requests and connections, runs power operations, writes the pool's durable
trees, runs the background loops, sends the status reports and holds the
control-plane socket.

Handing over makes the old agent and its proxy **draining**:

- They accept no new connection.
- They finish each request they have received.
- An HTTP keep-alive connection closes once its current response is sent
  (`Connection: close`; `GOAWAY` on HTTP/2), so the client's next request
  opens a new connection, which reaches the active one.
- An upgraded stream (exec, terminal, the create socket), a `CONNECT` tunnel
  and a DNS-over-TLS connection run until they end.
- No loop runs, no report is sent, and no power operation starts.
- The process exits when its last connection ends, or at the **drain bound,
  30 minutes** after the handover, cutting whatever is left.
- The process that took over sweeps nothing of the pool's shared state (its
  images, snapshot builds, trees, assets, the proxy's spools and retired
  audit columns) until the draining one has exited: a sweep removes what its
  own process holds nothing of, and the draining one still holds its own.
  *(Amended: the first live handover had the new agent remove the old one's
  snapshot build mid-build.)*
- What the old proxy audits while it drains is recorded where it is read
  (ADR 0130 §4): in place, in the same audit tree as the new proxy's. For a
  successor host, it is carried over once the drain ends (§5 step 7).

The bound is a fixed constant, not a provider setting. Thirty minutes covers
most builds, long commands and downloads. A longer stream, such as a person's
terminal, is cut as it is today, and is attached again (`terminal attach`).

The upgrade watch (26-11-14-624 §1, 294, 602) counts the active agent's first
report. A draining agent sends none, so the drain never holds the server's
readiness.

### 4. In place: the listening sockets outlive the processes

On the pool's machine, systemd socket units hold the listening sockets of the
agent (the ingress address and, behind an edge, the tunnel listener), the
proxy (its mTLS listener, credentials and DNS, and its loopback control API,
which the agent reads the audit through) and the BuildKit mediator.
The agent and the proxy run as instances of template units, alternating
between two slots. Every instance of a slot accepts from the same sockets.

A handover in place:

1. `apply` lays the new bundle, and starts the idle slot rather than
   restarting the running one. The new agent comes up as a **standby**. It
   prepares, accepts nothing, and reaches the control plane through the
   active agent's socket.
2. The provider records the agent start (602 §1, at the same moment: once
   placement is closed, before `ProvisionBox`).
3. The old agent and proxy stop accepting and begin to drain (§3).
4. The new ones accept, take over the control-plane socket, and send the
   marked first report (602 §2).

A connection that arrives between steps 3 and 4 waits in the socket's queue.
It is never refused. Neither the server nor any sandbox changes anything in
this scenario: the address they dial is the same, and the socket hands their
next connection to the active process.

*(Amended: the standby is ready once its listeners, channel and front are
made, and says it is active only once it has built everything it serves with,
so one that fails after taking the lock never says so. A new agent that still
does not say it is active within two minutes of step 3 is stopped, and the old
slot's units are restarted, which cuts what it still carried: the pool is
never left with nothing accepting. It starts on what was laid, since the old
binary is gone. Every exit but a drain's is restarted.)*

**Power across the two processes.** The per-sandbox power lock and the boot
mark move from the agent's memory to a lock file in the pool's lock tree, held
with an open file description lock (fcntl(2) `F_OFD_SETLK`), which another
process can be asked about without taking it. *(Amended: this said the
sandbox's tree, which a delete removes while it holds the lock, and
`flock`, which can be asked about only by taking it.)* A power operation the old agent still has in flight (a boot's
wait outlives the operation, `power.go`) holds the lock until it ends, and
the new agent's operation on that sandbox waits for it. Suspend receipts are
already on disk.

**The control-plane channel.** For a pool with no `controlPlaneUrl`, the
server dials a new channel once the old agent closes its own, which reaches
the new agent, since it alone accepts. The old agent closes it once the
connections it forwarded onto it have ended or are idle (each has its answer,
and has carried nothing for half a second since then and since the drain
began; a call still waiting on its answer holds it), within 20 seconds of
step 3: its
own calls to the control plane, from then on, reach it through the socket the
new agent serves. *(Amended: this said the server dials once the handover
reaches step 4, and keeps the old channel until the old agent closes it. The
server learns of step 4 from nothing but the new agent's first report, which
needs the channel. So the new agent serves from step 4, before that report,
which it makes once its channel opens: the pool is the one the old agent
registered, under the same key.)*

**Only disco-vm asks.** The pool bundle's units take this shape on every
pool, since one bundle serves them all. A handover runs only when the
provider's placement asks for it. The Docker engine does not ask, so its
`apply` and `RestartPoolUnits` restart as today.

### 5. A successor host: the pool's trees move, and its sandboxes are given the new address

1. **disco-vm makes a successor host in the pool.** 077 gives a pool one
   host. A handover needs a second for its length, which leaves at the end.
   The successor is on the pool's network on boxd, so the pool's members
   reach it as they reach the first.
2. **It is provisioned as the pool's box, as a standby.** Its agent does not
   serve until step 5.
3. **The old agent hands over its power and writes.** It starts no power
   operation and accepts no write to the pool's trees (a Git push to an
   origin or the cache). It finishes those in flight, and stops its loops and
   reports. Streams and egress keep flowing through it. A new request the
   server routes to the pool waits meanwhile, as one for a pool coming up
   does. It is not refused.
4. **The server carries the pool's durable trees** from the old host to the
   successor through each machine's guest operations: the identity tree, the
   proxy tree (its CAs, control key and secrets file) and the data tree. It
   copies them once while the old agent still serves, and copies what
   changed after step 3. The server holds them in memory only and stores
   nothing. The cache tree is not carried, and rebuilds. The successor
   therefore holds the same key and CAs: every sandbox's bootstrap still
   verifies it, and nothing is enrolled again.
5. **The successor becomes active.** It sends its marked first report.
   The pool's runtime state records it as the host that
   `acquireAgentClient` returns, and the old one as draining. The server
   dials the successor's channel, and keeps the old one until it closes.
6. **The sandboxes are given the new address.** The runtime-config document
   carries the pool host's address for a direct path too. The successor's
   first document to each sandbox names it, and the document's address wins
   over the bootstrap's. A sandbox's bridges dial it for each new
   connection. A connection to the old host stays where it is.
7. **The old host drains** (§3). Its proxy accepts new connections until
   every running sandbox has applied the document that names the successor,
   or until the bound. Once its proxy has exited, the server carries its
   proxy's audit to the successor, the records it wrote while draining
   included, so they are read on the pool as before. The old host is then
   removed, and the keys issued for it are revoked.

A sandbox whose agent does not take the document's address (one pinned to an
agent from before this) keeps dialing the old host. At the bound, it loses its
pool until its next start, whose bootstrap names the successor.

### 6. Which drivers go first

- **In place:** boxd and archil. It needs nothing of the driver: the machine,
  its URLs and its sandboxes' fences stay as they are.
- **A successor host:** boxd first. A successor on the pool's network is
  reached by the pool's members as the first host was.
- **archil** follows once disco-vm can move a member's fence to a successor
  host. Its members' egress is allowed to the old host's name alone (077 §2's
  table).

## Open, for implementation

- *(Answered by #1118.)* The units' names, and how `apply` starts the idle
  slot: `discobox-pool-agent@{a,b}` and `discobox-proxy@{a,b}`, started by
  `discobox-pool-agent units apply`.
- *(Answered by #1118.)* The proxy's audit database takes both slots' writes,
  in turn; a column the new schema retires is dropped only once the old
  proxy has exited.
- How the delta copy of §5 step 4 finds what changed.
- disco-vm's interface for a successor host, settled with 077.

## Alternatives rejected

- **A redirect the clients recognize (a special 30x).** In place there is
  nowhere else to send a client: the machine has one ingress address, and its
  sandboxes dial one proxy address. A shared socket moves new connections
  without a word to either client. For a successor host, much of what a
  sandbox's bridges carry is not HTTP (DNS over TLS, BuildKit's gRPC, raw
  tunnels), so a redirect would be answered at the bridge rather than by the
  program that sent the request. A redirect also arrives only in place of a
  connection. The runtime-config document already carries an edge's address
  per revision (619 §3), and the server already decides which agent each
  lease reaches. Telling each client ahead of time means no connection is
  turned away.
- **Each process binds its own listener with `SO_REUSEPORT`.** The kernel
  spreads new connections over both listeners, and resets those still queued
  on a listener when it closes. One socket systemd holds has one queue that
  either process accepts from.
- **Pass the listener from the old process to the new (a hot restart).**
  systemd stops a unit's whole cgroup on restart, so the old process cannot
  outlive its own restart. Keeping it with `KillMode=none` leaves it
  unsupervised.
- **A new identity and new CAs for a successor host.** Every sandbox's
  bootstrap pins the pool's key and trusts its CAs. Both are fixed in the
  bootstrap, so every running sandbox would have to be provisioned again.
- **The old host sends its trees to the successor itself.** 077 §2's fifth
  guarantee lets a host be reached only by its members and its maker. The server already acts
  on both machines through their guest operations, and is already trusted
  with everything those trees authenticate to.
- **Two builders side by side.** BuildKit's state is one directory.
- **A successor host for every upgrade.** That costs a machine and cold
  caches each time, where the in-place apply already works.
- **Identify each agent in its reports.** 294 and 602 rejected it. A
  draining agent sends no report, so the first-report rule still tells the
  new agent from the old.

## Consequences

- For up to 30 minutes after a handover, a pool's machine runs two agents
  and two proxies. A successor host costs a second machine for that time.
- A successor host's handover holds power operations and pool writes for as
  long as the final copy of the trees takes.
- Streams outlast an upgrade up to the bound. A terminal held past it is
  attached again.
- The pool-agent, proxy and discovm design docs change with the
  implementation: the active and draining agent, the socket units, the
  on-disk power lock, and the successor host.

## Deferred

- **The Docker engine and its drivers.** The owner asked for disco-vm alone.
  Revisit when an upgrade of a Docker pool must not cut its streams. Most of
  the needed shape is §§3–4, since the bundle is shared.
- **disco-vm's local host agent** (vz, hcs; #64). Revisit when it is staged.
- **The drain bound as a setting.** Revisit when a pool's streams routinely
  outlast 30 minutes.
- **A successor host on request, or as repair.** Only a moved base makes one.
  Revisit when an operator needs to move a pool to a fresh machine.

## Relations

Narrows 953 §5, 602 §4, 624's Deferred, 481 §2 and 619 §3 as the header
says, for a disco-vm pool's machine. Depends on 077 for a successor host.
Keeps 0063's identity key across both scenarios.
