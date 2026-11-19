# 26-11-19-641 — Harness bundles declare display metadata and public sign-in

- **Status**: Proposed
- **Date**: 2026-11-19
- **Issue**: [#1458](https://github.com/discobox-ai/discobox/issues/1458)

## Context

Disco builds its picker from the project's harness configs and maintains a
second table keyed by slug for presentation and login. A new harness or a
local variant can appear in the list before the client knows how to show it
or start its login.

On main at `9926bf17f`, `harness/driver.go` already reads the bundle's
`harness.description`. `snapshotImageMetadata` in
`server/internal/resources/harnessconfigs/service.go` copies the save-back
definition and other bundle fields, omitting the description.
`server/internal/handlers/harness_configs.go` converts the stored configs
into the response declared in `api/openapi/server.yaml`.

The capture contract in [ADR 26-10-26-573](26-10-26-573-a-harness-exports-local-configuration-back-to-its-config.md)
uses registered provider definitions to validate captured credentials.
`pkg/harnesscapture/capture.go` requires refresh metadata when a provider
captures type `oauth`. Claude Code and Codex declare refresh metadata in
their bundles. Copilot captures a plain token even when the user obtains it
through OAuth device authorization.

## Decision

### 1. Publish the bundle's presentation with the config

The `harness` object in `image.json` declares optional `description`,
`vendor`, `icon`, `accentColor`, and `homepage`. Publish these as
optional read-only fields on `HarnessConfig` in the existing project list
and individual config responses. The config's name and slug still identify
the selectable config. A child can have a name such as “Codex (work)” while
sharing the bundle's presentation.

Descriptions and vendor names are bounded plain text. Authors supply and
license their mark as SVG path data in a fixed 24×24 view box. Validate the
path grammar and bound its size and complexity before publication. A client
creates its own SVG and sets the path through the DOM's `d` attribute with
a `currentColor` fill. The format accepts only path data, with no SVG
markup, URLs, styles, or executable content. `accentColor` accepts a
six-digit hex color and defaults visually to the client's text color.
`homepage` is an absolute HTTPS link opened only on user action. Treat all
text as text and display a generic mark when no icon is available.

### 2. Declare sign-in on the existing provider

Add an optional `signIn` to `harness.saveBack.providers[]` and its
read-only API representation. The provider ID identifies the option within
the selected config. `signIn` has an `account` label, public `clientId`,
`tokenUrl`, optional ordered `scopes` as individual strings, and a
discriminated flow. The token URL is the final exchange endpoint, or the
poll endpoint for the standard device grant.

| Flow kind | Additional fields | Meaning |
| --- | --- | --- |
| `pkce-paste` | `authorizeUrl`, `redirectUri` | Browser authorization with S256 PKCE and per-attempt state. The user returns the provider's callback result to the initiating client, which verifies state before exchanging the code. Claude Code uses this shape. |
| `device-code` | `deviceAuthorizationUrl` | Request a device code, show the provider's verification URL and user code, and poll the token URL with the device grant, honoring pending, slow-down, expiry, and cancellation. Copilot uses this shape with the CLI's public OAuth app and scopes. |
| `codex-device-code` | `deviceAuthorizationUrl`, `verificationUrl`, `pollUrl`, `redirectUri` | Use the [Codex device protocol](https://github.com/openai/codex/blob/main/codex-rs/login/src/device_code_auth.rs). Poll for the authorization code and exchange it with the verifier returned by the poll response using the declared redirect URI. |

Clients implement these fixed protocols, including their request encodings
and response parsing. A declaration supplies data only. Clients can skip
unsupported flow kinds and offer sign-in inside a box. The available
options come from the provider array, including on bundles with several
providers, with no lookup keyed by harness slug.

Keep the existing `oauth` field and its refresh semantics. When a provider
declares both, validation requires the same client ID and token URL in
both declarations. Existing refresh-only bundles still decode and renew.
Copilot can declare `signIn` with `types: ["token"]` and no `oauth`.
Presence of `signIn` confers no capability to refresh, capture, or route a
credential. Both definitions come from the registered bundle, and the
public contract contains no client secrets or per-attempt login material.

### 3. Treat bundles as data at the client trust boundary

Bundle inspection validates field bounds, the required fields for each
flow, and absolute HTTPS URLs with hosts and without userinfo or fragments.
This is format validation. Authority to make external requests belongs to
the client or its deployment.

An external login requires user action and an independent client trust
policy. The policy approves the effective flow, client ID, requested scopes,
redirect URI and endpoint set, including permitted verification URLs returned
by device endpoints. A bundle, its vendor label, or its slug cannot grant
itself approval. Clients can ship approvals for known grants and deployments
can add vetted third-party grants through their own policy. Without matching
approval the config remains visible and its in-box login remains available.

Enforce approval before opening an authorization or verification page, and
before sending credentials or login artifacts to an endpoint. Show the
actual destination host in the sign-in UI. Keep each attempt tied to its
approved declaration. A changed declaration requires a matching approval
before a new attempt can start. Network clients performing exchanges or
polling use approved public destinations, refuse HTTP redirects on those
requests, and exclude private, loopback, link-local, and infrastructure
addresses at connection time. They send only protocol fields for that
attempt, without Discobox credentials or ambient cookies. Any later
server-side login broker must enforce the same boundary itself. Listing
configs never fetches any declared URL or starts a login.

### 4. Use the existing snapshot lifecycle

Include the fields in `snapshotImageMetadata`, persistence and
`HarnessConfig.InheritBundle` in `server/internal/model/model.go`.
Registration, built-in seeding, bundle replacement, and explicit refresh
consume the validated bundle declaration. Replacing or refreshing clears
fields that the new bundle omits. If seeding skips unchanged digests, account
for snapshots created before these fields were persisted so that built-ins
can pick up their already-declared description at the same pin.

Use the atomic root and child publication in
`server/internal/resources/harnessconfigs/extends.go` and
[ADR 26-11-04-426](26-11-04-426-a-harness-config-follows-its-parents-bundle-and-keeps-its-own-account.md).
Children receive the same display and provider declarations and keep their
own account and bindings. Invalid metadata refuses publication without
disturbing the old snapshot. Existing rows remain readable with absent
fields until a snapshot writer populates them. Older bundles remain
registerable, and clients fall back to the config name, a generic mark and
the ordinary in-box login. Reads use the stored snapshot.

## Alternatives rejected

- **Keep client tables or infer login by slug.** A renamed or inherited
  config, third-party bundle, or additional provider cannot be described
  reliably by a harness slug. A central table of approvals can govern
  destinations without becoming a second catalog of harnesses.
- **Combine acquisition and refresh in the existing `oauth` object.**
  Independent capabilities could be modeled there, but consumers already
  interpret that object's presence as registered refresh parameters. An
  additive sign-in object on the same provider preserves the meaning of
  stored definitions and still lets validation prevent grant drift.
- **Return arbitrary SVG or fetch an icon URL.** The mark needs neither
  markup features nor network access. Path data permits tinting and avoids
  giving a bundle control of an active document or a remote image request.
- **Trust any HTTPS endpoint in a registered bundle.** HTTPS authenticates
  a host, while scope, client ID and endpoint approval determine whether
  that host is entitled to receive the sign-in attempt.

## Consequences

Delivery should demonstrate the list response for a new bundle, an existing
config after refresh, a child after its parent refreshes, and an older
bundle with absent metadata. It should exercise a refresh-only provider and
a sign-in-only token provider, invalid presentation metadata, and an
unapproved endpoint or changed grant. A picker can render from the list
alone. External sign-in availability depends on policy approval and
support for the declared protocol.

The exact storage layout, numeric metadata limits, approval policy
configuration and picker layout are implementation choices. External
credential persistence, binding, credential document construction and
account replacement belong to the separate external sign-in storage work.
