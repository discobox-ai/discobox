package pi

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/harness"
)

// publishedEvents reads the event names out of the extension's
// `const PUBLISHED_FIELDS = { ... }`.
func publishedEvents(t *testing.T) []string {
	t.Helper()
	text := readFile(t, "hook-extension.js")
	first := strings.Index(text, "const PUBLISHED_FIELDS = {")
	if first < 0 {
		t.Fatal("extension declares no PUBLISHED_FIELDS")
	}
	last := strings.Index(text[first:], "\n};")
	if last < 0 {
		t.Fatal("PUBLISHED_FIELDS is not terminated")
	}
	var out []string
	for _, match := range regexp.MustCompile(`(?m)^\s+([a-z_]+): \[`).FindAllStringSubmatch(text[first:first+last], -1) {
		out = append(out, match[1])
	}
	if len(out) == 0 {
		t.Fatal("PUBLISHED_FIELDS is empty")
	}
	return out
}

// The extension, the mapping table, and ADR 0146's rule are one thing in three
// places. Every event the extension publishes must either carry a canonical
// name or be listed here as having none — so adding one without deciding what
// it is called across harnesses fails rather than quietly recording a hook
// nothing portable can match.
func TestEveryPublishedEventHasACanonicalNameOrIsKnownNotTo(t *testing.T) {
	// pi facts Claude Code has no word for. Each is still recorded and still
	// waitable under pi's own name (ADR 0146 §3).
	noCanonicalName := []string{"session_compact_failed"}
	published := publishedEvents(t)
	for _, event := range published {
		canonical := harness.CanonicalHookEvent(Driver{}.ID(), event)
		if slices.Contains(noCanonicalName, event) {
			if canonical != "" {
				t.Errorf("event %s now maps to %q; drop it from noCanonicalName", event, canonical)
			}
			continue
		}
		if canonical == "" {
			t.Errorf("event %s has no canonical name and is not listed as having none", event)
		}
	}
	for _, event := range noCanonicalName {
		if !slices.Contains(published, event) {
			t.Errorf("noCanonicalName lists %s, which the extension does not publish", event)
		}
	}
}

// The mapping table must not name a pi event the extension never publishes:
// a translation for an event nothing emits is a claim about pi that nothing
// tests.
func TestMappedEventsAreAllPublished(t *testing.T) {
	published := publishedEvents(t)
	for event, want := range map[string]string{
		"session_start":          "SessionStart",
		"session_shutdown":       "SessionEnd",
		"before_agent_start":     "UserPromptSubmit",
		"tool_call":              "PreToolUse",
		"tool_result":            "PostToolUse",
		"session_before_compact": "PreCompact",
		"session_compact":        "PostCompact",
		"agent_settled":          "Stop",
	} {
		if got := harness.CanonicalHookEvent(Driver{}.ID(), event); got != want {
			t.Errorf("event %s maps to %q, want %q", event, got, want)
		}
		if !slices.Contains(published, event) {
			t.Errorf("event %s has a canonical name but the extension never publishes it", event)
		}
	}
}

// The stream-rate events are the reason the extension keeps an allowlist: each
// publish spawns a process and writes a row. agent_end is per loop, not per
// run; a retry or a queued message continues after it.
func TestStreamRateEventsAreNotPublished(t *testing.T) {
	published := publishedEvents(t)
	for _, event := range []string{"message_start", "message_update", "message_end", "tool_execution_start", "tool_execution_update", "tool_execution_end", "turn_start", "turn_end", "context", "before_provider_request", "agent_end", "input"} {
		if slices.Contains(published, event) {
			t.Errorf("extension publishes %s, which is excluded on purpose", event)
		}
	}
}

// A publish holds a tool call up while it runs, so it is bounded, as the other
// images' command hooks are, to between one and three seconds.
func TestPublishIsBounded(t *testing.T) {
	match := regexp.MustCompile(`const PUBLISH_TIMEOUT_MS = (\d+);`).FindStringSubmatch(readFile(t, "hook-extension.js"))
	if match == nil {
		t.Fatal("extension declares no PUBLISH_TIMEOUT_MS")
	}
	if ms, _ := strconv.Atoi(match[1]); ms < 1000 || ms > 3000 {
		t.Fatalf("PUBLISH_TIMEOUT_MS = %d, want 1000–3000", ms)
	}
}

type publishedHook struct {
	Provider string         `json:"provider"`
	Event    string         `json:"event"`
	Payload  map[string]any `json:"payload"`
}

// runExtension runs the extension under node against synthetic events, with a
// stub publisher on PATH, and answers what it registered, what reached the
// publisher, and what its handlers returned. It executes the extension rather
// than reading it: what the payload keeps is one function a test grepping for
// names would not notice change.
func runExtension(t *testing.T, socket string) (registered []string, published []publishedHook, returned []any) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the harness image's scripts run on Linux")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; the extension's behavior is not exercised")
	}
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "discobox-hook-publish"), `#!/bin/sh
node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>require("fs").appendFileSync(process.env.PUBLISHED,JSON.stringify({provider:process.argv[2],event:process.argv[4],payload:JSON.parse(s)})+"\n"))' -- "$@"
`)
	cmd := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "extension-driver.mjs"))
	cmd.Env = append(os.Environ(),
		"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"PUBLISHED="+filepath.Join(dir, "published"),
		"DISCOBOX_HOOK_SOCKET="+socket,
	)
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			t.Fatalf("run extension driver: %v: %s", err, exit.Stderr)
		}
		t.Fatalf("run extension driver: %v", err)
	}
	var result struct {
		Registered []string        `json:"registered"`
		Published  []publishedHook `json:"published"`
		Returned   []any           `json:"returned"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &result); err != nil {
		t.Fatalf("parse driver output %q: %v", out, err)
	}
	return result.Registered, result.Published, result.Returned
}

// What the extension does, not what it says: the events worth recording reach
// the publisher in order and nothing else does; each carries its own fields
// and no system prompt, transcript, image or tool output, under whatever name
// pi carries them; and no handler returns anything that could block, rewrite
// or continue the turn.
func TestExtensionPublishes(t *testing.T) {
	_, published, returned := runExtension(t, "/run/discobox/hooks.sock")
	var events []string
	byEvent := map[string]map[string]any{}
	for _, hook := range published {
		if hook.Provider != (Driver{}).ID() {
			t.Errorf("published under provider %q, want %q", hook.Provider, Driver{}.ID())
		}
		events = append(events, hook.Event)
		byEvent[hook.Event] = hook.Payload
		raw, err := json.Marshal(hook.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "SECRET-") || strings.Contains(string(raw), "base64") {
			t.Errorf("%s payload carries the conversation: %s", hook.Event, raw)
		}
	}
	want := []string{
		"session_start", "before_agent_start", "tool_call", "tool_result",
		"session_before_compact", "session_compact", "session_compact_failed", "agent_settled", "session_shutdown",
	}
	if !slices.Equal(events, want) {
		t.Fatalf("published\n got %v\nwant %v", events, want)
	}
	for event, wantPayload := range map[string]map[string]any{
		"session_start":      {"reason": "startup", "cwd": "/workspace"},
		"before_agent_start": {"messageType": "user", "prompt": "fix it", "cwd": "/workspace"},
		"tool_call":          {"toolName": "bash", "toolCallId": "c1", "input": map[string]any{"command": "ls"}, "cwd": "/workspace"},
		"tool_result":        {"toolName": "bash", "toolCallId": "c1", "input": map[string]any{"command": "ls"}, "isError": false, "cwd": "/workspace"},
		"agent_settled":      {"aborted": false, "cwd": "/workspace"},
	} {
		got, err := json.Marshal(byEvent[event])
		if err != nil {
			t.Fatal(err)
		}
		wantJSON, err := json.Marshal(wantPayload)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(wantJSON) {
			t.Errorf("%s payload = %s, want %s", event, got, wantJSON)
		}
	}
	for _, r := range returned {
		if r != nil {
			t.Fatalf("a handler returned %v; recording a hook must never change the turn", r)
		}
	}
}

// Outside a Discobox terminal there is nothing to publish to, and the extension
// registers nothing rather than spawning a process per event to find out.
func TestExtensionIsSilentWithoutTheHookSocket(t *testing.T) {
	registered, published, _ := runExtension(t, "")
	if len(registered) != 0 || len(published) != 0 {
		t.Fatalf("registered %v and published %v with no hook socket", registered, published)
	}
}
