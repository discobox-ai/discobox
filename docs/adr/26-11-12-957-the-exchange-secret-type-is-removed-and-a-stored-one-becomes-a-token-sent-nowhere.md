# 26-11-12-957 — The exchange secret type is removed, and a stored one becomes a token sent nowhere

- **Status**: Proposed. On acceptance it supersedes
  [26-10-08-452](26-10-08-452-a-secret-may-be-a-key-the-server-exchanges-for-a-short-lived-token.md)
  in full, including its rejected alternative "swap the key into the request
  body", and 452's status line says so.
- **Date**: 2026-11-12
- **Issue**: [#1066](https://github.com/discobox-ai/discobox/issues/1066)
- **Relates to**:
  - #1012, HTTP body places: a route names a JSON pointer in a request body
    where the pool proxy swaps a sentinel. That is how a key an API takes in
    a body reaches it now. #1012 does not touch `exchange`, and this ADR
    does not decide body places, except for the one point in §4.
  - [26-11-11-080](26-11-11-080-each-disco-vm-driver-is-a-provider-type-of-its-own-name-configured-by-its-own-fields.md)
    §9 (Proposed), a boxd pool's key swapped at `/api_key` by its own proxy,
    and its audit rules for that exchange.
  - [26-10-27-840](26-10-27-840-a-binding-may-hand-over-the-raw-value-held-in-memory-and-watched-for-at-the-proxy.md),
    a raw binding hands a discobox the value itself.
  - [26-10-27-880](26-10-27-880-access-to-a-secret-is-a-binding-held-to-static-routes-and-no-judge-decides-it.md)
    §4, a secret with no routes is sent nowhere.

## Context

Checked against `main` at `e1fad6120`.

ADR 452 added a third secret type, `exchange`, for an API that takes its
long-lived key once, in a request body, and answers with a short-lived token
(boxd: `POST https://app.boxd.sh/api/v1/auth/token`, `{"api_key": …}`, an
hour-long JWT). The server holds the key and a recipe, trades the key for a
token, and hands a discobox only the token. It lives in:

- **Model**: `model.SecretTypeExchange`; `Secret.ExchangeRecipe` (column
  `exchange_recipe`) and `Secret.Exchange` (filled on read);
  `SecretValue.Exchange`, the sealed fields; `model.ExchangeRecipe` and
  `model.SecretExchange`; `exchange` in the `type` enums of `Secret` and
  `SecretRejection.SecretType` (`server/internal/model/model.go`).
- **Service**: `server/internal/resources/secrets/exchange.go` (the request,
  `refuseInternalTarget`, `exchangeClient`), `recipe.go` (`checkRecipe`,
  templates, token paths), the `SecretTypeExchange` cases in `renew.go`, and
  `firstExchange`, the recipe-change rule and `validSecretType` in
  `service.go`. Described under `## Exchange` in its `DESIGN.md`.
- **API**: `ExchangeRecipe`, `SecretExchange`, `SecretValue.exchange`, the
  `exchange` field on create, update and read, and the `exchange` enum value
  (`api/openapi/server.yaml`, and what ogen generates from it).
- **CLI**: `secret create --type exchange --exchange-recipe … --field
  NAME=VALUE` (`cli/internal/cli/secret.go`); the TUI shows one and replaces
  its fields, but never creates one (`cli/internal/tui/secrets.go`).
- **Script**: `scripts/boxd-secret.sh` stores a boxd key with boxd's recipe.

452 kept the key out of a sandbox's reach in a body by rejecting a body swap:
the JWT the endpoint answers lands in the sandbox, with the key's full power,
usable from anywhere for an hour, and recorded in the response spool.

#1012 adds the body swap anyway, for 080 §9's pool proxy and for any route
that names a body place. The owner wants one way to send a key in a body,
and it is the proxy's. `exchange` is the other. It has the server send a
request built from a person's template to a URL they wrote, from the
server's own network position, so it carries its own fence against that
(`internalAddress`, checked again at the dial by `exchangeClient`), its own
template language (`recipe.go`), and its own rules on who may point a key
where.

## Decision

### 1. No `exchange` secret is created, and none is resolved

`exchange` leaves every secret `type` enum, in the model and the API. A
create or update naming it is refused as any unknown type is (a 400). The
`exchange` fields leave the API's create, update, read and value schemas,
with `ExchangeRecipe` and `SecretExchange`. The server makes no exchange, so
`exchange.go`, `recipe.go`, their tests and the exchange half of `renew.go`
go, and renewal is OAuth's alone again. The CLI loses `--type exchange`,
`--exchange-recipe` and `--field`; the TUI loses the exchange card and
choice. `scripts/boxd-secret.sh` goes.

The secrets `DESIGN.md` loses `## Exchange`, and the model `DESIGN.md` names
two types.

### 2. A stored exchange secret becomes a `token` secret sent nowhere

A data step in `Migrate`, with the retirement (the `database/REVIEW.md` rule
on a retired enum value), rewrites every row with `type = 'exchange'`, in one
transaction per secret:

- **Its type becomes `token`.**
- **Its value keeps what a person stored, and nothing the server made.**
  With one stored field (boxd's `api_key`), the token is that field's value.
  With more than one, the token is the fields as a JSON object of name to
  value, so nothing a person typed is lost; no route can use that, and a
  person replaces or deletes it. The last exchanged token, its expiry and
  the field map are dropped from the sealed value, which is sealed again
  with the project's key.
- **Its routes become none**, so it is sent nowhere (880 §4), whatever
  its bindings say. Its `routeConstraints` stay; they only ever narrow.
- **`allowRaw` is cleared**, deleting its raw bindings as clearing it
  always does (840 §1). A raw binding handed over the token; after this
  step it would hand over the key.
- **Its value lifetime is the key's**: `ttl_seconds` 0, no
  `value_expires_at`, and the format re-read from the key unless a person
  set it (`format_set`).
- **Its `secret_rejections` rows go.** They name a recipe that no longer
  exists and a remedy (replace the fields) that no longer applies.

Its plain bindings, grants and delegations stay. They resolve nothing
while the secret has no routes, and work again once a person gives it one.

Then `exchange_recipe` is dropped from `secrets`. A log line per migrated
secret names its project and secret and says it now has no routes.

### 3. Nothing is widened by the migration

Under `exchange` the key went to one URL, from the server, and a discobox
held a token that worked on the secret's routes. A `token` secret holding the
key and keeping those routes would send the key itself on every one of them,
from every bound sandbox. Writing a body-place route from the recipe would
put the token in the sandbox, which `exchange` never did. Both widen what
a person approved, so the migration does neither. A person who wants the
old behaviour's successor adds the route (§5). The cost is that a boxd
secret stops working at upgrade until someone does.

### 4. What a body place owes the response it provokes

452 rejected a body swap because the answer, a token with the key's full
power, lands in the sandbox and in the response spool. The first half is now
accepted, as a raw binding's value in the sandbox is (840): the key goes
only to the one host, method, path and JSON pointer a person named, and the
token is the provider's to bound (boxd: an hour).

The second half is not. The response body of a request the proxy swapped a
body-place sentinel into is never recorded or served by the audit, and a
response header carrying a credential is redacted as a swapped header is.
That is 080 §9's rule for the pool agent's driver route, made the rule for
every body place. It is owed by #1012.

### 5. boxd is a token with a body-place route

With #1012, a person stores a boxd key as a `token` whose route is
`POST app.boxd.sh/api/v1/auth/token` with the body place `/api_key`. A
sandbox holding a `bxd_`-shaped sentinel sends it there, and receives boxd's
JWT. How that route is spelled on the CLI, and whether a script or a
registry entry writes it, is #1012's. This ADR only removes the old path.

## Alternatives rejected

- **Keep `exchange` beside body places.** Two ways to send one key, and the
  one kept would be the one that has the server fetch person-written URLs from
  its own network position. Its fences (`internalAddress`, the dial check,
  no redirects, recipe-within-routes, recipe-only-with-the-key, only a
  person writes one) exist only because of that.
- **Migrate to a body-place route from the recipe.** It is the faithful
  translation for boxd, but it puts a token in the sandbox that 452 kept out,
  without anyone deciding it (§3). It also depends on #1012's route shape,
  and only translates a recipe of one JSON field whose template is exactly
  `{field}`, with no templated headers.
- **Migrate to a `token` with its routes kept.** The key would go wherever
  the token went, as a header, from every bound sandbox (§3).
- **Keep the last exchanged token as the value.** It dies within the hour,
  and the key a person stored would be lost.
- **Delete exchange secrets and their bindings.** It loses a key a person
  stored and the bindings made for it, with nothing to tell them why. A
  secret with no routes shows the same thing safely, and a person
  deletes it if they choose.
- **Refuse to upgrade while an exchange secret exists.** It stops a server
  over one credential, for an operator who may not be the person who
  stored it.

## Consequences

- A boxd secret stored with `scripts/boxd-secret.sh` stops resolving at the
  upgrade. A sandbox bound to it gets nothing swapped until a person gives the
  secret a route, which needs #1012.
- A CLI older than this change still lists secrets (the enum lost a value,
  not gained one). It can still send `--type exchange`, which the server
  refuses.
- The exchange's fence against internal addresses goes with it
  (`internalAddress`, `refuseInternalTarget`, `exchangeClient`). An OAuth
  refresh, which also POSTs to a URL captured with the secret
  (`SecretValue.TokenURL`, through `tokenHTTPClient`), keeps the rules it
  has today. Whether it should take that fence is not decided here.
