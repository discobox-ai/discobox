package claudecode

import (
	"regexp"

	"github.com/obot-platform/discobox/prompter/internal/agent"
)

type promptDriver struct{}

func init() {
	agent.RegisterPromptDriver(promptDriver{})
}

func (promptDriver) Kind() agent.Kind {
	return agent.KindClaudeCode
}

func (promptDriver) Command(request agent.RunRequest, providerSessionID string) agent.PromptCommand {
	args := []string{"--bare", "--permission-mode", "bypassPermissions", "-p", request.Prompt, "--output-format", "json"}
	if request.Model != "" {
		args = append(args, "--model", request.Model)
	}
	if request.Agent != "" {
		args = append(args, "--agent", request.Agent)
	}
	if request.Reasoning != "" {
		args = append(args, "--effort", request.Reasoning)
	}
	if providerSessionID != "" {
		args = append(args, "--resume", providerSessionID)
		return agent.PromptCommand{Command: agent.Command{Name: "claude", Args: args, Dir: request.Workdir}}
	}
	if request.SessionID != "" && isUUID(request.SessionID) {
		args = append(args, "--session-id", request.SessionID)
		return agent.PromptCommand{Command: agent.Command{Name: "claude", Args: args, Dir: request.Workdir}, DirectSessionID: true}
	}
	if request.SessionID == "" {
		args = append(args, "--no-session-persistence")
	}
	return agent.PromptCommand{Command: agent.Command{Name: "claude", Args: args, Dir: request.Workdir}}
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func isUUID(value string) bool {
	return uuidPattern.MatchString(value)
}
