# 0109 — A bound UDP port is listed, and forwarded as datagrams

- **Status**: Proposed
- **Date**: 2026-09-11
- **Relates to**: [ADR 0046](0046-listening-ports-are-polled-and-probed-in-the-background.md),
  whose deferred "Unix domain sockets and UDP" this settles for UDP;
  [ADR 0049](0049-forwarded-ports-are-bound-near-their-number-and-held.md),
  whose forwarder this extends; and
  [ADR 0094](0094-an-image-declares-services-in-the-format-a-repository-does.md),
  whose stated `protocol:` gains a value.

## Context

ADR 0046 reports only listening TCP, and defers UDP until "a concrete
forwarding case needs" it, noting that nothing could forward it through
ADR 0024's TCP ingress. The cases are ordinary development work: a DNS server
under test, a game or voice server, a QUIC/HTTP-3 endpoint beside its TCP
twin, a syslog or StatsD collector, mDNS. Each binds a UDP port a person wants
to reach from their own machine, and today the sandbox neither mentions it nor
has a way to carry it.

UDP differs from TCP in the three places the existing pipeline depends on:

- **There is no listen state.** A server's socket and a client's are the same
  kind of socket. `/proc/net/udp{,6}` lists both, with state `07` for an
  unconnected socket and `01` for one that called `connect(2)`. An unconnected
  client — a QUIC client, a resolver that uses `sendto`, a WebRTC stack — sits
  on an ephemeral port in state `07`, indistinguishable from a server except by
  its number.
- **There is nothing to probe.** No handshake, no banner, no error for a
  question the service does not understand: silence is the answer to almost
  everything, and a datagram a service does act on is one it acts on for real.
- **There are no connections to forward.** A forward has to invent them, and
  decide when one has ended.

## Decision

### 1. Discovery reads the UDP tables, and skips the ephemeral range

The watcher reads `/proc/net/udp{,6}` beside the TCP tables, keeps sockets in
state `07` owned by the same uid ADR 0046 filters on, and drops any whose port
lies inside the kernel's ephemeral range (`/proc/sys/net/ipv4/ip_local_port_range`,
read per scan from the sandbox's own network namespace, `32768 60999` when it
cannot be read). A port outside that range was bound on purpose; one inside it
is almost always a client the kernel gave a number to.

A UDP port and a TCP port with the same number are two ports. The watcher's
state, the snapshot, and every consumer key a port by its number *and* its
transport — including the exclusion of the agent's own listener, which is a TCP
port and hides no UDP socket of the same number.

### 2. A UDP port is reported as `udp`, and never probed

Its protocol is `udp` from the tick it appears, so it is never queued for the
probe. What it carries is not classifiable without sending a datagram a live
service would act on, and the answer would usually be silence.

A declaration may state `protocol: udp` (ADR 0094). That is how a UDP server
discovery cannot see reaches the listing: one root holds, or one that bound a
number inside the ephemeral range. A declaration that states nothing is still a
TCP port, probed as before. A declaration names one transport for all its
ports, so a service serving both takes a second declaration, with
`start: never`, for its UDP side.

### 3. `udp` is a protocol value, not a new field

`SandboxAgentListeningPort.protocol` gains `udp`. The transport is a function
of the protocol — `udp` is UDP, everything else is TCP — and the pair
`(port, protocol is udp)` identifies a listed port.

The snapshot lists a number's UDP entry before its TCP one. A client older
than this keys a port by number alone and keeps the last entry it reads for
it, so this order leaves it the TCP entry — the one it can forward, with the
bind address it needs to dial.

### 4. The tunnel is a sibling route, one datagram per frame

`GET .../udp/attach?host=&port=` exists at the control-plane edge, the pool
agent, and the sandbox-agent, beside `tcp/attach` and gated by a new
`udp:connect` scope. The sandbox-agent opens a *connected* UDP socket to
`host:port` from inside the sandbox's network namespace, so only the target's
replies come back, and bridges it to `execstream/frame`: each `Input` frame is
one datagram sent, each `Stdout` frame one datagram received. A refused
datagram (ICMP port unreachable, surfaced as `ECONNREFUSED` on the next read)
does not end the tunnel — a UDP server that is restarting is the normal case
for it. The tunnel ends when the websocket does.

### 5. The forwarder invents flows, one tunnel each, ended by idleness

`portforward` binds a UDP port by the same nearest-number rule as a TCP one,
independently of it — the two port spaces are separate, so a sandbox's
`tcp/53` and `udp/53` can both be `localhost:53`-adjacent at once. Each local
peer address that sends a datagram is a flow with its own tunnel, and so its
own source port inside the sandbox, which is what lets a server there tell
clients apart and what routes each reply back to the peer that caused it. A
flow ends after 60 seconds with no datagram in either direction — longer than
QUIC's common 30-second idle timeout, so a quiet QUIC connection dies of its
own timer rather than of ours. A flow leaves its binding's table before its
tunnel is torn down, and an idle one never while a datagram is queued for it,
so the datagram that arrives as a flow goes quiet starts the next one rather
than being lost. A flow whose tunnel breaks or cannot be opened drops what was
queued for it, which is a loss UDP already allows for.

A UDP binding on loopback listens on both loopbacks, at the same port. A UDP
client of `localhost` sends to whichever address resolves first — `::1` on
macOS — and hears nothing if the binding is on the other, since nothing is
refused in a way it notices. A TCP client falls back by itself, so TCP
bindings stay single.

Bindings are sticky exactly as TCP ones are (ADR 0049 §3). A UDP target is
dialed at a literal loopback address rather than `localhost`: a TCP dial of a
name tries both families and keeps whichever connects, and a UDP dial has no
connect to fail, so the name would pin whichever family resolved first.

`discobox proxy --port` accepts `5353/udp` (and `8080/tcp`, the default).

## Alternatives rejected

**A separate `transport` field on the listing.** The structurally obvious
shape. Rejected because `SandboxAgentListeningPort` is
`additionalProperties: false`, and the generated decoder every existing CLI
carries rejects an object with a field it does not know — so a sandbox-agent
reporting one would erase *every* port from an older client's listing, TCP
included, not just the UDP ones it cannot use. An enum value it does not know
is decoded as-is. The CLI already reads `protocol` to decide what a port is;
`tcp` is already a transport-level answer; `udp` is its twin.

**Listing every unconnected UDP socket.** The ephemeral-range filter is a
heuristic, and it misses a server that binds a number inside the range.
Rejected anyway, because without it every unconnected client socket in the
sandbox is listed and forwarded — one per QUIC connection an agent's tooling
opens — and a listing that is mostly noise stops being read. The miss has a
remedy (a declaration); the noise would not.

**Probing UDP ports.** Rejected as having no safe question to ask. A service
either ignores a datagram it does not understand, which classifies nothing, or
acts on it, which is a probe with side effects.

**One tunnel per binding, with peer addresses inside the frames.** Fewer
websockets. Rejected because the sandbox side would still need a socket per
peer — sharing one source port would merge every local client into one in the
server's eyes, and leave no way to route a reply — so it adds a multiplexing
protocol to save websockets while keeping the sockets. TCP already spends one
websocket per connection.

**`tcp/attach?network=udp`.** One route instead of two. Rejected because a
sandbox-agent that predates it ignores the parameter and dials TCP, which fails
confusingly or, worse, reaches a TCP service on the same number. A new route
answers 404 on an old agent.

**Reusing `tcp:connect`.** The authorization boundary is the same (whoever may
exec in the sandbox can reach its services either way), so this was close.
Rejected because a scope names what it grants, and the cost of a second one is
three constants.

## Consequences

- A flow quieter than 60 seconds is re-dialed on its next datagram with a new
  source port inside the sandbox. A UDP protocol keyed on the client's address
  (a QUIC connection with no keepalive, a WireGuard session) sees a new client.
- Every datagram crosses a websocket through the control plane and the pool
  agent, like every forwarded TCP byte: a developer path, not a media path.
  Latency is the control plane's, and loss is whatever a websocket over TCP
  makes of it — none, at the cost of head-of-line blocking.
- A UDP server in the ephemeral range is not discovered. Declare it.
- An older CLI sees `udp` ports, draws them under their own group, and forwards
  them as TCP; each attempt fails inside the sandbox and is reported. Where a
  number is served on both transports it keeps the TCP entry (§3's order), so
  a TCP forward that worked before still does.
- SSH is unchanged: `direct-tcpip` has no UDP counterpart.

## Deferred

- **Unix domain sockets** remain deferred on ADR 0046's terms.
- **A per-binding flow cap.** A local sender cycling source ports opens a
  tunnel per port. TCP has the same shape — a websocket per accepted
  connection, uncapped — and binds loopback by default; revisit both together
  if `--address` beyond loopback turns out to be used against untrusted
  networks.
