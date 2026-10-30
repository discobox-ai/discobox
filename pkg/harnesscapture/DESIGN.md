# Harness capture protocol

This package owns the versioned harness export, immutable candidate, redacted
preview, and acknowledged handoff contracts (ADR 26-10-26-573). It has no IO.
Definitions originate in registered bundle metadata.

It also owns the route a harness bundle declares (`Route`, a
`secretroute.Route`): for a secret the image declares (`harness.Secret.Routes`)
and for the secret a save-back provider writes alike. A bundle built before
routes named host patterns declares `{host, path}`, and a route a judge read
also a `use`; that shape reads through `secretroute.FromLegacy` (h and `*.h`,
p and below, a use as blocked) until the bundle is rebuilt. A legacy route
`FromLegacy` cannot convert is refused, so the bundle does not load — failing
closed its own way — where the server's stored routes and the pool's old file,
which must still be read, go through `secretroute.FromLegacyList`. A sandbox export cannot
extend the provider set, refresh endpoint, settings exclusions, or credential paths.

Validate before accepting a candidate or exposing its files as a preview.
`FileChange.Previous` is the control plane's, set on a preview from its stored
config; a candidate carrying one is invalid.
Credential objects and exporter output are private; never log them. Public
candidate IDs are random, never hashes of credential material. Delivery baselines
and conditional write revisions have different meanings and remain separate.

Settings preserve arbitrary structured preferences but reject manifest-declared
credential and local paths. The exporter applies those same exclusions before
returning files; server validation is independent of the sandbox. Exclusions
match object path segments (`*` one, `**` any depth). Legacy scalar-allowlist
definitions require a metadata refresh rather than silently dropping protection.
Credential documents permit only registered metadata leaves and exact JSON
secret references (`{"$secret":"ENV_NAME"}`). Only declared credential files
resolve those objects; ordinary settings are literal, including template-like
strings. Missing or unregistered bindings fail without writing empty tokens. Provider OAuth metadata
is authoritative; captured metadata cannot redirect a refresh request.
