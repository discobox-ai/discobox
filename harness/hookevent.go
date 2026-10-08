package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// ClaudeCodeProvider is the hook provider whose event vocabulary is the
// canonical one (ADR 0146 §1). It is the provider a claude-code image's hooks
// publish under, and matches that driver's ID; the constant lives here rather
// than being imported from the driver because the drivers import this package.
const ClaudeCodeProvider = "claude-code"

// canonicalHookEvents maps a provider's own hook event names onto Claude Code's
// vocabulary (ADR 0146 §1). A provider missing from this map, or an event
// missing from its table, has no canonical name — CanonicalHookEvent answers
// "" and the hook is recorded under the name its harness used and no other
// (ADR 0146 §3).
//
// An entry that maps a name to itself is not redundant: it is the statement
// that this harness's event means what Claude Code's event of that name means.
// Two harnesses spelling an event the same way is a fact about how they were
// built, not a contract, so each agreement is recorded here on purpose rather
// than assumed from the spelling.
//
// claude-code is deliberately absent: its events are canonical by definition,
// including ones added after this table was written, which is exactly the case
// a table would get wrong.
var canonicalHookEvents = map[string]map[string]string{
	// Codex adopted Claude Code's hook vocabulary wholesale — 11 of its 12
	// events are spelled identically. Interrupt is the twelfth and is
	// deliberately absent: Claude Code has no counterpart to it, so it has no
	// canonical name until Claude Code grows one. Recording it as "Interrupt"
	// here would squat a name Claude Code has not chosen (ADR 0146 §3).
	"codex-cli": {
		"PreToolUse":        "PreToolUse",
		"PermissionRequest": "PermissionRequest",
		"PostToolUse":       "PostToolUse",
		"PreCompact":        "PreCompact",
		"PostCompact":       "PostCompact",
		"UserPromptSubmit":  "UserPromptSubmit",
		"SubagentStart":     "SubagentStart",
		"SubagentStop":      "SubagentStop",
		"Stop":              "Stop",
		"SessionStart":      "SessionStart",
		"SessionEnd":        "SessionEnd",
	},
	// opencode names its lifecycle in dot.lower.case and shares no spelling
	// with Claude Code, so every entry here is a real translation rather than
	// a recorded agreement. Its image publishes more events than appear here
	// (ADR 0147): the ones left out — permission.replied, command.executed,
	// todo.updated, session.deleted, server.connected, installation.updated —
	// are opencode facts Claude Code has no word for, and are recorded under
	// their own names alone.
	//
	// session.deleted is deliberately not SessionEnd: Claude Code ends a
	// session when it terminates, and opencode deletes one on an explicit
	// removal, which is a different event that happens to sound alike.
	//
	// session.idle, session.created, session.error and session.compacted mean
	// the root session's here, and only because the image's plugin publishes
	// those four for the root alone. opencode publishes each of them per
	// session, and its task tool runs sub-sessions, so unfiltered they would
	// name a subagent's turn Stop — or its compaction PostCompact — and end a
	// wait early (ADR 0147 §4). The name carries no session, so the
	// distinction cannot be made here: if the plugin ever stops filtering,
	// all four of these entries have to go.
	"opencode": {
		"tool.execute.before": "PreToolUse",
		"tool.execute.after":  "PostToolUse",
		"session.idle":        "Stop",
		"session.error":       "StopFailure",
		"session.created":     "SessionStart",
		"permission.asked":    "PermissionRequest",
		"session.compacted":   "PostCompact",
		"file.edited":         "FileChanged",
	},
	// pi names its lifecycle in snake_case and shares no spelling with Claude
	// Code, so every entry here is a real translation. Its image publishes
	// more events than appear here: the ones left out — session_info_changed,
	// agent_start, agent_end, turn_start, turn_end, session_compact_failed,
	// model_select — are pi facts Claude Code has no word for, and are
	// recorded under their own names alone.
	//
	// agent_settled, not agent_end, is Stop: agent_end closes one low-level
	// run, after which pi may still retry, recover from an overflow, or
	// deliver a queued follow-up, and agent_settled is pi saying it will not
	// continue on its own — which is what a wait for the turn's end wants.
	//
	// model_select is deliberately not PreModelSwitch or PostModelSwitch: pi
	// announces the model it selected, once, and does not say whether the
	// switch has happened, so neither name is a match.
	"pi": {
		"session_start":          "SessionStart",
		"session_shutdown":       "SessionEnd",
		"before_agent_start":     "UserPromptSubmit",
		"agent_settled":          "Stop",
		"tool_call":              "PreToolUse",
		"tool_result":            "PostToolUse",
		"session_before_compact": "PreCompact",
		"session_compact":        "PostCompact",
	},
	// omp is a fork of pi and keeps most of its vocabulary, so most entries
	// here are the same translations; two are omp's own. session_stop is
	// modeled on Claude Code's Stop hook — it carries stop_hook_active and
	// the last assistant message, and its handler may ask for a continuation
	// — and omp has no agent_settled. tool_approval_requested is omp asking
	// for permission, which is what PermissionRequest means. The events left
	// out — session_switch, agent_start, agent_end, turn_start, turn_end,
	// tool_approval_resolved, credential_disabled — are recorded under their
	// own names alone.
	//
	// omp's subagents run as processes of their own, which the launcher's
	// --hook does not reach, so every event here is the root session's.
	"omp": {
		"session_start":           "SessionStart",
		"session_shutdown":        "SessionEnd",
		"before_agent_start":      "UserPromptSubmit",
		"session_stop":            "Stop",
		"tool_call":               "PreToolUse",
		"tool_result":             "PostToolUse",
		"tool_approval_requested": "PermissionRequest",
		"session_before_compact":  "PreCompact",
		"session_compact":         "PostCompact",
	},
}

// CanonicalHookEvent answers the Claude Code name for one harness's hook
// event, or "" when Claude Code has no name for it (ADR 0146 §3).
//
// It is applied when a hook is recorded, never when one is read: a wait matches
// hook events in SQL, so the canonical name has to be a stored column
// (ADR 0146 §4).
func CanonicalHookEvent(provider, event string) string {
	provider = strings.TrimSpace(provider)
	event = strings.TrimSpace(event)
	if provider == "" || event == "" {
		return ""
	}
	if provider == ClaudeCodeProvider {
		return event
	}
	return canonicalHookEvents[provider][event]
}

// CanonicalHookEventsVersion fingerprints the mapping above, so a store can
// tell whether the table has changed since it last applied it to rows already
// recorded (ADR 0146 §7).
//
// It covers the translations only. claude-code needs no entry because its
// events are canonical by definition, and adding a provider that has no
// entries cannot change any stored row's canonical name.
func CanonicalHookEventsVersion() string {
	sum := sha256.New()
	for _, provider := range slices.Sorted(maps.Keys(canonicalHookEvents)) {
		events := canonicalHookEvents[provider]
		for _, event := range slices.Sorted(maps.Keys(events)) {
			// Length-prefixed, so no two tables can hash alike by moving a
			// delimiter from one field into the next.
			for _, field := range []string{provider, event, events[event]} {
				fmt.Fprintf(sum, "%d:%s", len(field), field)
			}
		}
	}
	return hex.EncodeToString(sum.Sum(nil))[:16]
}
