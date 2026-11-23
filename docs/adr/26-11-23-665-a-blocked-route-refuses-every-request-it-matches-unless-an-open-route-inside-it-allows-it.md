# 26-11-23-665 — A blocked route refuses every request it matches, unless an open route inside it allows it

- **Status**: Proposed
- **Date**: 2026-11-23
- **Issue**: #1583
- **Supersedes in part**
  [26-10-27-880](26-10-27-880-access-to-a-secret-is-a-binding-held-to-static-routes-and-no-judge-decides-it.md)
  §4's "the most specific routes a request falls on decide (the longest host,
  then the longest path), and a blocked one among them refuses". The rest of
  §4 stands: a list is read on its own, every list must allow, lists never
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
the example of a block, and say it "carves a route out of a wider one". A
person, and an agent asking for a credential, read a block as a rule that
holds wherever it matches. A total order over path patterns cannot say that:
`/repos/OWNER/REPO/**` and `/repos/*/*/pulls/*/merge` overlap, and neither
lies inside the other, so whichever the order puts first, one reading of
"more specific" is wrong. Counting every literal segment instead of the
prefix would rank them 3 against 3 and let the block win here, but
`/repos/OWNER/REPO/pulls/**` (4) would beat it again.

Within one list, the rank decides only one thing. Two open candidates both
allow, two blocked ones both refuse; the rank matters only when an open route
and a blocked route both match, which is exactly the question of whether the
open route is an exception to the block. (It also chooses which near miss a
refusal names, which does not change any decision.)

## Decision

### 1. A block holds wherever it matches; an exception must lie inside it

Within one list, a request is allowed when an open route it matches **lies
inside, and outranks, every blocked route it matches**. With no blocked
route matching, any open route that matches allows, as today. Otherwise the
list refuses, naming a blocked route that matched.

An open route O lies inside a blocked route B, for a request, when every
request O's matching part could match, B matches too:

- **Host and path**: O's host pattern and path pattern that matched this
  request are each inside one of B's, by the containment `ProveContained`
  already uses (`hostContained`, `pathContained`). Only O's patterns that
  reach this request count, so an open route that also names other hosts or
  paths is still an exception where it meets the block.
- **Methods**: B names none, or O names some and each is one of B's.
- **GraphQL and MCP**: B has no such block, or O has the same kind and its
  operations, fields or tools are each inside one of B's. An open protocol
  route matches only when every operation falls inside it, and a blocked one
  when any does, so this is a subset of B's matches.
- **Body and WebSocket places**: B has no place block (a block without one
  refuses every use, ADR 880 §4), or O has the same kind and each of its
  places is one of B's.

"Outranks" is today's `rank`, unchanged. Inside B and outranking it, O names
a narrower host or path than B; an open route with B's host and path and only
fewer methods, or a narrower protocol block, does not lift B (see
Alternatives).

So `api.github.com/repos/*/*/pulls/*/merge=blocked` refuses the merge beside
`api.github.com/repos/OWNER/REPO/**`, and every other route that reaches a
merge path without being written inside the block. Copilot's
`githubcopilot.com/mcp/**=blocked` under the rest of its host, and
`api.githubcopilot.com/mcp mcp=delete_*=blocked` beside the open
`api.githubcopilot.com/mcp`, decide as they do today: the first has no open
route inside it to lift it, and the second matches only a `delete_*` call,
which the open route (no MCP block, so not inside it) cannot lift.
`api.github.com/repos/*/*/pulls/*/merge=blocked` beside
`PUT api.github.com/repos/OWNER/REPO/pulls/8/merge` still lets that one merge
through: the open route is inside the block and names more of the path.

### 2. Nothing allowed today is newly allowed

If a list allows a request under §1, it allows it today: an open route that
outranks every matching block has a higher rank than any of them, so no block
is among today's highest candidates. §1 only refuses more, and only where an
open route and a block overlap without one lying inside the other.

Stored routes are read in place. Nothing is migrated or rewritten, and a
route that meant a carve-out now is one.

### 3. Every reader of a list agrees

- `Decide` / `DecideGrants` in `pkg/secretroute` carry §1. Every list, every
  grant and every use (`Request.At`) are decided as today, each list on its
  own.
- `HostAllowed` (`hostOpen`) already asks whether an open route covering the
  host outranks every host-wide block covering it. A host-wide block names no
  methods, paths or protocol, and of two host patterns that both cover a host
  the one with the higher score lies inside the other, so its answer already
  agrees with §1; it is left as it is, with a test holding it to `Decide`.
- `ProveContained` refuses any parent list holding a block, and is untouched.
- The legacy-route upgrade's `outranksOnNarrowing`
  (`server/internal/database/retire_grants.go`) ran once to avoid widening a
  migrated list under the old ranking. §1 refuses at least as much, so it
  stays.

The pool proxy is where the decision is enforced, and it takes §1 when its
agent is replaced on the server's upgrade (ADR 26-11-14-624). A pool still on
an earlier agent reads routes the old way until then.

### 4. What a person reads says the same

The words that describe a block change with the code: `=blocked` "refuses
every request it matches, except one an open route written inside it allows",
in `route_help.go` (and `docs/public/cli.md`, generated from it),
`docs/agent-credentials-protocol.md`, the access skill and CLI help, and
`pkg/secretroute`'s `DESIGN.md`/`REVIEW.md` entries. The example
`api.github.com/repos/*/*/pulls/*/merge=blocked` stays, since it now does what
it says.

### Not decided here

- **Git pushes to a protected branch.** The report notes that a route on
  `github.com/OWNER/REPO.git/**` cannot tell a push to `main` from a push to a
  topic branch, since the branch is in the pack protocol's body, not the
  path. A block on the merge API does not stop that. Whether routes read
  git's ref updates is a separate decision.
- **The exact containment predicate's code.** §1 names the relation; whether
  it reuses `openContained` with the blocked side's "none means any" rules or
  is written beside it is the implementation's, as long as both are tested
  against `Decide` on the cases above.

## Alternatives rejected

- **A block always wins where it matches (deny overrides).** Simplest to
  state, but it takes away the exception inside a block that ADR 880 §4 and
  `HostAllowed` support: a host-wide block with one path open under it, or a
  block on all merges with one repository's merge open. People write those
  on purpose, and a list with no way to say "everything here but this" pushes
  them to enumerate every open path instead.
- **Fix the ranking: count every literal segment, or rank by segments
  first.** Any total order over overlapping, incomparable patterns lets one
  of them win where the person meant the other. It moves the failure to a
  different pair (`/repos/OWNER/REPO/pulls/**` over the merge block), and
  stays a rule nobody can predict by reading the routes.
- **Refuse, at write, a list whose block overlaps an open route it does not
  contain.** That is the common case §1 fixes (`OWNER/REPO/**` with a merge
  block across all repositories), and a person asked for exactly it. It
  would also leave stored grants enforced the old way.
- **Let a method-only or protocol-only exception lift a block** (`GET` beside
  the same host and path `=blocked`, read as "read-only here"). Today the two
  tie and the block refuses, so nobody's grant depends on it, and allowing it
  would be the one place §1 newly allows a request. Deferred: revisit if
  people ask for "everything here but these methods" and that cannot be said
  with method routes instead.
- **Let a block on one list refuse across lists.** Rejected by ADR 880 §4 for
  the precedence it brings; nothing here needs it, since the issue's routes
  were on one list.

## Consequences

- The routes in the report refuse the merge, as 403 `blocked by proxy`, and
  the request is recorded under `audit http --blocked`.
- A list that overlapped an open route with a block it did not contain was
  open where it read closed. It now refuses there, which may stop something a
  box was doing through that gap. That is the fix.
- `pkg/secretroute`'s tests gain the cases above, with `Decide` and
  `HostAllowed` checked against each other, and a property test that §1
  never allows what the old ranking refused.
