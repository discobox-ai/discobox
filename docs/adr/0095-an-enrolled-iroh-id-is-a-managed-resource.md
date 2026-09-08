# 0095 — An enrolled iroh ID is a managed resource, and the file is the way back in

- **Status**: Accepted
- **Date**: 2026-09-04
- **Supersedes**: [ADR 0052](0052-iroh-is-an-optional-endpoint-scheme.md) §5's file-only enrollment
- **Amended by**: [0097](0097-a-discobox-address-is-versioned-and-names-no-transport.md), in two respects: §2's hex spelling of the ID, and the name of the resource and its commands (`iroh-ids` and `admin iroh` become `peers` and `admin peer`). Both are spellings — the ID remains the resource's own identity and its primary key, and every decision here stands. Amended rather than superseded because nothing has shipped against it.

## Context

[ADR 0052](0052-iroh-is-an-optional-endpoint-scheme.md) §5 gave iroh one
authorization layer: `<data dir>/authorized_ids`, a file of hex endpoint IDs
read on every connection, admitting nobody when absent. It borrowed that shape
from [ADR 0024](0024-ssh-is-a-control-plane-ingress-onto-execs.md) §5's
`authorized_keys` — but only half of it. SSH has **two** layers: the file, which
is the operator's own key and works before any API access exists, and
project-scoped keys, which are a managed resource with CRUD through the API and
`discobox admin ssh-key`. iroh got the file and nothing else.

ADR 0052 §5 did consider a second layer and declined it, on the grounds that
"scoped to a project" is what `ProjectAuthorizer` already decides from user
membership. That reasoning is still correct and is not what this ADR disturbs.
What it settled by omission is a different question: whether enrollment itself
is an API operation. It is not, today, and the consequences are:

- **Enrolling a client requires shell on the server host.** The one thing an
  iroh endpoint exists to avoid is needing another way to reach the machine, and
  enrolling the second client needs exactly that. An operator who has one
  working client still has to `ssh` somewhere and edit a file.
- **Nothing can be listed.** `authorized_ids` is a text file; there is no way to
  ask a server who may reach it, which is the first question anyone asks about
  an allowlist.
- **Nothing is recorded.** No label and no enrollment time, so an ID that
  nobody remembers adding cannot be told from one that is still in use.
  `authorized_keys(5)` tolerance means a typo silently grants nothing, and
  nothing reports that either. *Who* enrolled it is not on this list: with one
  default user there is one possible answer, and recording it becomes a gain
  under the same condition §1 defers project scoping on.

`discobox admin iroh-id` is meanwhile the only iroh command, and it prints one
value. The verb an operator actually needs — enroll this ID over there — has no
command at all, on either side.

This ADR supersedes ADR 0052 §5's single-layer decision. The rest of ADR 0052
stands.

## Decision

### 1. An enrolled iroh ID is a server-scoped resource

`/iroh-ids` gains `GET` and `POST`, `/iroh-ids/{irohId}` gains `DELETE`, at the
top level rather than under `/projects/{projectId}`. The
resource carries the endpoint ID, an optional name, and its timestamps.

There is no `CreatedBy`. `model.SSHKey` has one, and copying the field set
wholesale would copy it, but nothing writes it — `resources/sshkeys/service.go`
leaves it at its zero value and `sshd` falls back to the default user ID when it
reads one back empty. A column that is always empty records nothing and invites
a reader to believe it records something. Add it when a server has more than one
user, which is the same condition this section defers project scoping on.

Project scoping stays deferred, with ADR 0052 §5's reasoning and its condition
unchanged: an iroh connection carries the entire control-plane API, so an
enrolled ID authenticates as a *user* and the existing authorization pipeline
decides the rest. Revisit when a server has more than one user to map IDs onto.
Putting the route under a project would advertise a boundary the transport does
not have.

Being top-level means no existing authorizer applies to it. `ProjectAuthorizer`
and `PoolRouteAuthorizer` both return "not applicable" and the middleware falls
through to 403, so the route must be named somewhere. It is added to
`authenticatedAllowedPaths` — `/iroh-ids` exactly and `/iroh-ids/` as a prefix —
which is `internal/auth/REVIEW.md`'s sanctioned mechanism for a route with no
resource-specific authorizer: an explicit allow-list rather than authorization
by exclusion.

It is emphatically **not** added to `IsPublicPath`. `GET /ssh` is public because
it must be fetchable before any credential exists and publishes only an address
and a host public key; an enrollment endpoint fails both halves of that test,
and `internal/auth/REVIEW.md` says not to widen the list without them.

That leaves the route authorized for any authenticated principal, and
`DefaultUserAuthenticator` authenticates every request that is not a pool
runtime route as the default user with `ScopeAll`. The consequence is accepted
deliberately and stated rather than discovered: **anything that can already
reach this API can enroll an iroh ID**, including a pool guest dialing the
carrier hub, which is served by the same router as every other listener. In
scope this grants nothing new — such a caller already has `ScopeAll` and can
read every secret, create sandboxes, and shut the server down. In *kind* it
does: an enrollment is durable and external, surviving the sandbox, the pool,
and a restart, where the reach it was minted from was transient and inside.

The allow-list has no method dimension — `isAuthenticatedAllowedPath` matches
the path and nothing else — so `DELETE` is authorized on exactly these terms
too, and revocation is the worse half. A caller that mints itself a credential
has taken something; a caller that revokes every managed enrollment has locked
the operator out, and by §6 the next dial is refused. What that operator falls
back to is shell on the host and a text editor, which is the first grievance in
this ADR's Context. The file layer is what bounds the damage: `authorized_ids`
is not reachable through the API at all, so an operator who kept a line in it
still has a way in. That is a second reason §3 does not migrate the file away,
beyond the database-unreadable case it already gives.

Two narrower rules were considered and declined for now:

- **Gate on connection provenance** — tag each listener through
  `http.Server.ConnContext` with how the connection arrived, and admit
  enrollment only from local IPC or an already-enrolled iroh peer. This is the
  right end state, and it would also build the half of ADR 0052 §5 that was
  specified and never implemented: iroh connections were to carry their ID into
  the request context and an authenticator ahead of `DefaultUserAuthenticator`
  was to turn it into a `Principal`. Nothing does today, which is why every iroh
  request is simply the default user. Doing it here would make an enrollment API
  wait on a transport-identity change it does not otherwise need.
- **Local IPC only** — refuse enrollment that did not arrive on the unix socket
  or named pipe. Safe, small, and it removes the reason to build this: §5's
  point is that every enrollment after the first can be made remotely by an
  already-enrolled client.

Revisit when connection provenance exists, or when the carrier hub's callers get
a principal of their own instead of inheriting the default user's. Either one
turns this from a stated exposure into an enforceable rule, and the allow-list
entry is what gets narrowed when it does.

### 2. The endpoint ID is the resource's identity

There is no generated `id.Prefix*` identifier. The endpoint ID is already
unique, stable, operator-visible, and the exact string that appears in
`authorized_ids`, and it is the primary key.

A generated ID was rejected because it would make `discobox admin iroh ls` print
an identifier that is neither the value the operator pasted nor the value in the
file — three names for one enrollment, of which the API's would be the only one
that is not the address. It would also mean a change to `github.com/discobox-ai/x`
to reserve a prefix, for an identifier the system does not need.

`DELETE` resolves its path segment against stored rows the way `firstByID` does,
so a unique prefix works. It cannot resolve it with `endpoint.ParseIrohID`,
which is deliberately strict about length (a truncated ID is a different
identity, not a prefix of this one) — that strictness is right for an address
being dialed and wrong for a row being named, and the two paths stay separate.

Dropping `ParseIrohID` does not mean dropping validation. The segment is checked
against `^[0-9a-f]{1,64}$` before it reaches the store, because `firstByID`'s
non-generated branch builds `column LIKE value || '%'` from the raw value, and a
64-character hex ID always takes that branch — there is no `_` for
`id.IsGenerated` to find. `%` and `_` are LIKE wildcards, so `DELETE
/iroh-ids/%25` would match every row, and `firstByID`'s `len(matches) != 1`
check would then *succeed* on a server with exactly one enrollment, revoking a
credential the caller never named. SSH keys are masked from this because their
generated IDs take the exact-match branch; an endpoint ID is the case the helper
was not written for. Revocation is the one operation here that must never act on
a row the operator did not name, so the check is part of the decision rather
than an implementation detail.

### 3. Two layers, file first, exactly as SSH

`authorized_ids` stays, unchanged, and is checked before the database. It is the
operator's way back into a server whose API is what they are trying to reach,
which is ADR 0024 §5's argument and does not weaken by having a managed layer
beside it. Existing deployments need no migration and no backfill.

An important difference from SSH, stated rather than left to be discovered: the
two iroh layers grant the *same* principal — the server's default user with
`ScopeAll` — because that is what any iroh connection authenticates as (ADR 0052
§5). They differ in management path and provenance, not in reach. ADR 0024 §5's
"the broader grant wins" therefore has nothing to arbitrate here yet; file-first
ordering is kept anyway, so that the recovery layer is the one that answers when
both do, and so the ordering already means the right thing if the grants ever
diverge.

API-only, with a one-time backfill of the file, was rejected on ADR 0024 §5's
own terms: it leaves no path for an operator to recover a server whose only
credential is inside it, and makes admission depend on the database being
readable — a dependency the current file loader does not have.

### 4. Admission is built before the database and learns the store later

`configureIroh` runs before `database.New`, because the server binds before it
initializes and answers while initializing
(`server/internal/server/server.go`). The accept-time `Authorize` callback
therefore cannot capture a store: there is not one yet. Admission becomes a
value constructed at bind time with the data directory, handed the store once
`NewApp` returns.

**The endpoint contract changes to make this expressible**, and that is the
load-bearing part of this section rather than a detail of it.
`endpoint.IrohConfig.Authorize` becomes

    Authorize func(ctx context.Context, id IrohID) error

from `func(IrohID) bool`, in the **root module** — the stable contracts module
every other module imports. Both halves are needed and neither is cosmetic. The
error is the close reason the peer reads: `IrohEndpoint.authorize` already
returns one and today can only ever synthesize "not authorized on this server",
so a peer refused because the server had not finished starting is told it is not
enrolled. The context is what bounds the wait below; there is none at the hook
today, and none to be had from iroh-go's `Listener.serve`, which calls the
authorizer with the connection and nothing else.

The context is therefore the **listener's**, not the connection's. It is created
with the endpoint and canceled when the endpoint closes, which is what ends the
wait in the case that matters: if `database.New`, `db.Migrate`, `NewApp` or
`EnsureHarnessAvailable` fails, `Run` returns, `cleanupListeners` closes the
endpoint, and every parked waiter is refused with a reason rather than dying
unanswered with the process. The wait additionally carries a deadline, so a
store that is merely slow does not hold a peer open indefinitely.

Within those bounds a connection in the window is **waited on, not refused**.
The file layer answers immediately; a managed ID waits. Refusing outright would
tell a correctly enrolled client it is not authorized because the server was
still opening its database — a wrong answer that an operator would spend the
afternoon chasing in the wrong file, and it inverts the property the
bind-before-initialize ordering exists to provide.

The wait is not free, and the cost is accepted rather than hidden: the gate
cannot tell an unenrolled peer from one that might be in the store, so during
the window *every* non-file peer parks, and the endpoint ID is disclosed to
every relay that forwards for it (ADR 0052 §5). Anyone holding the address can
therefore hold open a per-connection goroutine, pre-authorization, for as long
as the window lasts. The deadline and the listener cancellation are what make
that a bounded cost instead of an unbounded one, and they are why the wait is
specified here rather than left to the implementation.

Binding the iroh listener after the database opens was rejected: that is exactly
the pre-bind slowness the startup handler was built to eliminate, and it would
restore the failure it removed, where a client cannot tell a server still coming
up from one that died.

### 5. `discobox admin iroh` is a noun with verbs, and only one of them is local

    discobox admin iroh id            # this machine's endpoint ID (no API)
    discobox admin iroh ls            # who may reach this server
    discobox admin iroh add [ID]      # enroll; no argument enrolls this machine
    discobox admin iroh rm ID...      # revoke

`id` is the odd one and stays because it is the odd one: it reads and generates
a local key file and talks to no server, which is what makes it usable on a
machine that has no access yet. Everything else is an ordinary API call through
`a.apiClient()`, so it inherits the transport the rest of the CLI uses and works
over the local socket or over iroh itself.

That is what closes the bootstrap loop. The first enrollment is
`discobox admin iroh add <id>` run on the server host over its unix socket,
which is already authenticated as the default user by filesystem permissions —
no file editing. Every enrollment after that can be made by an already-enrolled
client, remotely, which is the point.

The socket is not a *privileged* path, though, and §1 is where that matters:
the same route answers on every listener the router serves, so "run it on the
server host" is how an operator will use it, not a restriction the server
enforces.

`discobox admin iroh-id` remains as a hidden alias for `iroh id`. It is the
command in operators' notes and the only iroh command that has ever existed;
breaking it to save one line of cobra wiring is a cost paid by users to tidy the
source. (ADR 0052 §6 writes it as `disco box iroh-id`, which was never a real
command — the alias is kept for the one in `cli/internal/cli/admin.go`, not for
the prose.)

### 6. Revocation takes effect on the next connection, not the current one

Both layers are read per connection, so removing an ID refuses the next dial
without a restart. It does not tear down connections already established — the
same contract `authorized_keys` has here, where `LoadAuthorizedKeys` runs in
`PublicKeyCallback` and never again for the life of the connection.

Terminating live connections on revoke was considered and deferred: it needs a
registry of open iroh connections keyed by endpoint ID and a close path into
`endpoint`, and it changes what revocation means for the SSH layer too, which
would then be the only one that lets a revoked peer stay. Revisit when either
layer gains session termination, and give both the same semantics when it does.

## Consequences

- An operator with one working client can enroll and revoke the rest over iroh,
  without shell on the server host. Losing every enrolled client still costs a
  trip to the machine, which is what the file is for.
- `discobox admin iroh ls` answers "who can reach this server", and enrollments
  carry a label and a creation time. The file's entries do not appear in that
  listing — they are a different layer, and a listing that merged them would
  imply `rm` could delete a line the API does not own.
- Enrollment *and revocation* are reachable from every transport the router
  serves, by any principal the pipeline authenticates — today, all of them. A
  caller inside a pool can mint itself a credential that outlives the pool, or
  delete every managed enrollment and leave the operator with only whatever is
  in `authorized_ids`. This is carried knowingly (§1) and is the first thing to
  revisit when connection provenance lands.
- The database becomes load-bearing for admitting a managed client. A server
  whose database will not open admits only file-layer IDs, which is the intended
  degradation and the reason the file is not migrated away.
- One more thing is reachable before HTTP authorization runs: the admission
  gate now issues a store read per accepted connection. It is a single indexed
  lookup on a table an operator writes by hand, and it fails closed.
- `server/internal/irohd` grows from two files that read the data directory into
  the package that owns admission, and gains a dependency on the store. The file
  loader inside it does not change.
- The root module's `endpoint` package changes shape: `IrohConfig.Authorize`
  gains a context and returns an error instead of a bool (§4). Every caller is
  in this repository — `server/internal/server` configures it and the
  `endpoint` tests exercise it — but it is a contract in the module the CLI,
  hooks, pool agent and sandbox agent all import, so it is a wider change than
  the enrollment feature it exists to serve.
- A peer that connects while the server is still opening its database waits
  instead of being refused, holding a goroutine for up to the admission
  deadline. That is reachable by anyone who knows the endpoint ID, which is not
  a secret.
