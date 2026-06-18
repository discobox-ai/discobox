package discobot

import "github.com/obot-platform/discobox/prompter/internal/agent"

func init() {
	agent.Register(agent.StaticDetector{
		AgentKind: agent.KindDiscobot,
		EnvKeys:   []string{"DISCOBOT_SESSION_ID"},
	})
	agent.Register(agent.StaticDetector{
		AgentKind:    agent.KindDiscobot,
		ProcessNames: []string{"disco", "discobot"},
	})
}
