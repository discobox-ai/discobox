package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Command describes one provider CLI invocation.
type Command struct {
	Name string
	Args []string
	Dir  string
}

// CommandResult captures provider CLI output.
type CommandResult struct {
	Stdout []byte
	Stderr []byte
}

// CommandExecutor executes provider CLI invocations.
type CommandExecutor interface {
	Run(context.Context, Command) (CommandResult, error)
}

// PromptCommand describes the CLI invocation and whether the provider can use
// RunRequest.SessionID directly as its own session identifier.
type PromptCommand struct {
	Command         Command
	DirectSessionID bool
}

// PromptDriver maps a normalized request to one provider CLI invocation.
type PromptDriver interface {
	Kind() Kind
	Command(RunRequest, string) PromptCommand
}

var promptDrivers = map[Kind]PromptDriver{}

// RegisterPromptDriver adds one provider-specific prompt adapter.
func RegisterPromptDriver(driver PromptDriver) {
	promptDrivers[driver.Kind()] = driver
}

// PromptDriverFor returns the registered prompt adapter for one agent kind.
func PromptDriverFor(kind Kind) (PromptDriver, bool) {
	driver, ok := promptDrivers[kind]
	return driver, ok
}

type execCommandExecutor struct{}

func (execCommandExecutor) Run(ctx context.Context, command Command) (CommandResult, error) {
	cmd := exec.CommandContext(ctx, command.Name, command.Args...)
	cmd.Dir = command.Dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return CommandResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, err
}

// CLIRunner adapts one detected coding-agent CLI to the normalized prompter
// contract.
type CLIRunner struct {
	Driver   PromptDriver
	Executor CommandExecutor
	Store    SessionStore

	storeErr error
}

// NewCLIRunner creates a default runner for one agent kind.
func NewCLIRunner(kind Kind) CLIRunner {
	store, err := DefaultSessionStore()
	driver, _ := PromptDriverFor(kind)
	return CLIRunner{
		Driver:   driver,
		Executor: execCommandExecutor{},
		Store:    store,
		storeErr: err,
	}
}

func (r CLIRunner) Run(ctx context.Context, request RunRequest) (RunResult, error) {
	if strings.TrimSpace(request.Prompt) == "" {
		return RunResult{}, errors.New("empty prompt")
	}
	if strings.TrimSpace(request.Workdir) == "" {
		return RunResult{}, errors.New("empty workdir")
	}
	if r.Executor == nil {
		r.Executor = execCommandExecutor{}
	}
	if r.Driver == nil {
		return RunResult{}, errors.New("missing prompt driver")
	}
	kind := r.Driver.Kind()

	persistent := request.SessionID != ""
	providerSessionID := ""
	if persistent {
		if r.storeErr != nil {
			return RunResult{}, r.storeErr
		}
		record, ok, err := r.Store.Get(kind, request.Workdir, request.SessionID)
		if err != nil {
			return RunResult{}, fmt.Errorf("read session mapping: %w", err)
		}
		if ok {
			providerSessionID = record.ProviderSessionID
		}
	}

	promptCommand := r.Driver.Command(request, providerSessionID)
	result, err := r.Executor.Run(ctx, promptCommand.Command)
	if err != nil {
		return RunResult{}, fmt.Errorf("run %s: %w%s", kind, err, stderrSuffix(result.Stderr))
	}

	parsed := ParseProviderJSONOutput(result.Stdout)
	if parsed.Text == "" {
		parsed.Text = strings.TrimSpace(string(result.Stdout))
	}
	if parsed.SessionID == "" {
		parsed.SessionID = providerSessionID
	}
	if parsed.SessionID == "" && promptCommand.DirectSessionID {
		parsed.SessionID = request.SessionID
	}

	if persistent {
		if parsed.SessionID == "" {
			return RunResult{}, fmt.Errorf("%s did not report a provider session id for persistent session %q", kind, request.SessionID)
		}
		if err := r.Store.Put(SessionRecord{
			Agent:             kind,
			Workdir:           request.Workdir,
			CallerSessionID:   request.SessionID,
			ProviderSessionID: parsed.SessionID,
		}); err != nil {
			return RunResult{}, fmt.Errorf("write session mapping: %w", err)
		}
	}

	return parsed, nil
}

func stderrSuffix(stderr []byte) string {
	text := strings.TrimSpace(string(stderr))
	if text == "" {
		return ""
	}
	return ": " + text
}

// ParseProviderJSONOutput extracts normalized text and provider session id from
// JSON or JSONL provider output.
func ParseProviderJSONOutput(output []byte) RunResult {
	var result RunResult
	for _, line := range bytes.Split(output, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var value any
		if err := json.Unmarshal(line, &value); err != nil {
			continue
		}
		if sessionID := findStringByKeys(value, sessionIDKeys); sessionID != "" {
			result.SessionID = sessionID
		}
		if text := findStringByKeys(value, textKeys); text != "" {
			result.Text = text
		}
	}
	return result
}

var sessionIDKeys = map[string]struct{}{
	"sessionid":  {},
	"session_id": {},
	"threadid":   {},
	"thread_id":  {},
}

var textKeys = map[string]struct{}{
	"text":    {},
	"result":  {},
	"output":  {},
	"content": {},
	"message": {},
}

func findStringByKeys(value any, keys map[string]struct{}) string {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if _, ok := keys[normalizeJSONKey(key)]; ok {
				if text, ok := child.(string); ok && text != "" {
					return text
				}
			}
		}
		for _, child := range typed {
			if text := findStringByKeys(child, keys); text != "" {
				return text
			}
		}
	case []any:
		for _, child := range typed {
			if text := findStringByKeys(child, keys); text != "" {
				return text
			}
		}
	}
	return ""
}

func normalizeJSONKey(key string) string {
	key = strings.ToLower(key)
	key = strings.ReplaceAll(key, "-", "_")
	return key
}
