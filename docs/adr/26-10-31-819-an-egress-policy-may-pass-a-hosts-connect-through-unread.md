# 26-10-31-819 — An egress policy may pass a host's CONNECT through unread

- **Status**: Accepted
- **Date**: 2026-10-31
- **Narrows**:
  [26-10-01-032](26-10-01-032-a-sandboxs-egress-is-denied-when-any-of-its-levels-says-so.md)
  §§1, 2, 4 and 6 (a level gains a passthrough list, which a discobox may
  write only within its own, a box a discobox creates does not inherit, and
  the proxy enforces beside `allow`).
- **Relates to**:
  [26-10-27-840](26-10-27-840-a-binding-may-hand-over-the-raw-value-held-in-memory-and-watched-for-at-the-proxy.md)
  §4, whose watch for a raw value does not reach a passthrough tunnel;
  [0149](0149-a-host-certificate-is-trusted-for-one-sandbox-when-a-person-pins-it.md),
  whose pins do not apply to one.

## Context

The pool proxy MITMs every allowed `CONNECT`: it terminates the sandbox's TLS
with a per-host certificate from the MITM CA, reads each request, and sends it
on over a TLS connection of its own. That is what swaps sentinels, watches for
raw values, enforces a tunnel's authority (`421`), caches, and records bodies.

Some traffic must not be read, or cannot be:

- a client that pins its server's certificate or carries its own CA and
  cannot be pointed at the MITM CA (mobile and desktop SDKs, some update
  agents, a vendored gRPC client);
- a protocol inside the tunnel that is not HTTP, or not HTTP the proxy
  parses (a database or a message broker reached over an HTTPS proxy, a
  TLS-wrapped custom protocol);
- traffic whose content a person decides should stay end to end between the
  sandbox and the origin: a client-certificate login the proxy cannot
  present, or a service whose terms or data forbid interception.

SOCKS already carries bytes unread to any allowed host, but most programs in
a sandbox know only `HTTPS_PROXY`, and some ignore `ALL_PROXY`. There is no
way to say "this host's tunnel is not intercepted".

## Decision

**An egress level gains `passthrough`: hosts whose `CONNECT` the proxy
splices to the origin unread instead of intercepting. A person may set it
on any level. A discobox may set a box's own level, at create or later, only
to hosts its own passthrough covers. Every such tunnel is still audited with everything visible from
outside it. It decides only how an allowed tunnel is carried, never whether
it is allowed.**

### 1. Shape

```yaml
EgressPolicy:
  allow: [string]
  denyByDefault: bool
  passthrough: [string]   # "*.corp.example", "pinned.vendor.com", "10.2.0.0/16"
```

An entry is an `allow` entry (26-10-01-032 §1): a domain pattern, an IP, or a
CIDR, host only, validated when written by `egress.ValidateEntry`. The policy
is a JSON column on every level, so an absent field reads as empty and there
is nothing to migrate.

### 2. Merge: a union for a person's box, nothing inherited for a discobox's

```
person's box:   passthrough = project ∪ pool ∪ harness ∪ sandbox
discobox's box: passthrough = sandbox
```

A box a person creates carries the union of the four levels. A box a
discobox creates (`CreatedBySandboxID` set, immutable after create) inherits
none: its passthrough is its own level alone, empty unless the create asked
for hosts the creator holds (§5). Passthrough is never copied onto a box a
person did not decide it for.

The effective policy carries the list whether or not the sandbox is
restricted. A passthrough host is still held to `allow`: on a restricted
sandbox, a host passthrough names and `allow` does not is refused like any
other. `passthrough` never admits a host.

A harness image does not declare passthrough hosts. `egressHosts` are what an
agent needs to run, which a person need not weigh; giving up inspection is a
person's decision, made at a level.

### 3. What the proxy does with it

At `CONNECT`, in this order:

1. The gate host and the pool hosts are intercepted, as now. Passthrough
   never reaches the Discobox API or a pool service.
2. The filter refuses a host the sandbox may not reach, as now.
3. A host the effective `passthrough` matches, by the `CONNECT` authority's
   host as `allow` matches it, is accepted and spliced to `host:port`. It is
   dialed the way a tunnel is dialed today: by the destination dialer, which
   refuses the pool's own loopback, through `ConnectDial`, which honors an
   upstream proxy and its exemptions.
4. Everything else is intercepted, as now.

Only `CONNECT` changes. A plain `http://` request is read as before, whatever
its host; SOCKS was never read.

Inside a passthrough tunnel the proxy does nothing it does on a read one: no
sentinel swap, no raw-value watch, no `421` for a misdirected `Host`, no
upgrade of a plaintext request to TLS (ziglang/zig#19878), no cache, no body
spool, no host-trust pin. The sandbox's client verifies the origin against
its own trust store, which holds the public roots and the MITM CA.

### 4. Audit: every tunnel is recorded, with all the proxy can see

The proxy cannot read a passthrough tunnel, but it still records everything
it can see from outside, in the HTTP audit, on the row's `tunnel` record:

- **When the tunnel opens:** the client, `CONNECT` and its authority, the
  matching `passthrough` entry, the address dialed, and the upstream proxy
  if there was one. A dial that fails is recorded with its error and a
  `502`, and nothing is spliced.
- **From the client's first flight**, peeked before splicing and passed on
  unchanged: whether it is a TLS ClientHello, and if so its SNI, ALPN
  protocols, and offered versions. An SNI that names a host other than the
  authority is recorded as a mismatch. It is not refused: the bytes go to
  the authority whatever the SNI says, and policy judged the authority. The
  peek waits a few seconds at most, while the origin's bytes already flow to
  the client, so a protocol whose server speaks first is not held up.
- **When it closes:** the bytes each way, the duration, and which side
  closed, or the error that ended it.

A tunnel is two rows, sharing a tunnel ID: an **open** row, written once the
dial and the peek are done, and a **close** row, written when it ends. A
row is never updated after it is written, because audit followers read
forward by row ID (`after_id`) and would never see the update. Two rows
also mean a tunnel that is still open, or one cut short when the proxy
stopped, is on record by its open row. A failed dial is one row. The span
is `proxy.http.passthrough`, carrying none of the ClientHello's own text,
which only the audit row's redactor may keep.
A reader of the audit sees that the sandbox talked to the host, under which
name, how much, and for how long, and that nothing more was recorded.

### 5. A discobox may set passthrough only within its own

A person may set passthrough on any level, to anything: the project, pool,
and harness config through their `PATCH`, and a box through its config
`PATCH`.

A discobox may write one level: a box's own, when it creates the box or
later through that box's update (`PATCH` of the sandbox, which the sandbox
role reaches for this alone). Every entry it writes must be
covered by an entry of the **caller's** effective `passthrough`, by
`entryCovers`, read when the write is made. So a child created with `[]`
can later be given `[foo]` by a discobox that holds `foo`, and taken back
to `[]`, but never given a host its caller does not hold. A discobox holding
no passthrough can write only `[]`.

- **Which boxes.** The boxes a discobox may already act on: those it
  created (26-10-27-880 §9), whose effective policy its own covers
  (26-10-01-032 §4). A discobox is not its own creator, so it never sets
  its own. That coverage now includes `passthrough` (below), so a discobox
  can never write to a box that sends more unread than it does.
- **Only that field.** The sandbox role's update may carry `config.egress`
  alone, and its `allow` and `denyByDefault` must equal the stored level's.
  Every other part of the update stays refused to it, as today.
- **Independent of restriction.** An unrestricted discobox can reach
  everything, but the proxy reads all of it. A box with wider passthrough
  would carry, unread, what the caller's proxy would have watched for.
- **Checked when written.** A caller whose passthrough a person later
  narrows does not narrow what it already wrote, as 26-10-01-032 §4 checks
  a create only when it is made. The person narrowing it can narrow those
  boxes too.

A box a discobox creates inherits no level (§2), so a discobox cannot gain
passthrough by picking a pool or harness config whose level a person set.

Acting on an existing box in any way (26-10-01-032 §4) also requires the
target's effective `passthrough` to be covered by the caller's, by the same
rule. Otherwise a discobox could type into a sibling with wider passthrough
and send traffic through it unread.

### 6. Delivery, and older pools

The effective policy is delivered as today (26-10-01-032 §5): on the create,
then pushed on every change. Nothing new reaches the pool but the field.

Passthrough is sent to the pool only when it is not empty, as raw secrets
are (26-10-27-840), because a pool agent older than this refuses a policy
with a field it does not know. A sandbox with none is pushed exactly what it
was before. A create or push that carries passthrough to an older pool is refused and
retried until that pool moves onto the current agent, which it does when it
is next reconciled. Meanwhile the pool keeps the policy it last took, and
intercepts every tunnel. That fails toward inspection: nothing passes unread
until a pool that understands passthrough has been told it.

### 7. Sentinels and raw values on a passthrough host

- A sentinel bound to a passthrough host is never swapped. The origin
  receives the sentinel and refuses it. Nothing is leaked by that — a
  sentinel is a handle the pool alone resolves — but the binding does not
  work, and nothing refuses the write that causes it, because a binding's
  routes and the levels that name passthrough hosts are written separately.
  The egress and binding documentation say it.
- A raw value sent to a passthrough host is not watched for (26-10-27-840
  §4). That ADR already states the watch is a guard against mistakes, not a
  boundary, and that an unread connection defeats it; SOCKS is one today.
  Passthrough adds a channel a person chose, on hosts they named.

## Alternatives rejected

- **Passthrough admits the host.** One list would be shorter to write, but a
  level that names a passthrough host would then open a restricted sandbox
  to it, so a harness config's passthrough entry would widen a project's
  `denyByDefault`. 26-10-01-032 keeps restriction and the hosts it admits
  apart for the same reason; this keeps how a host is carried apart from
  both.
- **A discobox's box inherits the levels, as for `allow`.** Then a
  passthrough a person set on the project for the boxes they run would reach
  every box an agent spins up, and a discobox could gain hosts by picking a
  pool or harness config. `allow` widens what is reachable but leaves it
  read; passthrough gives up reading, and only a person's decision for a box
  they made, or a subset handed down, should do that.
- **Refuse the sandbox role any passthrough at all.** Handing on a subset
  gives nothing away that the caller does not already send unread, and
  refusing it would make a box whose client needs passthrough unable to
  delegate work to a child that runs the same client.
- **A discobox sets passthrough only at create.** A box created with `[]`
  that turns out to need a host its parent holds would have to be recreated,
  or wait for a person, when the parent could hand it on with nothing given
  away. Covering the write by the caller's own is what keeps it safe, and
  that holds at create and after.
- **Refuse a tunnel whose SNI names another host.** The bytes go to the
  authority whatever the SNI says, and the authority is what policy judged.
  Refusing would break clients that legitimately send a different SNI. The
  mismatch is recorded.
- **A per-sandbox resource an agent asks for**, approved by a person, as a
  host trust is (ADR 0149). The cases that need passthrough are properties
  of a client or a host, known when a project is set up, not discovered one
  request at a time; and a request an agent writes would ask a person to
  stop the proxy reading the agent's own traffic on the agent's word. If
  agents prove to find these hosts mid-task, a request that writes the
  sandbox level after approval can be added without changing this shape.
- **Ports in entries** (`host:port`). Egress entries are host only, and one
  vocabulary across `allow` and `passthrough` keeps `entryCovers` the one
  coverage rule. A host whose one port must be read and another not is
  unusual enough to wait for.
- **Refuse passthrough on a sandbox holding a raw binding.** It would not
  close what SOCKS already leaves open, and it would make a raw binding
  silently break a host's client elsewhere in the sandbox.
- **Fall back to passthrough when interception fails** (the client refuses
  the MITM certificate). The proxy would decide, on a failure it cannot tell
  apart from an attack, to stop reading traffic. It stays a person's choice.
- **A pool-wide list in the proxy's config.** A pool serves many sandboxes,
  and a project's operator, not only a pool's, knows its pinned clients; the
  egress levels are already where both say what a sandbox's traffic may do.

## Consequences

A person can name hosts whose TLS stays end to end between the sandbox and
the origin, and certificate-pinning clients and non-HTTP protocols work
through `HTTPS_PROXY`. On those hosts the pool proxy keeps only what it sees
from outside: who connected where, under which SNI and ALPN, how much, and
how long. Credentials there must be raw or the
sandbox's own, a raw value there is not watched for, and the audit has no
bodies. The egress policy grows a third field across the API, CLI, console
and the create and act-on checks, and the merge learns whether a box was
created by a discobox. The proxy change is confined to the `CONNECT` handler,
a ClientHello peek, and the audit row.
