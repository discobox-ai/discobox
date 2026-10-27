# 26-10-27-939 — A route may name any host

- **Status**: Accepted (narrows [26-10-27-880](26-10-27-880-access-to-a-secret-is-a-binding-held-to-static-routes-and-no-judge-decides-it.md) §4's hosts and §8's migration of a secret's host, of a harness config's uncovered binding, and of several grants on one variable)
- **Date**: 2026-10-27

## Context

ADR 26-10-27-880 §1 says a secret with no routes is sent nowhere, and §4
lets a route name exact hosts or `*.suffix`. Nothing can say "any host".

Many secrets today say nothing about where they go, and are sent anywhere:

- every inline secret a create carries (`discobox run -s KEY=value`), which
  is stored with no host and no routes;
- a secret bound through a grant with no hosts — the wildcard grant, an
  explicit administrative act (ADR 26-10-02-393 §1).

Under §1 as written, the upgrade that turns grants into bindings would leave
each of these sent nowhere, and an inline secret could not be created without
naming a host. Neither is what the person who made them asked for, and the
wildcard grant was asked for in so many words.

## Decision

### 1. `*` is a route to any host

A route's host may be `*`: it covers every host. It is the least specific
route there is — any route naming a host is more specific — so within one list
a narrower route, open or blocked, still answers for what it covers (880 §4).
It is written, stored and shown as `*`, and its path, when it has one, is
matched as on any other route.

A secret with no routes is still sent nowhere (880 §1). `*` is how a person
says "anywhere", rather than saying nothing.

### 2. What is created without a host gets `*`

An inline secret a create carries with no host is stored with the route `*`.
One carried with a host gets that host as its route. A secret a person creates
through the API or the CLI gets the routes they name and nothing else; the CLI
asks for at least one.

### 3. The upgrade gives `*` where today's credential goes anywhere

In addition to 880 §8:

- A secret with no host and no routes gets the route `*`.
- A grant with no hosts becomes a binding with the route `*`, so the binding
  keeps covering what the grant did.

Each is logged, so a person can find the credentials that go anywhere and
narrow them.

Two more upgrade rules 880 §8 leaves open, which no request can answer:

- A harness config's binding that no live grant covered is dropped, with the
  sentinels it delivered, and logged. §8 turns an uncovered binding into a
  pending request, but a harness config has no discobox to file one: what
  asks is a sandbox.
- Several live grants covering one target's variable become one binding,
  whose routes are every one of their hosts and whose expiry is the earliest
  of theirs. One binding per variable cannot keep them apart, and the
  earliest expiry is the one that widens nothing.

## Alternatives rejected

- **Sent nowhere until a person adds routes.** 880 §1 read literally. Every
  inline secret and wildcard grant stops working at the upgrade, and an inline
  secret could not be made without naming a host.
- **No routes means any host.** It keeps today's behaviour by keeping today's
  silence: a credential that goes anywhere would look like one nobody has
  thought about, and §1's "nowhere" would mean "everywhere".

## Consequences

A credential that goes anywhere says so in its routes, where a person reads
it and can narrow it. Lists, listings and the console show `*` as a route.
