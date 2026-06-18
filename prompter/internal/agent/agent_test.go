package agent_test

import (
	"testing"

	"github.com/obot-platform/discobox/prompter/internal/agent"
	_ "github.com/obot-platform/discobox/prompter/internal/agent/all"
)

func TestDetectHonorsPrompterAgent(t *testing.T) {
	detected := agent.Detect(map[string]string{
		"DISCOBOX_PROMPTER_AGENT": "Discobot",
		"DISCOBOT_SESSION_ID":     "ignored",
	})

	if detected.Kind != agent.KindDiscobot {
		t.Fatalf("expected %q, got %q", agent.KindDiscobot, detected.Kind)
	}
	if detected.Source != "DISCOBOX_PROMPTER_AGENT" {
		t.Fatalf("expected DISCOBOX_PROMPTER_AGENT source, got %q", detected.Source)
	}
}

func TestDetectDiscobotSessionID(t *testing.T) {
	detected := agent.Detect(map[string]string{
		"DISCOBOT_SESSION_ID": "session-1",
	})

	if detected.Kind != agent.KindDiscobot {
		t.Fatalf("expected %q, got %q", agent.KindDiscobot, detected.Kind)
	}
	if detected.Source != "env:DISCOBOT_SESSION_ID" {
		t.Fatalf("expected DISCOBOT_SESSION_ID source, got %q", detected.Source)
	}
}

func TestDetectFromEnvKey(t *testing.T) {
	detected := agent.Detect(map[string]string{
		"CODEX_THREAD_ID": "thread-1",
	})

	if detected.Kind != agent.KindCodex {
		t.Fatalf("expected %q, got %q", agent.KindCodex, detected.Kind)
	}
	if detected.Source != "env:CODEX_THREAD_ID" {
		t.Fatalf("expected CODEX_THREAD_ID source, got %q", detected.Source)
	}
}

func TestDetectFromEnvValue(t *testing.T) {
	tests := map[string]struct {
		env  map[string]string
		kind agent.Kind
	}{
		"opencode": {
			env: map[string]string{
				"OPENCODE":     "1",
				"OPENCODE_PID": "123",
			},
			kind: agent.KindOpenCode,
		},
		"gemini": {
			env: map[string]string{
				"GEMINI_CLI": "1",
			},
			kind: agent.KindGeminiCLI,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			detected := agent.Detect(test.env)
			if detected.Kind != test.kind {
				t.Fatalf("expected %q, got %q", test.kind, detected.Kind)
			}
		})
	}
}

func TestDetectFromAncestry(t *testing.T) {
	detected := agent.DetectFrom(map[string]string{}, []agent.Process{
		{PID: 2, PPID: 1, Comm: "bash", Exe: "/usr/bin/bash"},
		{PID: 1, PPID: 0, Comm: "claude", Exe: "/usr/local/bin/claude"},
	})

	if detected.Kind != agent.KindClaudeCode {
		t.Fatalf("expected %q, got %q", agent.KindClaudeCode, detected.Kind)
	}
	if detected.Source != "process:claude" {
		t.Fatalf("expected process source, got %q", detected.Source)
	}
}

func TestDetectDiscobotFromDiscoAncestry(t *testing.T) {
	detected := agent.DetectFrom(map[string]string{}, []agent.Process{
		{PID: 2, PPID: 1, Comm: "bash", Exe: "/usr/bin/bash"},
		{PID: 1, PPID: 0, Comm: "disco", Exe: "/opt/discobot/bin/disco"},
	})

	if detected.Kind != agent.KindDiscobot {
		t.Fatalf("expected %q, got %q", agent.KindDiscobot, detected.Kind)
	}
	if detected.Source != "process:disco" {
		t.Fatalf("expected process source, got %q", detected.Source)
	}
}

func TestDetectUnknown(t *testing.T) {
	detected := agent.Detect(map[string]string{})

	if detected.Kind != agent.KindUnknown {
		t.Fatalf("expected %q, got %q", agent.KindUnknown, detected.Kind)
	}
	if detected.Source != "" {
		t.Fatalf("expected empty source, got %q", detected.Source)
	}
}

func TestDetectWithDoesNotLoadProcessAncestryForEnvironmentMatch(t *testing.T) {
	ancestryCalls := 0
	detected := agent.DetectWith([]agent.Detector{
		agent.StaticDetector{
			AgentKind: agent.KindCodex,
			EnvKeys:   []string{"CODEX_THREAD_ID"},
		},
		agent.StaticDetector{
			AgentKind:    agent.KindClaudeCode,
			ProcessNames: []string{"claude"},
		},
	}, &agent.Sources{
		EnvironmentProvider: func() map[string]string {
			return map[string]string{"CODEX_THREAD_ID": "thread-1"}
		},
		ProcessAncestryProvider: func() []agent.Process {
			ancestryCalls++
			return []agent.Process{{Comm: "claude"}}
		},
	})

	if detected.Kind != agent.KindCodex {
		t.Fatalf("expected %q, got %q", agent.KindCodex, detected.Kind)
	}
	if ancestryCalls != 0 {
		t.Fatalf("expected process ancestry not to be loaded, got %d calls", ancestryCalls)
	}
}

func TestDetectWithLoadsProcessAncestryOnceForProcessDetectors(t *testing.T) {
	ancestryCalls := 0
	detected := agent.DetectWith([]agent.Detector{
		agent.StaticDetector{
			AgentKind: agent.KindCodex,
			EnvKeys:   []string{"CODEX_THREAD_ID"},
		},
		agent.StaticDetector{
			AgentKind:    agent.KindOpenCode,
			ProcessNames: []string{"opencode"},
		},
		agent.StaticDetector{
			AgentKind:    agent.KindClaudeCode,
			ProcessNames: []string{"claude"},
		},
	}, &agent.Sources{
		EnvironmentProvider: func() map[string]string {
			return map[string]string{}
		},
		ProcessAncestryProvider: func() []agent.Process {
			ancestryCalls++
			return []agent.Process{{Comm: "claude"}}
		},
	})

	if detected.Kind != agent.KindClaudeCode {
		t.Fatalf("expected %q, got %q", agent.KindClaudeCode, detected.Kind)
	}
	if ancestryCalls != 1 {
		t.Fatalf("expected process ancestry to be loaded once, got %d calls", ancestryCalls)
	}
}

func TestEnviron(t *testing.T) {
	env := agent.Environ([]string{"DISCOBOX_PROMPTER_AGENT=discobot", "INVALID", "EMPTY="})

	if env["DISCOBOX_PROMPTER_AGENT"] != "discobot" {
		t.Fatalf("expected DISCOBOX_PROMPTER_AGENT to be preserved, got %q", env["DISCOBOX_PROMPTER_AGENT"])
	}
	if _, ok := env["INVALID"]; ok {
		t.Fatal("expected invalid environment entry to be ignored")
	}
	if env["EMPTY"] != "" {
		t.Fatalf("expected empty value to be preserved, got %q", env["EMPTY"])
	}
}
