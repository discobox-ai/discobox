package api

import "sort"

// EventDetailInfo describes one details object field for a known audit event.
type EventDetailInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Required    bool   `json:"required,omitempty"`
}

// EventTypeInfo describes one known hook daemon audit event type.
type EventTypeInfo struct {
	Type        string            `json:"type"`
	Description string            `json:"description"`
	Details     []EventDetailInfo `json:"details,omitempty"`
}

// KnownEventTypes returns the user-facing catalog for audit event types emitted
// into hook_events. Keep this list in sync with production recordEvent and
// recordEventTx emitters.
func KnownEventTypes() []EventTypeInfo {
	daemonSessionDetails := []EventDetailInfo{
		{Name: "daemon_session_id", Description: "daemon_sessions row ID", Required: true},
		{Name: "session_id", Description: "hook daemon session ID", Required: true},
		{Name: "repo_root", Description: "repository root for the daemon session", Required: true},
		{Name: "version", Description: "daemon build/runtime version", Required: true},
		{Name: "pid", Description: "daemon process ID", Required: true},
		{Name: "started_at", Description: "daemon session start timestamp", Required: true},
	}
	types := []EventTypeInfo{
		{Type: "daemon.heartbeat.failed", Description: "The daemon failed to update its daemon session heartbeat.", Details: []EventDetailInfo{
			{Name: "daemon_session_id", Description: "daemon_sessions row ID whose heartbeat update failed", Required: true},
			{Name: "session_id", Description: "hook daemon session ID", Required: true},
			{Name: "repo_root", Description: "repository root for the daemon session", Required: true},
			{Name: "error", Description: "heartbeat persistence error", Required: true},
		}},
		{Type: "daemon.shutdown", Description: "A daemon session ended gracefully and recorded its end time.", Details: append(append([]EventDetailInfo{}, daemonSessionDetails...),
			EventDetailInfo{Name: "last_heartbeat", Description: "last heartbeat timestamp recorded before shutdown", Required: true},
			EventDetailInfo{Name: "ended_at", Description: "daemon session end timestamp", Required: true},
			EventDetailInfo{Name: "end_reason", Description: "shutdown reason", Required: true},
		)},
		{Type: "daemon.shutdown.requested", Description: "A client requested daemon shutdown over the socket API.", Details: []EventDetailInfo{
			{Name: "session_id", Description: "hook daemon session ID", Required: true},
			{Name: "repo_root", Description: "repository root for the daemon session", Required: true},
		}},
		{Type: "daemon.started", Description: "A daemon session started and recorded its daemon_sessions row.", Details: daemonSessionDetails},
		{Type: "daemon.terminated", Description: "A previous daemon session was found without a graceful end time and was closed at its last heartbeat.", Details: append(append([]EventDetailInfo{}, daemonSessionDetails...),
			EventDetailInfo{Name: "last_heartbeat", Description: "last heartbeat timestamp from the stale daemon row", Required: true},
			EventDetailInfo{Name: "ended_at", Description: "synthetic end timestamp assigned during recovery", Required: true},
			EventDetailInfo{Name: "end_reason", Description: "termination reason", Required: true},
		)},
		{Type: "discovery.reload.failed", Description: "Hook discovery reload failed after hook configuration changed.", Details: []EventDetailInfo{
			{Name: "repo_root", Description: "repository root used for discovery", Required: true},
			{Name: "error", Description: "discovery error", Required: true},
		}},
		{Type: "discovery.reloaded", Description: "Hook discovery completed and definitions were refreshed.", Details: []EventDetailInfo{
			{Name: "repo_root", Description: "repository root used for discovery", Required: true},
			{Name: "hooks", Description: "number of discovered hooks", Required: true},
		}},
		{Type: "execution.paused", Description: "Global hook execution was paused.", Details: []EventDetailInfo{
			{Name: "paused", Description: "resulting global pause state", Required: true},
			{Name: "scope", Description: "execution scope, always global", Required: true},
		}},
		{Type: "execution.resumed", Description: "Global hook execution was resumed.", Details: []EventDetailInfo{
			{Name: "paused", Description: "resulting global pause state", Required: true},
			{Name: "scope", Description: "execution scope, always global", Required: true},
		}},
		{Type: "file.change.ignore.failed", Description: "Git ignore filtering failed; the daemon continued with unfiltered candidate changes.", Details: []EventDetailInfo{
			{Name: "repo_root", Description: "repository root used for ignore filtering", Required: true},
			{Name: "changes", Description: "number of candidate changes", Required: true},
			{Name: "error", Description: "ignore filter error", Required: true},
		}},
		{Type: "file.change.observed", Description: "A filesystem change was durably recorded with base commit and diff details.", Details: []EventDetailInfo{
			{Name: "change_id", Description: "observed_file_changes row ID", Required: true},
			{Name: "path", Description: "repository-relative path", Required: true},
			{Name: "kind", Description: "created, modified, or deleted", Required: true},
			{Name: "base_commit", Description: "commit used as the diff base"},
			{Name: "diff", Description: "best-effort per-file git diff"},
			{Name: "created_at", Description: "observed change row timestamp", Required: true},
		}},
		{Type: "file.change.record.failed", Description: "The daemon failed to persist observed file changes for a watcher batch.", Details: []EventDetailInfo{
			{Name: "changes", Description: "number of changes in the batch", Required: true},
			{Name: "changed_files", Description: "changed-file summaries from the batch", Required: true},
			{Name: "error", Description: "persistence error", Required: true},
		}},
		{Type: "hook.enqueued", Description: "A file change matched a hook and the hook was added or merged into the queue.", Details: []EventDetailInfo{
			{Name: "changes", Description: "number of matched changes", Required: true},
			{Name: "changed_files", Description: "changed files enqueued for the hook", Required: true},
			{Name: "change_ids", Description: "observed_file_changes IDs associated with the enqueue", Required: true},
		}},
		{Type: "hook.enqueue.failed", Description: "A matching hook could not be added or merged into the queue.", Details: []EventDetailInfo{
			{Name: "changes", Description: "number of matched changes", Required: true},
			{Name: "changed_files", Description: "changed files that would have been enqueued", Required: true},
			{Name: "change_ids", Description: "observed_file_changes IDs associated with the failed enqueue", Required: true},
			{Name: "error", Description: "enqueue error", Required: true},
		}},
		{Type: "hook.log", Description: "One line of hook process output was captured and stored.", Details: []EventDetailInfo{
			{Name: "line_id", Description: "hook_logs row ID", Required: true},
			{Name: "line", Description: "captured output line", Required: true},
		}},
		{Type: "hook.paused", Description: "A specific hook was paused.", Details: []EventDetailInfo{
			{Name: "paused", Description: "resulting hook pause state", Required: true},
			{Name: "scope", Description: "execution scope, always hook", Required: true},
		}},
		{Type: "hook.resumed", Description: "A specific hook was resumed.", Details: []EventDetailInfo{
			{Name: "paused", Description: "resulting hook pause state", Required: true},
			{Name: "scope", Description: "execution scope, always hook", Required: true},
		}},
		{Type: "hook.run.finished", Description: "A hook run finished with success or failure status.", Details: []EventDetailInfo{
			{Name: "status", Description: "terminal run status", Required: true},
			{Name: "exit_code", Description: "hook process exit code", Required: true},
			{Name: "success", Description: "whether the run succeeded", Required: true},
			{Name: "error", Description: "runner or process error text"},
		}},
		{Type: "hook.run.requested", Description: "A client requested a hook run.", Details: []EventDetailInfo{
			{Name: "force", Description: "whether the request bypassed normal skip checks", Required: true},
			{Name: "phase", Description: "phase requested for the run, when phase-targeted"},
			{Name: "enqueued", Description: "whether a queued run was enqueued", Required: true},
		}},
		{Type: "hook.run.skipped", Description: "A requested hook run was skipped, usually because it already succeeded and was not forced.", Details: []EventDetailInfo{
			{Name: "reason", Description: "skip reason", Required: true},
			{Name: "force", Description: "whether the request used force", Required: true},
			{Name: "phase", Description: "phase requested for the run, when phase-targeted"},
			{Name: "enqueued", Description: "whether a queued run was enqueued", Required: true},
		}},
		{Type: "hook.run.started", Description: "A queued hook run started executing.", Details: []EventDetailInfo{
			{Name: "changed_files", Description: "number of changed files supplied to the run", Required: true},
			{Name: "changed_paths", Description: "repository-relative paths supplied to the run", Required: true},
			{Name: "change_ids", Description: "observed_file_changes IDs supplied to the run", Required: true},
			{Name: "invocation_id", Description: "hook_invocations row ID", Required: true},
		}},
		{Type: "watch.snapshot.persist.failed", Description: "The daemon failed to persist the watcher snapshot checkpoint.", Details: []EventDetailInfo{
			{Name: "files", Description: "number of watcher snapshot entries", Required: true},
			{Name: "error", Description: "checkpoint persistence error", Required: true},
		}},
		{Type: "workspace.snapshot.created", Description: "A workspace snapshot patch was created for a run.", Details: []EventDetailInfo{
			{Name: "snapshot_id", Description: "workspace_snapshots row ID", Required: true},
			{Name: "parent_id", Description: "parent workspace snapshot ID"},
			{Name: "base_commit", Description: "base commit used to build the snapshot", Required: true},
			{Name: "tree_hash", Description: "captured Git tree hash", Required: true},
			{Name: "patch_bytes", Description: "stored patch size in bytes", Required: true},
			{Name: "changed_files", Description: "number of captured changed files", Required: true},
			{Name: "omitted_files", Description: "paths intentionally omitted from capture", Required: true},
			{Name: "max_file_bytes", Description: "per-file capture size limit", Required: true},
			{Name: "observed_change_ids", Description: "observed change IDs linked to the snapshot"},
		}},
		{Type: "workspace.snapshot.failed", Description: "The daemon failed to create a workspace snapshot patch for a run.", Details: []EventDetailInfo{
			{Name: "repo_root", Description: "repository root being snapshotted", Required: true},
			{Name: "error", Description: "snapshot capture error", Required: true},
		}},
	}
	sort.Slice(types, func(i, j int) bool { return types[i].Type < types[j].Type })
	return types
}
