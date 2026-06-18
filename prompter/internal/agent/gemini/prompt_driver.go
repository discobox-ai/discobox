package gemini

import (
	"regexp"

	"github.com/obot-platform/discobox/prompter/internal/agent"
)

type promptDriver struct{}

func init() {
	agent.RegisterPromptDriver(promptDriver{})
}

func (promptDriver) Kind() agent.Kind {
	return agent.KindGeminiCLI
}

func (promptDriver) Command(request agent.RunRequest, providerSessionID string) agent.PromptCommand {
	args := []string{"--prompt", request.Prompt, "--output-format", "json", "--skip-trust"}
	if request.Model != "" {
		args = append(args, "--model", request.Model)
	}
	if providerSessionID != "" {
		args = append(args, "--resume", providerSessionID)
		return agent.PromptCommand{Command: agent.Command{Name: "gemini", Args: args, Dir: request.Workdir}}
	}
	if request.SessionID != "" && isUUID(request.SessionID) {
		args = append(args, "--session-id", request.SessionID)
		return agent.PromptCommand{Command: agent.Command{Name: "gemini", Args: args, Dir: request.Workdir}, DirectSessionID: true}
	}
	return agent.PromptCommand{Command: agent.Command{Name: "gemini", Args: args, Dir: request.Workdir}}
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func isUUID(value string) bool {
	return uuidPattern.MatchString(value)
}
