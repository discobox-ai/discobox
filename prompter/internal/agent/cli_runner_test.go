package agent

import (
	"context"
	"path/filepath"
	"testing"
)

type fakeExecutor struct {
	commands []Command
	result   CommandResult
	err      error
}

func (f *fakeExecutor) Run(_ context.Context, command Command) (CommandResult, error) {
	f.commands = append(f.commands, command)
	return f.result, f.err
}

type fakePromptDriver struct {
	kind            Kind
	command         Command
	directSessionID bool
}

func (d fakePromptDriver) Kind() Kind {
	return d.kind
}

func (d fakePromptDriver) Command(_ RunRequest, _ string) PromptCommand {
	return PromptCommand{Command: d.command, DirectSessionID: d.directSessionID}
}

func TestCLIRunnerCreatesPersistentMapping(t *testing.T) {
	executor := &fakeExecutor{result: CommandResult{Stdout: []byte(`{"session_id":"provider-1","text":"done"}` + "\n")}}
	store := SessionStore{Path: filepath.Join(t.TempDir(), "sessions.json")}
	workdir := t.TempDir()
	driver := fakePromptDriver{
		kind:    KindCodex,
		command: Command{Name: "codex", Args: []string{"exec", "--json", "do work"}, Dir: workdir},
	}
	runner := CLIRunner{Driver: driver, Executor: executor, Store: store}

	result, err := runner.Run(context.Background(), RunRequest{
		SessionID:   "caller-1",
		Prompt:      "do work",
		Model:       "gpt-5.5",
		Reasoning:   "high",
		ServiceTier: "flex",
		Workdir:     workdir,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Text != "done" || result.SessionID != "provider-1" {
		t.Fatalf("expected normalized result, got %#v", result)
	}
	if len(executor.commands) != 1 {
		t.Fatalf("expected one command, got %d", len(executor.commands))
	}
	assertCommand(t, executor.commands[0], Command{
		Name: "codex",
		Args: []string{"exec", "--json", "do work"},
		Dir:  workdir,
	})

	record, ok, err := store.Get(KindCodex, workdir, "caller-1")
	if err != nil {
		t.Fatalf("get mapping: %v", err)
	}
	if !ok || record.ProviderSessionID != "provider-1" {
		t.Fatalf("expected mapping to provider-1, ok=%v record=%#v", ok, record)
	}
}

func TestCLIRunnerResumesMappedSession(t *testing.T) {
	executor := &fakeExecutor{result: CommandResult{Stdout: []byte(`{"session_id":"provider-1","text":"done again"}` + "\n")}}
	store := SessionStore{Path: filepath.Join(t.TempDir(), "sessions.json")}
	workdir := t.TempDir()
	if err := store.Put(SessionRecord{Agent: KindOpenCode, Workdir: workdir, CallerSessionID: "caller-1", ProviderSessionID: "provider-1"}); err != nil {
		t.Fatalf("put mapping: %v", err)
	}
	driver := fakePromptDriver{
		kind:    KindOpenCode,
		command: Command{Name: "opencode", Args: []string{"run", "--session", "provider-1", "continue work"}, Dir: workdir},
	}
	runner := CLIRunner{Driver: driver, Executor: executor, Store: store}

	_, err := runner.Run(context.Background(), RunRequest{
		SessionID: "caller-1",
		Prompt:    "continue work",
		Agent:     "build",
		Model:     "openai/gpt-5.5",
		Reasoning: "high",
		Workdir:   workdir,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	assertCommand(t, executor.commands[0], Command{
		Name: "opencode",
		Args: []string{"run", "--session", "provider-1", "continue work"},
		Dir:  workdir,
	})
}

func TestCLIRunnerAllowsEphemeralWithoutSessionMapping(t *testing.T) {
	executor := &fakeExecutor{result: CommandResult{Stdout: []byte(`{"text":"done"}` + "\n")}}
	store := SessionStore{Path: filepath.Join(t.TempDir(), "sessions.json")}
	workdir := t.TempDir()
	driver := fakePromptDriver{
		kind:    KindCodex,
		command: Command{Name: "codex", Args: []string{"exec", "--json", "--ephemeral", "do work"}, Dir: workdir},
	}
	runner := CLIRunner{Driver: driver, Executor: executor, Store: store}

	result, err := runner.Run(context.Background(), RunRequest{Prompt: "do work", Workdir: workdir})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.SessionID != "" {
		t.Fatalf("expected no session id for ephemeral run, got %#v", result)
	}
	assertCommand(t, executor.commands[0], Command{
		Name: "codex",
		Args: []string{"exec", "--json", "--ephemeral", "do work"},
		Dir:  workdir,
	})
}

func TestDirectUUIDProviderStoresCallerUUIDAsProviderSession(t *testing.T) {
	executor := &fakeExecutor{result: CommandResult{Stdout: []byte(`{"text":"done"}` + "\n")}}
	store := SessionStore{Path: filepath.Join(t.TempDir(), "sessions.json")}
	workdir := t.TempDir()
	uuid := "11111111-1111-1111-1111-111111111111"
	driver := fakePromptDriver{
		kind:            KindClaudeCode,
		command:         Command{Name: "claude", Args: []string{"--session-id", uuid, "do work"}, Dir: workdir},
		directSessionID: true,
	}
	runner := CLIRunner{Driver: driver, Executor: executor, Store: store}

	result, err := runner.Run(context.Background(), RunRequest{SessionID: uuid, Prompt: "do work", Workdir: workdir})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.SessionID != uuid {
		t.Fatalf("expected direct UUID as provider session, got %#v", result)
	}
	assertCommand(t, executor.commands[0], Command{
		Name: "claude",
		Args: []string{"--session-id", uuid, "do work"},
		Dir:  workdir,
	})
}

func TestParseProviderJSONOutputReadsJSONLines(t *testing.T) {
	result := ParseProviderJSONOutput([]byte("not json\n" + `{"type":"session","thread_id":"thread-1"}` + "\n" + `{"type":"message","message":"final text"}` + "\n"))
	if result.SessionID != "thread-1" || result.Text != "final text" {
		t.Fatalf("expected parsed result, got %#v", result)
	}
}

func assertCommand(t *testing.T, got Command, expected Command) {
	t.Helper()
	if got.Name != expected.Name || got.Dir != expected.Dir || !equalStrings(got.Args, expected.Args) {
		t.Fatalf("expected command %#v, got %#v", expected, got)
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
