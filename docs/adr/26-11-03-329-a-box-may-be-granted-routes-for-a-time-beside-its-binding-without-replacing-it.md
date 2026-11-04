# 26-11-03-329 — A box may be granted routes for a time beside its binding, without replacing it

- **Status**: Accepted (amended before implementation landed: Leaves standing names §4, not §3, as what adds to 022 §4; §§3 and 6 say a listing reads the resolved bindings, not the delivered rows)
- **Date**: 2026-11-03

  Accepted by the owner by merging #502, 2026-11-03.

- **Issue**: [#501](https://github.com/discobox-ai/discobox/issues/501)
- **Amends**:
  - [26-10-27-880](26-10-27-880-access-to-a-secret-is-a-binding-held-to-static-routes-and-no-judge-decides-it.md)
    §2's "per env name, the narrowest binding applies … bindings do not
    stack". An addition (§1) stacks on the binding that applies, and is the
    only binding that does.
  - [26-11-02-022](26-11-02-022-a-credential-request-names-its-policies-and-asks-to-use-and-to-delegate-at-once.md)
    §5's "an approved part replaces the box's bindings of that kind". A use
    part may be approved as an addition instead (§3), which replaces
    nothing.
- **Leaves standing**: 880 §3's delegation, 880 §4's routes and how one list
  decides, 26-10-27-840's raw delivery, and 022 §§1–4 and 6 except where
  §4 below adds to them.

## Context

A box holds a credential in a variable through one binding: its routes and
its expiry together (880 §1). When a box asks for more and a person
approves, the approved use part **replaces** every sandbox-level `Use`
binding the box holds of the secret (022 §5, `pkg/bindingplan.Use`,
`resources/secrets.replaceBindings`). The one kept takes the approval's
routes **and its lifetime**, and an approval always has one
(`defaultApprovalTTL`: the ask, else `agentcreds.DefaultGrantTTL`, at most
`agentcreds.MaxGrantTTLSeconds`). So:

- **A base given for good is put on a clock.** A person binds a box's
  `GH_TOKEN` with no expiry. The box asks for one extra route for an hour.
  To grant it, the person must either re-ask the whole base inside the
  request, which then expires in an hour with the extra route, or deny it.
- **A box given its base under a delegation can't be given more by a
  person.** A worker's base binding has `ParentID` set to its manager's
  delegation. A person's approval has none, so `checkReplaced` refuses it
  (409, "already holds that credential under another binding").
- **A base inherited from a harness config or the project is shadowed.**
  The approval writes a sandbox-level binding in the variable, which is
  narrower and wins (`store.ResolveBindings`). For the hour, the box loses
  the base routes it inherited, and gets them back only when the approval
  lapses.

Enforcement has one shape. Each sentinel is materialized from **one**
binding (`sandbox_secrets`, one row per variable, `materializeSandboxSecrets`).
The pool is sent that sentinel's route lists
(`CreateOptions.SentinelRoutes`, `map[sentinel][][]Route`: the secret's,
the binding's, the parent's, from `store.RouteLists`). A request must pass
**every** list (`secretroute.Decide`, from `proxyagent.Authorize`). In each
list the most specific route decides, and a blocked route refuses only
within its own list (880 §4). The control plane checks the same lists when
it hands out the value (`ResolveSandboxSecret`, `secretroute.HostAllowed`),
and caps how long the pool caches it by the binding's expiry (`handOut`).

The pool has no clock on a list. A binding that lapses stops authorizing
when `BindingExpiryReconciler` deletes it and the sentinel reconciler
pushes the lists without it. Until that push lands, the pool still allows
its routes, though a resolve after the lapse is refused (`Live`).

## Decision

### 1. An addition is a use binding that stacks

`SecretBinding` gains `Adds bool`. A binding with `Adds` is an
**addition**. It is:

- a `Use` binding, never `Delegate`;
- on a sandbox, never a harness config or the project;
- delivered by sentinel, never raw;
- in a variable, with routes of its own and an expiry.

```go
type SecretBinding struct {
    // … as in 880 §1, 840 and 022
    Adds bool // stacks on the binding that applies in EnvName (§2)
}
```

A box may hold several additions of one secret in one variable. The unique
use index (`idx_secret_binding_use`: target and variable, where `use`)
becomes "where `use` and not `adds`": one plain use binding per target and
variable, as today, and any number of additions beside it. The change is a
migration that recreates the partial index. It rewrites no row, and every
existing binding is plain.

Everything else a binding has applies to an addition unchanged: its own
`secret_binding_events` (created, updated, deleted, expired), its expiry
swept by `BindingExpiryReconciler`, deletion with its target or secret, and
`ParentID` when a discobox handed it on under a delegation (880 §3). In that
case the addition is held to its parent's routes and expiry like any child.
A person deletes or edits an addition through binding CRUD like any
binding.

### 2. What applies in a variable is one plain binding and its additions

`store.ResolveBindings` keeps 880 §2's rule for plain bindings: per
variable, the plain use binding on the narrowest level that has one. To it
it adds every live addition the sandbox holds in that variable **of the
same secret**. The variable's sentinel is then authorized by any of them.

- **No plain binding in the variable:** the additions deliver it alone.
  The issue's base is not required. Without one, an addition is a binding
  with an expiry, as an approval makes today.
- **A plain binding of another secret in the variable:** the additions
  do not apply. They are not deleted. They stand like a harness config's
  binding that a sandbox's shadows today, and apply again if the variable
  comes back to their secret within their expiry. Listings show them as
  not applied, with the reason.
- **Several additions:** all apply, each with its own routes and expiry.

The `sandbox_secrets` row still names one binding, the one its sentinel is
materialized for: the plain binding, else the addition that lapses last.
When that changes, the row is repointed and keeps its sentinel, as
`materializeSandboxSecrets` already does when a binding moves within one
secret. A program holding the variable never sees an addition come or go.

### 3. The pool is sent grants, and any one of them may allow a request

A sentinel is authorized by one or more **grants**: one for the plain
binding and one for each addition that applies. A grant is what one
binding's route lists are today, `store.RouteLists(secret, binding,
parent)`, plus the binding's expiry (the earlier of its own and its
parent's).

```go
type SentinelGrant struct {
    Lists     [][]secretroute.Route // all must allow, as today
    ExpiresAt *time.Time            // none: until the push that removes it
}
// CreateOptions/UpdateOptions.SentinelRoutes map[string][][]secretroute.Route
// becomes SentinelGrants map[string][]SentinelGrant.
```

`proxyagent.Authorize` allows a request carrying a sentinel when **some**
grant of that sentinel has not expired and every one of its lists allows
the request (`secretroute.Decide`, unchanged). The pool refuses a grant at
its `ExpiresAt` by its own clock, so an addition's routes stop working at
expiry even before the sweep's push lands. The issue's "at expiry only the
temporary routes stop" is then exact, not eventual. The base's grant is
untouched by it.

Each grant is decided whole, and grants never merge into one list. A
blocked route in the base still refuses only within the base. It does not
cut into an addition, and an addition's broad route does not outrank one
of the base's more specific blocked routes inside the base's grant (880 §4,
"deny routes across lists" rejected).

The control plane decides the same way. `ResolveSandboxSecret` hands out
the value for a host that some standing grant of the sentinel allows. It
caps the cache by the latest expiry among the grants that allow that host.
`store.RouteLists` stays the one builder of a grant's lists, and
`store.BindingExpiry` of its expiry. A new `store.SentinelGrants` builds a
sentinel's grants from them, and is what the pool is sent and what resolve
checks. A listing builds each grant from the same two, over the resolved
bindings rather than the delivered rows: the bindings are what delivery
converges to, and a credential just granted is listed before its sentinel
has been delivered, as it is today.

### 4. A use part is approved to replace or to add

022 §4's per-part answer gains a choice for an approved use part:

- **`replace`**: 022 §5, unchanged. It replaces the box's sandbox-level
  use bindings of the secret, and now its additions of the secret too,
  since a replacing request states the box's whole use of it. An addition
  is never the binding kept: the plain one is updated in place, else a
  plain one is made, and every addition of the secret is deleted.
- **`add`**: the approval writes one addition in the asked variable, with
  the approved routes and lifetime. It replaces, updates and deletes
  nothing, so `checkReplaced` has nothing to refuse. A person may add
  beside a base held under a delegation.

The box says which it is asking for, and the approver may change it.

- **The ask:** `discobox-access request --add` (`"add": true` in the use
  part of `agentcreds.RequestBody`) marks the use part as routes in
  addition to what the box holds. Without it, the use part is a
  replacement, as today. `--add` with no use part is a usage error.
- **The answer:** `discobox secret request approve --use approve
  --use-as add|replace`, and the same choice on the console's request
  screen. Left out, it is what the box asked. A person may turn an `add`
  ask into a replacement or a replacement into an addition. An answer that
  differs from the ask is shown as edited, as edited routes are (022 §3).
- **A discobox answering its own discoboxes' requests** (880 §3, 022 §4)
  may approve either. Its addition is a child of its delegation.
- **The request records** the mode asked and the mode approved. An
  identical pending ask (022 §4) now also has the same mode.
- **`pkg/bindingplan`** gains `Add`, whose plan touches no standing
  binding. `Answer` takes the use part's mode, so the CLI and console
  preview what the server applies, from the same rule.

The delegate part has no `add`. It is still replaced (022 §5).

### 5. The approver sees what is new, and for how long

On the request screen (console), `discobox secret request get` and
`approve`, a use part shows, beside 022 §3's roles and route summaries:

- **What applies in the variable now:** each grant, plain and additions,
  with its routes and its expiry, or "no expiry".
- **The asked routes not already named:** each asked route that no
  current grant names, compared as `secretroute.Format` writes them, as
  the console's `routeDiff` already compares. It is labeled as a
  comparison of routes as written. It is not a claim that the others are
  already allowed, since containment over globs is not decided (880,
  "Check a child's routes within its parent at approval").
- **The lifetime each mode would give:** under `add`, "these routes until
  T; the rest unchanged". Under `replace`, 022 §5's preview. When a kept
  binding would take a lifetime shorter than it has now, including none,
  it says so: "the routes held without expiry would expire at T".

So choosing between "extend the base" and "a separate grant for an hour"
is made with both outcomes on the screen.

### 6. The box sees its grants

`agentcreds.Credential` keeps one entry per variable. It gains `grants`:
for each grant, the hosts its open routes name (`model.OpenRouteHosts`)
and its expiry. The existing `hosts` and `expiresAt` are kept as the union
of hosts and the latest expiry, so an older reader stays correct about
where the variable may go and until when. `discobox-access list` prints
each grant on its own line. `ListSandboxCredentials` builds them from the
sandbox's resolved bindings, each with `store.RouteLists` and
`store.BindingExpiry` (§3).

## Alternatives rejected

- **An expiry on each route of one binding.** It needs no stacking, but a
  route's lifetime would enter the route shape that policy files share,
  and ADR 584 §1 keeps lifetime out of a policy. The sweep would edit a
  binding's routes rather than remove a record, so an hour's grant would
  have no record of its own to revoke, audit or show as one thing. And
  one list with mixed lifetimes still ranks a temporary route against the
  base's blocked ones (§3).
- **A grant table layered over a binding.** It is the shape 880 removed:
  a second record beside the binding that names the same target and
  secret, which every reader must remember to join. An addition is a
  binding, and gets bindings' history, expiry, delegation and cascades for
  nothing.
- **Let every sandbox-level use binding in a variable stack, with no
  flag.** A plain sandbox binding shadows the harness config's and the
  project's by design (880 §2). Without the flag, an hour's grant over an
  inherited base would either still shadow it (the failure in Context) or
  every sandbox binding would stop shadowing. The flag says which one a
  binding is.
- **Merge the addition's routes into the base's list for its lifetime.**
  Within one list the most specific route decides and a blocked route
  refuses (880 §4). A union would let one grant's blocked routes cut into
  the other's, or its broad routes outrank them. Separate grants keep each
  binding decided as it was approved.
- **An addition that points at its base by ID and dies with it.** The base
  may be on a harness config or the project, may be repointed to another
  binding, or may lapse first. A worker's base under a delegation expires
  on its own schedule. Tying the addition to the variable and secret
  instead needs no base, and is never left pointing at a binding that no
  longer delivers the variable.
- **Deliver the extra routes in a second variable.** The program reads
  `GH_TOKEN`. It does not know to use `GH_TOKEN_EXTRA` for some calls.
- **Infer the mode: grant only the routes the box doesn't hold, for the
  approval's time.** Deciding which asked routes are "already held" is
  containment over globs and protocol lists, which 880 declined to decide
  at approval. The box says which it means, and a person reads both
  outcomes (§5).
- **Leave expiry to the sweep's push.** An addition is often minutes or an
  hour. A push that fails, retries with backoff or races a pool restart
  would let its routes run on past the time the person granted. The pool
  already holds a clock, and a grant carries its expiry.

## Consequences

- A person can give a box an hour's route beside a base held for good,
  inherited from a harness config, or handed on by a manager. The base's
  routes and lifetime are not touched, and at the hour the pool refuses
  only the added routes.
- A box can still narrow or restate its access by asking to replace.
  Replacing clears its additions too.
- The pool update API's `sentinelRoutes` becomes grants with expiries.
  The server, pool agent and proxy change together: `secretroute.Decide`
  is called per grant, and `Authorize` reads the time.
- The request record, the approve call, `bindingplan`, the console's
  request view, `discobox secret request get`/`approve`,
  `discobox-access request`/`list`, `agentcreds`, and
  `docs/agent-credentials-protocol.md` change. So do `secrets/DESIGN.md`,
  `sandboxes/DESIGN.md`, `pool-agent/DESIGN.md`, `access/DESIGN.md`,
  `bindingplan/DESIGN.md` and the `discobox-access` skill.
- One migration recreates `idx_secret_binding_use` as a partial index that
  excludes additions, and adds the request's mode columns (absent for
  existing rows: `replace`).

## Open for implementation

- **A pool that predates grants.** How the server tells one is left to
  implementation. Whatever it is must fail closed for the addition only:
  such a pool is sent the plain binding's lists, as today, and never an
  update it refuses whole, which would also hold back a removal.
- **The console's layout** of the mode choice and the per-grant rows, and
  the exact flag spellings beyond `--add` and `--use-as`.

## Deferred

- **Additions on a harness config or the project.** Requests target a
  sandbox, and nothing asks for one there. Revisit if a person wants a
  standing set plus a temporary set for every box of a harness config.
- **A delegate part approved as an addition.** A manager handing on more
  for an hour would need children held to one of several delegate
  grants. Revisit when a manager needs a temporary delegation.
- **An addition beside a raw binding.** The value itself is in the box,
  and the proxy only watches for it. What a wider grant would mean there
  is left until a raw binding needs one.
