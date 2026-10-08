# 26-10-08-452 — A secret may be a key the server exchanges for a short-lived token

- **Status**: Accepted
- **Date**: 2026-10-08
- **Relates to**: [ADR 0011](0011-oauth-secrets-refresh-server-side-on-resolve.md)
  (an OAuth access token is renewed on resolve);
  [ADR 0132](0132-a-credential-rejected-after-its-retry-is-recorded-against-its-secret.md)
  (a refused credential is renewed before it is recorded);
  [ADR 0149](0149-a-host-certificate-is-trusted-for-one-sandbox-when-a-person-pins-it.md)
  (a person, not a discobox, says where traffic may go)

## Context

Some APIs do not take their long-lived key on every call. boxd takes a `bxd_`
API key once, in a JSON body, at `POST https://app.boxd.sh/api/v1/auth/token`,
and answers with an hour-long JWT that every gRPC call then carries as
`Authorization: Bearer`. The same shape is common: Vault AppRole
(`role_id`/`secret_id` for a client token), Docker Hub (username and password
for a token), and OAuth's client-credentials grant (RFC 6749 §4.4).

None of it works from a discobox. The proxy swaps sentinels in headers and,
optionally, query parameters, never in bodies (`proxy/DESIGN.md`), so the
exchange receives the placeholder and answers 401. Neither secret type fits:

- `token` is one string handed out as stored. Storing the JWT works for an hour.
- `oauth` renews by spending a refresh token at a token URL, and everything
  around it is built for that token rotating on use: one renewal at a time,
  guarded by `updated_at`, never retried in another encoding. A static key
  rotates nothing, and its request and response are not OAuth's.

## Decision

### 1. A third secret type, `exchange`

An `exchange` secret stores the fields a person was given (`SecretValue.Exchange`,
such as `api_key`), sealed with the rest of the value and never returned. The
server trades them at a token endpoint for a short-lived token, which it keeps
in `SecretValue.Token` with its expiry in `AccessTokenExpiresAt`, the fields an
OAuth access token uses. The proxy, the judge, grants, and sentinels see a
token in a header, as they do for the other two types.

After the exchange, the renewal is OAuth's: renewed on resolve within the skew
of expiry, one renewal per secret at a time, the resolution's expiry capped by
the token's own, a failed renewal serving the token on hand. A rejection
renews before anything is recorded, as ADR 0132 does for OAuth. An exchange the
endpoint refuses (a 4xx) is recorded as `refresh-failed`: the stored key is
dead, and a person has to replace it.

### 2. The exchange is a recipe the secret carries

The recipe is data on the secret (`Secret.ExchangeRecipe`, given as
`exchange` on create): the https endpoint POSTed to, the fields a person
stores, the body and headers built from them as `{name}`, whether the body is
form-encoded, the path to the token in the JSON answer, and the path to its
expiry. It says where a key is sent, never what the key is, so it is a column
shown on read, not part of the sealed value.

Its URL is where a long-lived key goes, so the recipe is held to five rules:

- **A key goes only where its token may go.** The URL's host must sit inside
  the secret's host binding, and an exchange secret must have one. A key bound
  to `boxd.sh` is exchanged at `app.boxd.sh` or not at all, and a later change
  to the binding — by an update, or by an approval that rebinds the secret —
  is held to the recipe. A redirect from the endpoint is not followed, since
  it would re-send the key wherever it names.
- **A recipe changes only with the key.** An update that names a new recipe
  must replace the value beside it, so a stored key is never pointed at a new
  endpoint by somebody who never had it.
- **Only a person gives one.** A discobox may not, whatever else it may write.
- **It is sound before anything is sent**: https only, every template naming a
  declared field, every field sent, a token path present.
- **It never reaches an internal address**: loopback, private, link-local, and
  the like, checked when the recipe is given and again at the dial. Whoever
  writes a recipe chooses both the binding and the URL, so the binding rule
  bounds the key but not the server's own network position, which a recipe
  would otherwise borrow — and a refused exchange reads the endpoint's answer
  back to them, cut short.

Anyone who may write a project's secrets may give a recipe. The binding rule
is what keeps that safe: it adds no destination a grant on the secret could not
already reach.

### 3. The first exchange happens when the secret is stored

Creating an `exchange` secret, or replacing its value, runs the exchange before
writing. A key the endpoint refuses is refused there, as a 400, rather than at
a discobox's first call an hour later. A stored token also gives the sentinel
its shape from the token, a JWT for boxd, which is what the sandbox's client
expects to hold.

### 4. boxd is set up by a script, not the registry

`scripts/boxd-secret.sh` stores a boxd key with boxd's recipe, bound to
`boxd.sh`. A discobox asks for it by name, variable `BOXD_TOKEN`, and host
`boxd.sh`; the `boxd` CLI and disco-vm both read a token from that variable and
send it as a bearer.

## Alternatives rejected

- **Swap the key into the request body.** It makes the exchange work, but the
  JWT the endpoint answers with lands in the sandbox: a credential with the
  key's full power, usable from anywhere for an hour, and recorded in the
  response spool. Every other credential here never exists inside a sandbox.
- **An `oauth` secret with no refresh token.** It would bring OAuth's rotation
  and encoding rules to a key that has neither, and OAuth's request and
  response shape to an endpoint that does not speak it.
- **A `token` with a refresh command.** A person's client runs the command;
  for an hour-long token that is a person in the loop every hour.
- **Recipes only in the well-known registry.** Every provider would be a code
  change and a release before anyone could store its key, and boxd is not yet a
  credential worth a registry entry. The registry's one advantage — reviewed
  code names the endpoint — is what the binding rule gives a person's recipe:
  the endpoint is inside the host the person already trusts with the token.

## Deferred

- **Recipes in the well-known registry**, as presets a request by ID fills
  in. Worth it once a credential is common enough that every project would
  type the same recipe.
- **Exchanges that need signing**: GitHub App installation tokens, GCP service
  account assertions, AWS SigV4. A recipe is a templated request, and these
  need code.

## Consequences

- `exchange` is a new value in every secret `type` enum. A CLI older than this
  change refuses a secret listing that contains one, as it would any unknown
  enum value; the CLI and server are released together.
- The TUI does not create an exchange secret, since its card has nowhere to
  write a recipe; it opens one, and replaces its fields. `discobox secret
  create --type exchange --exchange-recipe ... --field NAME=-` stores one.
