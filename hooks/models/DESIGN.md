# Models Design

`models` contains the hooks database models used by the GORM store.

## Responsibilities

- Define exported GORM structs for all hook-owned tables.
- Own table names, GORM tags, and model lifecycle hooks such as generated IDs.
- Provide storage-facing value types used inside persisted JSON columns, such as
  changed files and run results.
- Stay independent from daemon HTTP handlers and client transport concerns.

## API Reuse Policy

API responses may reuse a database model from this package when the database
shape is already the intended API contract. If an endpoint needs projection,
redaction, compatibility behavior, or a shape that combines multiple tables,
define that response DTO in `hooks/api` instead.

## Schema ERD

```mermaid
erDiagram
    hook_definitions {
        string id PK
        string name
        string description
        string type
        string engine
        string run_as
        bool blocking
        string pattern
        json ignore
        string phase
        string subagent
        string prompt
        string abs_path
        string rel_path
        bool has_shebang
        bool executable
        json extensions
        string config_hash
        datetime created_at
        datetime updated_at
    }

    hook_statuses {
        string hook_id PK,FK
        string status
        bool paused
        int run_count
        int fail_count
        string last_run_id FK
        string last_error
        datetime created_at
        datetime updated_at
    }

    pending_hooks {
        string hook_id PK,FK
        int position
        json changed_files
        json change_ids
        bool blocked
        string blocked_by_hook_id FK
        datetime created_at
        datetime updated_at
    }

    hook_runs {
        string id PK
        string invocation_id FK
        string hook_id FK
        string status
        int exit_code
        json changed_files
        json change_ids
        string error
        datetime started_at
        datetime finished_at
    }

    hook_invocations {
        string id PK
        string hook_id FK
        string run_id FK
        datetime requested_at
    }

    observed_file_changes {
        string id PK
        string path
        string kind
        string base_commit
        string diff
        datetime created_at
    }

    workspace_snapshots {
        string id PK
        string parent_id FK
        string base_commit
        string tree_hash
        bytes patch
        int patch_bytes
        json changed_files
        json omitted_files
        int max_file_bytes
        json observed_change_ids
        datetime created_at
    }

    hook_invocation_changes {
        string id PK
        string invocation_id FK
        string change_id FK
    }

    hook_logs {
        string id PK
        string hook_id FK
        string run_id FK
        string line
        datetime created_at
    }

    hook_events {
        string id PK
        string type
        string hook_id FK
        string run_id FK
        string message
        json details
        datetime created_at
    }

    daemon_states {
        string key PK
        string value
        datetime updated_at
    }

    daemon_sessions {
        string id PK
        string session_id
        string repo_root
        int version
        int pid
        datetime started_at
        datetime last_heartbeat
        datetime ended_at
        string end_reason
    }

    watched_files {
        string path PK
        bool is_dir
        int size
        int mode
        datetime mod_time
        datetime updated_at
    }

    hook_definitions ||--o| hook_statuses : tracks
    hook_definitions ||--o{ pending_hooks : queues
    hook_definitions ||--o{ hook_runs : executes
    hook_definitions ||--o{ hook_invocations : requests
    hook_definitions ||--o{ hook_logs : emits
    hook_definitions ||--o{ hook_events : audits

    hook_runs ||--o{ hook_logs : captures
    hook_runs ||--o{ hook_events : audits
    hook_runs ||--o| hook_invocations : records
    hook_runs ||--o| hook_statuses : last_run

    hook_invocations ||--o{ hook_invocation_changes : includes
    observed_file_changes ||--o{ hook_invocation_changes : input_to

    workspace_snapshots ||--o{ workspace_snapshots : parent_of

    hook_definitions ||--o{ pending_hooks : blocks
```

## Table Purposes

`hook_definitions` stores the current discovered hook metadata from
`.discobox/hooks`, including parser-normalized execution settings and the config
hash used to detect definition changes.

`hook_statuses` stores the latest durable scheduler state for each hook,
including pause state, run/failure counters, last run ID, and last error.

`pending_hooks` stores the serial queue. Each row represents one queued hook with
its current position, accumulated changed files, observed change IDs, and any
blocked-by relationship.

`hook_runs` stores hook execution history. It records the run status, process
exit code, consumed changed-file payload, error text, and start/finish times.

`hook_invocations` stores one invocation attempt linked to a hook and run. It is
the stable parent for the observed-change join rows used as that invocation's
inputs.

`observed_file_changes` stores every daemon-observed file change that was
durably processed, including repository-relative path, change kind, base commit,
and best-effort per-file diff.

`workspace_snapshots` stores whole-workspace snapshot history. Rows are
append-only and hold database-owned patch data plus structured summaries of
captured and omitted paths. The parent ID links snapshots into a linear history
per session DB; the tree hash is used to avoid duplicate rows for identical
workspace state.

`hook_invocation_changes` stores the many-to-many join between hook invocations
and observed file changes, preserving the exact change records used as hook
inputs.

`hook_logs` stores line-oriented hook output for each run.

`hook_events` stores the global audit trail for daemon, file, queue, hook,
snapshot, and API actions.

`daemon_sessions` stores daemon process lifetimes. Startup closes any previously
unended row for the same session at its last heartbeat, emits
`daemon.terminated`, then inserts a new row and emits `daemon.started`.
Graceful shutdown sets `ended_at` and emits `daemon.shutdown`; the running daemon
updates `last_heartbeat` periodically.

`daemon_states` stores singleton daemon key/value state, such as global pause
state, that is not tied to a specific hook run or queue item.

`watched_files` stores the daemon's latest full watcher snapshot. It is not tied
to hook definitions; it is a restart checkpoint used to diff the current
repository state after idle daemon shutdown.

`hook_events` is the global audit trail. Event details should intentionally
duplicate the important facts from source tables when needed so an audit reader
can understand what happened without joining through ephemeral queue or history
state. For example, `file.change.observed` duplicates observed change path, kind,
base commit, diff, timestamp, and row ID even though the full row also exists in
`observed_file_changes`.
