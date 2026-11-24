# 26-11-24-684 — An exec attach's keepalive is a frame in its own stream

- **Status**: Proposed
- **Date**: 2026-11-24
- **Relates to**: #1695, #1408, #1680, #1268,
  [0008](0008-attach-stream-packages.md) (its Context's "the sandbox-agent's
  websocket bridge is a byte pipe that never decodes a frame" stops holding
  for an attach that negotiates this keepalive: the bridge reads frame
  headers, §3),
  [resume `DESIGN.md`](../../pkg/execstream/resume/DESIGN.md) (its heartbeat
  source, §6).

## Context

An exec attach (every `discobox exec`, terminal and console attach: all go
through `openSandboxExecAttach` in `cli/internal/cli/sandbox_execs.go`) is a
websocket from the CLI to the sandbox agent, carrying the framed stream of
`pkg/execstream/frame`. The control plane and pool agent forward the upgrade
as bytes. On the agent, `shimproxy.AttachWebSocket`
(`sandbox-agent/shimproxy/shimproxy.go`) copies raw bytes between the
websocket and the exec shim's socket; it never decodes a frame. Both ends keep
the websocket alive with the websocket's own ping and pong: the agent's
`keepalive`/`awaitPong` (as #1697 leaves them) and the CLI's
`websocketAttachFrames.keepalive`, each every 30s with a 10s timeout. The CLI's
timing heartbeat (`websocketAttachFrames.Probe`) is a websocket ping too.

coder/websocket v1.8.14 writes every control frame, a ping or a pong, under a
fixed five-second context (`writeControl`, `write.go:231`), and when a frame
write that has taken the connection's one writer outlives its context the
library closes the whole connection (`setupWriteTimeout`, `conn.go:171`). A
data write through `websocket.NetConn` has no such bound: it waits as long as
the peer takes to read. An automatic pong is written from inside `Read`
(`handleControl`, `read.go:316`).

#1697 (closing #1408) keeps the agent from pinging, or answering a ping
(`OnPingReceived`), while one of its own output writes is pending, and judges
a ping only while the attach stayed idle. Pending is all it can see. A
websocket write returns once the kernel has queued its bytes, so a client that
pauses right after an output burst can leave the send path full with nothing
pending. The next ping or pong then stalls, and the library closes the attach
five seconds later, taking the rest of the output and the exit status with it.
The triage on #1695 reproduces this at the head of #1697 with a test that
gates the agent's conn once the output frame is written; it fails 5 runs of 5,
and with a pause under five seconds `awaitPong` reports "keepalive ping
unanswered" instead, because a ping not yet written looks like one
unanswered. The CLI's end has the same shape behind its own blocked input
writes (#1680).

The root of it: a control frame interleaved with data cannot be abandoned
once its first byte is written, so a frame written into a stalled stream must
either wait like data or end the connection. coder/websocket chose to end it,
and gives no way to choose otherwise or to learn when a ping's frame has left.

The attach is a public API (`api/openapi/sandbox.yaml`, the exec `attach`
operation), and an old CLI fails on a frame type it does not know
(`pkg/execstream/client/session.go`: "unexpected frame type"). Any new frame
has to be asked for.

## Decision

### 1. The keepalive is two frames in the attach's own stream

`pkg/execstream/frame` gains two transport frames, `KeepalivePing` (15) and
`KeepalivePong` (16). A ping's payload is an 8-byte big-endian counter; a pong
echoes the payload of the ping it answers. They belong to the physical
connection between the CLI and the sandbox agent: neither ever reaches the
exec shim (`execstream/host`) or the attach session (`execstream/client`), and
the resumable stream (`execstream/resume`) neither positions, retains nor
replays them. Each end strips the frames it receives before anything above it
sees them.

Because they are data frames on the websocket, they queue behind output and
input exactly as data does. Nothing about them has a fixed deadline, so a
keepalive frame written into a full send path waits, as a pipe would, and
never ends the attach.

### 2. An attach asks for it with a websocket subprotocol

The CLI dials with the subprotocol `discobox-exec-keepalive`. The agent's
`websocket.Accept` offers it; when both agree, the attach uses §§1 and 3–5 and
neither end sends a websocket ping or answers one (`OnPingReceived` returns
false), so no control frame is ever written on that connection except the
closing one.

When the subprotocol is not agreed (a client that does not ask, an agent that
predates this, a hop that drops the header), both ends keep today's websocket
keepalive with #1697's guards. A mismatch falls back on its own, in both
directions, because each end reads the agreed subprotocol from the handshake
rather than assuming it.

### 3. The agent's bridge reads frame headers

On an agreed attach, `AttachWebSocket`'s two copies become frame-aware:

- **Output** (shim to client) copies frame by frame, the header and then the
  payload in bounded chunks, never buffering a whole frame (a payload may be
  16 MiB). Keepalive frames the agent sends go out through the same writer,
  only between frames.
- **Input** (client to shim) reads each frame's header. A `KeepalivePong` is
  consumed by the keepalive; a `KeepalivePing` is consumed and answered by
  queueing a `KeepalivePong` for the output writer. Every other frame is
  forwarded to the shim unchanged.

Answering never blocks the input copy: the pong is queued, and only the
newest unanswered ping is answered (an older one is superseded). A shim
running from an earlier agent release, still alive after the agent upgraded
in place, never sees a keepalive frame.

### 4. A ping is judged only once its frame has been written

Both ends follow one rule:

- A ping is sent every interval (30s, as today), whether or not the attach is
  busy: there is no idle guard, because a ping can no longer stall into a
  close.
- Its timeout (10s, as today) starts when the write of its frame returns,
  not when it was queued. A ping waiting behind output or input is not yet
  sent, never unanswered.
- The peer is taken for gone only if, for the whole timeout after the ping
  was written, a read was waiting on the connection and nothing at all
  arrived from the peer. Any frame arriving proves the peer alive (a pong
  comes after everything the peer sent before it, so data still arriving
  means it is on its way). A read that stopped because this end's consumer
  paused, or because the shim is not reading the input, counts no pong
  missing, as #1268 and #1697 already rule.
- A connection that closes, or a write that fails, ends the attach as today.

A peer that dies while a keepalive frame is queued behind a blocked write is
found when the transport fails that write, as a dead peer behind blocked
output is found today.

### 5. The CLI's end moves with it

`websocketAttachFrames` on an agreed attach runs §4 with its frames written
through `directAttachFrames`' writer, and answers the agent's pings the same
way §3 does, queued and never from inside `ReadFrame`'s blocking path. Its
pings and pongs then queue behind its own blocked input writes instead of
failing five seconds into them, which settles #1680 for every agreed attach.

### 6. The timing heartbeat uses the same frames

On an agreed attach `Probe` sends a `KeepalivePing` and waits for its pong.
Its round trip then includes any wait behind output or input already queued
on the connection, which the resume `DESIGN.md` heartbeat source and its
interpretation table must say. On a fallback attach it stays a websocket
ping.

### Open, for implementation

- Where the frame-aware copy and keepalive live: one transport in the root
  module that both ends use (so the rule in §4 is written once), or each in
  its end's package. The rule and the frames are this ADR's; the package is
  not.
- Whether the payload counter alone suffices, or a pong also carries the
  sender's monotonic send time for the heartbeat's round trip.

## Alternatives rejected

- **A websocket library, or a coder/websocket fork or upstream change, whose
  control writes take the caller's deadline and do not close.** A control
  frame that waits without a deadline is ordered behind data exactly as §1's
  frames are, so it buys the same queueing at the cost of owning a fork of
  the websocket library across the root, `cli` and `sandbox-agent` modules
  (and every other websocket in the repository that shares it), or waiting
  on an upstream change of a deliberate behaviour. It still would not tell
  the keepalive when a ping's frame left (`Conn.Ping` returns only on the
  pong), which §4 needs, and an automatic pong blocked inside `Read` would
  stall the input copy behind output. Rejected: more to own, and still short
  of §4.
- **Checking the socket's queued bytes against its send buffer before each
  ping or pong (`SIOCOUTQ`/`SO_SNDBUF`).** Linux only, while the agent also
  runs in macOS sandboxes; it has to reach the raw socket through a hijacked
  connection that may be TLS; the check and the write race the output copy;
  and a free kernel buffer says nothing about intermediate hops. It does not
  help the CLI's end. Rejected.
- **No keepalive on the attach; TCP keepalive or `TCP_USER_TIMEOUT`
  instead.** These cover one TCP hop, and the attach crosses the control
  plane's and pool agent's proxies, each its own connection. An idle attach
  would no longer keep NATs and proxy idle timeouts open, nor find a dead
  client end to end. Rejected.
- **In-band frames for every client, without negotiation.** An older CLI, or
  any other client of the public attach API, ends the attach on the first
  frame type it does not know. The resume handshake does accept such a break
  (its `DESIGN.md`, Wire Compatibility), but that break costs only a
  CLI/sandbox version skew; this one would cost every client that is not
  ours. Rejected in favour of §2.
- **Carrying the keepalive in the shim (the host answers pings).** The shim
  is the far end of the process, not of the connection: the frames would
  have to cross the agent's unix socket hop and the host's fan-out, a
  lingering shim from an older agent would not answer, and a shim not
  reading its socket would look like a dead client. The websocket ends at
  the agent, so the keepalive does too. Rejected.

## Consequences

- The agent's websocket bridge decodes frame headers on agreed attaches;
  the HTTP-upgrade attach (`AttachHTTPUpgrade`) and fallback websocket
  attaches stay byte pipes.
- Two keepalives exist side by side: the in-band one on agreed attaches, and
  the websocket ping with #1697's guards for every other client. The #1695
  window remains on fallback attaches only.
- #1680 is settled on agreed attaches by §5, with no separate CLI-side guard
  needed there.
- A keepalive round trip can now take as long as the queue ahead of it; a
  reader of the heartbeat must not read a long one as a slow network.
- `sandbox-agent/DESIGN.md`, `cli/DESIGN.md` (Attach Stream Pattern), the
  resume `DESIGN.md` and the root `REVIEW.md` websocket keepalive note change
  with the implementation.

## Deferred

Removing the websocket-ping fallback. Revisit when no supported CLI release
lacks the subprotocol and the public attach API's documentation has named
the in-band keepalive as the supported one for a release; until then a
client that does not ask keeps today's behaviour.
