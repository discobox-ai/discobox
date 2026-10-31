# 26-10-31-152 — The control-plane channel is a WebSocket

- **Status**: Accepted, by the issue owner in #408 (narrows
  [26-10-30-875](26-10-30-875-a-remote-pool-reaches-its-control-plane-over-a-channel-the-server-opens.md)
  §1's HTTP upgrade to a binary WebSocket)
- **Date**: 2026-10-31
- **Relates to**:
  [26-10-23-719](26-10-23-719-a-sandbox-may-reach-its-pool-through-a-websocket-tunnel-with-its-mtls-inside.md)
  (a sandbox's WebSocket tunnel, with its mTLS inside)
- **Issue**: #408

## Context

ADR 26-10-30-875 §1 has the server open the control-plane channel to a remote
pool's agent as an HTTP upgrade on the agent's API, then run TLS 1.3 pinned
both ways, and a `cpmux` session inside it. The upgrade was implemented as
`Upgrade: discobox-control-plane/1`: a bare byte stream after a `101`.

A boxd pool against the development server never registered with it. The
agent's front sits behind boxd's hosted HTTPS edge; the upgrade answered, and
the inner TLS handshake then saw EOF at both ends. Hosted HTTP reverse proxies
document WebSocket forwarding; forwarding an upgrade to an arbitrary protocol
is not something they promise, and an edge may refuse it, or answer and not
carry the bytes after it. The ADR decided only "an HTTP upgrade on the
agent's API"; the protocol of the channel's own was the implementation's, and
nothing recorded a reason for it over a WebSocket, which is an HTTP upgrade on
the same route.

## Decision

The channel is a binary WebSocket on the same route
(`/api/project/{p}/pool/{p}/control-plane-channel`), with subprotocol
`discobox-control-plane.v1`, which both ends require. Its binary messages
carry the bytes of the channel's TLS; the TLS, its pinning to the pool's
enrolled key and the server's issuer key, and the `cpmux` session inside it
are 26-10-30-875 §1's, unchanged. So are the signed request and its scope, the
front that serves the route before the agent's first report, the redial, and
the carrier hub's hold on a pool's streams (§§2–3).

The handshake is asked for over HTTP/1.1 alone, as the upgrade was: a
WebSocket opens with an HTTP/1.1 `Upgrade`.

## Alternatives rejected

**Keep the custom upgrade.** It needs nothing a WebSocket does not, and a
front is only promised to carry WebSockets. Rejected because the channel's
whole point is a pool behind a front the server does not control.

**Carry the channel on `pkg/wstunnel`.** It already puts a byte stream on a
WebSocket for a sandbox's tunnel to its pool (ADR 26-10-23-719). Rejected for
what it adds: its `end`/`end-ack` messages and idle-tunnel reuse exist so short
streams can share long-lived WebSockets, and the channel is one connection for
the pool's life with its own multiplexing inside. It reuses the same library
(`coder/websocket`) and the same shape — the edge's transport outside, the two
ends' own TLS inside — without that protocol.

**Drop the inner TLS and trust the edge's.** A WebSocket through an HTTPS
edge is encrypted to the edge; it is not end to end, and a droplet's agent
port is plain HTTP. 26-10-30-875 §1's reasons for the inner TLS stand.

## Consequences

- Any front that forwards WebSockets carries the channel, boxd's edge
  included. A test reverse proxy that forwards WebSockets and refuses other
  upgrades proves it (`TestTheChannelOpensThroughAnEdgeThatForwardsOnlyWebSockets`).
- Both ends must speak the WebSocket channel: a pool agent from before this
  change refuses the new dial with 426, and the server's redial succeeds once
  the pool runs an agent from the same release.
