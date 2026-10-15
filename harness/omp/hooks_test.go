package omp

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

// extensionSet reads one `const NAME = new Set([...])` out of the extension.
func extensionSet(t *testing.T, name string) []string {
	t.Helper()
	text := readFile(t, "hook-extension.js")
	first := strings.Index(text, name+" = new Set([")
	if first < 0 {
		t.Fatalf("extension declares no %s", name)
	}
	last := strings.Index(text[first:], "])")
	if last < 0 {
		t.Fatalf("%s is not terminated", name)
	}
	var out []string
	for _, line := range strings.Split(text[first:first+last], "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, `"`) {
			continue
		}
		if event := strings.Trim(strings.TrimSuffix(line, ","), `"`); event != "" {
			out = append(out, event)
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s is empty", name)
	}
	return out
}

// The extension, the mapping table, and ADR 0146's rule are one thing in three
// places. Every event the extension publishes must either carry a canonical
// name or be listed here as having none — so adding one without deciding what
// it is called across harnesses fails rather than quietly recording a hook
// nothing portable can match.
func TestEveryPublishedEventHasACanonicalNameOrIsKnownNotTo(t *testing.T) {
	// omp facts Claude Code has no word for. Each is still recorded and still
	// waitable under omp's own name (ADR 0146 §3).
	noCanonicalName := []string{"before_subagent_spawn", "auto_retry_start", "credential_disabled"}
	published := extensionSet(t, "PUBLISHED_EVENTS")
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

// The mapping table must not name an omp event the extension never publishes:
// a translation for an event nothing emits is a claim about omp that nothing
// tests.
func TestMappedEventsAreAllPublished(t *testing.T) {
	published := extensionSet(t, "PUBLISHED_EVENTS")
	for _, event := range []string{
		"session_start", "session_shutdown", "before_agent_start", "tool_call", "tool_result",
		"tool_approval_requested", "session_before_compact", "session_compact", "session_stop",
	} {
		if harness.CanonicalHookEvent(Driver{}.ID(), event) == "" {
			t.Errorf("event %s lost its canonical name", event)
		}
		if !slices.Contains(published, event) {
			t.Errorf("event %s has a canonical name but the extension never publishes it", event)
		}
	}
}

// Every lifecycle event with a canonical name depends on the extension
// dropping a subagent's copy of it, and "every" is meant literally: a session
// event published for subagents too would end a wait at the first subagent.
// Tool events are not session events and must not be filtered.
func TestEveryCanonicalSessionEventIsMainOnly(t *testing.T) {
	mainOnly := extensionSet(t, "MAIN_ONLY_EVENTS")
	for _, event := range extensionSet(t, "PUBLISHED_EVENTS") {
		isSession := strings.HasPrefix(event, "session_") || event == "before_agent_start"
		if isSession && harness.CanonicalHookEvent(Driver{}.ID(), event) != "" && !slices.Contains(mainOnly, event) {
			t.Errorf("%s has a canonical name but is published for subagents too", event)
		}
		if strings.HasPrefix(event, "tool_") && slices.Contains(mainOnly, event) {
			t.Errorf("%s is filtered to the main session, so a subagent's tool use goes unrecorded", event)
		}
	}
}

// The stream-rate events are the reason the extension keeps an allowlist: each
// publish spawns a process and writes a row.
func TestStreamRateEventsAreNotPublished(t *testing.T) {
	published := extensionSet(t, "PUBLISHED_EVENTS")
	for _, event := range []string{"message_start", "message_update", "message_end", "tool_execution_update", "turn_start", "turn_end", "context", "before_provider_request"} {
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
// than reading it: the filtering this image depends on is one line that a test
// grepping for names would not miss if it were deleted.
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

// What the extension does, not what it says: a subagent's lifecycle is
// dropped and the main session's is not; tool calls are published whichever
// session makes them; the transcript stays out of the payload; and no handler
// returns anything that could block, rewrite or continue the turn.
func TestExtensionPublishes(t *testing.T) {
	_, published, returned := runExtension(t, "/run/discobox/hooks.sock")
	var events []string
	for _, hook := range published {
		if hook.Provider != (Driver{}).ID() {
			t.Errorf("published under provider %q, want %q", hook.Provider, Driver{}.ID())
		}
		events = append(events, hook.Event)
	}
	want := []string{
		"tool_call", "tool_result", // the subagent's; its lifecycle was dropped
		"session_start", "before_agent_start", "tool_call", "tool_result",
		"session_before_compact", "session_compact", "session_stop", "session_shutdown",
	}
	if !slices.Equal(events, want) {
		t.Fatalf("published\n got %v\nwant %v", events, want)
	}
	for _, hook := range published {
		// Nothing but scalars, the agent, and a tool's input: no transcript,
		// system prompt, image, tool output or compaction entries, under
		// whatever name omp carries them.
		for key, value := range hook.Payload {
			switch value.(type) {
			case map[string]any, []any:
				if key != "agent" && key != "input" {
					t.Errorf("%s payload carries %s", hook.Event, key)
				}
			}
		}
		agent, _ := hook.Payload["agent"].(map[string]any)
		if agent["kind"] == nil {
			t.Errorf("%s payload names no agent: %v", hook.Event, hook.Payload)
		}
	}
	if call := published[0].Payload; call["toolName"] != "bash" || call["input"].(map[string]any)["command"] != "ls" {
		t.Errorf("tool_call payload = %v, want the tool and its input", call)
	}
	if prompt := published[3].Payload; prompt["prompt"] != "fix it" {
		t.Errorf("before_agent_start payload = %v, want the prompt", prompt)
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
