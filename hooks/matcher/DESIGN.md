# Matcher Design

The matcher package decides which hooks are affected by a debounced set of file
changes. It is pure policy: given hook definitions, repository metadata, and
changed paths, it returns deterministic queued-hook candidates for the daemon to
persist and execute.

## Responsibilities

- Normalize changed paths to repository-relative slash paths.
- Filter out paths ignored by Git before hook matching.
- Apply global `.discobox/hooks/ignore` patterns.
- Apply each hook's `pattern` and hook-specific `ignore` / `exclude` patterns.
- Preserve change kind (`created`, `modified`, `deleted`) in match results.
- Return deterministic hook order and changed-file lists for queue insertion.

## Inputs and Outputs

Inputs:

- repository root
- hook definitions from `parser`
- file changes from `watcher` / daemon debounce batches
- global hook ignore patterns

Outputs:

- ordered matched hook IDs
- per-hook matched file changes
- skipped path metadata for diagnostics, when requested

## Git Ignore Policy

The matcher should treat Git as authoritative. Prefer batch calls to
`git check-ignore --stdin` or an equivalent abstraction over hand-rolled
`.gitignore` interpretation. Watcher-level ignore support is an optimization, not
the final policy boundary.

If Git is unavailable or the root is not a Git worktree, return a clear error;
the hooks daemon assumes Git-root operation.

## Pattern Semantics

Patterns match repository-relative paths with `/` separators. The target syntax is
Discobot-compatible glob matching:

- `*.go`
- `**/*.go`
- `src/**/*.ts`
- `*.{ts,tsx}`
- `{package.json,pnpm*.yaml}`

Deleted files should match by their previous path even when no filesystem entry
exists. Matching should not stat paths unless explicitly needed for diagnostics.

## Determinism

Ordering matters because execution is serial and failures block later hooks. The
matcher must return stable order, preferably parser order with ID tie-breakers.
Within each hook, changed files should be sorted by path and then change kind.

## Non-Responsibilities

- Do not execute hooks.
- Do not mutate queued state or the database.
- Do not debounce watcher events.
- Do not discover hook files.
