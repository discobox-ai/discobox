# Store Design

The store package owns session-scoped hook persistence using GORM. It translates
daemon state transitions into durable rows and provides read APIs for status,
outputs, and run history. Audit event writes are also schema-checked here:
`RecordEvent` and transactional `recordEventTx` validate event type names and
required `details` fields against `hooks/api.KnownEventTypes` before inserting
`hook_events` rows.

## Responsibilities

- Open session databases through `github.com/obot-platform/discobox/gormdb`.
- Auto-migrate or otherwise initialize the hook schema.
- Use public GORM persistence structs from `hooks/models` for migrations and
  database queries.
- Persist discovered hook definitions and config hashes.
- Persist current hook status.
- Persist queued hook state and accumulated changed files.
- Persist hook run history and invocation records.
- Persist workspace snapshot history and omission metadata.
- Record daemon/session metadata needed for status and idle shutdown.
- Validate audit event type names and required `details` fields before writing
  `hook_events`.
- Provide transactional helpers for scheduler state transitions.

## Database Ownership

The session daemon is the only writer. CLI commands mutate state through the Unix
socket API, not by writing SQLite directly. Store APIs may still expose read-only
queries for tests and diagnostics.

## Default Backend

Default to one SQLite database per session using `gormdb`. Keep models portable
to Postgres where practical, but optimize initial behavior for SQLite WAL and a
single writer.

Use the same physical connection pattern as the server database layer: construct
write/read GORM handles through `gormdb.Open`, keep writes on the write handle,
and use the read handle for list/status/output queries. Generated row IDs use
lowercase ULID strings from the root `id` package; only natural keys such as
`hook_id` and fixed daemon state keys should be primary keys without generated
IDs.

## Core Models

Suggested tables:

- `hook_definitions`: current discovered hook metadata.
- `hook_statuses`: latest status per hook.
- `hook_runs`: immutable or append-only run history.
- `pending_hooks`: internal serial queue table with accumulated changed files and observed change IDs.
- `observed_file_changes`: every daemon-observed file change, base commit, and per-file Git diff.
- `hook_invocations`: every hook invocation attempt, linked one-to-one to a run.
- `hook_invocation_changes`: invocation-to-observed-change join rows.
- `hook_logs`: per-line hook output.
- `hook_events`: global audit trail for daemon, file, queue, hook, and API
  actions. Writes are validated against `api.KnownEventTypes`; unknown event
  types or missing required `details` fields are rejected before insert.
- `daemon_states`: key/value execution state such as global pause.
- `daemon_sessions`: daemon process lifetime records with heartbeats and end times.
- `workspace_snapshots`: whole-workspace snapshot history, including parent snapshot, base commit, tree hash, binary patch, changed-file summary, explicit omitted-file records, and size cap.
- `watched_files`: latest full watcher snapshot checkpoint.

Status values:

- `idle`
- `queued`
- `running`
- `success`
- `failure`

## Transactions

State transitions that must be atomic:

- discovery refresh plus status synthesis for new hooks
- enqueue matched hooks plus changed-file merge
- mark running plus create run, invocation, and invocation-change rows
- finish run plus update status, counters, and queue
- append per-line hook output logs and audit events after validating event
  types/details against `api.KnownEventTypes`
- pause/resume hook or global execution state
- append workspace snapshot records with their serialized changed-file and
  omitted-file summaries

## Changed Files Storage

Store changed files as first-class database records. Every observed file change
gets an `observed_file_changes` row containing path, kind, base commit, and the
best-effort Git diff for that path against the base commit. Queued hooks carry
observed change IDs, and hook invocations persist join rows to the change records
used as invocation inputs. Do not write changed-file payload files for hook
scripts; expose compact path lists through environment variables and keep durable
payloads in the database.

## Changed File Carry-Forward

The store owns changed-file input continuity:

- `MarkRunning` consumes the queued row at run start after copying its changed
  files and observed change IDs into `hook_runs` and `hook_invocations`.
- If more changes for the same hook arrive while it is running, they create a new
  queued row instead of modifying the in-flight run.
- If the run fails and a new queued row exists for that hook, merge the failed
  run inputs into that queued row. The next run sees both the failed inputs and
  the newly observed inputs.
- If the run fails and no new queued row exists, do not auto-create a retry.
  The next enqueue for that hook merges the last failed run inputs with the new
  changed files.
- If the previous run succeeded, later file-change enqueues use only newly
  changed files; successful inputs are not carried forward.
- A forced manual run is the exception: it explicitly copies the latest run's
  changed files and observed change IDs into the forced queued run, even if the
  latest run succeeded.

## Phase-Gated Queue Selection

Phase is stored on hook definitions. Enqueue operations still persist phase hooks
when their patterns match, but queue selection is phase-aware: unphased hooks are
eligible for normal draining, while hooks with a phase are returned only when the
daemon supplies that phase as active. This lets file changes queue phase work
without auto-running it before an explicit phase run request.

## Workspace Snapshot Storage

Workspace snapshots are append-only database records, not Git refs. Each row
stores the captured tree hash, base commit, parent snapshot ID, binary patch
bytes, patch size, changed-file summary, omission summary, and capture size cap.
The daemon may skip inserting a row when the temporary tree equals `HEAD` or the
latest stored snapshot tree. Omission records are part of the snapshot payload so
readers can distinguish "unchanged" from "intentionally not captured" paths.

## Audit Event Validation

`hook_events` is persistence, but event shape validation also belongs at the
store write boundary because every daemon, manager, and transactional store event
passes through `RecordEvent` or `recordEventTx` before a row is inserted. The
store imports `hooks/api` for the audit metadata catalog and validates event
types and required detail fields against `api.KnownEventTypes`.

Unknown event types and missing required detail values are write errors in
normal runtime paths. Under Go test binaries, the same validation errors panic so
unit tests fail at the emitting code path instead of silently storing invalid
audit data. Keep this dependency narrow: the store may depend on the API package
for the stable audit event catalog, but it must not depend on socket handlers,
generated transport code, or server internals.

## Non-Responsibilities

- Do not spawn hook processes.
- Do not watch files.
- Do not expose network or socket APIs directly.
- Do not import server internals.
