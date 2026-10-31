# 26-10-30-457 — Harness settings are literal and credentials use JSON bindings

- **Status**: Accepted (supersedes 26-10-26-573 §§1, 4, 6, 7 only for executable credential/config file templates and Codex trust templating; 0059's sandbox credential-file rendering mechanism; 26-10-14-718 §2, 26-10-15-259 §2, and 26-10-15-848 §3 only for templated credential interchange documents)
- **Date**: 2026-10-30

## Context

A settings review compares saved template source with normalized preferences.
Codex's workspace trust suffix therefore appears as an edit alongside a real
TUI preference change. Template evaluation also exposes the public sandbox
configuration as a runtime programming interface inside otherwise ordinary files.

The agreed product decision is to remove file-template support. Credentials
already arrive as a current env-name-to-sentinel map and still require per-box
installation, but ordinary preferences do not need runtime evaluation.

## Decision

1. Shared and repository settings are literal. File contracts expose no
   template flag, settings suffix or public-configuration rendering context.
2. Registered credential documents use exact JSON objects
   `{"$secret":"ENV_NAME"}` at schema-registered secret leaves. The sandbox
   resolves them using the current sentinel map and JSON encoding. This is
   data binding, without conditionals, loops or expression evaluation. Missing
   and unregistered bindings fail rather than writing empty credentials.
3. Bundles retain static system defaults and hooks. Harness startup owns
   unavoidable local state: Codex supplies workspace trust as a process config
   override; Claude merges onboarding and trust into local state under the
   capture lock. Portable exports omit generated trust and other local state.
4. Configure seeding, installation, repair, capture, reconsideration and handoff
   all consume the same reference representation. Credential generation checks,
   acknowledged handoff, user-authorized save, and protection of newer local
   logins remain as decided in 26-10-26-573.
5. Upgrade durable files, save-back definitions, receipt baselines, sandbox
   bootstraps and capture checkpoints without evaluating legacy source. Convert
   exact credential references and known shipped trust suffixes. Unknown
   executable templates fail explicitly, preserving the original stored state.

The proxy's authorization, sentinel replacement, OAuth refresh ownership and
401 retry behavior remain unchanged. Native database stores remain writable
only through their CLI's supported export/import operations.

## Alternatives rejected

- Preserve templates and hide generated hunks in the UI: that fixes one review
  surface while leaving configure, capture and validation coupled to source
  programs instead of preferences.
- Write generated trust into shared user settings at every launch: this makes
  runtime bookkeeping part of the document capture is trying to review. Prefer
  a process/system layer; use a serialized local state update when the vendor
  offers no suitable separate layer.
- Put credential values in the bundle: sentinels belong to runtime bindings,
  not reusable bundle content. Explicit JSON references preserve portable
  metadata while installing each sandbox's own sentinel.

## Consequences

Preferences can be parsed and compared without rendering, and literal template-
like text remains literal. Harnesses still own vendor-specific local state and
credential conversion. Existing registered bundles using retired executable
attributes require refresh/rebuild; persisted supported documents migrate in
place rather than being discarded.
