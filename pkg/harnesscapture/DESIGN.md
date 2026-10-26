# Harness capture protocol

This package owns the versioned harness export, immutable candidate, redacted
preview, and acknowledged handoff contracts (ADR 26-10-26-573). It has no IO.
Definitions originate in registered bundle metadata. A sandbox export cannot
extend the provider set, refresh endpoint, portable fields, or credential paths.

Validate before accepting a candidate or exposing its files as a preview.
Credential objects and exporter output are private; never log them. Public
candidate IDs are random, never hashes of credential material. Delivery baselines
and conditional write revisions have different meanings and remain separate.

Settings carry only registered scalar leaves. Credential templates permit only
registered metadata leaves and exact sentinel references. Provider OAuth metadata
is authoritative; captured metadata cannot redirect a refresh request.
