# 26-11-17-182 — A vz VSOCK connection is always drained, and the guest sends only what the host has granted

- **Status**: Accepted
- **Date**: 2026-11-17
- Accepted by the owner by merging #1344, 2026-11-17
- **Amended**: 2026-11-18, §5, during implementation and before anything
  shipped: a guest service whose listen address is fixed in the image takes
  the framing from the kernel command line the driver renders,
  `discobox.vsock=yamux`.
- **Extends**: [ADR 0062](0062-macos-pools-run-vz-vms-with-an-independently-released-guest-image.md)'s
  transport ("VSOCK in both directions, terminated by the driver itself") with
  a rule for every connection on it. Nothing in 0062 is superseded.
- **Issue**: #1334 (part of #1330)

## Context

On a vz pool, a box that streams a large output from another box in the same
pool, through the Discobox API, stops the pool's VM. Guest CPU 0 stops, the
guest panics with `Hard LOCKUP`, and every box in the pool is down until the
server replaces the VM about four minutes later. It reproduced 2 of 2 on
v0.20.0-rc.1. The same stream run from the Mac delivers 200 MB in about 1 s.

**The cycle.** Virtualization.framework serves every VSOCK connection of a VM
from one serial dispatch queue (`com.apple.virtualization.virtio.vsock-device`).
It delivers guest data to the host with a blocking `writev` into the host end
of a Unix socket pair, whose receive buffer is 8 KiB by default
(`net.local.stream.recvspace`). The guest's CPU 0 thread waits synchronously on
that queue. In the hairpin:

1. the child's output arrives on a port-3002 connection;
2. `httputil`'s `copyFromBackend` writes it into the parent's port-3001
   connection;
3. the framework can read that write only once its queue is free;
4. the queue is blocked in `writev` on the 3002 connection, because the server
   has stopped reading it while its own write to 3001 waits.

Both the VM process sample (every sample of both lockups) and the server
goroutine dump (`VirtioSocketConnection.Write` under `copyFromBackend`) show
this. The same holds without a hairpin: any host consumer that stops reading
any guest stream, for longer than 8 KiB of the guest's output, blocks the queue
for every connection of the VM.

What `main` (`190d5647a`) has between the framework and its consumers:

- **Nothing.** The `discobox-ai/vz` binding duplicates the framework's
  descriptor and wraps it as a plain `net.Conn` (`socket.go:269-297` at
  `v3.7.2-0.20261114085742-69bde4a48a94`). `VirtioSocketConnection` keeps that
  conn unexported and implements no `syscall.Conn`, so nothing outside the
  binding can reach the socket.
- `vzvm.Connect` and `vzvm.Listen` (`server/providers/vz/internal/vzvm/vm_darwin.go:282-311`)
  hand those conns straight to their consumers:
  - port 3002 to the pool-agent proxy (`driver.go:477-489`);
  - 3003 to the lifecycle client (`:587`);
  - 3004 to the Docker client (`:463`);
  - 3001 into `carrierhub` (`serveControlPlane`, `:640`), whose `Push`
    blocks until the server accepts;
  - the outbound proxy ports to `pooloutbound.ServeRelay` (`:374`).
- The binding's listener queues one accepted connection
  (`make(chan connResults, 1)`, `socket.go:100`), so a connection the
  consumer has not yet accepted is not read by anyone.
- On vz, a 3001 connection is one HTTP/1.1 connection from the pool agent's
  plain `http.Transport` (`pkg/wire/wire.go:211`), not a multiplexed carrier.
  `cpmux` multiplexes wslc's channel, not vz's.

**Measured: a larger host buffer is not backpressure.** macos-worker patched
the binding to set `SO_RCVBUF` on every host connection and ran the hairpin
(A) and a stalled reader from the Mac (B) on v0.20.0-rc.1 (full report:
#1334, comment 6963495913):

| `SO_RCVBUF` | A: hairpin | B: stalled reader |
| --- | --- | --- |
| unset (8 KiB) | lockup | lockup after 98,304 B |
| 256 KiB | lockup | lockup |
| 1 MiB | lockup | lockup |
| 4 MiB | lockup | guest stalls ~30 s, recovers, stream cut at 98,304 B |
| 8 MiB (the kernel maximum) | lockup | not healthy; 98,304 B counted |

B counted 98,304 bytes in every run. At 1, 4 and 8 MiB alike, the Mac showed
the stalled connection's Unix socket at Recv-Q 8192, and `SO_SNDBUF` stayed
8192. Raising the host end's receive buffer does not enlarge what the socket
holds: the space is set on the framework's sending end, which the host does
not own.

The framework does not hold the guest to the host socket's free space: it
credits the guest more than the socket holds, about 8 KiB whatever the host
sets, then blocks in `writev`.
Because of that, the host cannot apply per-connection backpressure through
the framework at all. Any stop in reading, at any buffer size, eventually
blocks the shared queue. Flow control has to live above VSOCK, between the
guest's sender and the host's reader.

## Decision

### 1. Every vz VSOCK connection carries a flow-controlled stream

Both ends of every VSOCK connection on a vz pool speak yamux, the multiplexer
`pool-agent/cpmux` already uses, with one stream per connection. yamux gives
each stream a receive window: a sender may have at most that many unread
bytes outstanding, and the reader grants more only as its consumer reads. A
guest whose host consumer stops reading therefore blocks **in the guest**, in
its own `Write`, on that stream alone. The framework's queue never sees it.

This covers every port and both directions: 3001, 3002, 3003, 3004, and the
outbound relay ports. The connection model and the port map are unchanged:
the connection that carried a request or a stream still carries it, now
framed. Whether connections are pooled or multiplexed further is #1335's
question, not this ADR's (see Alternatives).

### 2. The host reads every connection, always, from the moment it exists

`vzvm` wraps a connection in its yamux session before `Connect` returns it.
On the listening side, `vzvm` runs its own accept loop that starts the session
as soon as the framework hands over the connection, and only then queues it
for the consumer's `Accept`. The session's receive loop reads the socket
continuously and copies each frame into that stream's buffer.

- It never waits on a stream's reader (`handleStreamMessage` →
  `readData`).
- It never waits on the consumer's `Accept`: a full accept backlog resets the
  stream, `incomingStream`.
- The only thing it can wait on is the session's own send queue, for at most
  `ConnectionWriteTimeout`, after which that one session closes.

So no consumer, however slow or stalled, can stop the host reading the
framework's socket.

Consumers receive the stream as a `net.Conn` and do not change. Closing the
stream closes the session and the connection.

### 3. The bound, and what a full buffer means

**The bound is one stream window, 256 KiB, per connection:** yamux's initial
window, which is also its minimum. Its `MaxStreamWindowSize` is set to the
same value, so the window never grows past it. Host memory for a stalled
connection is therefore at most 256 KiB in the stream's buffer, plus the
socket's 8 KiB. It never grows with the size of the stream.

At VSOCK's latency, 256 KiB is far above the bandwidth-delay product, so the
window costs no throughput. The delivering change confirms this on the
hairpin, which must still deliver 200 MB.

**A full buffer is the normal, safe state of a stalled stream.** The host has
granted nothing more, so the guest is blocked writing that stream, and the
host keeps reading the socket and finds nothing more for that stream.
Nothing is reset, and nothing waits on the framework.

**Data beyond the window is a protocol violation, and only that resets.**
yamux answers it with `ErrRecvWindowExceeded`, a GoAway, and the end of that
session. Because each session carries one connection, that is a reset of one
connection, never of the pool. A framed peer never does this. It happens only
with a peer that does not speak the framing (§5).

### 4. Nothing is asked of the socket buffer, and the binding is unchanged

The host socket holds about 8 KiB, and the host cannot raise it (Context).
yamux lets a sender put one full initial window, 256 KiB, on a new stream
before the receiver has granted anything, so the socket fills whenever the
guest sends faster than §2's session reads. That is safe for one reason: the
session waits on nothing but the CPU. A `writev` that finds the socket full
waits only until that goroutine next runs. It does not wait on a consumer,
on another connection, or on the queue itself, so it cannot close a cycle.
The deadlock in Context needed a reader waiting on the queue; §2 makes that
impossible, and is the whole guarantee.

So the host sets no `SO_RCVBUF`, and `discobox-ai/vz` needs no change: `vzvm`
wraps the `net.Conn` the binding already returns. The binding's one-slot
accept channel is drained by `vzvm`'s own accept loop (§2), never by a
consumer.

### 5. The framing is part of the guest compatibility surface, selected by the endpoint

The framing is selected by the endpoint the driver renders, not inferred. It
is a distinct `pkg/wire` scheme for a framed VSOCK endpoint, as the scheme
alone decides a transport today. libkrun keeps plain `vsock://` and is
unchanged. Its host side is libkrun's own forward, not this framework, and it
is out of scope here.

*Amended:* `discobox-vsock-guest`'s listeners have no endpoint to render:
their ports are fixed in the image's units, and libkrun boots the same image
plain. The driver renders their framing on the guest's kernel command line
instead: `discobox.vsock=yamux` frames them, and its absence leaves them
plain. vz's command line carries it; libkrun's does not. It is still the
host's statement, never a probe, and it joins the compatibility surface below.

Guest-side owners:

- The pool agent (3001, 3002) and the outbound relay
  (`discobox-cp-relay --outbound`) ship with discobox.
- `discobox-vsock-guest` (3003, 3004) ships in the guest image, on its own
  release line.

The VSOCK port map is already listed as that line's compatibility surface
(vz `DESIGN.md`, Release boundary). The framing joins it, so this is a
coordinated release:

1. a `vm/v*` guest whose `discobox-vsock-guest` speaks the framing;
2. then the server change, with `guestimage.DefaultVMImage` re-pinned to that
   guest in the same change.

A host that meets an unframed guest, such as an older `guestImageDir` or an
old local guest build, does not deadlock: §2 still reads everything, and the
first unframed bytes end that session as a protocol error. The failure names
a guest that does not speak the framing. There is no unframed fallback: it
would be the deadlock this ADR removes.

### 6. Tests

- **A Linux model test** of §§1–3, which every CI run executes. It uses a
  socket pair whose writer blocks when the reader's buffer is full, standing
  in for the framework's queue, and a hairpin through two framed connections
  with a stalled consumer. It asserts that the "queue" writer never blocks
  and that the hairpin delivers every byte.
- **`test:vz-stress` gains the hairpin**, on a real guest. One guest stream
  goes into a host consumer whose output returns into the guest on another
  connection, plus a stalled reader beside a second connection that must
  keep answering. This issue's Done when are its pass criteria.

## Consequences

- A stalled or slow reader stalls its own stream and nothing else. That
  covers the hairpin, a Mac client of `discobox exec` piped into a slow
  reader, a stalled outbound upstream, and a slow Docker client. The guest is
  told to wait, and no legitimate transfer is reset.
- Per connection, the host holds at most one window in the stream plus one
  socket's 8 KiB, about 264 KiB, whatever the stream's length. Connections in
  flight times that is the worst case.
- Every vz VSOCK byte pays a 12-byte frame header per frame and periodic
  window updates. That is negligible at these sizes.
- The guest image and the server move together once (§5). Development
  against an older guest fails loudly at the first framed connection.
- `vzvm`'s `Connect`/`Listen` return framed conns, and the driver's
  consumers are unchanged. `CloseWrite` stays unimplemented, as 0062's
  `DESIGN.md` requires. yamux's stream `Close` is a half-close, but exposing
  it is a separate decision.

## Alternatives

- **(a) Drain into a bounded buffer and reset the connection on overflow,
  with no framing.** This was the issue's Wanted, and it keeps the queue
  free. Rejected because it turns every slow reader into a failure. A
  hairpin whose inbound side outruns its outbound side overflows any fixed
  bound and is reset instead of delivered, contrary to #1334's Done when. It
  would also reset a guest's upload through a configured outbound proxy,
  such as a `docker push` or `git push`, whenever the uplink is slower than
  VSOCK. That is the usual case, not an edge.
- **(b) Drain into an unbounded buffer.** No resets, but one stalled reader
  grows the server without limit, which is the original stall moved into the
  server's memory.
- **(c) A bounded buffer that spills to disk.** Same unboundedness on disk,
  and a stalled reader of a large stream fills the user's disk.
- **A larger host `SO_RCVBUF`** (cloudnautique's fix 2 on #1330), alone or
  beside the framing. Measured above: up to the kernel maximum, the socket
  still holds 8 KiB, so it changes nothing.
- **One session per guest for all traffic, as wslc's `cpmux` channel.**
  Fewer connections and cancellable stream opens would also serve #1335 and
  #1336. Rejected here because it changes the connection model and the
  port map, a larger and separate decision that belongs to #1335. Per-stream
  windows would hold under it too, so this ADR does not foreclose it.
- **A purpose-built credit protocol instead of yamux.** It would be a smaller
  frame and less code on the wire, but it would be a second flow-control
  implementation to get right. yamux is already a dependency and already the
  framing of a pool channel (`cpmux`).
- **Policy in the binding** (the fork drains and buffers). Rejected because
  the fork stays a thin, upstreamable binding (vz `REVIEW.md`), and `vzvm`
  can do all of it on the `net.Conn` the binding already returns (§4).
