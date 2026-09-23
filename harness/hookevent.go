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
