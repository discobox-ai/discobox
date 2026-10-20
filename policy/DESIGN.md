# policy

What a credential grant authorizes: statements, the definitions of their words,
and what their variables resolved to at approval. See
[ADR 26-10-20-782](../docs/adr/26-10-20-782-approved-access-is-a-policy-decided-at-the-network-boundary-fast-path-first.md)
§§1, 2, 4. This package is the contract and the code that needs no reading;
nothing here decides whether a request is one of a statement's actions.

## What is here

| File | What it holds |
| --- | --- |
| `policy.go` | `Policy`, `Statement`, `Resource`, `Scope`, and `Validate`: the shape a person approves, and every way one is refused. `Variables` lists the variables the statements use. |
| `evaluate.go` | `Match`, `Parts`, `Atom`, and `Evaluate`: decided parts of each statement, and whether the atom is a supporting read, per atom of a request, combined in the two phases of §2. |
| `facts.go` | `Facts`, the shape of the control plane's record of a discobox (§4; built by the server's `Sandbox.PolicyFacts`), the variables a policy may use, `Resolve` at approval and `Bind` at a decision. |

It imports only the standard library, so `agentcreds` can carry a policy and
stay portable.

## The rules it exists to keep

**Structure is fixed; words are open.** Actions and types are any words. Every
word but the defaults — `read`, `create`, `update`, `delete`, `manage`, and
`anything`/`anywhere` — has a definition in the policy, and the defaults cannot
be given one, so no definition quietly widens `read`. A definition no statement
uses is refused: a person reads every one.

**An allow names where it applies.** Only a deny may say `anywhere`. A
condition is present and non-empty, or absent. No word, definition or
condition holds a control character: all of it is shown to the person
approving it.

**Order never matters.** `Evaluate` allows when every atom matches some allow,
or is a supporting read, and no atom matches any deny. A known deny refuses
outright; an atom every allow is known not to cover, and known not to be a
supporting read, is refused; anything unknown that could change the answer
leaves it `Undecided`, for whatever decides next. Only a judge says an atom is
a supporting read (§2); a fast path says `No` for a write and leaves a read it
cannot place `Unknown`. Callers supply the
parts — a fast path, a judge — and never combine them themselves.

**Variables are a whole name, from the record, never the discobox.** `$this`,
`$primary.repo|host|branch`, `$sources.<slug>.repo`, and the observed
`$primary.upstream` and `$primary.default_branch` — which resolve to nothing
until observed facts exist. A variable that does not resolve matches nothing.

**Resolved at approval, bound at a decision.** `Resolve` records each
variable's value against the discobox the grant is for; the server does it when
it mints a grant, and ignores any `resolved` a client sends. `Bind` reads the
facts again: a variable matches only when it resolved then and resolves now to
the same value. A policy handed to another discobox resolves and binds against
that discobox.
