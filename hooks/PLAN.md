# Hooks Implementation Plan

This file records working plan details that are more provisional than
`DESIGN.md`. Update it as implementation choices are made.

## Source Reference

The first implementation should draw from the temporary analysis of
`github.com/obot-platform/discobot/agent-go`:

- `internal/hooks/parser.go`: hook front matter, normalized IDs, discovery rules.
- `internal/hooks/executor.go`: script execution, timeout, output capture,
  process group kill behavior.
- `internal/hooks/status.go`: status concepts and counters to translate to DB
  rows.
- `filewatcher`: stable snapshot-diff watcher design.
- `internal/hooks/precommit.go`: conceptual reference only; avoid copying the
  shell-side JSON status mutation approach.

Do not import Discobot `agent-go` packages. Copy/adapt only code that is generic
enough for this module and rename Discobot-specific contracts to Discobox.

## Proposed Initial Package Shape

Start small and split only when code pressure justifies it:

- root package: public hook model and high-level orchestration-facing types.
- `parser`: hook file format, front matter parsing, discovery, validation, and
  pattern/ignore metadata normalization.
- `matcher`: Git-ignore filtering, glob matching, and changed-file-to-hook
  mapping.
- `watcher`: stable file watcher.
- `runner`: process execution.
- `store`: GORM models, migrations, query/update helpers.
- `daemon`: session daemon, scheduler, Unix socket API, startup lock, paths.
- `client`: typed Unix-socket client for CLI and integrations.
- `cmd/discobox-hooks`: CLI entrypoint, if the command lives in this module.

## Phase 1 Scope

- Resolve Git root and `.discobox/hooks` discovery.
- Support `session` and `file` script hooks.
- Use the Discobot-compatible front matter format.
- Keep session hooks manual-only; run them only when explicitly requested over
  the CLI/API.
- Watch Git root and batch file changes using a five-second quiet-period
  debounce.
- Filter ignored files according to Git ignore behavior.
- Match created, modified, and deleted paths against hook patterns.
- Run hooks serially; stop on first failure.
- Persist definitions, queue, statuses, and run history through GORM.
- Default to session-scoped SQLite through `gormdb`.
- Expose status, output, pause/resume, run, and shutdown over a Unix socket.
- Start the daemon on demand under a startup lock.
- Shut down after an idle timeout.

## Phase 2 Scope

- Explicit pre-commit installation command.
- Generated pre-commit hook that calls the CLI/daemon.
- Claude Code and Codex script-hook examples.
- Output tailing/streaming over the socket API.
- Stronger Git ignore implementation if initial watcher filtering is not
  authoritative enough.

## Open Design Questions

- Exact idle timeout default.
- Whether continuous writes use only a quiet-period debounce or also a maximum
  batch window.
- How pre-commit should select a session when more than one session exists for a
  repository.
- Whether `engine: ai` should be rejected, ignored, or treated as a validation
  warning before native AI execution exists.
- Whether failed queued hooks should automatically retry on any later matching
  change or only when changed files overlap the hook's last failed file set.

## Prototype AI Script Shape

Use script hooks for AI workflows. The daemon passes changed file data through
stable environment variables. The script calls an external AI CLI and exits
non-zero when the hook should fail.

Example shape:

```bash
#!/usr/bin/env bash
#---
# name: Claude Review
# type: file
# pattern: "**/*.go"
#---
set -euo pipefail

changed_files_json="$DISCOBOX_CHANGED_FILES_JSON"
prompt="Review these changed files. Reply SUCCESS if acceptable, otherwise FEEDBACK.\n\n$changed_files_json"
response="$(claude -p "$prompt")"
printf '%s\n' "$response"

case "$response" in
  SUCCESS*) exit 0 ;;
  *) exit 1 ;;
esac
```
