package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

type promptResult struct {
	Text      string `json:"text"`
	SessionID string `json:"sessionID"`
}

type promptDriver interface {
	Name() string
	Model() string
	EphemeralPrompt() string
	FirstPrompt(codeWord string) string
	SecondPrompt() string
}

type staticPromptDriver struct {
	name  string
	model func() string
}

var registeredPromptDrivers []promptDriver

func registerPromptDriver(driver promptDriver) {
	registeredPromptDrivers = append(registeredPromptDrivers, driver)
}

func init() {
	registerPromptDriver(staticPromptDriver{name: "claude-code", model: envModel("DISCOBOX_PROMPTER_E2E_CLAUDE_MODEL", "")})
	registerPromptDriver(staticPromptDriver{name: "codex", model: envModel("DISCOBOX_PROMPTER_E2E_CODEX_MODEL", "")})
	registerPromptDriver(staticPromptDriver{name: "gemini-cli", model: envModel("DISCOBOX_PROMPTER_E2E_GEMINI_MODEL", "")})
	registerPromptDriver(staticPromptDriver{name: "opencode", model: envModel("DISCOBOX_PROMPTER_E2E_OPENCODE_MODEL", "")})
	registerPromptDriver(staticPromptDriver{name: "discobot", model: discobotOpenAIModel})
}

func (d staticPromptDriver) Name() string {
	return d.name
}

func (d staticPromptDriver) Model() string {
	if d.model == nil {
		return ""
	}
	return d.model()
}

func (staticPromptDriver) EphemeralPrompt() string {
	return "Reply with exactly DISCOBOX_PROMPTER_E2E_EPHEMERAL_OK."
}

func (staticPromptDriver) FirstPrompt(codeWord string) string {
	return "Remember the code word " + codeWord + ". Reply with exactly DISCOBOX_PROMPTER_E2E_FIRST_OK."
}

func (staticPromptDriver) SecondPrompt() string {
	return "What code word did I ask you to remember? Reply with exactly DISCOBOX_PROMPTER_E2E_SECOND_OK followed by the code word."
}

func TestAgentPromptE2E(t *testing.T) {
	if os.Getenv(envPrompt) != "1" {
		t.Skipf("set %s=1 with %s to run real agent prompt e2e tests", envPrompt, envAgents)
	}
	cases := e2eCases()
	enabled := enabledAgents(t, cases)
	if len(enabled) == 0 {
		t.Skipf("set %s to a comma-separated list or all to run real agent prompt e2e tests", envAgents)
	}

	workspace := t.TempDir()
	prompter := buildPrompter(t, workspace)
	cacheDir := e2eCacheDir(t)
	t.Logf("using e2e cache directory: %s", cacheDir)

	for index, tc := range enabled {
		driver := requirePromptDriver(t, tc.Name)
		t.Run(tc.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout(t))
			defer cancel()

			requireCredentials(t, tc)
			agentBin := tc.Install(ctx, t, cacheDir, tc.EnvKey)

			ephemeral := runPrompterPrompt(ctx, t, workspace, tc, agentBin, promptCommand(prompter, "", driver.EphemeralPrompt(), driver.Model()))
			if !strings.Contains(ephemeral.Text, "DISCOBOX_PROMPTER_E2E_EPHEMERAL_OK") {
				t.Fatalf("expected ephemeral response to contain DISCOBOX_PROMPTER_E2E_EPHEMERAL_OK, got %#v", ephemeral)
			}

			sessionID := promptSessionID(index)
			codeWord := "bramble-" + tc.EnvKey
			first := runPrompterPrompt(ctx, t, workspace, tc, agentBin, promptCommand(prompter, sessionID, driver.FirstPrompt(codeWord), driver.Model()))
			if first.SessionID == "" {
				t.Fatalf("expected persistent first response to include provider session id, got %#v", first)
			}
			if !strings.Contains(first.Text, "DISCOBOX_PROMPTER_E2E_FIRST_OK") {
				t.Fatalf("expected first response to contain DISCOBOX_PROMPTER_E2E_FIRST_OK, got %#v", first)
			}

			second := runPrompterPrompt(ctx, t, workspace, tc, agentBin, promptCommand(prompter, sessionID, driver.SecondPrompt(), driver.Model()))
			if !strings.Contains(second.Text, "DISCOBOX_PROMPTER_E2E_SECOND_OK") || !strings.Contains(second.Text, codeWord) {
				t.Fatalf("expected second response to include follow-on marker and code word %q, got %#v", codeWord, second)
			}
		})
	}
}

func requirePromptDriver(t *testing.T, name string) promptDriver {
	t.Helper()
	for _, driver := range registeredPromptDrivers {
		if driver.Name() == name {
			return driver
		}
	}
	var names []string
	for _, driver := range registeredPromptDrivers {
		names = append(names, driver.Name())
	}
	sort.Strings(names)
	t.Fatalf("no prompt e2e driver for %q; known drivers: %s", name, strings.Join(names, ", "))
	return nil
}

func runPrompterPrompt(ctx context.Context, t *testing.T, workspace string, tc agentCase, agentBin string, command string) promptResult {
	t.Helper()
	output := runAgent(ctx, t, workspace, tc, agentBin, promptExecutionPrompt(command))
	return extractPromptResult(t, output)
}

func promptExecutionPrompt(command string) string {
	return "Run this exact shell command from the current working directory and report only its stdout. The stdout is a single JSON object; do not wrap it in markdown or add explanation: " + command
}

func promptCommand(prompter string, sessionID string, prompt string, model string) string {
	parts := []string{shellQuote(prompter)}
	if sessionID != "" {
		parts = append(parts, "--session-id", shellQuote(sessionID))
	}
	if model != "" {
		parts = append(parts, "--model", shellQuote(model))
	}
	parts = append(parts, "--prompt", shellQuote(prompt))
	return strings.Join(parts, " ")
}

func extractPromptResult(t *testing.T, output string) promptResult {
	t.Helper()
	for _, candidate := range jsonCandidates(output) {
		var result promptResult
		if err := json.Unmarshal([]byte(candidate), &result); err == nil && result.Text != "" {
			return result
		}
	}
	t.Fatalf("expected output to contain prompter JSON result, got:\n%s", output)
	return promptResult{}
}

func jsonCandidates(output string) []string {
	var candidates []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "```json")
		line = strings.TrimPrefix(line, "```")
		line = strings.TrimSuffix(line, "```")
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "{") && strings.HasSuffix(line, "}") {
			candidates = append(candidates, line)
		}
	}
	for start := 0; start < len(output); start++ {
		if output[start] != '{' {
			continue
		}
		if candidate, ok := nextJSONObject(output[start:]); ok {
			candidates = append(candidates, candidate)
		}
	}
	return candidates
}

func nextJSONObject(value string) (string, bool) {
	depth := 0
	inString := false
	escaped := false
	for i, char := range value {
		if inString {
			if escaped {
				escaped = false
				continue
			}
			switch char {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch char {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return value[:i+1], true
			}
		}
	}
	return "", false
}

func promptSessionID(index int) string {
	return fmt.Sprintf("11111111-1111-4111-8111-%012d", index+1)
}

func envModel(name string, defaultValue string) func() string {
	return func() string {
		if model := strings.TrimSpace(os.Getenv(name)); model != "" {
			return model
		}
		return defaultValue
	}
}
