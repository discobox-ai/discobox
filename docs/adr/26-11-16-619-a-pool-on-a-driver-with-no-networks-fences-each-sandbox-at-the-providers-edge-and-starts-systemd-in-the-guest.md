# 26-11-16-619 — A pool on a driver with no networks fences each sandbox at the provider's edge, and starts systemd in the guest

- **Status**: Accepted
- **Date**: 2026-11-16
- Accepted by the owner by merging #1270, 2026-11-17
- **§5 superseded in part by**: [26-11-21-395](26-11-21-395-a-machine-pool-freezes-a-sleeping-sandbox-where-its-driver-can-and-stops-it-where-it-cannot.md)
  — its first bullet's "a sleep is a stop on archil, as on every machine
  pool" and "discobox never asks Archil for a pause": a machine pool freezes
  a sleep and a suspend where its driver can. The rest of §5 stands.
- **§1 and Context superseded in part by**: [26-11-28-634](26-11-28-634-an-edge-fenced-sandbox-also-reaches-its-providers-mount-hosts-while-its-data-disk-is-mounted.md)
  — guarantee 2's "names the host the pool machine is reached at and
  nothing else": while the sandbox's data disk is mounted it also names the
  mount hosts its provider adds. Context's "A guest with a data disk holds
  that disk's mount token, readable by root" and the Open item on replaying
  it: no mount token was found in the guest. The rest of §1 stands.
- **Issue**: #1053 (row A0 of the remote-drivers plan, #947)
- **Supersedes in part** (implementation of each has shipped, so these are
  superseded, not amended):
  - [26-11-11-963](26-11-11-963-one-vm-provider-type-every-pool-runs-a-linux-pool-agent-in-a-machine-its-driver-makes.md)
    §5's "A driver carries a pool only if it does one of the two". §1 below
    adds a third way. 963's two ways and the rest of §5 stand.
  - [26-10-23-551](26-10-23-551-a-vm-pool-bootstraps-its-own-agent-from-a-bundle-and-provisions-its-machines.md)
    §4's "A driver whose machines cannot be made so is not enabled", for a
    driver that meets §1's third way. It also narrows 551 §1's "On boot,
    systemd starts the agent", and
    [26-10-27-767](26-10-27-767-a-machine-pool-runs-what-every-pool-runs.md)'s
    pool provisioned as a Docker pool is. On a driver whose guest init is
    not systemd, systemd is started as §4 says. The units are unchanged.
- **Narrows**:
  - [26-10-29-481](26-10-29-481-a-machine-sandbox-and-its-pool-reach-each-other-at-their-private-urls-over-tls-the-pools-ca-issues.md)
    §3's edge row. The edge form stands (owner, Q4), and on such a driver
    the edge must also check a credential (§2).
  - [26-11-12-355](26-11-12-355-a-machine-sandboxs-resolver-is-the-dns-stub-and-its-bridges-resolve-the-pool-against-the-providers.md)
    §3. On a driver whose fence closes DNS, a bridge dials the address it is
    given and resolves nothing (§3).
- **Relates to**:
  - [26-11-11-080](26-11-11-080-each-disco-vm-driver-is-a-provider-type-of-its-own-name-configured-by-its-own-fields.md)
    §9: archil's pool machine holds a sentinel for the key, which its proxy
    swaps. This ADR is the fence 080 waits for.
  - [26-10-23-719](26-10-23-719-a-sandbox-may-reach-its-pool-through-a-websocket-tunnel-with-its-mtls-inside.md)
    (the tunnel through an edge);
  - [26-10-30-875](26-10-30-875-a-remote-pool-reaches-its-control-plane-over-a-channel-the-server-opens.md)
    (the control-plane channel);
  - [26-10-16-337](26-10-16-337-a-discobox-asks-to-sleep-and-names-when-to-wake.md)
    (sleep) and
    [26-10-16-420](26-10-16-420-a-docker-pool-freezes-a-sleeping-sandbox-and-a-person-can-suspend-one.md)
    (a Docker pool freezes a sleep);
  - disco-vm ADRs 26-11-03-676 (archil's guest operations), 26-11-12-604
    and 26-11-14-610 (a service's URLs and their scopes).

## Context

The owner wants archil carried (2026-11-16): "now that boxd is working can we
get archil done." Their answers on #947 fix the shape:
- **Q3:** Archil's sandbox firewall is evaluated as the provider-side
  control.
- **Q4:** 481 §3's edge form is kept.
- **Q5:** systemd is added and started in the guest, so the units are
  unchanged. No supervisor.
- **Q6:** the pool agent runs on an Archil sandbox of its own, as 963 §1 has
  every pool.

disco-vm's archil driver (`pkg/machine/archil`, pinned at `dc22eafdbd02`,
`docs/drivers/archil.md`) passes disco-vm's conformance suite on Archil
itself. What it reports today:
- **No `Isolation`, so no networks.** `Capabilities.Isolation` is one bit
  for two things: an instance "stripped of what the provider gives a
  machine by default" and "placed on networks of the caller's".
- **An egress allow-list fence**, set at create and read back.
- **Port 7302 exposed** at `https://7302-<route>.<zone>.archil.app`, which
  is both the ingress URL and the pool URL.
- **PID 1 is Archil's own init**, which runs nothing of the image's.
- **A session lasts at most 24 hours.** Then Archil pauses the sandbox.

discobox refuses archil's pools in four places:
- `fences` (`server/providers/discovm/provider.go:215`);
- `requireMachineFence` (`pool-agent/poolhop.go:94`);
- `NewMachineSandboxRuntime`, given `Isolate: true`
  (`pool-agent/machines.go:105`, `sandboxruntime/machine.go:139`);
- `createOptions`, which asks for isolation and a network
  (`sandboxruntime/machine.go:974`).

963 §5's rule is: isolate a sandbox on a network shared only with its pool
machine, or make it with no network device and the server's peer forward.
archil meets neither.

**What the spikes found** (Archil aws-us-east-1, 2026-11-16; discobox-ai/vm#86
and #1053's comments):
- **Egress.**
  - `none` closes everything, DNS included.
  - An allow-list connects to the allowed **name** whatever address the
    guest dialed, so fronting to another address fails.
  - With an allow-list, port 53 stays open to `8.8.8.8` and `1.1.1.1`, the
    resolvers the guest's `resolv.conf` names. These are the real Google and
    Cloudflare resolvers, not intercepted. That is a recursive-DNS channel
    out. Every other address on 53 is closed.
  - **A deny rule closes it.** `deny: ["8.8.8.8","1.1.1.1"]`, set in the
    create body, read back unchanged. Inside, nothing resolves, and an
    allowed host is still reached by its name at any address
    (`curl --resolve deb.debian.org:443:192.0.2.10` → 200).
- **Inbound.** The firewall is egress-only.
  - An exposed port is public with no auth. One anonymous `GET` to a
    stopped sandbox's URL started it.
  - An **unexposed** port with a **port token**
    (`POST /api/sandboxes/{id}/port-tokens`) is reached at the same hostname
    only with `X-Archil-Token`. Without it: `401`, and a stopped sandbox
    stays stopped. Port tokens work for HTTP/1.1 over HTTPS only, and are
    checked when a connection opens.
  - No address is a pool's alone: sandboxes egress through shared NAT.
- **What Archil plants in a guest.**
  - No account credential, no cloud metadata, no integration and no
    account disk.
  - A guest with a data disk holds that disk's mount token, readable by
    root. A token taken from one sandbox and used in another failed
    authorization. Replay from outside Archil was not tried.
  - `enable_service_ingress` defaults to false, so the guest's own
    `archil services` gets no public hostname.
- **systemd.**
  - `archil services create systemd -- unshare --pid --fork --mount-proc
    --kill-child /lib/systemd/systemd --system` runs systemd as PID 1 of its
    own PID namespace, under Archil's own supervisor. It comes back at every
    boot. `is-system-running` reports `running`, with nothing failed.
  - It has no cgroup namespace of its own, so systemd takes the guest's
    cgroup root.
  - Commands reach systemd's namespaces only through `nsenter`. disco-vm's
    exec and copy run in the root namespace.
- **The pause.** `max_ttl_seconds: 86400` and `on_timeout: "pause"` are
  fields of each sandbox.

## Decision

### 1. A third way to carry a pool: fenced at the provider's edge

**A driver carries a pool if it does one of 963 §5's two, or this third.**
It makes each sandbox's machine so that all four of these hold, each held
by the provider outside the guest, where the guest's root cannot loosen it:

1. **No peer network.** The machine shares no network with any other
   machine. It reaches another machine of the account only at that
   machine's URL, through the provider's edge.
2. **Egress fenced to the pool's public host alone, DNS included.** The
   allow-list names the host the pool machine is reached at and nothing
   else. No resolver answers the guest. On archil, the driver denies the
   resolvers Archil gives the guest, in the same create body.
3. **Its listener answers only the pool.** The machine's service URL answers
   only a caller that presents a credential the provider's edge checks, and
   the guest is never given it (§2). Nothing else in the guest gets a public
   listener: on archil, the driver creates every machine with service
   ingress off. An address rule would count only if the address were the
   pool's alone, and on archil none is.
4. **Nothing of the provider's account in the guest.** Either the driver
   gives a machine nothing of the account, or it strips what it gives at
   create. A credential that reaches only the guest's own resources, such as
   archil's mount token for the guest's own data disk, is the guest's own.
   It is not the account's.

**How each is known.** discobox decides from what disco-vm reports, never
from a driver's name. `Capabilities.Isolation` splits in two:
- **Bare:** whether a machine holds nothing of the account. Either the
  driver gives none, or it strips at create. Today's `Isolated` flag becomes
  "strip", asked only of a driver that gives something.
- **Networks:** whether the driver places machines on the caller's
  networks.

A driver that meets the third way reports it as a fence it can hold: no
networks, Bare, an allow-list that closes DNS, and a credentialed URL (§2).
disco-vm's conformance suite checks each claim on the driver itself: a
fenced guest resolves no name through any resolver, and its URL answers
`401` with no credential. The field names are disco-vm's to choose.

**What the runtime's `Isolate` and `Network` mean, then:**
- `Isolate` asks the driver to strip what it gives a machine. It is set
  only where the driver is not Bare already.
- `Network` is empty on a driver that has no networks.
- The four refusals in Context give way to the one rule of three ways,
  checked where 963's implementation puts §5's rule.

**What `MachineToken` keys off.** `hostCredential` sets it from
`Capabilities.Isolation` (`server/providers/discovm/poolkey.go:265`). That is
true for boxd, and false for archil only by accident. It keys instead off
whether the driver gives a machine something of the account that the pool
machine was not stripped of. That is the Bare half above. boxd: true, as
today. archil: false.

### 2. The edge form stands, and the edge checks a credential the guest never holds

The pool reaches a sandbox's agent through Archil's edge, which terminates TLS
and speaks plain HTTP to the agent. The agent checks the pool's token on every
request (481 §3's edge row, kept, Q4). In front of it, the edge itself admits
only a holder of the port credential:

- **A sandbox machine's port 7302 is never exposed.** Its URL is reached
  only with a port token. disco-vm's driver mints the token and gives it to
  its caller, the pool's engine, beside the URL (`Service`). The token is
  never placed in the guest. disco-vm reports this URL as the machine's
  pool URL with a credential, and gives no ingress URL. So the runtime's
  create, ingress off and pool URL on, both strict, holds unchanged.
- **The pool machine's 7302 is credentialed too.** It is not a guest-root
  listener a sandbox controls (767's build steps aside, already costed
  there). But an exposed port lets anyone start the machine and bill the
  account.
  - The server holds one token for its control-plane channel (875).
  - The pool's sandboxes share one more token, which the server mints and
    places in the pool's bootstrap. The pool hands it to each sandbox with
    its tunnel endpoint (719).
  - A sandbox holds the pool's listener credential, never its own.
- **One token for all sandboxes, not one each.** The pool's engine cannot
  name its own machine (963 §2), so a token per sandbox would put the server
  in every create. Behind that token, the pool's listener is no more exposed
  than a boxd pool's public URL, which every request already authenticates
  through.
- **What carries the header.** Every dialer through such an edge carries the
  credential as a header that disco-vm names. discobox does not hard-code
  `X-Archil-Token`. Each dialer pins HTTP/1.1: the control-plane channel,
  the tunnel and the pool's agent client.

How a token's life is kept (an expiry renewed, or none and revoked with its
machine) is disco-vm's, under one rule: it never outlives the machine it
reaches.

### 3. A sandbox reaches its pool's host without DNS

With §1's DNS closed, a sandbox's bridges cannot resolve the pool's host,
which 355 §3 resolves against the recorded provider resolver. The fence
connects an allowed connection by its name, so the address dialed does not
choose the peer.
- **The pool gives each sandbox an address to dial**: the pool host's, as
  the pool machine last resolved it, in the runtime-config document,
  renewed as the document is.
- **The bridge dials that address with the pool's host as the server
  name.** It resolves nothing.
- **481's objection does not reach this.** 481 rejected a fixed address
  because it goes stale. Here a stale address still reaches the pool,
  because the edge routes by name.

Inside the sandbox, the DNS stub stays the resolver (355 §§1–2). It carries
every lookup to the pool over mTLS, so names resolve and are audited as on
boxd.

### 4. systemd is added to the guest and started by the driver

The pool machine and every sandbox machine run the same units as on boxd,
under systemd. There is no supervisor of discobox's own (Q5).

- **The base carries systemd.** A sandbox cannot install it, because its
  only way out is the proxy bridge, which is itself a systemd unit.
  - An archil provider's default base is a Debian OCI image of discobox's
    own, pinned by digest, carrying `systemd`, `systemd-sysv`, `dbus` and
    `nftables`. Where it is built and released is open (Open).
  - A base the provider names must carry `/lib/systemd/systemd`. The
    provisioner refuses one that does not, and says why.
- **The driver starts it, and puts every guest operation inside it.**
  - Asked at create, disco-vm's archil driver registers systemd with
    Archil's own supervisor, as the spike did. Archil starts it at every
    boot.
  - Every exec and copy then runs in systemd's namespaces. So
    `provision apply --wait`, `systemctl` and a file copied to `/run` all
    act where the units run, and `poolruntime.ProvisionBox` and the pool's
    provisioner are unchanged.
  - Archil's mounts reach systemd's namespace: the data disk at
    `/.discobox/data`, mounted at each start, possibly after systemd. This
    holds whether by slave propagation or by order.
  - discobox calls no Archil command itself. `archil-client`, `nsenter` and
    the namespace are the driver's.
- **systemd as Archil's PID 1 is not possible.** Archil's init is PID 1.
  systemd as a plain process refuses to run as a system instance, so a PID
  namespace of its own is needed.

### 5. Archil's 24-hour pause is undone, and a sleep stays a stop

- **discobox's sleep (337) is a stop on archil, as on every machine pool.**
  337 lets the runtime choose between a freeze and a stop. A Docker pool
  freezes (`docker pause`, 420 §2). A machine pool stops, because
  disco-vm's contract has no freeze: `machine.Machine` has no pause, and
  the pool's `MachineSandboxRuntime` stops on a sleep and refuses a suspend
  (`pool-agent/sandboxruntime/machinepower.go`). No disco-vm driver pauses a
  machine on discobox's behalf. Two only resume a machine their provider
  paused: boxd's `StartVm` resumes a suspended machine, and archil's
  `Start` and first exec resume a paused sandbox. A stop checkpoints the
  root disk. discobox never asks Archil for a pause.
- **A pause discobox did not ask for is undone by the machine's owner.**
  - The server resumes a paused pool machine at its next reconcile; the
    driver's `Start` resumes a paused sandbox.
  - The pool agent resumes a paused sandbox it wants running at its next
    reconcile.
  - disco-vm reports a paused machine as paused, so the owner can tell it
    from running. Today `Attach` calls it running and resumes it only at
    its first exec.
  - A pause is never reported to the server as a sleep, and never counts
    as idle.
- **If Archil lets a sandbox run with no session limit**, through
  `max_ttl_seconds` or `on_timeout`, the driver makes every machine so, and
  the resume stays as the fallback. Whether it can is open.

## Alternatives rejected

- **An archil-specific kind of pool**, such as a pool agent on the host or
  a supervisor in place of systemd. The owner ruled it out (Q5, Q6). One
  pool shape is 963 §1's point.
- **The exposed port, with the agent's token alone** (481 §3's edge row as
  it was).
  - The guest's root owns 7302 and can answer it itself, so the URL is an
    inbound channel the fence and the proxy's audit never see.
  - Anyone can start a stopped sandbox and bill the account.
- **An address rule for the listener.** Archil's sandboxes leave through
  shared NAT, so no address is the pool's alone.
- **Accepting the DNS channel as a stated cost.** The owner had it checked
  first. One deny rule in the create body closes it, and the sandbox loses
  nothing, because the stub already resolves through the pool.
- **Egress `none`, with another path to the pool.** Archil has none: a
  sandbox reaches another only at its URL, over the internet path.
- **discobox starting systemd itself**, by calling `archil-client` and
  `nsenter` from the provisioner. That puts Archil in discobox's code. disco-vm
  owns a guest's operations (its ADR 26-11-03-676).
- **systemd without a PID namespace.** systemd refuses to run as a system
  instance unless it is PID 1.
- **A port token per sandbox for the pool's listener.** It puts the server
  in every create (§2). The shared token's exposure is boxd's public pool
  URL's.
- **Mapping discobox's sleep onto Archil's pause, for archil alone.** It
  would keep memory and wake faster, as a Docker pool's freeze does (420).
  But a freeze on a machine pool belongs in disco-vm's contract and the
  machine runtime, for every driver that can freeze (boxd's `SuspendVm`
  too), not in one driver's special case. Archil's pause is also bounded
  by the same session limit. Deferred below.

## Deferred

- **A machine pool that freezes a sleep and a suspend** (420 §§2–3 on
  disco-vm). It needs a freeze in `machine.Machine`, reported as its own
  state, and a `MachineSandboxRuntime` that uses it. Revisit when a
  machine pool's user loses a warm harness to a sleep's stop.

## Open, for implementation

- The names of disco-vm's capability fields (§1) and of its credentialed-URL
  result (§2).
- A port token's life and renewal (§2), within its rule.
- Where the archil base image is built and released, and its pin (§4).
- Whether systemd gets a cgroup namespace of its own, so it takes a subtree
  rather than the guest's cgroup root (§4). It is untried. The spike saw
  nothing break without one.
- How Archil's mounts reach systemd's namespace: slave propagation, or order
  (§4).
- Whether Archil's session limit can be lifted per sandbox, and whether a
  resume starts a new 24 hours (§5). A9's run past 24 hours answers the
  second.
- Whether the mount token replays from outside Archil. It decides nothing
  here, because the token reaches only the guest's own disk (§1.4). The
  provider documentation states it either way.

## Consequences

- **disco-vm's archil driver:**
  - closes DNS in its fence;
  - creates every machine with service ingress off;
  - gives 7302 a port token, never exposing the port, and hands the token
    to its caller;
  - registers systemd and runs guest operations in its namespaces when
    asked;
  - reports paused as paused;
  - passes conformance cases for each of these.
- **disco-vm's capabilities:** `Isolation` splits into Bare and Networks
  (§1). Every driver and every discobox caller of `Isolation` is updated,
  with no shim.
- **server/providers/discovm:**
  - `fences` becomes the three-way rule;
  - `hostCredential` keys `MachineToken` off Bare;
  - the pool machine's URL is dialed with the server's token;
  - the sandboxes' token is minted and placed in the pool's bootstrap;
  - a paused pool machine is resumed.
- **pool-agent:**
  - `requireMachineFence`, `NewMachineSandboxRuntime` and `createOptions`
    take the three-way rule;
  - `Isolate` is set only where the driver is not Bare, and `Network` is
    empty on a driver with none;
  - the hop's edge form carries the credential header and gives each
    sandbox an address to dial (§3);
  - the agent client pins HTTP/1.1 through such an edge;
  - a paused sandbox the pool wants running is resumed.
- **sandbox-agent / proxy bridge:** a bridge with a given address and a
  credential header dials that address and sends the header.
- **The archil base image** is published and pinned (§4).
- **Docs:** the archil provider's documentation states the fence, the
  credentialed listener, systemd's start and the pause.
- **`pool-agent/REVIEW.md`'s "A machine's public port"** names this ADR for
  archil once the code lands.
