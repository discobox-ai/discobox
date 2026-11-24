# 26-11-23-665 — A request needs an open route and no blocked route

- **Status**: Proposed
- **Date**: 2026-11-23
- **Issue**: #1583
- **Supersedes in part**
  [26-10-27-880](26-10-27-880-access-to-a-secret-is-a-binding-held-to-static-routes-and-no-judge-decides-it.md)
  §4's "the most specific routes a request falls on decide (the longest host,
  then the longest path), and a blocked one among them refuses", and its
  `Blocked` field's "refuses what it is the most specific route for". The rest
  of §4 stands: a list is read on its own, every list must allow, lists never
  take precedence over one another, and open and blocked routes read what
  they cannot settle in opposite directions.

## Context

A grant held these two routes on one list:

```
api.github.com/repos/OWNER/REPO/**
api.github.com/repos/*/*/pulls/*/merge=blocked
```

and `PUT /repos/OWNER/REPO/pulls/8/merge` from the box was sent with the
credential swapped in, and merged the PR. Nothing was recorded as blocked.
A report on the issue with the block written as
`PUT api.github.com/repos/OWNER/REPO/pulls/*/merge=blocked` was refused, as
meant.

That is the ranking ADR 880 §4 chose, working as written.
`secretroute.decideList` (`pkg/secretroute/decide.go`) keeps the candidates
of the highest `rank` and refuses only if a blocked route is among them.
`rank` is host first, then `pathRank`, whose first key is the number of
literal segments **before the first `*`**:

| Route | literal | segments |
|---|---|---|
| `/repos/OWNER/REPO/**` (open) | 3 | 3 |
| `/repos/*/*/pulls/*/merge` (blocked) | 1 | 6 |
| `/repos/OWNER/REPO/pulls/*/merge` (blocked) | 5 | 6 |

The open route outranks the first block and is the only candidate, so the
block never refuses anything the open route also reaches. The second block
outranks it, which is why the second report held.

The shape that fails is the one we teach. `discobox secret bind --help`
(`cli/internal/cli/route_help.go`), `docs/agent-credentials-protocol.md` and
the access skill all give `api.github.com/repos/*/*/pulls/*/merge=blocked` as
the example of a block. A person, and an agent asking for a credential, read
a block as a rule that holds wherever it matches. A ranking cannot promise
that: `/repos/OWNER/REPO/**` and `/repos/*/*/pulls/*/merge` overlap and
neither lies inside the other, so whichever an order puts first, one reading
of "more specific" is wrong.

The ranking exists only so that an open route can be an exception written
inside a block. Two open candidates both allow and two blocked ones both
refuse; the rank decides only between an open route and a block. And a list
is already deny-by-default: a request no open route matches is refused. So an
exception inside a block can always be written as open routes alone, naming
what is open instead of what is shut around it, and the ranking buys nothing
that cannot be said without it.

## Decision

### 1. Deny overrides: an open route must match, and no blocked route may

Within one list, a request is allowed when **some open route matches it and
no blocked route matches it**. Routes do not rank. Specificity, host labels
and path segment counts take no part in the decision.

The decision short-circuits both ways:

1. Look for the first open route that matches. None: refuse, as "this
   credential has no route to …" (or the near miss `Decide` already names).
2. Look for the first blocked route that matches. One: refuse, as "route X
   refuses …", naming it.
3. Otherwise allow.

Order within a list does not change the outcome, only which route a refusal
names. This is the deny-overrides combining rule of XACML, and AWS IAM's
explicit deny: no grant, no access; any matching block, no access.

Everything else about matching is unchanged:

- **Every reading.** A request is decided at every reading of its path
  (`RequestPaths`, with encoded slashes and dot segments); a block matching
  any reading refuses, and every reading needs an open route.
- **Every list, every grant, every use.** `Decide` holds the request to each
  list, `DecideGrants` to each grant, and both to each use of the credential
  (`Request.At`), as today.
- **What a route cannot settle.** An open route matches only when every
  method, operation and call falls inside it and the body was read; a blocked
  route matches when any one does, or when the body is unreadable. A blocked
  route without a body or message block refuses every use (ADR 880 §4,
  ADR 26-11-06-442).

So `api.github.com/repos/*/*/pulls/*/merge=blocked` refuses every merge,
beside any open route. `DELETE api.github.com/repos/acme/web=blocked` beside
`api.github.com/repos/acme/web/**` refuses the delete without the `**`
tie-break `pkg/secretroute/REVIEW.md` warns about, and
`api.githubcopilot.com/mcp mcp=delete_*=blocked` beside the open
`api.githubcopilot.com/mcp` still refuses only a `delete_*` call.

### 2. Nothing refused today is allowed

A request allowed under §1 has an open route and no matching block; today it
has an open candidate and no block to outrank it, so today allows it too. §1
only refuses more: where an open route reaches into a block, the block now
wins, whatever their ranks.

Stored routes are read in place, with nothing migrated. A person's list that
used an open route as an exception inside a block now refuses there. Such a
list is written again as open routes alone.

### 3. The harness policies we ship say what is open

The built-in Copilot routes, in `harness/copilot/image.json` and repeated for
the Copilot provider in `harness/opencode/image.json` and
`harness/omp/image.json`, use exceptions inside blocks:

```
githubcopilot.com, *.githubcopilot.com                        open
githubcopilot.com, *.githubcopilot.com  /agents/**, /mcp/**    blocked
githubcopilot.com, *.githubcopilot.com  /agents/swe/internal/memory/**,
                                        /mcp/readonly/**, /v1/messages/**   open
api.github.com, *.api.github.com  /copilot_internal/**,
                                  /repos/github/copilot-cli/releases/**     open
github.com, *.github.com                                      blocked
```

Under §1 its read-only MCP, its agent memory and its token exchange at
`/copilot_internal` would be refused. They are rewritten as open routes only:

- The `github.com, *.github.com` block goes. Nothing else on the list opens a
  GitHub host but the two `api.github.com` paths, so default deny already
  shuts the rest.
- The whole-host `githubcopilot.com` route and its `/agents/**, /mcp/**`
  block are replaced by the paths Copilot uses there, alongside the three that
  were exceptions. Which paths those are is found against the real CLI when
  this is built (open below), not decided here.

A configured harness secret takes its image's routes again whenever the image
is re-read (`syncConfiguredSecretRoutes`,
`server/internal/resources/harnessconfigs/secrets.go`), so stored Copilot
credentials pick up the rewrite with the release that carries it. The
rewrite ships in the same release as §1.

### 4. Every reader of a list agrees

- `Decide` / `DecideGrants` carry §1, and `rank`, `pathRank` and their
  comparison leave the decision. A near-miss refusal may still pick which
  open route to name by how closely it matched; that choice never allows or
  refuses anything.
- `HostAllowed` (`hostOpen`) becomes: some open route covers the host and no
  host-wide block covers it. A host-wide block now shuts its hosts whatever
  open route also covers them, as `Decide` would refuse every request there.
- `ProveContained` still refuses a parent list holding a block, and is
  untouched. Under §1 a block only ever subtracts, so a proof that reasons
  about parent blocks becomes possible. It is not built here.
- The legacy-route upgrade's `outranksOnNarrowing`
  (`server/internal/database/retire_grants.go`) drops an open route that
  would have outranked a block once narrowed to its host. Under §1 no open
  route outranks a block, so the guard is no longer needed. Whether it goes
  is the implementation's; leaving it only drops routes the block would have
  refused anyway.
- The pool proxy enforces the decision, and takes §1 when its agent is
  replaced on the server's upgrade (ADR 26-11-14-624). A pool still on an
  earlier agent decides the old way until then.

### 5. What a person reads says the same

`=blocked` "refuses every request it matches, whatever else the list
allows", and a list "allows only what an open route matches". That goes into
`route_help.go` (so `docs/public/cli.md` when it is generated),
`docs/agent-credentials-protocol.md`, the access skill and `access/cli.go`'s
help, and `pkg/secretroute`'s `DESIGN.md` and `REVIEW.md`. The sentence that
says the most specific route decides is removed everywhere. The example
`api.github.com/repos/*/*/pulls/*/merge=blocked` stays, since it now does
what it says.

### Open for implementation

- **Copilot's `githubcopilot.com` paths** (§3), taken from what the real
  CLI calls, with the existing Copilot driver tests held to them.
- **Whether a write warns** when a block covers an open route entirely, so
  that open route can never allow anything. It is refused by §1 either way;
  a warning would only tell the writer.

### Not decided here

- **Git pushes to a protected branch.** The report notes that a route on
  `github.com/OWNER/REPO.git/**` cannot tell a push to `main` from a push to a
  topic branch, since the branch is in the pack protocol's body, not the
  path. A block on the merge API does not stop that. Whether routes read
  git's ref updates is a separate decision.

## Alternatives rejected

- **Keep ranking, and let an open route lift a block only when it lies wholly
  inside it** (this ADR's first draft). It fixes the reported routes and keeps
  exceptions inside blocks, but it needs a containment relation in every
  dimension (hosts, paths, methods, GraphQL, MCP, body and message places)
  evaluated per request, and a person still has to reason about which route
  wins. Since a list is deny-by-default, every such exception can be written
  as open routes alone, so the extra rule buys no expressiveness.
- **Fix the ranking: count every literal segment, or rank by segments
  first.** Any order over overlapping, incomparable patterns lets one of them
  win where the person meant the other. It only moves the failure to another
  pair (`/repos/OWNER/REPO/pulls/**` over the merge block).
- **Refuse, at write, a list whose block overlaps an open route.** That is the
  common case §1 makes work (`OWNER/REPO/**` with a merge block on every
  repository), and a person asked for exactly it. It would also leave stored
  grants decided the old way.
- **Let a block on one list refuse across lists.** Rejected by ADR 880 §4 for
  the precedence it brings; §1 needs nothing across lists, since the issue's
  routes were on one list.

## Consequences

- The routes in the report refuse the merge, as 403 `blocked by proxy`, and
  the request is recorded under `audit http --blocked`.
- A block holds wherever it matches. A person reads a list as: what the open
  routes name, less what any block names.
- A person's stored list that relied on an exception inside a block refuses
  there after the upgrade, and must be written as open routes. The built-in
  Copilot policies are rewritten in the same release (§3).
- `TestDecideSpecificity`'s exception cases (an exact path or host under a
  wider block, `x.test/mcp/readonly/**` under `x.test/mcp/**=blocked`, a host
  winning over a path) change to refusals, or move to lists written as open
  routes. `TestHostAllowed` changes with `hostOpen`. A property test holds §1
  to never allowing what the old ranking refused.
