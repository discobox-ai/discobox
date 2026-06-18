package discobot

import "github.com/obot-platform/discobox/prompter/internal/agent"

type promptDriver struct{}

func init() {
	agent.RegisterPromptDriver(promptDriver{})
}

func (promptDriver) Kind() agent.Kind {
	return agent.KindDiscobot
}

func (promptDriver) Command(request agent.RunRequest, providerSessionID string) agent.PromptCommand {
	args := []string{"--print", "--json"}
	if request.Model != "" {
		args = append(args, "--model", request.Model)
	}
	if request.Agent != "" {
		args = append(args, "--subagent", request.Agent)
	}
	if request.Reasoning != "" {
		args = append(args, "--reasoning", request.Reasoning)
	}
	if request.ServiceTier != "" {
		args = append(args, "--service-tier", request.ServiceTier)
	}
	if providerSessionID != "" {
		args = append(args, "--resume", providerSessionID)
	}
	args = append(args, request.Prompt)
	return agent.PromptCommand{Command: agent.Command{Name: "disco", Args: args, Dir: request.Workdir}}
}
