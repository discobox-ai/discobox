package opencode

import "github.com/obot-platform/discobox/prompter/internal/agent"

type promptDriver struct{}

func init() {
	agent.RegisterPromptDriver(promptDriver{})
}

func (promptDriver) Kind() agent.Kind {
	return agent.KindOpenCode
}

func (promptDriver) Command(request agent.RunRequest, providerSessionID string) agent.PromptCommand {
	args := []string{"run", "--format", "json", "--dir", request.Workdir}
	if request.Model != "" {
		args = append(args, "--model", request.Model)
	}
	if request.Agent != "" {
		args = append(args, "--agent", request.Agent)
	}
	if request.Reasoning != "" {
		args = append(args, "--variant", request.Reasoning)
	}
	if providerSessionID != "" {
		args = append(args, "--session", providerSessionID)
	} else if request.SessionID != "" {
		args = append(args, "--title", request.SessionID)
	}
	args = append(args, request.Prompt)
	return agent.PromptCommand{Command: agent.Command{Name: "opencode", Args: args, Dir: request.Workdir}}
}
