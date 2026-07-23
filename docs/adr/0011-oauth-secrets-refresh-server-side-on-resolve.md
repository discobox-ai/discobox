# 0011 — OAuth secrets refresh server-side, on resolve

- **Status**: Proposed
- **Date**: 2026-07-22

## Context

A harness credential is stored as a project `Secret`, injected into a sandbox as
a **sentinel**, and swapped for its real value by the proxy on outbound requests
— the proxy resolves the sentinel through `ResolveSandboxSecret`, which returns
the decrypted value only while a live grant covers the sandbox (ADR 0009,
`proxy/DESIGN.md`). Every secret type so far — `git`, `ssh`, `bearer` — holds a
static credential: stored once, served unchanged until replaced by hand.

Claude Code's subscription login is not static. `/login` yields an OAuth **access
token** that expires in ~8 hours plus a **refresh token** used to mint the next
access token. The refresh token *rotates on every use*: a successful refresh
returns a new refresh token and invalidates the one presented. The current
claude-code configure path sidesteps this by using `claude setup-token`, which
mints a single ~1-year token with no refresh token — long-lived, non-rotating,
and exactly the shape the platform already handles as a `bearer`.

We want the rotating credential instead: shorter-lived access tokens, refreshed
automatically, with the long-lived refresh token as the only durable secret. That
forces two questions the static types never raised — *who* refreshes, and *when*
— and the rotation makes a wrong answer actively destructive: two parties
refreshing the same credential each spend a refresh token the other still holds,
and one of them is now bricked until a human re-logs in.

## Decision

**An `oauth` secret type refreshes lazily inside `ResolveSandboxSecret`, on the
control plane, serialized per secret. The grant is unchanged; the token's own
expiry drives re-resolution.**

### 1. `oauth` is a bearer that carries its own refresh material

`SecretValue` gains `RefreshToken`, `TokenURL`, `ClientID`, and
`AccessTokenExpiresAt`. The current access token stays in `Token`, so the proxy
swap is byte-identical to a `bearer` and needs no change. The resolve handler
already emits `Token` alone, so the refresh token, endpoint, and client id
**never leave the control plane** — they are used only to refresh and are dropped
on the way out.

### 2. The control plane is the single writer, refreshing on resolve

`ResolveSandboxSecret` is the one place that decrypts and hands out a value. When
the access token is within `oauthRefreshSkew` (5 min) of expiry, it POSTs
`grant_type=refresh_token` to `TokenURL`, re-encrypts the rotated pair, and
returns the new access token. There is no background refresher: a token is
refreshed exactly when a request needs it and never for an idle sandbox.

Rotation safety is structural, not best-effort:

- A per-secret `singleflight` collapses concurrent resolves in one process onto a
  single upstream refresh, so a rotating refresh token is spent once.
- The persisted write is guarded by the row's `updated_at`
  (`UpdateSecretValueIfUnchanged`): a refresh that lands in another process bumps
  `updated_at`, so the loser sees zero rows affected, re-reads, and serves the
  winner's freshly rotated credential rather than clobbering it with a spent one.

### 3. The token's expiry, not the grant's, drives re-resolution

The resolution's `ExpiresAt` is capped at `min(grant expiry, access-token
expiry)`. The proxy caches a resolved value until that time, so its cache lapses
right as the token ages out and it re-resolves — which is what triggers the next
refresh. The **grant stays persistent**: consent means "this harness is
configured", not "this 8-hour token is valid", so tokens rotate silently with no
re-approval.

### 4. Refresh failure is not fatal while a token is on hand

If the upstream refresh fails but a (near-expired) access token is still stored,
that token is served and the error is dropped. A `401` from Anthropic at use time
is the authority on whether the credential still works — the same principle ADR
0009 applies to a revoked credential's sentinel.

### 5. Configure captures the rotating blob via `/login`

`claude-code/configure.sh`'s subscription path runs `claude /login` (equivalent
to starting claude and typing `/login`), then reads the full `claudeAiOauth` blob
(access token, refresh token, expiry) from `~/.claude/.credentials.json` and
returns it as an `oauth`-typed secret, with the fixed Anthropic
`tokenUrl`/`clientId` filled in by the script. It no longer uses
`claude setup-token`. Because the configure sandbox has no source, the image's
`.claude.json` trusts no directory, so the script first merges
`hasTrustDialogAccepted` for the workspace into `~/.claude.json` — otherwise
`claude /login` stops at the trust dialog.

## Alternatives rejected

**Keep using `claude setup-token` (the long-lived token).** The status quo, and
the simplest: it is a static `bearer`, no new type, no refresh. Rejected because
a ~1-year non-rotating token is precisely the credential we want *not* to hold. A
short-lived access token behind a rotating refresh token limits the blast radius
of a leak to hours and makes revocation meaningful; the long-lived token is a
standing liability sitting in every sandbox's proxy path.

**Refresh in a background loop instead of on resolve.** A reconciler wakes
periodically and refreshes tokens near expiry. Rejected: it is a second moving
part that must track which secrets are in use to avoid refreshing dead ones, and
it buys nothing — the lazy path already refreshes exactly when a request needs
the token and never for an idle sandbox. It also widens the rotation-collision
window (the loop and a live resolve can both decide to refresh) that the
on-resolve path closes with one lock.

**Let the pool-agent or proxy hold the refresh token and refresh it.** The
component doing the swap already fetches the value; let it refresh too. Rejected
because the refresh token would then have to leave the control plane, and each
proxy process keeps its own value cache — several caches, several potential
refreshers, no shared point to serialize them. Only the control plane sees every
resolve converge, so only it can be the single writer rotation demands. Keeping
the refresh token server-side also keeps the most durable half of the credential
out of the sandbox host entirely.

**Tie the grant's expiry to the access token's expiry.** Set
`grant.ExpiresAt ≈ token expiry` and treat expiry as the refresh signal.
Rejected because it conflates two different clocks. A grant is *consent*; the
access-token TTL is an operational detail of one credential. If the grant lapsed
every 8 hours, `ResolveSandboxSecret` would return `pending` and raise a
`SecretRequest` — asking the user to re-approve a harness they already
configured. Capping only the *resolution's* `ExpiresAt` gets the proxy to
re-resolve on the token clock while the grant stays on the consent clock.

**Block at the proxy (fail-fast 503 / retry) during refresh.** Have the proxy
return a retry signal while a refresh is in flight and rely on the client to
re-drive. Rejected as unnecessary: the resolve call is already synchronous, so an
outbound request simply waits out the one extra round-trip while the server
refreshes, and never sees a half-rotated token. The only serialization that
*must* exist is server-side, across proxies — which the singleflight plus
`updated_at` guard provides — and a proxy-side block cannot provide it anyway.

**Compare-and-swap on the stored ciphertext for the atomic write.** Guard the
refresh write by the old encrypted value rather than `updated_at`. Rejected
because sealing is non-deterministic (per-write nonce), so equal plaintext
produces unequal ciphertext and the guard would never match. `updated_at` is a
monotonic per-row version that already advances on every write.

## Consequences

- `model.go` gains `SecretTypeOAuth` and four `SecretValue` fields; the `Secret`
  and `SecretRequest` type enums list `oauth`. The new fields are server-only —
  the resolve handler still emits `Token` alone, so nothing reaches the pool that
  did not before.
- `store.UpdateSecretValueIfUnchanged` adds an `updated_at`-guarded value write,
  reusing the existing `ErrGenerationConflict` optimistic-concurrency signal.
- `resources/secrets` gains `oauth.go` (refresh HTTP call, freshness test,
  per-secret `singleflight`) and an `oauth` branch in `ResolveSandboxSecret`.
  `golang.org/x/sync` becomes a direct dependency.
- `claude-code/configure.sh` replaces the `setup-token` path with a `/login`
  capture and emits an `oauth` value; `applyConfigureOutput` stores it verbatim,
  as it already does for `bearer`. `harness/claude-code/driver_test.go` asserts
  the new contract.
- **The proxy and pool-agent are unchanged.** They already honor the resolution's
  `ExpiresAt`; capping it at the token expiry is the entire integration.
- A claude-code sandbox must reach `console.anthropic.com` for the access token
  to be usable, and the control plane must reach it to refresh — a new outbound
  dependency of the control plane, not just the sandbox.
- **ToS boundary.** Anthropic restricts Pro/Max OAuth tokens to Claude Code and
  Claude.ai. Running the real `claude` binary in the sandbox stays within that;
  the control plane refreshing the token out-of-band is the part that leans on
  the client-emulation boundary, and is a deliberate, revisitable choice.
- The interactive `/login` capture cannot be exercised without a browser, so
  `configure.sh`'s OAuth path is validated by contract test and shell syntax but
  needs a live configure-sandbox run before it is trusted end to end.
