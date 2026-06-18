package gemini

import "github.com/obot-platform/discobox/prompter/internal/agent"

func init() {
	agent.Register(agent.StaticDetector{
		AgentKind: agent.KindGeminiCLI,
		EnvEquals: map[string]string{
			"GEMINI_CLI": "1",
		},
	})
	agent.Register(agent.StaticDetector{
		AgentKind:    agent.KindGeminiCLI,
		ProcessNames: []string{"gemini"},
	})
}
