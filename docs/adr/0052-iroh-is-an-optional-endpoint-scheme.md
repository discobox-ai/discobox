# 0052 — iroh is an optional endpoint scheme, and each hop names its endpoint package

- **Status**: Accepted
- **Date**: 2026-08-19

## Context

A Discobox server is reachable from wherever it is listening. In practice that
means a unix socket on the user's own machine, because that is the only endpoint
`disco` configures for itself (`localServerEnv` deliberately refuses to open a
TCP port). Reaching a server on another machine today means the operator binds
`DISCOBOX_SERVER_LISTEN` to a TCP address and then owns everything that follows:
a routable address, a firewall hole or a tunnel, TLS, and a name.

None of that is work Discobox helps with, and the last item is the one that
does not go away. A control plane running on a laptop behind NAT, in a cloud
VM, or in a WSL guest has no stable address to publish, so "run the server
there, use it from here" is not a thing a user can currently do.

The transport that would fix this already has a seam waiting for it.
`localipc` resolves an endpoint URL by scheme into exactly two things — a
`net.Listener` for the server and a `(baseURL, *http.Client)` pair for the
client — and every caller in the CLI and server goes through those two
functions. Websockets ride the same `*http.Client` (`DialOptions.HTTPClient`),
and `git` rides `StartLoopbackProxy`, which is itself built on `HTTPClient`. A
new scheme therefore reaches the whole product, not part of it.

iroh dials ed25519 public keys instead of IP addresses, over QUIC, with NAT
traversal and relay fallback. Its endpoint ID is a stable address that survives
the machine moving networks, which is the property missing above.

## Decision

### 1. iroh is a scheme, not an API surface

`iroh://<endpoint-id>` joins `http`, `https`, `unix`, and `npipe` as an endpoint
scheme. The server serves its ordinary HTTP handler over accepted iroh streams;
the client dials iroh streams under an ordinary `*http.Client`. Nothing above
the transport changes: the generated OpenAPI client, the auth middleware, the
exec attach websocket, the SSH bridge, the TCP tunnel, and the project stream
all work because they are already written against `net.Listener` and
`*http.Client` rather than against TCP.

A separate iroh-native RPC protocol was rejected. It would mean a second wire
contract for the same operations, a second client to keep in sync with
`api/openapi/server.yaml`, and no reuse of the authorization pipeline — all to
avoid HTTP/1.1 framing over a stream that already provides ordered reliable
bytes. An iroh stream is a `net.Conn`; the cheapest correct thing to put on it
is the protocol we already speak.

HTTP/3 over iroh was also rejected. It would put QUIC streams to their intended
use, but websocket upgrade over HTTP/3 requires extended CONNECT (RFC 9220),
which neither our websocket library nor our handlers speak. Every long-lived
Discobox stream is a websocket, so HTTP/3 would break the majority of the
product to modernize the minority. HTTP/1.1 over one bidirectional stream keeps
hijack working, which is what attach depends on.

### 2. Each hop names its own endpoint package, and neither is called after a transport

`localipc` and `pool-agent/endpoint` were the same abstraction discovered twice.
Both exported `Parse`, `HTTPClient`, `Listen`, and an `Endpoint` type, both
decided transport from a URL scheme, and both implemented unix-socket HTTP.
`localipc` becomes `endpoint` in the root module, and `pool-agent/endpoint`
becomes `pool-agent/wire`.

Folding both into one package in the root module was the first plan and was
rejected on inspection. `pool-agent/endpoint` reaches VSOCK through
`pool-agent/vsock`, so moving it up would put a Linux guest transport and its
third-party dependency into the module that the CLI, hooks, and sandbox-agent
all import, purely to deduplicate about forty lines of unix and TCP
boilerplate. The two also answer to different callers: one `Listen` returns the
advertised address and an unlink hook because a control-plane socket must be
reclaimed, the other returns a bare listener. Unifying them means a six-field
`Endpoint` and an options struct on every call — a worse package than the two
it replaces.

What was actually wrong was the naming. `localipc` described a transport
mechanism rather than a role, and stopped being true the moment a global
peer-to-peer scheme joined it. `endpoint` names the vocabulary both hops speak,
and belongs to the hop the user configures: `--server`, the listener, git's
bridge. `wire` names the guest-side hop and keeps VSOCK where its dependency
belongs. Neither package is named for a transport, so neither is falsified by
adding one.

Giving both packages the name `endpoint` was rejected outright: a name shared by
two packages implementing the same idea identifies nothing, and
`server/providers/libkrun/provider.go` imports both. `transport` was rejected
for the guest hop after the fact — it collides with `server/internal/transport`,
which would have forced an import alias in `server/providers/dockerworker`,
trading one ambiguity for another.

### 3. Capabilities are properties of an endpoint, not a prefix test

`isLocalEndpoint` means two different things at its three call sites: "I can
start this server myself" for auto-launch, and "git cannot dial this, bridge
it" for `gitServerURL`. Those answers agree for `unix` and `npipe` and diverge
for `iroh`, which needs the loopback bridge and must never be auto-launched.

The parsed `Endpoint` answers each question directly, and the string test is
deleted rather than extended. A scheme that is added later declares its own
capabilities instead of being pattern-matched from three call sites.

### 4. The implementation is the iroh-ffi bindings, behind a build tag

Discobox binds iroh through `git.coopcloud.tech/decentral1se/iroh-go`, the
UniFFI bindings over the upstream Rust crate, gated by an `iroh` build tag that
is **off by default**.

The alternative was `github.com/tmc/go-iroh`, a clean-room pure-Go port that is
wire-compatible with Rust iroh and hands out `net.Listener` and `net.Conn`
directly. It was rejected on supply chain rather than on ergonomics: it is
unversioned, has one maintainer, and vendors a fork of `crypto/tls` to add
RFC 7250 raw public keys — meaning the code parsing unauthenticated packets
from the internet would not inherit Go's TLS security fixes. Binding the same
Rust implementation that n0 ships, tests, and patches is the more durable base
for a transport whose entire job is to be safely internet-facing.

The costs are accepted rather than disputed, and they are real:

- The bindings ship `linux/amd64` and `linux/arm64` only. This is a packaging
  choice, not a platform limit: the target list is one line in the project's
  `scripts/generate.sh`, built with `cross`, and its README offers more targets
  on request. Discobox builds its own artifacts rather than waiting — Apple
  targets need a macOS runner, which `cross` cannot provide, and Windows needs
  `x86_64-pc-windows-gnu` because cgo links through gcc and cannot consume an
  MSVC `.lib`.
- Linking through cgo puts a C toolchain on every machine that builds an
  iroh-enabled binary and complicates cross-compilation. The alternative is the
  loader this repository already uses for its database: `turso.tech/database/
  tursogo` reaches a Rust library through `ebitengine/purego` with a separate
  platform-libs module and no cgo at all. Prefer that shape for the iroh libs;
  it keeps `CGO_ENABLED=0` and leaves cross-compilation working.
- The FFI exposes `RecvStream.Read(sizeLimit uint32) ([]byte, error)` with no
  context, deadline, or cancellation, so a read in flight cannot be abandoned
  without losing the bytes it consumed — and `http.Server` sets read deadlines
  routinely (`ReadHeaderTimeout`, `IdleTimeout`). The adapter resolves this
  with a single pump goroutine that owns the FFI read loop and hands chunks
  over a channel: a deadline expires on the channel receive rather than on the
  FFI call, pending bytes stay queued for the next `Read`, and
  `SetReadDeadline` means what `net.Conn` says it means. The pump reads at most
  one chunk ahead, so this buys correct deadlines rather than unbounded
  buffering.

  Write deadlines have no such escape: the peer may have received any prefix of
  a partial write, so the stream's framing is no longer known. A write deadline
  therefore resets the stream. That asymmetry is deliberate and is the one
  place the FFI's shape still shows through.

The build tag is opt-in rather than opt-out because the default build must stay
pure Go and cross-compilable on every platform we ship. A tag that defaults on
would make `go build` fail on a macOS laptop, which inverts which case is the
exception. The tag is the first non-platform build constraint in the repository,
so CI must build and test the tagged configuration explicitly; tagged code that
no pipeline compiles is deleted code that still occupies a file.

`Parse` recognizes `iroh://` in **both** builds. A binary compiled without the
tag rejects an iroh endpoint with "this build does not include iroh support"
rather than "unsupported scheme", because the first sentence tells the operator
what to do and the second sends them looking for a typo.

### 5. The endpoint ID authenticates; an allowlist authorizes

An iroh connection is mutually authenticated TLS 1.3 with RFC 7250 raw public
keys: the dialer verifies the server's key equals the ID it dialed, and the
server requires the client to present a key and prove possession. Dialing an ID
is therefore key pinning, with no CA to mis-issue and no trust-on-first-use
window, and a hostile relay can delay or drop traffic but cannot read or
impersonate it. The client half of authentication needs nothing from us:
`--server iroh://<id>` *is* the pin.

That authenticates the peer and authorizes nothing. iroh accepts any client key
and surfaces the identity to the application. Discobox's HTTP API meanwhile
authenticates every request as the default user with `ScopeAll`
(`DefaultUserAuthenticator`), because a unix socket made the filesystem the
access control list. An iroh listener with no further check is therefore an
unauthenticated control plane for anyone holding the endpoint ID.

The allowlist is that check, in the layer ADR 0024 §5 established for SSH:

| SSH | iroh |
| --- | --- |
| `<data dir>/ssh_host_ed25519_key` | `<data dir>/iroh_endpoint_key` |
| `<data dir>/authorized_keys` | `<data dir>/authorized_ids` |
| `<cli state dir>/ssh/id_ed25519` | `<cli state dir>/iroh/id_ed25519` |

`authorized_ids` holds one hex endpoint ID per line, with `#` comments, read on
every connection so enrolling and revoking take effect without a restart. A
missing file admits nobody. A malformed line is skipped rather than failing the
file, which is `authorized_keys(5)`'s own tolerance and fails closed.

An unenrolled ID is refused at iroh accept, before any HTTP exists, with the
reason carried in the QUIC close so the caller reads "not authorized on this
server" instead of a bare disconnect. Rejecting later, as an HTTP 401, was
considered and declined: it exposes the whole handler surface to peers that have
no business reaching it. Connections that pass the gate carry their ID into the
request context via `http.Server.ConnContext`, exactly as `internal/sshd`
carries a principal built in its `PublicKeyCallback`, and an authenticator ahead
of `DefaultUserAuthenticator` turns it into a `Principal`. That ordering is
load-bearing: `DefaultUserAuthenticator` answers every request with `ScopeAll`,
so an iroh authenticator placed after it would never be consulted.

Project-scoped iroh IDs are deferred, unlike their SSH counterpart. An SSH
project key works because SSH is an ingress onto execs and can carry narrow
scopes over one project's sandboxes; iroh carries the entire control-plane API,
where "scoped to a project" is what `ProjectAuthorizer` already decides from
user membership. So an enrolled ID authenticates as a *user* and the existing
pipeline authorizes it. Revisit when a server has more than one user to map IDs
onto; the mapping is a resource then, not a second scope system.

Treating the endpoint ID or a connection ticket as the credential was rejected.
The ID is an address by construction: it is disclosed to every relay that
forwards for it, it is in every ticket handed out, and it is published to DNS
if discovery is ever enabled. A model where holding the address grants
`ScopeAll` makes the relay operator an administrator of every server that uses
it.

### 6. The listener is opt-in, and both identities are persistent

An iroh listener is bound only when `DISCOBOX_SERVER_LISTEN` names one, as a
scheme in the endpoint list rather than a variable of its own:

    DISCOBOX_SERVER_LISTEN=unix:///run/discobox/server.sock,iroh://

`iroh://` with no ID is the listen form — the identity comes from the key file,
so there is nothing about the address to configure. The dialable
`iroh://<endpoint-id>` is what `endpoint.Listen` returns as its display value
and what the server logs at startup. SSH earned its own `DISCOBOX_SSH_LISTEN`
because it is a different protocol with a different handler; iroh serves the
same HTTP handler, so it belongs with the other endpoints. `requireLocalListenEndpoint`
still prepends a unix socket when none is configured, so a server told to listen
only on iroh keeps its local socket and cannot be reachable *only* from the
network.

Auto-launch continues to bind the unix socket and nothing else, so running
`disco` never publishes a laptop as a side effect.

Both keys are generated once and never rotated implicitly. The server's is its
address: rotating it invalidates every client's configured endpoint, the same
property and rule as the SSH host key. The client's is what an operator
enrolled: regenerating it silently revokes that machine.

Discovery is the one place iroh is worse than SSH, and it is accepted rather
than solved. `disco box ssh-config` can *fetch* `GET /ssh` to learn the SSH
endpoint because it already has a working transport; an iroh address cannot be
fetched, because it **is** the transport. So it travels out of band: the server
prints it, `disco box iroh-id` prints the client's, and enrollment is a person
pasting one into the other's file.

Because it travels out of band, the address has to be complete. An endpoint ID
alone is not routable — resolving one needs a discovery service, and this
deployment has none until the relays do — so the URL carries the addresses that
reach the peer as repeated `?addr=` parameters, and `Listen` advertises them:

    iroh://<endpoint-id>?addr=192.0.2.7:57286&addr=127.0.0.1:57286

This is the ticket idea in URL form. It is what makes two peers on one machine
work at all, and it degrades correctly: once discovery is deployed the bare
`iroh://<endpoint-id>` resolves and the parameters become an optimization.

Relays are assumed to be self-hosted. The default relay map in the ecosystem
points at n0 infrastructure, which is acceptable for development and not for a
product whose availability would then depend on it.

## Consequences

- A user runs the server anywhere and reaches it from anywhere with
  `disco --server iroh://<id>`, including from behind NAT on both ends, with no
  port forwarding, no TLS certificate, and no DNS name.
- The default `disco` and `discobox-server` binaries are unchanged: pure Go,
  cross-compiled, no iroh code linked. An iroh-enabled build is a distinct
  artifact with a distinct platform matrix, and the release pipeline grows a
  Linux cgo target.
- `localipc` and `pool-agent/endpoint` stop existing as separate packages.
  Imports change across the CLI, server, providers, and pool agent in one
  change; `isLocalEndpoint` is deleted.
- A second remote-access surface exists alongside SSH (ADR 0024), with its own
  listener and its own pre-authentication attack surface — QUIC and a TLS
  handshake reachable by any peer that learns the endpoint ID.
- Endpoint IDs join SSH public keys as enrolled credentials, sharing one
  allowlist model and one set of commands.
- The server holds a persistent identity key whose loss changes its address for
  every client, and whose disclosure lets an attacker impersonate the server to
  clients that have not pinned anything else.
