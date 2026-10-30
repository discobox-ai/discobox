# 26-10-30-775 — A user can force-save a reviewed harness candidate

- **Status**: Accepted (narrows [26-10-26-573](26-10-26-573-a-harness-exports-local-configuration-back-to-its-config.md) §3's conflict-resolution requirement)
- **Date**: 2026-10-30

## Context

Harness save-back rejects a candidate when its config or credential revisions
no longer match. The preview already compares its settings against the current
stored files. Requiring a new candidate and explicit reconsider selections for
every conflict prevents a user from saving the reviewed changes even when they
want those changes to win.

## Decision

A user may explicitly force-save the reviewed immutable candidate. Ordinary
saves still return 409 for a stale config or credential revision. The save API
accepts `force=true`, exposed as `admin harness save --force`. The console offers
an overwrite confirmation after a revision conflict and retries the same
candidate with force only when the user accepts. Cancellation sends no retry.

Force skips only config and credential revision comparisons. The transaction
still locks the current config and bound secrets, validates the candidate and
its manifest, checks the sandbox's current harness assignment, applies the
candidate patch, and records the usual receipt and dirty marks. Unrelated
files remain intact; a hand-bound project secret is not modified in place.
Repeated saves use the durable receipt rather than replaying credentials.

The API marks an overridable revision conflict with problem type
`urn:discobox:harness-config-revision-conflict`. Other conflicts preserve their
recovery instructions and do not offer an overwrite that cannot succeed.

This narrows ADR 26-10-26-573 §3: force may commit the original candidate without
creating a replacement against current conditional revisions. Reconsider
remains available for selecting changes against the latest remote state.
User authorization, immutable candidate identity, delivery provenance,
credential validation and acknowledged sentinel handoff retain their existing
rules. Replacing a captured OAuth value remains a deliberate choice that may
already be stale; force does not resolve upstream OAuth refresh races.

## Alternatives

- **Require reconsider for every conflict.** Retains the previous revision
  discipline but makes the reviewed diff insufficient for a deliberate overwrite.
- **Reload revisions and retry automatically.** Conceals a concurrent change and
  turns a normal save into authorization to replace it. Explicit force or
  console confirmation makes the user's choice visible.
