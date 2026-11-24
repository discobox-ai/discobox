# 26-11-24-077 — disco-vm holds a pool: discobox asks for its host and its members, and the driver fences them

- **Status**: Proposed
- **Date**: 2026-11-24
- **Issue**: #1522 (found while working on #1057)
- **Would supersede in part** (each has shipped, so these would be
  superseded, not amended):
  - [26-11-16-619](26-11-16-619-a-pool-on-a-driver-with-no-networks-fences-each-sandbox-at-the-providers-edge-and-starts-systemd-in-the-guest.md)
    §1's "How each is known", its "What the runtime's `Isolate` and
    `Network` mean" and "What `MachineToken` keys off". Its four guarantees
    stand, as archil's way of meeting §2 below. Also §2's "One token for all
    sandboxes", where the driver now mints the reach (§3), and §2's minting of
    the sandboxes' token by the server.
  - [26-11-11-963](26-11-11-963-one-vm-provider-type-every-pool-runs-a-linux-pool-agent-in-a-machine-its-driver-makes.md)
    §5's peer forward "which the server sets at create", and its rule "A
    driver carries a pool only if it does one of the two". The forward tunnel
    and the rest of §5 stand.
  - [26-10-23-551](26-10-23-551-a-vm-pool-bootstraps-its-own-agent-from-a-bundle-and-provisions-its-machines.md)
    §4's "on boxd every sandbox machine is created `isolated` … on a network
    shared only with its pool's machine; and the pool's machine is on that
    network and no other", as something discobox asks for. It becomes boxd's
    way of meeting §2. §4's provisioning key and its stated costs stand.
- **Relates to**:
  - [26-10-29-481](26-10-29-481-a-machine-sandbox-and-its-pool-reach-each-other-at-their-private-urls-over-tls-the-pools-ca-issues.md)
    §§2–4 (the hop's forms, and refusing a pool before its first report);
  - [26-10-23-719](26-10-23-719-a-sandbox-may-reach-its-pool-through-a-websocket-tunnel-with-its-mtls-inside.md)
    (the tunnel);
  - [26-11-12-355](26-11-12-355-a-machine-sandboxs-resolver-is-the-dns-stub-and-its-bridges-resolve-the-pool-against-the-providers.md)
    (the DNS stub);
  - [26-10-30-875](26-10-30-875-a-remote-pool-reaches-its-control-plane-over-a-channel-the-server-opens.md)
    (the control-plane channel);
  - disco-vm ADRs 26-11-17-311 (`Bare`, `Networks`, `CloseDNS`, credentialed
    URLs), 26-11-14-610 (a service's ingress and pool URLs) and 26-10-31-990
    (vz's vsock relay between instances).

## Context

The owner, 2026-11-21: "discovm is only for discobox use, so the discovm pool
is supposed to represent discobox pool. The pool is the isolation boundary,
then it just matters who can talk to whom."

Today discobox builds the pool boundary itself from disco-vm's parts. disco-vm
reports `Bare`, `Networks`, its egress fences (`EgressAllow`,
`EgressCloseDNS`) and `URLs.Credential` (disco-vm ADR 26-11-17-311), and
discobox works out from them how a sandbox's machine is held to its pool:

- **`sandboxruntime.PoolFencing`** (`pool-agent/sandboxruntime/fencing.go:35`)
  picks `FencedOnNetwork` (boxd) or `FencedAtEdge` (archil). Four callers ask
  it:
  - the server's provider check, `fences` (`server/providers/discovm/provider.go:222`);
  - the server's pool machine, `poolMachine.converge` (`poolmachine.go:297`);
  - the pool agent at start (`pool-agent/agent.go:221`);
  - the machine runtime, `NewMachineSandboxRuntime`
    (`pool-agent/sandboxruntime/machine.go:159`).
- **The server places the pool machine** on the pool's network or on none
  (`poolNetworks`, `poolmachine.go:607`). It refuses to adopt a machine under
  the pool's name on other networks, or stripped (`poolmachine.go:328`).
- **The pool agent picks the sandbox's side**:
  - the hop's form, from whether the pool URL equals the ingress URL
    (`Bootstrap.reachedThroughEdge`, `pool-agent/bootstrap.go:184`);
  - closing DNS at an edge (`poolHop.fencedBy`, `pool-agent/poolhop.go:102`);
  - the network, the strip and the fence of every machine
    (`createOptions`, `sandboxruntime/machine.go:1124`).
- **`MachineToken`** (whether the pool machine holds an in-VM token of the
  account) is read off `Capabilities.Bare == BareStrip`
  (`server/providers/discovm/poolkey.go:507`).
- **The held driver's way is not built.** 963 §5 has the server set a vsock
  peer forward from each sandbox VM to the Linux pool VM. `PoolFencing`'s own
  comment says no pool reaches one yet. There is no code of it to move.

So every new driver changes both repositories: #1057 and #1518 touched the
server, the pool agent and their docs only to accept archil. And disco-vm
does not know which machine is a pool's host. The pool machine sits in the
provider's scope, named for the pool, and the sandboxes in the pool's scope
(963 §2). Nothing in disco-vm ties the two together.

## Decision

### 1. A disco-vm pool is a discobox pool, with one host and its members

discobox asks disco-vm for a machine in one of two **roles** in a disco-vm
pool, which is the discobox pool's scope (`machinePool`):

- **The host.** Always a Linux machine. It runs the pool agent. There is one
  per pool. **The provider's engine** makes it, the server's, holding the
  provider's credential, as `poolMachine.create` does today.
- **A member.** A sandbox's machine, or a snapshot build's, of the pool's
  sandbox OS: Linux, macOS or Windows. **The pool's own engine** makes it,
  the pool agent's, with the pool's issued key (551 §4).

Asked for a member, disco-vm knows the pool's host from the pool itself.
discobox names neither a network nor a forward target. What each role is
asked with is discobox's: name, base or snapshot, size, volumes, and the
member's ingress URL off (610). How the fence is made is never discobox's.

- **The pool's engine still cannot name its host.** It cannot exec on it,
  stop it or destroy it, as on a held driver today (963 §2). The host is
  the pool's in disco-vm's records. It is not a machine the pool's key acts
  on.
- **Where the driver keeps the host is the driver's.** Persisted pools do not
  move. An existing boxd pool machine, in the provider's scope and named for
  the pool, becomes that pool's host without being remade. The upgrade is an
  adoption: the server's next reconcile asks disco-vm for the pool's host,
  and disco-vm finds the machine 963 §2 placed.

### 2. What disco-vm guarantees, per driver

For a driver that holds a pool (§4), disco-vm guarantees each of these, held
by the provider or the host process outside the member's guest, where its root
cannot loosen it:

1. **A member holds nothing of the account.** No credential, peer or
   integration of the provider's. A credential that reaches only the guest's
   own resources is the guest's (619 §1.4).
2. **A member reaches its host and nothing else**, DNS included. No resolver
   answers it beyond what reaching its host needs. If a resolver answers
   the host's name, it answers nothing outside the pool.
3. **A member is reached only by its host.** Nothing else of the account and
   nothing on the internet reaches its service. A URL that a front serves is
   answered only for a credential the front checks and the guest never holds.
4. **A member reaches no other member,** and no machine of another pool.
5. **The host is reached by its members and by its maker**, and by no other
   machine of the account. It reaches what it needs: its provider, the
   control plane and the internet, through the pool's proxy for its
   sandboxes.

How each driver holds them is its own, and disco-vm's documentation states
it per driver:

| Driver | How it holds the pool |
|---|---|
| boxd | A private network for the pool alone. The host is on it, off the org's default. Each member is stripped (`isolated`). |
| archil | No network. Each member's egress is allowed to the host's public name alone, with Archil's resolvers denied. The host's listener and each member's are reached only with port tokens. Service ingress is off. |
| vz, libkrun, hcs | The host is a Linux VM. Members (macOS, Windows or Linux) have no network device, on every boot. A vsock forward that the driver sets at the member's create goes from a fixed guest port to the host's service port. |

disco-vm's conformance suite proves each claim on each driver that reports
§4's capability, from inside a member:
- nothing resolves but what §3's reach needs;
- every address but the host's is closed;
- another member is unreachable, and so is the provider's API with anything
  found in the guest;
- the member's URL refuses a caller with no credential.

### 3. A member's create returns how it reaches its host, and the host gets how it reaches each member

**The member's reach to its host**, returned by the member's create (and by a
lookup of an existing member, so a restarted pool agent gets it again):

- **How it dials**: a URL whose host and port are dialed over TCP, or a guest
  vsock port to dial, on a member with no network device.
- **An address**, when the member has no DNS: the address to dial in place
  of the URL's name, which still names the server for TLS and the front's
  routing (619 §3).
- **A credential**, when a front checks one: the header that carries it, its
  token and its expiry (disco-vm's `Credential`, as 311 has it).
- **Whether something on the path terminates TLS** before the host's
  listener.

The credential reaches the host's listener and nothing else. It never
outlives the member. Whether each member gets its own token, or the pool's
members share one, is the driver's. 619 §2 shared one only so that the
server would not be in every create. With the driver minting it at the
member's create, the server is in no create.

**The host's reach to each member** is the same shape, from the pool's
engine: the member's pool URL with its credential (311's `Engine.URL`), or a
`Dial` through the engine on a driver whose members have no network device.
**The maker's reach to the host** (the server's control-plane channel, 875) is
returned by the host's create the same way.

**What discobox does with it.** discobox carries the reach into each
sandbox's runtime-config document and bootstrap, as it carries `RuntimeEdge`
today. It picks its own protocol from the reach alone, with no driver's name
and no capability:
- **The tunnel or direct.** A path that terminates TLS, or a vsock port, is a
  tunnel (719; 963 §5's forward tunnel). Anything else is direct.
- **The agent's TLS.** Plain HTTP behind a front that terminates TLS (481
  §3's edge row). The pool's setting everywhere else.
- **The DNS stub's bridges.** They dial the address when there is one, or
  resolve the name when there is not.

The proxy, mTLS, the DNS stub and the runtime-config document are unchanged.
How the host gets its provisioning key (551 §4) is unchanged.

### 4. One capability, "holds a pool", refused in one place

- **disco-vm reports which member OSes a driver holds a pool for**, with a
  Linux host. Examples: boxd and archil hold Linux members; vz holds macOS
  and Linux; hcs holds Windows. The field's name is disco-vm's.
- **The server refuses it once**: at provider and pool create, where `fences`
  refuses today. A pool whose sandbox OS the driver does not hold is
  refused, and says so.
- **Nothing else in discobox checks it.** It is not checked at the pool
  agent's start, in the hop or in the runtime. disco-vm's member create
  refuses a pool its driver does not hold, so a pool that got past the
  server's check still makes no unfenced member. That refusal is disco-vm's
  backstop. It is not a second discobox rule.
- **`MachineToken`** becomes something disco-vm reports of the host: whether
  the host holds an in-VM credential of the account (551 §4's stated cost).
  It is no longer read off `Bare`.

### 5. What leaves discobox, and what stays

**Leaves** (once built):
- `sandboxruntime.Fencing`, `PoolFencing` and its four callers' switches;
- `poolNetworks`, and the network and strip checks on adopting a pool
  machine;
- `poolHop.fencedBy`, and `reachedThroughEdge`'s inference of an edge from
  equal URLs;
- the network, `Bare` and egress fence in `createOptions`, and the runtime's
  `HoldToPool`, `Network` and `Egress`;
- the re-read of a found machine's fence (`fenced`). disco-vm's lookup of a
  member answers for its fence;
- the server-set peer forward of 963 §5. It was never built, so nothing is
  removed.

discobox reads none of `Bare`, `Networks`, `Egress` or `URLs.Credential` to
carry a pool. Whether disco-vm keeps them for its other callers
(`disco-vm run`) is disco-vm's.

**Stays**, as the issue's "Stays in discobox": its protocol on top of who can
talk to whom (§3), the pool proxy, the DNS stub, the runtime-config document,
and how the host gets its provider credential.

## Alternatives rejected

- **discobox keeps deciding from capabilities** (619 §1). Every driver then
  changes both repositories, and the rule for each way is written twice, as
  #1057 and #1518 showed. A held driver would add a third way to every
  caller of `PoolFencing`.
- **disco-vm reports the fencing kind** (`network`, `edge`, `held`) for
  discobox to switch on. The per-way logic stays in discobox, behind one
  enum, and a new way still changes both repositories.
- **disco-vm takes discobox's protocol too** (the tunnel, mTLS, the proxy).
  disco-vm then knows discobox's wire, and the owner put disco-vm's boundary
  at who can talk to whom. The reach (§3) is enough for discobox to choose its
  protocol without knowing the driver.
- **The host in the pool's own scope, made by the pool's engine.** The pool's
  key could then exec on, stop or destroy the machine it runs on, and the
  held driver's fence, which is the scope (963 §2), would be lost. Every
  persisted boxd pool machine would also have to move.
- **The pool agent checks the capability too, at start**, as 481 §4 has it.
  That is two places that must agree. A disco-vm that holds the guarantee
  refuses an unfenced member at create, which is the backstop a second
  discobox check was.
- **The server keeps minting the members' token for the host's listener**
  (619 §2). That keeps an archil detail in the server. The driver, which
  makes the member and knows its host, mints the reach at the member's
  create with no server in the path.

## Open, for implementation

- The names of disco-vm's roles, its reach type and its capability field
  (§§1, 3, 4).
- How disco-vm records a pool's host so that the pool's engine knows it but
  cannot act on it. This includes, on boxd, adopting today's pool machine in
  the provider's scope (§1).
- How archil's driver mints the host-listener token at a member's create
  with the pool's issued key, and whether that key can (§3). If it cannot,
  the token is minted at the host's create and handed to the pool, still
  within §3's rule.
- Whether boxd's pool-network resolver answers names beyond the pool (§2.2).
  disco-vm's conformance case answers it. If it does, boxd's members close
  DNS too, and get the host's address in their reach.
- Whether hcs and libkrun can relay a vsock forward from a member to the host
  across their pools, as vz's shim does (disco-vm ADR 26-10-31-990).

## Consequences

- **disco-vm** (filed in discobox-ai/vm once this is accepted):
  - the host and member roles in a pool;
  - §2's guarantee per driver, with conformance cases;
  - the reach in a member's create and lookup, and in the host's;
  - §4's capability;
  - each driver's documentation states how it holds a pool;
  - a held driver's vsock forward is set by the driver.
- **server/providers/discovm**:
  - `fences` becomes §4's one check;
  - the pool machine is created as the pool's host, and an existing one is
    adopted;
  - `MachineToken` comes from disco-vm's host report;
  - the sandboxes' listener token is no longer minted and placed in the
    bootstrap.
- **pool-agent**:
  - the hop is built from the reach;
  - the runtime creates members with no network, strip or fence of its own;
  - `fencing.go` is removed.
- **sandbox-agent / proxy bridge**: unchanged in what they speak. They are
  given the reach in place of `RuntimeEdge`, which it replaces.
- **Docs**: `pool-agent/DESIGN.md`, `pool-agent/REVIEW.md`'s "A machine's
  ingress port", and `server/providers/discovm/DESIGN.md` describe the roles
  and the reach in place of the ways.
