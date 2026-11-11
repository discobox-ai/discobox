# 26-11-11-857 — A credential stored to answer a request is held to the request's hosts

- **Status**: Accepted (written Accepted in its implementation PR, by the
  owner's instruction for issue #1017; the owner accepts it by merging that PR)
- **Date**: 2026-11-11
- **Issue**: [#1017](https://github.com/discobox-ai/discobox/issues/1017)
- **Leaves standing**: [26-10-27-880](26-10-27-880-access-to-a-secret-is-a-binding-held-to-static-routes-and-no-judge-decides-it.md)
  §§1–4: a secret is sent only on its routes, and every route list a grant
  is held to must allow a request; [26-11-02-022](26-11-02-022-a-credential-request-names-its-policies-and-asks-to-use-and-to-delegate-at-once.md)
  §§3–5; [26-11-03-329](26-11-03-329-a-box-may-be-granted-routes-for-a-time-beside-its-binding-without-replacing-it.md)
  §§4–5; [26-11-04-616](26-11-04-616-whole-credential-is-an-explicit-approval-scope.md)
  §§1–4; [26-10-27-840](26-10-27-840-a-binding-may-hand-over-the-raw-value-held-in-memory-and-watched-for-at-the-proxy.md) §2.

## Context

A box asks for a credential the project does not hold, and a person types it
in to answer. The TUI stored it with every route the request asked for, use
and delegation together, as editable rows of the new-secret form: 26 rows for
discobox-ai/factory-demo's sample run, 20 of them distinct. The approval then
made a grant of the same routes.

Checked against main at `d6c392a3c`:

- `store.RouteLists` holds a grant to the secret's routes, the binding's and
  any parent delegation's, each a separate list; every list must allow a
  request.
- `ApproveSecretRequest` writes the part's requested routes (scope `routes`)
  as the binding's own unless the person narrows them.

So the secret's copy of the request's routes added no restriction the grant
did not already make. It did tie the credential to that one request: the
next request for the same token on another repository finds a secret whose
routes do not cover it, and the person types the token in again under a new
name, or edits the secret.

## Decision

A credential stored to answer a request is held to **the hosts the request
names**, any method and path on them. The grant made with it is held to the
routes the part was approved for: as asked, unless the person changes them.

- The secret still never goes to a host nobody asked for. Its hosts are
  the exact names an open route of the request names
  (`secretroute.OpenRouteHosts`): a host the request names only to block
  adds none (ADR 26-11-04-616 §1), and no pattern is one — not `*`, and not
  `*.suffix`, since nothing here tells a public suffix (`*.com`) from a
  company's, and a box could add a harmless-looking route on one beside its
  real ask. A request naming no exact host has no new credential stored from
  it; the person stores one on the secrets screen with the routes they
  choose.
- The last step says the hosts the new credential will be stored for,
  before it is stored.
- What the box may do is the grant's, as before (ADR 26-10-27-880 §4).
- A later request on those hosts can be answered with the same credential;
  one naming another host cannot, and the dialog says which routes are not
  covered.

The guided dialog that stores it (issue #1017) delivers by sentinel only and
grants a use part as an addition or a replacement as the request asked. An
addition with a secret other than the one delivering the variable is granted
as a replacement, and the dialog says so. Handing over the value itself
(ADR 26-10-27-840) and choosing the other mode stay on
`discobox secret request approve` (`--raw`, `--use-as add|replace`).

## Alternatives rejected

- **The requested routes, as before.** No safer than the grant, which already
  holds the box to them, and it makes a credential nobody can reuse without
  typing it in again.
- **No routes, set by a person afterwards.** A secret with no routes is sent
  nowhere (ADR 26-10-27-880 §1): the approval would grant a credential every
  request refuses.
- **`*`, anywhere, or a host pattern.** The grant would still hold the box to
  its routes, but the secret would carry no ceiling of its own (`*`) or
  nearly none (`*.com`), and every later grant of it would rest on the grant
  alone.
- **Raw delivery and the addition/replacement choice in the guided dialog.**
  Each is a step most answers never need; the owner's design for the dialog
  keeps it to the decisions every request has. Revisit if a box's requests
  regularly need either answered from the TUI.

## Consequences

- A credential typed in once serves every later request on its hosts.
- A request that names a host pattern beside exact hosts gets a new
  credential held to the exact ones only; the routes it asked for on the
  pattern are approved but never sent on. The last step lists them under the
  hosts it is stored for, as a saved secret that does not cover a route is
  marked; a person who wants them stores the credential on the secrets screen
  with the routes they choose.
- A person who wants it narrower edits its routes on the secrets screen (F4);
  the new-credential step does not ask, so it stays one name and one value.
- The secret's routes are wider than the grant's. Anyone binding it by hand
  later (`discobox secret bind`) binds it to its hosts unless they narrow the
  binding, as with any secret stored with host routes.
