package codex

import (
	"fmt"

	"github.com/obot-platform/discobox/prompter/internal/agent"
)

type promptDriver struct{}

func init() {
	agent.RegisterPromptDriver(promptDriver{})
}

func (promptDriver) Kind() agent.Kind {
	return agent.KindCodex
}

func (promptDriver) Command(request agent.RunRequest, providerSessionID string) agent.PromptCommand {
	common := []string{"--json"}
	if request.Model != "" {
		common = append(common, "--model", request.Model)
	}
	if request.Reasoning != "" {
		common = append(common, "--config", fmt.Sprintf("model_reasoning_effort=%q", request.Reasoning))
	}
	if request.ServiceTier != "" {
		common = append(common, "--config", fmt.Sprintf("model_service_tier=%q", request.ServiceTier))
	}
	if providerSessionID != "" {
		args := append([]string{"exec", "resume"}, common...)
		args = append(args, providerSessionID, request.Prompt)
		return agent.PromptCommand{Command: agent.Command{Name: "codex", Args: args, Dir: request.Workdir}}
	}
	args := append([]string{"exec"}, common...)
	if request.SessionID == "" {
		args = append(args, "--ephemeral")
	}
	args = append(args, request.Prompt)
	return agent.PromptCommand{Command: agent.Command{Name: "codex", Args: args, Dir: request.Workdir}}
}
