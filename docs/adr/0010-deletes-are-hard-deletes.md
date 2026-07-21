# 0010 — Deletes are hard deletes

- **Status**: Proposed
- **Date**: 2026-07-21

## Context

Eleven of the twenty-five persisted models carried `gorm.DeletedAt`: `User`,
`Project`, `ProjectMember`, `Pool`, `Sandbox`, `SandboxProviderInstance`,
`Secret`, `SecretRequest`, `SecretGrant`, `SandboxSecret`, and
`HarnessConfigSecretBinding`. `model/DESIGN.md` made that the rule for "primary
mutable resources", with hard delete as the documented exception.

A soft-deleted row is still a row. It still occupies every unique index on its
table, because those indexes do not know about `deleted_at`. Six of the eleven
have one, and for each, deleting the thing and creating it again fails:

| Model | Unique index | Effect of deleting, then recreating |
| --- | --- | --- |
| `Secret` | `(project_id, type, host, unique_key)` | a user-created secret's `(type, host)` slot is burnt |
| `Pool` | `(project_id, name)` | the pool's name cannot be reused |
| `Project` | `(slug)` | the slug cannot be reused |
| `User` | `(email)`, `(provider, subject)` | that email can never be re-added |
| `HarnessConfigSecretBinding` | `(harness_config_id, env_name)` | the env name cannot be rebound |
| `SandboxSecret` | `(sentinel)`, `(sandbox_id, env_name)` | same shape |

This is not theoretical. `CreateSecret` → `DeleteSecret` → `CreateSecret` with
the same type and host fails with `UNIQUE constraint failed: secrets.project_id,
secrets.type, secrets.host, secrets.unique_key`.

The codebase had already met this problem three times and worked around it
locally rather than naming it:

- `HarnessConfig` was made an explicit hard-delete exception, documented as
  "so the same definition name can be enabled again without colliding with a
  hidden soft-deleted row" — the general problem, solved for one model.
- `UpsertHarnessConfigSecretBinding` listed `deleted_at` in its `DoUpdates`
  columns, resurrecting a tombstoned binding on conflict. Configure →
  deconfigure → configure only worked because of that line.
- `DeleteHarnessConfig` ran an `Unscoped()` update to clear `harness_config_id`
  on soft-deleted sandboxes, because tombstones still held the FK and blocked
  deletion.

Against that, soft delete was buying nothing. The only undelete path,
`RestoreSandboxProviderInstance`, had no callers. Deletion is already recorded
in the project event stream by `withResourceEvent`, so the audit trail does not
depend on the row. And every query that bypasses GORM's scoping — raw SQL, a
debug session, a test — sees deleted rows unless it remembers `deleted_at IS
NULL`. That misled the author of `test/bats/harness_configure.bats`: a
reconfigure appeared to leak a secret and orphan a grant, when it had correctly
replaced both.

`Secret` already nulls `encrypted_value` before soft-deleting, so credentials
were never retained in tombstones. The problem is the row, not the ciphertext.

## Decision

**No model carries `gorm.DeletedAt`. Deleting a row deletes it.**

`gorm.DeletedAt` is removed from all eleven models, along with the three
workarounds it required: the `deleted_at` entry in the binding upsert's
`DoUpdates`, the `Unscoped()` FK-clearing update in `DeleteHarnessConfig`, and
the `project_members.deleted_at IS NULL` predicates in the project joins.
`RestoreSandboxProviderInstance` is deleted as dead code.

`model/DESIGN.md` and `model/REVIEW.md` are inverted to match: hard delete is
the rule, and there is no exception list because there are no exceptions.

## Alternatives rejected

**Remove it from the secret-related models only.** The narrow reading of the
original concern — `Secret`, `SecretGrant`, `SandboxSecret`,
`HarnessConfigSecretBinding`, `SecretRequest`. Rejected because the bug is not
about secrets. `Pool`, `Project`, and `User` have exactly the same unique-index
collision, and "delete a pool, cannot recreate it with the same name" is at
least as user-visible as the secret case. Fixing half would also leave two
delete conventions in one model package, which is how the `HarnessConfig`
carve-out happened in the first place.

**Remove it only from the six models with unique indexes.** Fixes every
demonstrated bug with the smallest diff. Rejected because the rule "soft delete
unless the table has a unique index" is not a rule anyone can follow: adding a
unique index later would silently reintroduce the bug, and reviewers would have
to notice the interaction. The condition is invisible at the point where the
mistake is made.

**Keep soft delete and make the unique indexes partial** (`WHERE deleted_at IS
NULL`). Preserves tombstones and fixes the collisions. Rejected because it buys
nothing that is used and costs correctness everywhere else: every raw query
still has to remember the predicate, GORM does not generate partial indexes from
struct tags so each one becomes hand-written migration DDL, and SQLite and
Postgres would need separate handling. It fixes the symptom while keeping the
trap.

**Keep soft delete for audit.** The usual reason to keep tombstones. Rejected
because the audit trail is already elsewhere: `withResourceEvent` writes a
deletion event to the append-only `ProjectEvent` stream, which is hard-deleted
by design and outlives the row. A tombstone duplicates that, less legibly, in
the table it damages.

**Keep it on `SecretRequest`, which is closer to an audit record.** Rejected for
uniformity: it has no unique index, so the tombstone is harmless, but keeping
one soft-deleted model means the package rule is "hard delete, except" — the
shape this ADR exists to remove. Its history belongs in the event stream like
everything else.

## Consequences

- **Existing databases are deleted, not migrated.** The database is disposable,
  so this change writes no migration and no backfill. Carrying one across is not
  an option rather than merely inadvisable: nothing filters `deleted_at` any
  more, so every row previously soft-deleted comes back as live. A development
  database sampled while making this change held 9 live sandboxes and 11
  tombstoned ones, all 20 of which would have read as live.
- Deletion is now irreversible at the storage layer. If undelete is ever wanted,
  it has to be built as an explicit restore path with its own semantics, not
  recovered by removing a filter.
- `store.ErrNotFound` after a delete now means the row is gone rather than
  hidden, so tests and debug queries no longer need `deleted_at IS NULL` — the
  bats suite's SQL drops it.
- Cascades must be complete. Nothing is left behind holding a foreign key to a
  hidden row, but a cascade that was silently relying on a tombstone to keep a
  reference alive would now fail loudly. `DeleteSecret` already cascades its
  bindings and grants explicitly.
- `server/internal/store/hard_delete_test.go` pins the behavior: delete then
  recreate, for a secret, a pool, and a harness-config binding. Each fails if
  `gorm.DeletedAt` reappears on that model.
