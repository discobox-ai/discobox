# 0140 — A discobox reaches the discobox API through its pool, with a fixed role

- **Status**: Accepted (the rejection of authority by creator superseded, for source delivery, by [0149](0149-a-discobox-delivers-the-source-of-the-discoboxes-it-creates.md))
- **Date**: 2026-09-22
- **Relates to**: [ADR 0031](0031-agent-credentials-are-a-portable-protocol-with-ephemeral-sentinels.md),
  the protocol a discobox asks for a credential through and runs it under.

## Context

The use case is one agent that runs a development team. A *lead* discobox
creates the discoboxes the work needs and gives each the credentials its task
needs; the workers ask for what they turn out to lack, and the lead answers.

Nothing lets a discobox do any of this today:

- **The control plane is not reachable from a sandbox, and must not be.** A
  pool agent dials it over `http`, `vsock://`, or `unix://`, according to the
  provider. Every request that is not a pool's is answered as the configured
  user (`DefaultUserAuthenticator`), on every listener: what keeps a sandbox
  from it is only that the sandbox cannot reach it.
- **The control plane has no identity for a sandbox's own calls.** It
  authenticates pools, by a signed assertion checked against the pool's
  registered key (`PoolAuthenticator`), and it already trusts a pool to say
  which of its sandboxes a runtime call is for (`sandboxOwnedByPool`). The pool
  knows which sandbox is calling because it issued that sandbox's mTLS client
  certificate and mounted the key into that sandbox alone.
- **Asking for a credential and answering are separate worlds.** A sandbox asks
  through the agent credentials protocol (`discobox-access`); only a person
  answers, in the inbox.

The proxy already judges every request that carries a swapped credential
(`Resolver.Judge`, `proxy/DESIGN.md`), and today allows all of them: the
destination host is what is enforced.

## Decision

**A discobox reaches the discobox API through its pool proxy, at a reserved
host, and only while it runs a command under an approved use of the well-known
credential `ai.discobox.sandbox`. The control plane takes the pool's word for
which sandbox is calling and gives that sandbox one fixed role. The server
reads no grant and no use to decide what the role may do.**

### 1. Access is a well-known credential with no value

`ai.discobox.sandbox` joins the well-known registry, delivered in
`DISCOBOX_TOKEN` and sent to `api.discobox.internal`. A discobox asks for it
like any credential:

```
discobox-access request ai.discobox.sandbox --use "create the discoboxes working issues in org/repo"
```

A person approves it in the inbox. Approving it chooses no secret: nothing
stands behind the sentinel. It is a gate the pool holds, not a value it swaps.

A discobox calls the API under one of its approved uses, the way it uses any
credential: `discobox-access run --use <id> -- discobox new …`. The command sees
the sentinel in `DISCOBOX_TOKEN` and sends it as its bearer token.

### 2. The pool proxy forwards the reserved host, and nothing else reaches it

`api.discobox.internal` is never resolved and never sent to the internet. The
pool proxy terminates it as it terminates every host and:

- forwards the request only when it carries the sentinel of a live activation
  of `ai.discobox.sandbox` for the calling sandbox, and the judge allows it
  (today, always);
- strips the sentinel, and forwards to the control plane over the pool's own
  transport with the pool's signed assertion and the calling sandbox's ID, which
  it reads from the sandbox's mTLS client certificate;
- answers anything else itself with a 403 and never forwards it. Every other
  host is sent the placeholder when a credential does not resolve; this one
  fails closed, because the upstream is the control plane.

`DISCOBOX_API_URL=https://api.discobox.internal` is set in every exec, and the
`discobox` CLI is in the sandbox image, pointed at it.

### 3. The control plane takes the pool's word, and checks only that

A request the pool forwards is authenticated when the pool's assertion verifies,
the pool is not revoked, and the named sandbox is one the control plane placed
on that pool. The principal is that sandbox. Nothing a pool forwards is ever
answered as the default user.

The server does not check that the sandbox holds a grant for
`ai.discobox.sandbox`: the pool would not have forwarded the call otherwise, and
the pool is already trusted with every credential of every sandbox it hosts.

### 4. A sandbox has one role, a fixed list of routes

The **sandbox role** is:

- discobox create, list, and get;
- a create may pre-assign the new discobox uses of project secrets, minted as
  use grants for it in the transaction that creates it, so a create that cannot
  mint one creates nothing. A grant names a secret and a variable, or a
  well-known credential by its ID, which answers with the secret marked for it.
  It may not be `ai.discobox.sandbox` itself: a discobox that could give the
  API could give every credential onward without a person seeing it, so the
  API is granted only by a person approving a discobox's own request;
- secret requests: list, approve, and deny, with any project secret, as a person
  answers them — except a request for `ai.discobox.sandbox`, which only a
  person approves, for the same reason it is not given at create;
- secrets: list, so a request can be answered with one or a new discobox given
  one. The listing carries names and bindings, never a value.

Everything else is refused: start, stop, archive, purge, execs, terminals, port
tunnels, changing secrets, grants, pools, projects, harness configs, SSH keys,
peers, audit. A route that admits any authenticated caller does not admit a
sandbox: the role is decided before any of them.

A create from a sandbox may not carry inline secrets, which put a value inside
the new discobox where anything in it can read it; it gives uses, which only
`discobox-access` can take.
Asking for a credential is not in the role: a discobox asks for itself, through
`discobox-access`.

A discobox created by a sandbox is created as the user who created that
sandbox. A grant a sandbox mints, by pre-assigning or approving, records the
sandbox as its granter. Neither confers anything: no route is authorized by who
created a discobox, and nothing cascades along it.

### 5. The server reads no grant to authorize a call

Whether a call fits the use it was made under is the judge's question, asked in
the proxy against the use's text. The server's answer is the role, whatever the
use says. This keeps every judgement about what a use means in one place.

In this step a sandbox holding `ai.discobox.sandbox` may give any project
secret, by pre-assigning or approving. A delegation grant (`purpose: delegate`)
is therefore not consulted: with nothing to hold a sandbox to what it was
given, holding the discobox credential already is the privilege to hand
credentials on.

## Alternatives rejected

**Discoboxes form a tree, and a discobox reaches its descendants.** A parent
policy bounding depth and what a child may be given, requests routed to the
parent, archive and purge cascading down. It is a second authority system beside
grants, and it decides authority by position rather than by whether a call fits
the work.

**Authorize by who created a discobox.** The tree one level deep, with the same
question of what becomes of a creator's discoboxes when it goes. Who created a
discobox is recorded and confers nothing.

**A token minted per grant, checked by the server.** It vouches for nothing the
control plane cannot check itself from the pool's word, and adds a signing key,
a lifetime, and a leaked-token case. The pool is already the root of trust for
its sandboxes.

**The server evaluates grants and their uses.** It would split the judgement of
what a use allows between the server and the judge. The server holds a fixed
role; the judge reads uses.

**A handed-on grant lives only while the grant it came from does.** It makes
access depend on another discobox's lifetime, which is a hierarchy built from
grants. A grant a sandbox gives is independent once made.

**A grant that authorizes both using and delegating.** A person approving it
agrees to two things at once. A grant's purpose is one or the other.

**A relay in sandbox-agent, or calling the control plane's URL.** A second path
into the control plane beside the egress path every credential already takes;
and the control plane is not reachable from every provider's sandboxes.

## Deferred

- **The judge's decisions.** The proxy asks it about every call and it allows
  all of them. Revisit with the judge's own ADR, which decides what a call to
  this API must show to be allowed.
- **Holding a sandbox to what it was given.** Bounding what a sandbox
  pre-assigns or approves to its own delegation grants — same credential, uses
  taken from them, host within, expiry no later. Revisit with the judge, which
  can hold it there; until then the person approving `ai.discobox.sandbox` is
  the only bound.
- **A wider role.** Start, stop, archive, execs, terminal screen, input, and
  wait, and port tunnels, for a lead that drives its workers. Revisit when a
  lead needs to.
- **Network isolation between discoboxes.** Sandboxes in one pool share a
  bridge. Revisit when discoboxes are treated as adversaries.

## Consequences

- A sandbox holding `ai.discobox.sandbox` is, for credentials, as powerful as
  the person who approved it: it can give any project secret to any discobox in
  the project and answer any pending request. Approving it is the decision to
  trust that sandbox with the project's credentials.
- The pool proxy gains an upstream that fails closed and is reached over the
  pool's control-plane transport.
- The control plane gains an authenticator for forwarded sandbox calls and a
  table of the routes the sandbox role allows.
- The well-known registry gains a credential with no value, and approving it
  chooses no secret.
- The sandbox image carries the `discobox` CLI.
