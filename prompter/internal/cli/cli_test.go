package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/obot-platform/discobox/prompter/internal/agent"
)

func TestRunDetectOnly(t *testing.T) {
	var stdout, stderr bytes.Buffer
	runner := &recordingRunner{}

	err := runWithDeps(context.Background(), []string{"--detect-only"}, nil, &stdout, &stderr, okGetwd, deps{
		environ: func() []string {
			return []string{"DISCOBOX_PROMPTER_AGENT=discobot"}
		},
		pid:      testPID,
		ancestry: noAncestry,
		runnerFor: func(agent.Detected) (agent.Runner, bool) {
			return runner, true
		},
	})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if stdout.String() != "discobot\n" {
		t.Fatalf("expected detected agent on stdout, got %q", stdout.String())
	}
	if runner.called {
		t.Fatal("expected detect-only mode not to dispatch runner")
	}
}

func TestRunDetectOnlyUnknown(t *testing.T) {
	var stdout, stderr bytes.Buffer

	err := runWithDeps(context.Background(), []string{"--detect-only"}, nil, &stdout, &stderr, okGetwd, deps{
		environ: func() []string {
			return nil
		},
		pid:      testPID,
		ancestry: noAncestry,
		runnerFor: func(agent.Detected) (agent.Runner, bool) {
			t.Fatal("runnerFor should not be called in detect-only mode")
			return nil, false
		},
	})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if stdout.String() != "unknown\n" {
		t.Fatalf("expected unknown on stdout, got %q", stdout.String())
	}
}

func TestRunRequiresPrompt(t *testing.T) {
	err := runForValidation([]string{"--session-id", "s1", "--model-class", "fast"})

	if err == nil || err.Error() != "missing required --prompt or prompt argument" {
		t.Fatalf("expected missing prompt error, got %v", err)
	}
}

func TestRunAllowsEphemeralSession(t *testing.T) {
	var stdout, stderr bytes.Buffer
	runner := &recordingRunner{result: agent.RunResult{Text: "done"}}

	err := runWithDeps(context.Background(), []string{"--prompt", "do work"}, nil, &stdout, &stderr, okGetwd, deps{
		environ: func() []string {
			return []string{"DISCOBOX_PROMPTER_AGENT=discobot"}
		},
		pid:      testPID,
		ancestry: noAncestry,
		runnerFor: func(detected agent.Detected) (agent.Runner, bool) {
			if detected.Kind != agent.KindDiscobot {
				t.Fatalf("expected discobot detection, got %q", detected.Kind)
			}
			return runner, true
		},
	})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if !runner.called {
		t.Fatal("expected runner to be called")
	}
	if runner.request.SessionID != "" {
		t.Fatalf("expected empty session ID for ephemeral run, got %q", runner.request.SessionID)
	}
	assertJSONResult(t, stdout.String(), agent.RunResult{Text: "done"})
}

func TestRunUsesPositionalPromptFallback(t *testing.T) {
	var stdout, stderr bytes.Buffer
	runner := &recordingRunner{result: agent.RunResult{Text: "done"}}

	err := runWithDeps(context.Background(), []string{"--session-id", "s1", "do", "work"}, nil, &stdout, &stderr, okGetwd, deps{
		environ: func() []string {
			return []string{"DISCOBOX_PROMPTER_AGENT=discobot"}
		},
		pid:      testPID,
		ancestry: noAncestry,
		runnerFor: func(detected agent.Detected) (agent.Runner, bool) {
			if detected.Kind != agent.KindDiscobot {
				t.Fatalf("expected discobot detection, got %q", detected.Kind)
			}
			return runner, true
		},
	})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if !runner.called {
		t.Fatal("expected runner to be called")
	}
	if runner.request.Prompt != "do work" {
		t.Fatalf("expected positional prompt fallback, got %q", runner.request.Prompt)
	}
}

func TestRunGetwdError(t *testing.T) {
	getwdErr := errors.New("cwd unavailable")
	err := runWithDeps(context.Background(), []string{"--session-id", "s1", "--prompt", "do work"}, nil, nil, nil, func() (string, error) {
		return "", getwdErr
	}, deps{
		environ: func() []string {
			return []string{"DISCOBOX_PROMPTER_AGENT=discobot"}
		},
		pid:      testPID,
		ancestry: noAncestry,
		runnerFor: func(agent.Detected) (agent.Runner, bool) {
			t.Fatal("runnerFor should not be called when getwd fails")
			return nil, false
		},
	})

	if !errors.Is(err, getwdErr) {
		t.Fatalf("expected getwd error, got %v", err)
	}
	if !strings.Contains(err.Error(), "resolve current working directory") {
		t.Fatalf("expected cwd context in error, got %v", err)
	}
}

func TestRunDispatchesRunRequest(t *testing.T) {
	var stdout, stderr bytes.Buffer
	runner := &recordingRunner{result: agent.RunResult{Text: "done", SessionID: "provider-1"}}

	err := runWithDeps(context.Background(), []string{"--session-id", "s1", "--prompt", "do work", "--agent", "reviewer", "--model", "gpt-5.5", "--reasoning", "high", "--service-tier", "flex"}, nil, &stdout, &stderr, func() (string, error) {
		return "/workspace/project", nil
	}, deps{
		environ: func() []string {
			return []string{"DISCOBOX_PROMPTER_AGENT=discobot"}
		},
		pid:      testPID,
		ancestry: noAncestry,
		runnerFor: func(detected agent.Detected) (agent.Runner, bool) {
			if detected.Kind != agent.KindDiscobot {
				t.Fatalf("expected discobot detection, got %q", detected.Kind)
			}
			return runner, true
		},
	})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	expected := agent.RunRequest{
		SessionID:   "s1",
		Prompt:      "do work",
		Agent:       "reviewer",
		Model:       "gpt-5.5",
		Reasoning:   "high",
		ServiceTier: "flex",
		Workdir:     "/workspace/project",
	}
	if runner.request != expected {
		t.Fatalf("expected request %#v, got %#v", expected, runner.request)
	}
	assertJSONResult(t, stdout.String(), agent.RunResult{Text: "done", SessionID: "provider-1"})
}

func runForValidation(args []string) error {
	return runWithDeps(context.Background(), args, nil, nil, nil, okGetwd, deps{
		environ: func() []string {
			return []string{"DISCOBOX_PROMPTER_AGENT=discobot"}
		},
		pid:      testPID,
		ancestry: noAncestry,
		runnerFor: func(agent.Detected) (agent.Runner, bool) {
			return &recordingRunner{}, true
		},
	})
}

func okGetwd() (string, error) {
	return "/workspace/project", nil
}

func testPID() int {
	return 100
}

func noAncestry(int) ([]agent.Process, error) {
	return nil, nil
}

type recordingRunner struct {
	called  bool
	request agent.RunRequest
	result  agent.RunResult
}

func (r *recordingRunner) Run(_ context.Context, request agent.RunRequest) (agent.RunResult, error) {
	r.called = true
	r.request = request
	return r.result, nil
}

func assertJSONResult(t *testing.T, output string, expected agent.RunResult) {
	t.Helper()
	var got agent.RunResult
	if err := json.Unmarshal([]byte(output), &got); err != nil {
		t.Fatalf("expected JSON output, got %q: %v", output, err)
	}
	if got != expected {
		t.Fatalf("expected JSON result %#v, got %#v", expected, got)
	}
}
