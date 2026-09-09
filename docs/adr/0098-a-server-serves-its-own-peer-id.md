# 0098 — A server serves its own peer ID, and `discobox id` prints both halves

- **Status**: Accepted
- **Narrows**: [0052](0052-iroh-is-an-optional-endpoint-scheme.md) §6 — the address travels out of band *when iroh is the only way in*, which is not the case it is usually needed in. The rest of §6 stands.
- **Date**: 2026-09-08

## Context

Enrolling a client is two IDs meeting: the client's, which the server admits,
and the server's, which the client dials. One of them has a command
(`discobox admin peer id`). The other is a line in the server's log:

    this server's peer ID is d1-9bx2bfg-15gxtjb-…

[ADR 0052](0052-iroh-is-an-optional-endpoint-scheme.md) §6 decided that, and
its argument is sound where it applies: "`ssh-config` can *fetch* `GET /ssh` to
learn the SSH endpoint because it already has a working transport; an iroh
address cannot be fetched, because it **is** the transport."

That is true for a client whose only way in is iroh, and it is a chicken and
egg nothing can solve. It is false for every other client, and every other
client is the normal case: the person who needs the server's peer ID is
setting up a *remote* client, and they are standing on the server host with a
unix socket in front of them — a working transport, by §6's own test. The
argument for the SSH host key applies unchanged, and the ID is not even served.

So the value that is durable state on the server's disk, printed once at
startup, is reachable in practice by `discobox admin server logs | grep 'peer
ID'` — with the run's log rotated out from under it on a busy machine, or in
the journal, or nowhere at all if the server was started by something that
discarded its output. A log is not an interface.

## Decision

### 1. `GET /peer` serves this server's peer ID

One field, the peer ID, in the one written form (ADR 0097 §5). It is the same
shape and the same reasoning as `GET /ssh`, which serves the host key a client
pins: a server tells a client who it is, over the transport that client already
has.

The full `discobox://` address was rejected as the payload. The address is
`discobox://` followed by the ID with nothing in between (ADR 0097 §1), so the
ID is the entire content of it, and a document that carries a URL invites
carrying a URL with `?addr=` parameters on it — which is exactly what ADR 0097
§3 stopped advertising. The reader renders the address it needs.

Folding it into `GET /healthz` was rejected: health is polled by everything
that waits for a server to start, and it is public. Identity is not health.

### 2. It is absent when the server does not listen for peers

A server binds an iroh endpoint only when `DISCOBOX_SERVER_LISTEN` names one,
and loads or generates its key only then (ADR 0052 §6). That does not change to
answer this route: a server that generated an identity it never uses would be
handing out an address that answers nothing.

So `peerId` is optional in the response, and its absence is the answer to "does
this server listen for peers" — which is a question a client asking for the ID
is implicitly asking anyway.

### 3. It is authenticated, unlike `GET /ssh`

`GET /ssh` is public because `discobox admin ssh-config` has to read it before
any credential exists. Nothing needs a peer ID before authentication: over iroh
the caller is enrolled by definition, and over a local socket or HTTP it
authenticates as this server's user like every other request. A public path is
a decision to defend forever, and this one buys nothing.

The ID is not a secret — it is an address, disclosed to every relay that
forwards for it, and enrollment rather than obscurity is what grants access
(ADR 0095). Authenticating it is not protecting it; it is declining to add a
surface with no caller.

### 4. `discobox id` prints both halves

    $ discobox id
    client  d1-j0hcfcq-3haafhx-…
    server  d1-9bx2bfg-15gxtjb-…

The client's comes from this machine's key file, generated on first use exactly
as `discobox admin peer id` does. The server's comes from the address when the
address names one — a `discobox://` endpoint already carries it, and reading it
there costs no round trip and works while the server is unreachable — and from
`GET /peer` otherwise.

`discobox admin peer id` stays. It prints one value with nothing around it,
which is what a script consumes and what works with no server at all; this
prints the pair a person is comparing.

### 5. `--iroh` prints iroh's own hex form, and only prints it

    $ discobox id --iroh
    client  9f3c…                (64 hex characters)
    server  4afa25be01…

iroh writes an endpoint ID as 64 lowercase hex characters, and abbreviates it
to the first five bytes in its own tracing — `endpoint{id=4afa25be01}` in the
lines `--iroh-log` turns on. Nothing in Discobox writes that form, by ADR 0097
§6, and the cost lands on whoever is reading both logs at once: they have two
identifiers for one machine and no way to line them up.

This is output only. Hex is still refused everywhere Discobox reads an ID —
`ParseIrohID` rejects it, `authorized_ids` rejects it, the API rejects it — so
this cannot become a second accepted spelling by habit.

Printing both forms unconditionally was rejected. Two spellings of one identity
in one output is the thing ADR 0097 §5 removed, and a reader comparing an ID by
eye should not have to know which of two strings on the screen is the one to
paste. A flag makes seeing the transport's form a deliberate act by somebody
who is looking at the transport.

## Consequences

- The server's peer ID stops being a thing you grep a log for. Setting up a
  remote client is one command on each machine: `discobox id` on the server
  host says what to dial, `discobox id` on the client says what to enroll.
- `configureIroh`'s comment that this address "cannot be fetched over the API"
  is corrected where it stands: it cannot be fetched *over iroh, by a peer that
  is not yet enrolled*, which is the case that keeps the startup log line
  necessary. The line stays for that case.
- A client on a version of the server that predates `/peer` gets a 404 rather
  than an answer. Nothing migrates; the CLI reports what it got.
- The hex form now appears in one command's output. If that turns out to be
  where people copy IDs from, the cost is a support answer of "that is not a
  peer ID" — which is the trade §5 takes deliberately, and is reversible by
  removing a flag.
