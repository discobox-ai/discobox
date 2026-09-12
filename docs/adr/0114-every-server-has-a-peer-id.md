# 0114 — Every server has a peer ID, whatever it listens on

- **Status**: Accepted
- **Supersedes**: [0098](0098-a-server-serves-its-own-peer-id.md) §2 — the ID is no longer absent on a server that does not listen for peers — and [0052](0052-iroh-is-an-optional-endpoint-scheme.md) §6's "a key is loaded only for an endpoint that is bound". The iroh endpoint itself stays opt-in.
- **Date**: 2026-09-11

## Context

[ADR 0098](0098-a-server-serves-its-own-peer-id.md) §2 made a server's peer ID
absent unless it listens on `discobox://`, on ADR 0052 §6's reasoning that a
server should not generate an identity it never uses: "it would be handing out
an address that answers nothing". Most servers listen on a unix socket and
nothing else, so most servers have no peer ID.

[ADR 0113](0113-a-discobox-address-names-a-server-and-a-discobox.md) changed
what the ID is for. A client now keeps a list of servers, records each one's
peer ID when it registers it, and recognizes one server registered under two
addresses — its http address and its peer address — by that ID. A server
reached over http has none to record, so it cannot be recognized when it later
listens on iroh too; `discobox servers` shows a dash where its identity would
be; and whether a server has an identity at all depends on which transports it
was started with, which is the one thing ADR 0097 says an identity must not
depend on.

## Decision

### 1. The identity is loaded on every start

A server loads `<data dir>/iroh_endpoint_key`, generating it the first time,
whatever its listen endpoints are. It is an ed25519 key written by Go, like
the SSH host key every server already has; loading it needs neither iroh's
native library nor a socket. Its public half is the server's peer ID.

What stays opt-in is the endpoint: the library, the UDP socket, the relay and
the admission policy are set up only when a listen endpoint names `iroh://` or
`discobox://`, exactly as before. A server that was not asked to serve iroh
still opens nothing for it. What it has is an identity, not an address that
answers.

That is why ADR 0052 §6's objection does not carry over. The ID is not handed
out as an address — `GET /peer` serves it, and a client dials a server by the
address it was given — so an ID nobody can dial is an identity, which is what
it is now for.

### 2. `GET /peer` always serves it

The field stays optional in the contract, because a client still talks to
servers that predate this; for those, absent means "too old to have one" rather
than "does not listen for peers". Adding `iroh://` to a server's listen
endpoints later keeps the same ID, so a registration recorded over http matches
the server's peer address.

### 3. Over a transport that does not prove it, the ID is the server's word

Over iroh the handshake proves the server holds the key. Over a unix socket,
http or https, the client takes `GET /peer` at the server's word — the same
authority it grants everything else that endpoint answers, since whoever
answers the socket is the server as far as that client can tell.

Proof of possession — the server signing a nonce the client chose — was
considered and deferred. The ID is used today to label a server and to refuse
registering it twice, and a false one costs a confusing listing, not access.
Revisit when a recorded ID decides something that is: trusting a peer address
because its ID matches a server registered over http, say.

## Consequences

- Every server's data directory holds `iroh_endpoint_key` from its first start,
  and losing it changes the server's identity. It is backed up and moved with
  the data directory, as the SSH host key already is.
- `discobox id`, `discobox status` and `discobox servers` show a peer ID for
  every current server, over whatever transport reached it.
- A server reached over http and over iroh is recognized as one server.
- Nothing about listening changes: no socket, relay or library load for a
  server that does not name an iroh endpoint.
- The server logs its peer ID on every start, not only when it listens on iroh.
