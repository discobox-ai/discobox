# 26-10-23-719 — A sandbox may reach its pool through a WebSocket tunnel, with its mTLS inside

- **Status**: Accepted
- **Date**: 2026-10-23
- **Relates to**:
  [0148](0148-a-sandbox-resolves-names-through-its-pool-over-mtls.md)
  (sandbox DNS over mTLS, whose stub moves onto `bridge.Dialer` in §5),
  [0126](0126-a-sandbox-does-not-share-a-host-or-a-filesystem-with-its-pool.md)
  §7 (client certificates and their renewal),
  [0144](0144-a-pool-of-host-vm-sandboxes-runs-its-agent-on-the-host.md) §4
  (the URL's scheme picks the transport; the mTLS on top never changes).

## Context

A sandbox reaches every pool service — the egress proxy, the credentials
endpoint, the BuildKit mediator, and the DNS server — over mTLS, and the client
certificate's common name is its identity at the pool. `bridge.Dialer` opens
the transport its URL's scheme names (`https`, `vsock`, `unix`) and runs the
handshake on it, and every pool service reads the identity from the verified
peer certificate.

Some providers reach a pool only through an HTTPS edge that terminates TLS: a
managed load balancer or a platform's ingress. Such an edge never forwards the
sandbox's client certificate, so the pool cannot authenticate the sandbox. It
also forwards HTTP requests, not byte streams. The egress forwarder splices
`CONNECT`, absolute-form HTTP, and SOCKS5 onto one connection and the pool
chooses between them by the first byte; an edge passes none of that through,
and the DNS-over-TLS framing is not HTTP at all. So authentication is not the
only problem: the connection itself has to take a shape the edge forwards.

What every such edge does forward is a WebSocket upgrade.

## Decision

### 1. `wss` is a transport, and mTLS runs inside it

`pkg/wire` gains the scheme `wss://host[:port]/path`. It parses it, and leaves
opening it to `bridge.Dialer`, the one thing that knows where a stream on it
ends (§3): `wire` refuses to dial or listen on it. The Dialer opens a WebSocket
to that URL over the edge's TLS, verified against the system roots, through
`pkg/wstunnel`, and runs the same mTLS handshake on it that it runs on TCP,
vsock, or a unix socket: the pool's
server certificate verified against the mTLS CA as `serverName`, and the
sandbox's client certificate presented. Material renewal (0126 §7) is
unchanged.

The edge sees the outer TLS terminate and then ciphertext. Identity remains the
verified client certificate, end to end. No pool service learns which transport
carried a connection.

### 2. A tunnel carries one stream at a time, and says when one ends

Bytes travel in binary WebSocket messages. A stream ends with a text control
message rather than with the WebSocket's close:

- The side that closes its stream sends `end`. The peer reads it as EOF.
- Once the peer has closed its own end, it replies `end-ack`. Both sides then
  know the stream is fully drained and the tunnel is idle.
- A read error, a write error, a deadline, a WebSocket close, or a message the
  protocol does not allow closes the whole tunnel, never returning it to idle.

A tunnel never carries two streams at once. Concurrent streams take concurrent
tunnels, each with its own TCP connection and congestion window.

### 3. The dialer keeps idle tunnels and reuses them

Each `wss` Dialer keeps the tunnels whose streams ended cleanly, and a dial
takes an idle one before opening another. The limits are:

- a cap on idle tunnels per Dialer;
- an idle timeout, kept shorter than the edge's own idle timeout;
- a maximum age, kept shorter than the edge's longest request, after which a
  tunnel is retired at its next end of stream instead of going idle;
- WebSocket pings while a tunnel is in use, so a quiet stream is not cut as
  idle;
- a bound on what ending a stream may wait for: the write of its end, and the
  peer's answer, past which the tunnel is given up rather than held.

A reused tunnel the edge cut without either side noticing fails during the
inner TLS handshake, before any of the caller's bytes are sent. The dial is
then retried once on a new tunnel, as the DNS stub already retries a stale
connection it reused.

A stream on a reused tunnel costs the inner handshake alone: about one round
trip to the pool, against about two for direct mTLS today and for a new tunnel.

### 4. The pool serves tunnels on a plain HTTP listener

The pool's proxy unit serves a listener for the edge to forward to, with one
path per service. A handler accepts the upgrade and splices each stream onto
that service's own mTLS listener on the pool container's loopback, so the
inner mTLS ends exactly where it does over TCP. Each tunnel loops, accepting
one stream after another, until it closes.

Splicing rather than handing each stream to the service's accept loop in
process keeps the services as they are, at the cost of one loopback hop. The
splice comes from a loopback address of the tunnel listener's own, and the DNS
server exempts that address, and only it, from its per-source cap: every
sandbox behind the edge arrives from it. All of loopback cannot be exempted,
because the pool proxy carries any sandbox's connection to a loopback port.

Nothing names a caller until the mTLS inside a stream completes, and every
tunnel arrives from the edge, so the pool cannot limit each client: that is the
edge's, which must limit upgrades per client. The listener bounds the tunnels
it holds in total, sized from the descriptors they cost rather than set low
enough to bite in use; a new tunnel must begin its first stream within
seconds, so holding one costs a request; and the proxy's handshake is bounded
in time.

The listener is plain HTTP because the edge holds the public certificate and
presents no client certificate of its own; the inner mTLS is what authenticates
and protects the stream. An idle tunnel waits in that loop, not in the egress
proxy's first-byte detection, so the detector's five-second deadline does not
apply to it.

### 5. Every pool endpoint can be a `wss` URL

`PoolEndpoints.Proxy`, `Credentials`, and `DNS` may each name a `wss` URL.
`BuildKit` does not: a build's results come back through the pool registry,
which is unauthenticated and protected only by the pool's private network
(ADR 0126 §7), so no tunnel carries it, and a pool behind an edge names no
builder. `DNS` becomes a wire URL like the others, where it was a bare
`host:port`, and a bare `host:port` keeps meaning TCP so existing bootstraps
still resolve. The DNS stub dials through `bridge.Dialer` like everything else,
retiring its exception in `proxy/REVIEW.md`, and keeps its own connection reuse
above it.

### 6. The pool chooses, and direct mTLS stays the default

A pool states the `wss` URLs in its sandboxes' bootstrap only when it is
reachable solely through such an edge: when its container's environment names
the edge's URL. Every other pool keeps the transports it uses today. Nothing in
the sandbox probes or falls back between them.

No provider sets that URL yet. Every provider today runs a pool's sandboxes
beside it, on its own network; the one that places them behind an edge sets
the URL per pool and exposes the tunnel listener to the edge.

## Alternatives rejected

- **A signed header asserting the client certificate.** The bridge would send
  its certificate and a short-lived token signed with its key on the upgrade
  request, and the pool would verify both. It cannot be bound to the
  connection that carries it, because the edge terminates that connection's
  TLS. So the edge reads the header and can replay it until it expires. A
  single-use token ID does not help: a hostile edge withholds the original
  request and sends its own. For that window the edge is the sandbox at the
  pool, with its approved credentials swapped into whatever it sends, where
  with mTLS inside it sees only ciphertext. It would also end the rule that
  client identity is never taken from a header.
- **Multiplexing streams over one tunnel** (yamux, HTTP/2 streams). It saves a
  handshake per concurrent stream, but every stream then shares one TCP
  congestion window: a lost packet stalls them all, and one bulk download
  slows every other request the sandbox makes. Sequential reuse (§3) recovers
  the handshake for the common case without that coupling.
- **A tunnel per stream with no reuse.** Simpler, but every connection pays
  about two round trips to the pool plus the edge's handshakes. The end-of-stream
  messages (§2) are all reuse needs.
- **Trusting a client-certificate header set by the edge** (`X-Forwarded-Client-Cert`
  and similar). It needs an edge that requests and forwards client certificates,
  which the edges this is for do not offer. Even where one does, the pool would
  have to trust the edge with the sandbox's identity.
- **A custom `Upgrade` token, or `CONNECT` through the edge.** Edges forward
  WebSocket upgrades reliably and other upgrades or `CONNECT` rarely, if at all.
- **HTTP/2 extended `CONNECT` (RFC 8441) as the tunnel.** It runs over a
  shared HTTP/2 connection, so it brings back the coupling of multiplexing, and
  edge support is uneven.

## Consequences

- A pool behind a TLS-terminating edge works with no change to what identifies
  a sandbox, and the edge cannot read or impersonate its traffic.
- Bytes are encrypted twice over a tunnel, once to the edge and once to the pool.
  The per-byte cost is WebSocket framing, client masking, and a second AES-GCM
  pass: under 1% of bulk throughput.
- Each concurrent stream holds its own tunnel, so an edge's limit on concurrent
  connections, and any per-connection pricing, applies per sandbox stream. A
  parallel package install can open hundreds.
- An edge that caps how long one request may last cuts any stream longer than
  that, whatever pings do. The stream's client sees a reset, as it would if the
  pool restarted. Long-lived streams such as event streams and upgraded
  WebSockets must reconnect over such a pool. Direct mTLS has no such cap,
  which is one reason it stays the default.
- The DNS server address in `PoolEndpoints` becomes a wire URL.
- A pool behind an edge is as available as the edge keeps it: anyone who
  reaches the edge can hold tunnels open up to the listener's total, which
  only the edge can apportion per client. An edge that does not limit upgrades
  per client leaves the pool's sandboxes open to that.

## Deferred

- **Pre-dialing tunnels**, which would also cover the first stream's setup cost,
  is deferred until measurements on a real edge show the setup cost matters
  with reuse in place. It needs either the inner handshake done ahead or a
  change to the egress proxy's first-byte deadline.
- **Provider wiring** — which provider sets a pool's tunnel URL, and how the
  edge reaches the listener — waits for the first provider whose sandboxes
  reach their pool only through an edge.
- **Builds behind an edge** wait for the pool registry to authenticate its
  pulls (ADR 0126 §7), at which point it can take a tunnel of its own.
- **Per-pool tuning of the tunnel limits** (§3) is deferred until a second edge
  needs values the first does not. Until then they are constants chosen for the
  first provider this ships for.
