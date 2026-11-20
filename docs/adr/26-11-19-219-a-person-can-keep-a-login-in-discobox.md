# 26-11-19-219 — A person can keep a login in Discobox

- **Status**: Proposed
- **Date**: 2026-11-19
- **Issue**: [#1475](https://github.com/discobox-ai/discobox/issues/1475)

## Context

Disco Desktop signs a person in and delegates credential storage and renewal
to Discobox. Keeping a second copy of an OAuth refresh token creates two
renewers for a grant whose refresh token may rotate. The desktop needs to
spend its own GitHub access token, save harness logins obtained outside a
box, and show which accounts are connected.

Today the secret endpoints return metadata, and
`server/internal/resources/secrets/renew.go` owns renewal. The raw-delivery
opt-in in [ADR 26-10-27-840](26-10-27-840-a-binding-may-hand-over-the-raw-value-held-in-memory-and-watched-for-at-the-proxy.md)
allows a person to make a raw binding. That delivery has binding and proxy
controls. A token returned to a client can be spent outside those controls.

The three-call harness workaround writes a secret, a binding, and saved files
independently. `server/internal/resources/harnessconfigs/service.go` copies
configured files from the generic update. The transaction in
`server/internal/resources/harnessconfigs/capture.go` validates saved files
and credentials, tracks which secrets the config owns, and prepares delivery
to the originating sandbox.

## Decision

### 1. A person may reveal a secret's current token

`POST /projects/{projectId}/secrets/{secretId}/reveal` requires normal project
authorization through `ProjectAuthorizer` and a directly authenticated user
principal with a user ID. Issuer-asserted users, pools, and sandboxes are
excluded. The sandbox role omits the route,
and the service enforces the principal requirement even when called outside
that router.

A reveal requires the secret's `allowReveal` opt-in. It defaults to false,
including for all existing secrets. A directly authenticated user may set it
when creating the secret. The setting is immutable for that secret ID.
Metadata edits, credential replacement, OAuth renewal, and harness saves
that update an existing secret preserve it. Automatically created harness
secrets start with reveal disabled.

To make an existing secret revealable, create a new secret with
`allowReveal: true` and supply a fresh credential from its source. Move any
needed bindings to the new secret and remove the old secret when it has no
remaining consumers. For a rotating OAuth grant, obtain a new login and
avoid registering the same refresh token with two secret IDs. To withdraw
reveal, retire the revealable secret and, if still needed, recreate it with
reveal disabled. Revoke any already disclosed token at the provider when
immediate invalidation is needed.

`SecretReveal` contains the token, its expiry when known, and the revision
of the same persisted secret snapshot. A token secret yields its stored
token. An OAuth secret yields its access token. Use the earlier known bound
when the secret lifetime and OAuth token expiry both apply. The secrets
service uses the existing renewal path, including its skew and guarded persistence of a
rotated refresh token. After renewal it checks current reveal authorization
and reads the token, expiry, and revision together. A failed renewal may
leave an access token that is still within its stated lifetime. An empty or
known-expired token produces a sanitized error. The response has no refresh
token, OAuth client secret, or sealed value.

The route sends `Cache-Control: no-store`. Clients may retain the access token
in process memory until the stated expiry. When expiry is unknown, discard
the token after the operation that needed it. Use a fresh reveal after an
upstream refusal. Disable persistent and HTTP response caching. Every attempt records its outcome using authenticated actor
identity, project and secret IDs, and the disclosed revision on success.
Record the successful disclosure before returning the token. Request and
response bodies, token values, upstream response bodies, and renewal
credentials stay out of logs and traces.

### 2. A person may save a harness login directly to its config

`POST /projects/{projectId}/harness-configs/{harnessConfigId}/logins` requires
the same direct user requirement and the project membership authorization
used for config edits. The request carries the config revision the client read,
credentials (`provider`, `envName`, `type`, `value`, and optional account
metadata), and normalized files (`path`, `content`). Provider is the
registered save-back provider ID. The server resolves the provider and
variable together against the current definition, including its
multiple-account naming rule. A client supplies the normalized documents.
The server validates them using the same file, credential, size, and
reference checks as sandbox save-back.

Use the registered provider's credential routes and OAuth refresh settings. Validate the
normalized save against the locked manifest and revision. The external API takes no client-supplied token URL, OAuth client ID, request
encoding, or route policy. OAuth capture uses the registered public-client
contract. A provider that requires explicit manual routes or lacks registered credential
routes remains on the ordinary secret and binding API. The external path
does not inherit the legacy third-party capture fallback to any host. Validate that each credential reference
names a registered variable backed by a submitted credential or a usable
config binding after the proposed changes. Reject issued sentinels as
credential values.

Both entry points use the same transaction for the validated save: lock the
config and affected secrets in the existing order, patch saved files by
path, update an owned secret in place or create a config-owned secret,
replace the config's use binding, sync configured routes, and mark the config
and affected delivery dirty. New owned secrets use the save-back lifetime
with no binding TTL ceiling. Route changes preserve existing denials.
Replacing a binding to an independently managed secret leaves that secret
and its other bindings intact. A reset takes back the saved files and owned
secrets. Person-authored overrides keep their existing precedence and reset
semantics. A value replacement advances its delivery generation so stale
upstream refusals cannot be attributed to the new login. Attribute current
upstream rejections to the owning config through the existing binding and
generation path. Attribute rejected save requests to the authenticated user
and config with sanitized reasons.

The external save compares the submitted config revision inside the
transaction. It returns the committed config revision, without credential
material. At most one save based on a given revision commits, including
concurrent identical requests. A replay after success receives a conflict
and performs no writes. After an ambiguous outcome, the client re-reads the
config and treats a conflict as stale state. It must obtain a fresh sign-in
before submitting another save if it cannot establish that the first one
landed. Automatically resubmitting an old refresh token against a newer
revision could restore a token already rotated by Discobox.

Sandbox capture keeps its candidate receipt, overwrite semantics, origin
checks, and delivery acknowledgment. The external route has a config
revision precondition and produces neither a sandbox receipt nor a handoff.
It uses ordinary reconciliation to reach boxes running the config.

### 3. A secret can name its account and plan

A secret accepts optional typed display fields `account` and `plan`.
Account is a provider-supplied or person-entered display identifier such as
an email address or login. Plan describes the account's tier and applies to
token and OAuth secrets. Existing OAuth `subscriptionType` stays in its
current response for compatibility. A client can use it as a display
fallback when no plan was supplied.

These fields are display metadata for the stored credential. Authorization
continues to use the authenticated principal and the existing resource rules. Exporters can supply metadata they already
have, such as the
Claude account email, the Codex email from its local identity token, or
the Copilot login. The server accepts bounded display strings and stores
them with the credential. The exporter extracts the identifier locally and submits only the display
metadata.
Existing rows have no new metadata until supplied. On a metadata-only edit,
omission preserves a field and an explicit null clears it. When a caller
replaces the credential, omissions clear the corresponding fields to avoid
labeling a new login with the previous account. Server-side OAuth renewal
and capture entries that keep a credential preserve both fields.

Include these fields in secret list and read responses for directly
authenticated users authorized in the project. Account identifiers may be
personal data. Sandbox-scoped listings, issuer-asserted users, pool
resolutions, request previews, audit records, and delivery payloads omit
them.

## Alternatives considered

- Reuse `allowRaw` for reveal. Existing grants authorize delivery into a box
  under binding and proxy controls. Reusing them would let a client spend
  the token outside those controls without new consent.
- Allow an update to change `allowReveal`. Creation-time consent keeps an
  existing secret ID from being converted into a disclosure channel by a
  later edit. Changing disclosure policy requires a new secret ID.

- Use the generic secret response to expose the token. Existing clients read
  that response for inventory, and adding a token there would give those
  reads disclosure and caching semantics they did not request.
- Keep the three public writes or delegate the workflow to a temporary
  sandbox. Separate writes can leave partial state and bypass ownership and
  validation. A temporary box introduces a delivery origin for a login
  already completed on the user's machine.
- Replay external saves by overwriting the latest revision as in sandbox
  capture. A delayed retry can overwrite a more recent login or restore a
  spent refresh token. A durable idempotency receipt could recover the
  original result, but would add a retention contract and credential-bearing
  request comparison. The revision precondition gives a bounded retry
  contract without such a receipt.
- Store account details in arbitrary labels. Typed fields give clients
  consistent clearing and visibility rules and avoid putting personal
  identifiers in generic label views.

## Consequences

This proposal adds a narrow disclosure exception to the secrets design and
extends the shared save rules in
[ADR 26-10-26-573](26-10-26-573-a-harness-exports-local-configuration-back-to-its-config.md).
The overwrite decision in
[ADR 26-11-04-538](26-11-04-538-saving-a-harness-candidate-overwrites-without-a-revision-conflict.md)
continues to govern sandbox candidates. Provider destination policy and
person-owned files follow
[ADR 26-11-04-772](26-11-04-772-built-in-harness-credentials-have-explicit-provider-destinations.md)
and
[ADR 26-11-04-711](26-11-04-711-a-reset-keeps-a-persons-harness-files-and-a-save-never-writes-them.md).

Implementation decides the exact wire representation for absent expiry,
the audit event names and storage, and the display-field length limits.
