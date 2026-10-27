# 26-10-01-240 — A swapped request records the secrets it spent

- **Status**: Accepted (supersedes [0130](0130-an-audit-record-is-read-where-it-was-written-and-names-its-attestor.md) §3's empty field for an ordinary injected sentinel; use IDs superseded by [26-10-27-880](26-10-27-880-access-to-a-secret-is-a-binding-held-to-static-routes-and-no-judge-decides-it.md))
- **Date**: 2026-10-01
- **Relates to**: [ADR 0130](0130-an-audit-record-is-read-where-it-was-written-and-names-its-attestor.md)
  §3, which put the use ID on the proxy's http row and decided an ordinary
  sentinel's swap names nothing.

## Context

ADR 0130 §3 gave the pool proxy's http row `swapped_use_ids`: the approved uses
a request spent, the join to the verdict trail. A use exists only on the agent
credentials path, so a swap from an ordinary injected sentinel — a harness's
secrets, `discobox new --secret`, everything outside `discobox-access` — names
nothing, and §3 accepted that: "The row still records that a swap happened,
through the redaction it forces."

In use, that is not a record. The redaction says a header was redacted, and a
header a rewrite rule set is redacted the same way, so a reader cannot tell a
credential sent from a header set. It says nothing of which credential. The
commonest credentials in a discobox — the model provider's key, a harness's
token — are exactly the ordinary sentinels, so the trail is silent about the
credentials most requests carry. Asking "which requests did this key go out
on?" has no answer from any trail.

The control plane knows. `ResolveSandboxSecret` finds the secret a sentinel is
assigned to before it decides anything; it hands the pool the value and drops
the identity.

## Decision

### 1. The resolve answer names the secret

`ResolveSandboxSecretResponse` gains `secretId`, set on an approved answer: the
secret the value is. The pool agent's resolver carries it onto
`proxy/internal/secrets.ResolveResult.SecretID` for both sentinel kinds — on
the agent credentials path the activation's stable sentinel is resolved like
any other, so a judged use records its secret as well as its use.

### 2. The row records every secret it spent, by ID

`SecretID` rides beside `UseID` wherever a resolved value goes — the cache entry,
the value kept for the rotated-credential retry, the swap `Result` — and the
http row gains `swapped_secret_ids`, comma-joined like `swapped_use_ids`.
Plural for the same reason: one `Authorization: Basic` token can carry two
sentinels.

It is set where a value was substituted and nowhere else. A request the judge
refused was refused before anything was resolved (ADR 26-09-22-838 §4), so its
row keeps naming the uses from the verdict and no secret: none was spent.

**The ID, still never the sentinel.** §3's reasoning stands: a sentinel is a
bearer of the credential and does not belong in a trail kept to be read. A
secret ID authorizes nothing and names the credential as every other surface
names it.

### 3. Optional on the wire

`swappedSecretIds` is optional in `pool.yaml` and `server.yaml`, unlike
`swappedUseIds`. A pool agent and the server it reports to are upgraded
separately, and so are a CLI and its server: a required field would make a new
reader refuse every record an older writer sends. Absent and empty mean the
same — no secret was named — and rows written before this have the column
empty.

## Alternatives rejected

- **Record the swapped header names.** Cheap, and it would tell a swap from a
  rewrite rule. It still does not say which credential, which is the question.
- **Record the stable sentinel.** It identifies the credential without the
  control plane's help, and §3 rejected it for a reason that has not changed:
  it is a bearer of the credential and never expires.
- **Map sentinels to secrets on the pool.** The pool would need the assignment
  table; the resolver already asks the one component that has it, once per
  value rather than per request.

## Consequences

- The http trail can answer "which requests did this secret go out on" by
  itself, from the pool — which also makes a `--secret` filter on it a column
  match rather than an expansion of the secret's grants to use IDs.
- The pool proxy's audit database gains a column, added in place by its
  `AutoMigrate`; existing rows read it as empty.
