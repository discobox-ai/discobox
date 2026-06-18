package codex

import "github.com/obot-platform/discobox/prompter/internal/agent"

func init() {
	agent.Register(agent.StaticDetector{
		AgentKind: agent.KindCodex,
		EnvKeys:   []string{"CODEX_THREAD_ID"},
	})
	agent.Register(agent.StaticDetector{
		AgentKind:    agent.KindCodex,
		ProcessNames: []string{"codex", "openai-codex"},
	})
}
