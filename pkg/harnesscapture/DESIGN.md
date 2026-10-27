# Harness capture protocol

This package owns the versioned harness export, immutable candidate, redacted
preview, and acknowledged handoff contracts (ADR 26-10-26-573). It has no IO.
Definitions originate in registered bundle metadata. A sandbox export cannot
extend the provider set, refresh endpoint, settings exclusions, or credential paths.

Validate before accepting a candidate or exposing its files as a preview.
Credential objects and exporter output are private; never log them. Public
candidate IDs are random, never hashes of credential material. Delivery baselines
and conditional write revisions have different meanings and remain separate.

Settings preserve arbitrary structured preferences but reject manifest-declared
credential and local paths. The exporter applies those same exclusions before
returning files; server validation is independent of the sandbox. Exclusions
match object path segments (`*` one, `**` any depth). Legacy scalar-allowlist
definitions require a metadata refresh rather than silently dropping protection.
Credential templates permit only
registered metadata leaves and exact sentinel references. Provider OAuth metadata
is authoritative; captured metadata cannot redirect a refresh request.
