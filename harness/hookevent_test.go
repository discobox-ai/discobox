package harness

import "testing"

// Claude Code's vocabulary is the canonical one (ADR 0146 §1), so its events
// are canonical by definition — including ones added after the table in this
// package was written, which is the case a table would get wrong.
func TestCanonicalHookEventPassesClaudeCodeThrough(t *testing.T) {
	for _, event := range []string{"Stop", "PreToolUse", "PreModelSwitch", "AnEventShippedTomorrow"} {
		if got := CanonicalHookEvent(ClaudeCodeProvider, event); got != event {
			t.Errorf("CanonicalHookEvent(claude-code, %q) = %q, want %q", event, got, event)
		}
	}
}

// The agreement between two harnesses on a spelling is recorded, not assumed:
// codex is mapped through its own table even where the name is identical.
func TestCanonicalHookEventMapsCodex(t *testing.T) {
	for _, event := range []string{"Stop", "SessionStart", "PreToolUse", "SubagentStop"} {
		if got := CanonicalHookEvent("codex-cli", event); got != event {
			t.Errorf("CanonicalHookEvent(codex-cli, %q) = %q, want %q", event, got, event)
		}
	}
}

// The heart of ADR 0146 §3: an event Claude Code has no name for gets no
// canonical name, rather than a copy of the name its harness used. Copying it
// would squat a name Claude Code has not chosen, so that when Claude Code does
// choose one, stored rows already claim it.
func TestCanonicalHookEventIsEmptyWithoutAClaudeCodeName(t *testing.T) {
	for _, tc := range []struct{ provider, event string }{
		{"codex-cli", "Interrupt"},
		{"codex-cli", "SomethingCodexAddsLater"},
		{"a-third-party-harness", "Frobnicate"},
		{"a-third-party-harness", "Stop"},
	} {
		if got := CanonicalHookEvent(tc.provider, tc.event); got != "" {
			t.Errorf("CanonicalHookEvent(%q, %q) = %q, want the empty string", tc.provider, tc.event, got)
		}
	}
}

func TestCanonicalHookEventIgnoresBlanks(t *testing.T) {
	for _, tc := range []struct{ provider, event string }{
		{"", "Stop"},
		{"codex-cli", ""},
		{"  ", "  "},
	} {
		if got := CanonicalHookEvent(tc.provider, tc.event); got != "" {
			t.Errorf("CanonicalHookEvent(%q, %q) = %q, want the empty string", tc.provider, tc.event, got)
		}
	}
}

func TestCanonicalHookEventTrims(t *testing.T) {
	if got := CanonicalHookEvent(" codex-cli ", " Stop "); got != "Stop" {
		t.Errorf("CanonicalHookEvent = %q, want Stop", got)
	}
}

// The fingerprint gates a backfill, and its failure is silent in the direction
// that matters: a version that stops changing skips the backfill forever, so a
// mapping entry added later never reaches the rows it was added for. Nothing
// else notices, so it is checked here.
func TestCanonicalHookEventsVersionChangesWithTheTable(t *testing.T) {
	restore := func(saved map[string]map[string]string) func() {
		return func() { canonicalHookEvents = saved }
	}
	original := canonicalHookEvents
	base := CanonicalHookEventsVersion()

	for _, tc := range []struct {
		name  string
		table map[string]map[string]string
	}{
		{"a new provider", map[string]map[string]string{
			"codex-cli":     original["codex-cli"],
			"a-new-harness": {"its.event": "Stop"},
		}},
		{"a new event for a provider it already has", func() map[string]map[string]string {
			grown := map[string]string{"Later": "Stop"}
			for k, v := range original["codex-cli"] {
				grown[k] = v
			}
			return map[string]map[string]string{"codex-cli": grown}
		}()},
		{"a different canonical name for the same event", func() map[string]map[string]string {
			changed := map[string]string{}
			for k, v := range original["codex-cli"] {
				changed[k] = v
			}
			changed["Stop"] = "StopFailure"
			return map[string]map[string]string{"codex-cli": changed}
		}()},
		{"an entry removed", map[string]map[string]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer restore(original)()
			canonicalHookEvents = tc.table
			if got := CanonicalHookEventsVersion(); got == base {
				t.Errorf("version is %s both before and after %s", got, tc.name)
			}
		})
	}
	canonicalHookEvents = original
	if again := CanonicalHookEventsVersion(); again != base {
		t.Fatalf("version = %s after restoring the table, want %s", again, base)
	}
}

// Stable across calls despite ranging over maps, which Go deliberately
// randomizes — an unsorted fingerprint would change on its own and re-run the
// backfill on every start, which is the cost the stamp exists to avoid.
func TestCanonicalHookEventsVersionIsStable(t *testing.T) {
	first := CanonicalHookEventsVersion()
	for range 50 {
		if got := CanonicalHookEventsVersion(); got != first {
			t.Fatalf("version = %s, want the same %s every time", got, first)
		}
	}
	if first == "" {
		t.Fatal("version is empty")
	}
}

// Two different tables must not hash alike by moving a character across a
// field boundary, which is what the length prefixes in the fingerprint are for.
func TestCanonicalHookEventsVersionDoesNotCollideAcrossFieldBoundaries(t *testing.T) {
	original := canonicalHookEvents
	defer func() { canonicalHookEvents = original }()

	canonicalHookEvents = map[string]map[string]string{"ab": {"c": "Stop"}}
	first := CanonicalHookEventsVersion()
	canonicalHookEvents = map[string]map[string]string{"a": {"bc": "Stop"}}
	if second := CanonicalHookEventsVersion(); second == first {
		t.Errorf("tables {ab:{c}} and {a:{bc}} both hash to %s", first)
	}
}
