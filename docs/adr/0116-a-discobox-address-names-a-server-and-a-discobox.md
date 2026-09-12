# 0116 — A Discobox address names a server and a discobox, and a client lists every server it knows

- **Status**: Accepted
- **Supersedes**: [0097](0097-a-discobox-address-is-versioned-and-names-no-transport.md) §1's "the address is exact concatenation" — an address may also name a server by host, and a discobox on it. §1's peer-ID format, and §§3-7, stand.
- **Date**: 2026-09-11

## Context

A client talks to exactly one server: `--server`, or `DISCOBOX_SERVER`, or
the local default. Somebody with a laptop, a workstation and a cloud machine has
three servers and three separate listings, and nothing anywhere puts them side
by side. The only way to look at the second machine's discoboxes is to retype
its address in front of every command.

Nor can a discobox be handed to anybody. [ADR 0097](0097-a-discobox-address-is-versioned-and-names-no-transport.md)
made `discobox://<peer-id>` the address of a server and defined it as exact
concatenation, `discobox://` followed by the ID with nothing in between. That
names a server reached over iroh and nothing else: not a server reached over
https, not a server by a DNS name somebody can remember, and not one discobox on
it. "Attach to this" is a message with two IDs and a paragraph of instructions.

A server also has no name. Its only identity is its peer ID, and that exists
only when it listens for peers.

## Decision

### 1. An address is `discobox://<server>[/<discobox>]`

`<server>` is resolved by the first of these that applies:

| `<server>` | Means | Looked up? |
| --- | --- | --- |
| a peer ID — `d1-…`, or its compact form | iroh, that peer | never |
| an IP address, with or without a port | `https://<ip>[:<port>]` | no |
| a DNS name with a port | `https://<name>:<port>` | no |
| a DNS name without a port | whatever DNS says; `https://<name>` when it says nothing | yes |
| written `discobox+http://<host>[:<port>]` or `discobox+https://…` | that transport, said outright | no |

A peer ID is recognised before anything is looked up, so a `d1-` host never
reaches a resolver: it is an identity, and asking DNS about one would put the
transport's answer in the hands of whoever controls a lookup that was never
needed. `?addr=` is still accepted on a peer ID (ADR 0097 §3).

A name without a port is looked up as a TXT record at `_discobox.<name>` whose
value is a peer ID in its one written form (ADR 0097 §5). A record makes it
that peer over iroh; no record makes it `https://<name>`.

`_discobox` is ours and says what it means in the form every other Discobox
surface uses. iroh's own record was considered as a second source and does not
exist in the shape that would need: iroh 1.0's `_iroh` TXT records are
published under `_iroh.<z-base-32 endpoint ID>.<origin>`, so the ID is read out
of the record's *name* rather than its value, and the attributes it defines are
`relay`, `addr` and `user-data` — nothing that maps a domain an operator owns to
an endpoint ID. Reading `_iroh.<name>` for an `id=` would be a second record of
our own invention under iroh's label, which iroh's tooling neither publishes
nor reads. A record that holds something other than a peer ID, or two different
ones, fails the dial: somebody published it meaning iroh, and https is not what
they meant.

A lookup that *fails* — a timeout, a refused query, anything but "no such
record" — fails the dial rather than falling back to https. Falling back would
turn an iroh server behind a slow resolver into an https dial of a host that
does not serve https, and the error would name the wrong thing entirely.

`discobox+http://` is the one way to write plain http, and it exists because
the rules above cannot infer it: a bare host is https, and a server that serves
plain http — a development server on a port, a deployment where TLS is
terminated somewhere this address cannot see — would otherwise have no address
in this family at all, and so no way to name a discobox on it. Naming the
transport is deliberate work, which is the property https-by-default is
protecting: nothing becomes plain text because a rule guessed.

https, not http: an address somebody hands to somebody else crosses a network,
and a plain-text control plane is not a default to encode into one. `http://`
remains what `--server` already accepts for the case that is deliberate. The
server has no TLS listener, so a server reached this way is behind a proxy
that terminates TLS, and the client trusts the system's roots.

`<discobox>` is a discobox ID, full or a unique prefix — what `discobox attach`
already takes. An address with one is accepted wherever a command takes a
discobox; `--server` refuses it, since a server is what that flag names.

**Resolution is a step of its own, between parsing and dialing.** ADR 0097 §2
put resolution at `Parse`, which suits a spelling but not a DNS lookup: `Parse`
has no context, runs inside `Listen` and inside configuration validation, and is
called far more often than anything is dialed. So `Parse` stays syntax, a name
that needs a lookup parses to an endpoint that says so, and `Resolve` does the
lookup, with a context, only on the way to a dial. What comes out of it is an
iroh or an https endpoint. Nothing below it learns that names exist, which is
the same boundary §2 drew one step further down.

A registered server's name (§3) is **not** a `<server>`. An address is what gets
pasted to another person, and a name that means one machine here and another
there, or nothing at all, would make the same address point to different places
depending on who reads it.

### 2. A server has a name

The server's `name` setting (`DISCOBOX_SERVER_NAME`) names it, and defaults to
this machine's hostname. `GET /server` serves it, authenticated for ADR 0098
§3's reason.

It is what a client offers when it registers the server (§3). It does not have
to be unique, and nothing but a client's default choice depends on it.

A setting rather than a value an API can change: a name is something an
operator decides once, and a second place that can also decide it has to have
an answer for which of the two wins. It is not part of `GET /peer`, because a
server that does not listen for peers still has a name.

### 3. A client has one primary server and any number of registered ones

The **primary** is what it is today — `--server`, `DISCOBOX_SERVER`, or the
local default — and still the only one a client may start for itself.

**Registered** servers are names and addresses in
`<user config dir>/discobox/servers.json`, beside the server's own
`server.yaml`. That is configuration rather than state: what is in it is the
user's decision and means something to them, which is the line
`<state>` is drawn on (see [cli](../../cli/DESIGN.md#cli-state-directory)).

- `discobox servers` lists them (`ls`), and `add`, `rename` and `rm` change
  them. `add` connects to the server first and takes the name it offers unless
  given one.
- `--server` also takes a registered name, which makes that server the primary
  for the command.
- A server is one server however it is written: addresses are compared as the
  endpoints they parse to, so a primary that is also registered is listed once,
  under its registered name.
- A registration records the server's peer ID, from the address when it names
  one and from `GET /peer` otherwise, and `discobox servers` lists it. Two
  addresses carrying one peer ID are one server — the same server reached
  over http and over iroh is registered once. The ID is recorded when the
  server is registered, not refreshed: registering is when the server was
  asked, and a listing that asked every server again would be a listing that
  waits on every server again.

A kubectl-style *current context* — one server at a time, switched between —
was rejected. It is what `--server` already is, and seeing several at once is
the point.

### 4. A listing is every server's

`discobox ls`, the discobox picker, and the launcher list every server —
primary and registered — concurrently. A SERVER column appears when there is
more than one.

- A registered server that does not answer within a bound is a note, and the
  listing is everything else. The primary not answering fails the command, as it
  does today.
- A registered server is listed in its default project. `--project` names a
  project on the primary; project IDs are per server.
- **An operation goes to the server the discobox is on.** A command given a bare
  discobox ID asks the primary first and the registered servers after, so an ID
  copied from `ls` works wherever it came from. The launcher routes each row's
  verbs, terminals and pushes to its server.
- The launcher's credential inbox, and its harness and secret screens, are the
  primary's. A request is answered with one of its own server's secrets, so an
  inbox that gathered every server's requests would need a secrets dialog that
  follows each request to its server — every secret, grant and approval call
  routed by request — and that is deferred. Revisit when a registered server's
  discobox waiting on a credential is a thing somebody reports missing; until
  then it is answered with `discobox --server <name> tui`.

### 5. Create offers a server only when there is one to choose

A discobox is created on the primary. When another server is registered, the
run options show `server <name> (primary)` and can change it; when none is, the
option is not there. `discobox --server <name> run` does the same from a shell.

The harness is sent by slug and resolved by the server it is sent to, so the
choice carries across servers wherever each has a harness of that name.

### 6. An address that connects registers its server

`discobox attach discobox://<server>/<discobox>` — or any command given such an
address — registers `<server>` once the discobox has been reached on it, under
the name the server offers (suffixed `-2`, `-3`… when that name is taken), and
says so in a note.

Only on success, so a mistyped address leaves nothing behind. Registering is
what makes the next `discobox ls` include the server the user just reached; an
address that has to be registered by hand before it is useful is two steps where
one was asked for.

## Consequences

- `discobox://<peer-id>` means what it meant. Every address in someone's notes
  still works.
- An address can be pasted to another person and names one discobox on one
  server. An https server can be named by host, and an iroh server by a DNS name
  an operator controls.
- `endpoint` gains `Resolve`, and `HTTPClient` takes what it returns rather than
  a string. `Diagnose` reports resolution as a layer of its own, so "which
  transport did this name turn into, and why" is answered by `discobox status`.
- Every sandbox-taking command, and `ls`, may talk to more than one server. A
  registered server that is down costs a listing the bound, not a failure.
- A server reached over https is exactly as open as one reached over http:
  the server authenticates no user (every request is the default user), and
  this does not change that. TLS protects the path, not the door.
- A server that predates `GET /server` answers 404; the client falls back to
  naming it after its address.
- `servers.json` is the first file the CLI reads from the user's config
  directory.
