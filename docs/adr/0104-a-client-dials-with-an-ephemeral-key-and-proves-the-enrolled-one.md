# 0104 — A client dials with an ephemeral key and proves the enrolled one with a certificate

- **Status**: Accepted
- **Amends**: [0095](0095-an-enrolled-iroh-id-is-a-managed-resource.md) §2 in one respect — an enrolled ID remains the resource's identity, its primary key, and the value an operator adds and removes. It stops being the address a client dials *from*. Every other decision in 0095 stands, including the two layers, their ordering, and admission being built before the database.
- **Date**: 2026-09-10

## Context

Every `discobox` process on a machine loads the same iroh key. The path is
fixed per user — `~/.local/state/discobox/cli/iroh/id_ed25519` — with no
per-process component, so a TUI, a `discobox status`, and any other invocation
all present the same endpoint ID.

Two things exist exactly once per endpoint ID, and both are shared by those
processes:

- **The relay's routing slot.** `iroh-relay`'s server keeps one `active`
  connection per endpoint ID plus a list of displaced ones. A newer connection
  with the same ID takes the active slot; the displaced connection stays open
  and can still send, but stops receiving. If the active one goes away, the most
  recent displaced one is promoted back.
- **The pkarr/DNS record.** Each process publishes its own addresses under the
  shared ID, overwriting the others.

That is latent almost all of the time. While a client holds a direct path it
barely touches the relay, so the shared slot is uncontested and two processes
coexist for weeks without anyone noticing.

**A server restart is what exposes it.** The server comes back on a new
ephemeral UDP port, which kills every direct path its clients hold. A direct
path can only be re-established by hole punching, and hole punching is
coordinated over the relay — so every client falls back to the relay at the same
moment. They then take the active slot from each other mid-handshake, and a
handshake needs several round trips. Neither finishes. Existing clients cannot
reconnect and new ones cannot connect, because after a restart those are the
same operation: a fresh handshake over a contended slot.

The failure presents as "restarting the server breaks my clients", lasts as long
as any of them keeps retrying, and gets worse the more processes are running. It
reads as a NAT or discovery problem and is neither. Discovery resolves correctly
throughout; the same relay-only record is returned in the failing and the
succeeding case.

The controlled test is unambiguous. Same command, same server, same moment, only
a different identity:

```
XDG_STATE_HOME=<scratch> ./d status
  ✓ OK  connect  handshake with d1-etqbsvr  94ms
  ✗ FAILED admission  … is not authorized on this server
```

A 20-second timeout becomes a 94ms handshake, and the only failure left is the
expected one: a throwaway ID is not enrolled.

Upstream treats the pattern as at best undocumented. iroh issue #4464 asks n0 to
document whether a persistent endpoint ID may be reused, observing that
`RemoteStateActor` "keeps some shared state based on the EndpointId, so may mix
state between different connections", and relaying a Discord discussion that
reusing a persistent identity as an auth mechanism "will probably break Iroh
connections in unexpected ways". The issue is open and carries no maintainer
ruling; what it establishes is that the architecture assumes one endpoint ID is
one live endpoint, and that discobox is relying on behaviour nobody has
committed to.

The root cause is that one key does two jobs with opposite requirements:

| job | wants to be |
| --- | --- |
| the credential an operator enrolls | stable, machine-wide, long-lived |
| the address a live endpoint dials from | unique per running endpoint |

As long as they are the same key, concurrent CLI processes on one machine are in
a permanent race, and no amount of care at the transport layer removes it.

## Decision

### 1. A client's transport key is ephemeral and per process

Generated at startup, kept in memory, never written to disk, never enrolled,
never shown to an operator. Its public half is the endpoint ID the client dials
from, and it is meaningful to nobody: it is closer to a TCP source port than to
an identity.

Each process therefore has its own relay slot and its own discovery record, and
there is nothing left for concurrent processes to contend for.

**Servers keep a stable key.** A server's endpoint ID *is* its address — it is
what clients dial (ADR 0097) and what `GET /peer` serves (ADR 0098) — and the
data-directory singleton guarantees one server process per data directory, so a
server has no collision to solve. This decision is client-side.

### 2. The enrolled key signs a certificate binding it to the transport key

The client's enrolled key becomes a signing authority rather than an address. At
startup it signs a certificate naming the ephemeral key, and presents that
certificate when it connects.

The certificate is **not transferable**, and the reason is the whole design: it
names one specific ephemeral public key, and the QUIC handshake independently
proves that the peer holds that key's private half. Stealing a certificate buys
nothing without the ephemeral private key, which never leaves the process that
generated it.

This is the construction iroh itself uses to authorize endpoints to its
authenticated relays: a token that carries who issued it, **who it is for — the
public key of the endpoint presenting it** — what it grants and when it expires,
where presenting it from a different endpoint fails because "the handshake would
still have to prove ownership of that endpoint's secret key, which the token
alone does not give you". Adopting it puts discobox on the pattern upstream
supports instead of the one it warns about.

### 3. The certificate is a fixed 144-byte record

Normative. Fixed width rather than length-prefixed, so it is two reads and there
is no length to disagree about:

| field | bytes | meaning |
| --- | --- | --- |
| magic | 7 | `DBXPEER` |
| version | 1 | `0x01` |
| issuer | 32 | the enrolled public key |
| subject | 32 | the ephemeral public key this certificate is for |
| expiry | 8 | unix seconds, big endian |
| signature | 64 | ed25519 over `magic ‖ version ‖ issuer ‖ subject ‖ expiry` |

The magic and version are inside the signed input, which gives domain
separation: a signature produced for this purpose cannot be lifted into another
one.

`subject` is carried explicitly rather than implied by the connection, so the
server verifies the signature over exactly the bytes the client signed and
*then* compares. That keeps "this certificate is for a different endpoint"
distinguishable from "this signature is invalid" — two different operator
problems that would otherwise arrive as one refusal.

An expiry is carried even though a stolen certificate is already useless without
the ephemeral private key, because it bounds the damage from a process
compromised while running, and because iroh's own tokens carry one.

### 4. The ALPN says whether a certificate is coming

A server that understands certificates advertises a second ALPN,
`discobox/http/1+cert`, beside the one it serves today. A client presents a
certificate exactly when it negotiated that ALPN.

This is what ALPN is for, and it removes the guessing that would otherwise be
unavoidable: nothing in a connection tells you whether a stream is about to
carry a certificate or an HTTP request, and finding out by reading would consume
the first stream of a client that was never going to send one.

```
Authorize(conn):
    if conn.ALPN() is the certificate ALPN:
        stream := conn.AcceptConn(short deadline)
        read 8 bytes; refuse unless they are the magic and a version we know
        read the remaining 136
        refuse unless the signature verifies under issuer
        refuse unless subject == conn.RemoteID()
        refuse unless the certificate is unexpired
        claimed := issuer
    else:
        claimed := conn.RemoteID()      # today's path, no stream accepted
    return allowlistDecision(claimed)   # unchanged: file first, then the store
```

Backwards compatibility falls out of the negotiation rather than being bolted
on, and it runs in both directions. A client that dials the plain ALPN — every
client today — reaches the second branch, no stream is accepted, and its first
stream goes to the HTTP listener exactly as it does now. A new client meeting an
old server has its ALPN refused at the TLS layer, which is an unambiguous "this
server does not do certificates" rather than a misleading "not enrolled", and it
falls back to dialing with its enrolled key.

The allowlist decision is reached once, on whichever identity was actually
claimed, so a certificate-bearing peer is never first evaluated and logged as an
unenrolled one. `LoadAuthorizedIDs` first, then the managed store: ADR 0095 §3's
ordering and §4's wait, both unchanged. Refusal remains a close with a reason the
peer reads, so a bad certificate explains itself the way "not enrolled" already
does.

`endpoint.IrohConfig.Authorize` keeps its signature. The certificate is
transport machinery and lives in the `endpoint` package; what the server decides
is still only whether an identity may connect, so the server needs no change at
all.

### 5. What does not change

- `discobox admin peer add`, `discobox admin peer id`, `authorized_ids`, the
  `peers` resource, and the enrollment flow: all still operate on the stable
  per-machine key. It is now the issuer rather than the dialer.
- The principal an admitted peer receives: the server's default user with
  `ScopeAll`, exactly as ADR 0095 §3 states.
- Where admission happens: in the accept hook, before any stream reaches the
  HTTP router. Nothing unauthenticated reaches the handler surface, which is the
  property ADR 0052 §5 exists to protect.
- Revocation: checked per connection, so removing a peer takes effect on the
  next one.

## Consequences

The collision is gone for any pair where both ends have this, and concurrent CLI
processes stop being a race. Migration needs no flag day in either direction: an
old client meets the allowlist branch and is unaffected, and a new client
meeting an old server is refused as unenrolled, which it can detect and retry
with its enrolled key as the transport key — collision-prone, but working. The
corollary is that the fix only takes effect once both ends are new.

Against that:

- **The hook now does I/O on the unproven path.** One stream accept and one read
  of bytes the client sends unprompted, so it costs a message rather than a round
  trip — but it must be bounded, at a second or two. The gate already parks a
  connection waiting for the store (ADR 0095 §4), so a blocking hook is not new;
  waiting on a *remote party* is, and it deserves a much tighter bound.
- **An unenrolled peer can hold a connection for that deadline**, where today it
  is refused on an in-memory lookup. A small, deliberate widening of what an
  unauthorized party can cost.
- **One comparison carries the design.** If `subject == conn.RemoteID()` is ever
  dropped, certificates become transferable and the scheme fails open. It is the
  line to review, and the one a test must pin.
- **A second credential format to keep compatible.** Version 1 is fixed width to
  keep that cheap.

## Alternatives rejected

**Ephemeral key with authorization above the handshake** — either a
challenge/response after connecting, or a signed credential in the HTTP
`Authorization` header alongside `PoolAuthenticator`. Rejected on three grounds.
`DefaultUserAuthenticator` is last in the authenticator chain and authenticates
*every* request as the default user with `ScopeAll`, so a missing or invalid
credential falls through and is admitted rather than refused: the check would
have to be made conditional on the transport, in a direction where a mistake
fails open. The binding would need the connection's ephemeral ID in the request
context, which means wiring `ConnContext` and `endpoint.IrohPeer` — a seam that
exists and has no callers. And it would fork the model: discobox gates on the
transport (file permissions for a socket, the allowlist for iroh) and lets the
HTTP layer trust it, and this would leave one transport authenticated above the
connection and the rest below it. A challenge/response additionally costs a real
round trip, which the certificate does not.

**One endpoint per machine — a local daemon that CLI processes multiplex
through.** The strongest alternative: it fixes the collision for the same reason,
changes nothing about the security model, and removes the identity reuse
upstream warns about rather than working within it. Rejected because it adds a
process lifecycle to own — starting, supervising, socket ownership, crash
recovery, cleanup — against a certificate that is one function and one 144-byte
record with no new moving parts. Revisit if the certificate path proves
insufficient, or if a second reason to want a per-machine daemon appears; then
its cost is shared rather than paid for this alone.

**Pin the server's UDP port and publish its direct addresses in discovery.**
This would let cold clients dial direct and stop needing the relay slot. It
treats the trigger rather than the cause: the slot stays shared and anything else
that forces clients onto the relay at once — a client restart, a NAT rebind, a
direct path idling out — brings the failure back, rarer and harder to recognise.
It is also not currently reachable. iroh publishes relay addresses only by
default, and the filter that changes it is documented for endpoints "reachable
via public IP addresses", which a LAN server is not; the knob is not exposed
through iroh-go at all; and a published record is stale precisely after a
restart, which is the moment it would be needed. Finally it would put private
addresses into a public DNS record.

**Document the limitation and leave it.** The failure is intermittent, presents
as an unrelated timeout, and misdiagnoses as a network problem — the reason this
took two evenings and two false theories to find. Upstream's own position is
that the pattern is unsupported.

## Deferred

- **Per-peer principals.** If enrolled peers ever need to differ in reach, the
  certificate is where an issuer-scoped grant would live. That decision belongs
  to whatever introduces the distinction; today ADR 0095 §3's "same principal"
  still holds.
- **0-RTT.** If iroh-go exposes early data, the certificate could travel with
  the handshake and the hook would need no I/O at all. Revisit then; the wire
  format above is unaffected.
